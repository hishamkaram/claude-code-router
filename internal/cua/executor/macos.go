package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	defaultMacOSHelperPath      = "ccr-cua-macos"
	defaultMacOSStartupTimeout  = 10 * time.Second
	defaultMacOSShutdownTimeout = 5 * time.Second
)

// MacOSPreviewOptions configures the launch-scoped unsigned macOS preview
// helper. The helper token is learned from ready IPC and is never accepted in
// options, command arguments, or environment.
type MacOSPreviewOptions struct {
	Runner          CommandRunner
	HelperPath      string
	Args            []string
	Dir             string
	CommandEnvBase  []string
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

// MacOSPreview owns one ccr-cua-macos helper process for one CCR launch.
type MacOSPreview struct {
	process *managedProcess

	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter

	ctx    context.Context
	cancel context.CancelFunc

	responses       chan macOSReadResult
	readerDone      chan struct{}
	contextDone     chan struct{}
	closePipesOnce  sync.Once
	closeOnce       sync.Once
	closeErr        error
	shutdownTimeout time.Duration

	mu          sync.Mutex
	closed      bool
	token       string
	requestSeq  uint64
	commandSpec CommandSpec
}

type macOSReadResult struct {
	message macOSProtocolMessage
	err     error
}

// NewMacOSPreview starts ccr-cua-macos, validates ready/start JSONL IPC, and
// returns a launch-scoped executor. It is only supported on darwin.
func NewMacOSPreview(ctx context.Context, opts MacOSPreviewOptions) (*MacOSPreview, error) {
	return newMacOSPreview(ctx, opts, runtime.GOOS)
}

func newMacOSPreview(ctx context.Context, opts MacOSPreviewOptions, goos string) (*MacOSPreview, error) {
	if goos != "darwin" {
		return nil, fmt.Errorf("macOS CUA preview executor requires darwin")
	}
	if ctx == nil {
		return nil, fmt.Errorf("macOS CUA preview context is required")
	}
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}
	prepared, err := prepareMacOSPreview(opts)
	if err != nil {
		return nil, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	process, err := startManagedProcess(processCtx, runner, prepared.command)
	if err != nil {
		cancel()
		prepared.closePipes()
		return nil, fmt.Errorf("starting macOS CUA helper: %w", err)
	}
	executor := &MacOSPreview{
		process:         process,
		stdinReader:     prepared.stdinReader,
		stdinWriter:     prepared.stdinWriter,
		stdoutReader:    prepared.stdoutReader,
		stdoutWriter:    prepared.stdoutWriter,
		ctx:             processCtx,
		cancel:          cancel,
		responses:       make(chan macOSReadResult, 4),
		readerDone:      make(chan struct{}),
		contextDone:     make(chan struct{}),
		shutdownTimeout: prepared.shutdownTimeout,
		commandSpec:     prepared.command.clone(),
	}
	executor.startReader()
	executor.watchContext()

	startupCtx, startupCancel := context.WithTimeout(ctx, prepared.startupTimeout)
	defer startupCancel()
	if startErr := executor.start(startupCtx); startErr != nil {
		return nil, errors.Join(startErr, executor.Shutdown(ctx))
	}
	return executor, nil
}

func (executor *MacOSPreview) Name() string {
	return string(cua.ExecutorMacOSPreview)
}

func (executor *MacOSPreview) Check(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("checking macOS CUA helper: context is required")
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("checking macOS CUA helper: %w", ctx.Err())
	case <-executor.ctx.Done():
		return fmt.Errorf("checking macOS CUA helper: %w", executor.ctx.Err())
	default:
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.closed {
		return fmt.Errorf("checking macOS CUA helper: %w", cua.ErrManagedClosed)
	}
	select {
	case <-executor.process.done:
		return fmt.Errorf("checking macOS CUA helper: %w", executor.process.Wait())
	default:
		return nil
	}
}

func (executor *MacOSPreview) Execute(ctx context.Context, action cua.Action) (cua.Observation, error) {
	if ctx == nil {
		return cua.Observation{}, fmt.Errorf("executing macOS CUA action: context is required")
	}
	if err := action.Validate(); err != nil {
		return cua.Observation{}, err
	}
	payload, redactions, err := buildMacOSActionPayload(action)
	if err != nil {
		return cua.Observation{}, err
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.closed {
		return cua.Observation{}, fmt.Errorf("executing macOS CUA action: %w", cua.ErrManagedClosed)
	}
	id := executor.nextRequestID("action")
	request := macOSRequest{
		Version: macOSProtocolVersion,
		ID:      id,
		Type:    "action",
		Token:   executor.token,
		Action:  payload,
	}
	message, err := executor.requestLocked(ctx, request, "result", id, string(action.Kind), redactions)
	if err != nil {
		return cua.Observation{}, err
	}
	if action.Kind == cua.ActionScreenshot {
		return cua.Observation{
			Screenshot:  append([]byte(nil), message.Result.Screenshot...),
			ContentType: message.Result.ContentType,
		}, nil
	}
	return cua.Observation{}, nil
}

func (executor *MacOSPreview) Close() error {
	return executor.Shutdown(context.Background())
}

func (executor *MacOSPreview) Shutdown(ctx context.Context) error {
	if executor == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("shutting down macOS CUA helper: context is required")
	}
	executor.closeOnce.Do(func() {
		executor.closeErr = executor.close(ctx)
	})
	return executor.closeErr
}

func (executor *MacOSPreview) CommandSpec() CommandSpec {
	if executor == nil {
		return CommandSpec{}
	}
	return executor.commandSpec.clone()
}

func (executor *MacOSPreview) start(ctx context.Context) error {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	ready, err := executor.nextMessageLocked(ctx)
	if err != nil {
		return executor.failLocked(fmt.Errorf("reading macOS CUA ready message: %w", err))
	}
	if ready.Type != "ready" {
		return executor.failLocked(fmt.Errorf("macOS CUA helper sent %q before ready", ready.Type))
	}
	executor.token = ready.Token
	id := executor.nextRequestID("start")
	request := macOSRequest{
		Version: macOSProtocolVersion,
		ID:      id,
		Type:    "start",
		Token:   executor.token,
	}
	if _, err := executor.requestLocked(ctx, request, "started", id, "", nil); err != nil {
		return err
	}
	return nil
}

func (executor *MacOSPreview) close(ctx context.Context) error {
	executor.mu.Lock()
	if executor.closed {
		executor.mu.Unlock()
		return executor.finishClosed(ctx)
	}
	executor.closed = true
	id := executor.nextRequestID("close")
	request := macOSRequest{
		Version: macOSProtocolVersion,
		ID:      id,
		Type:    "close",
		Token:   executor.token,
	}
	closeCtx, cancel := macOSShutdownContext(ctx, executor.shutdownTimeout)
	_, requestErr := executor.requestLocked(closeCtx, request, "closed", id, "", nil)
	cancel()
	executor.mu.Unlock()
	if requestErr != nil {
		return executor.abort(errors.Join(fmt.Errorf("closing macOS CUA helper: %w", requestErr)))
	}
	_ = executor.stdinWriter.Close()
	return executor.finishClosed(ctx)
}

func (executor *MacOSPreview) finishClosed(ctx context.Context) error {
	waitCtx, cancel := macOSShutdownContext(ctx, executor.shutdownTimeout)
	defer cancel()
	waitErr := executor.waitProcess(waitCtx)
	executor.cancel()
	executor.closePipes()
	executor.waitReader()
	<-executor.contextDone
	return waitErr
}

func macOSShutdownContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (executor *MacOSPreview) requestLocked(
	ctx context.Context,
	request macOSRequest,
	expectedType string,
	expectedID string,
	expectedAction string,
	redactions []string,
) (macOSProtocolMessage, error) {
	redactions = append(redactions, executor.token)
	if err := executor.writeRequestLocked(ctx, request); err != nil {
		return macOSProtocolMessage{}, executor.failLocked(fmt.Errorf("writing macOS CUA %s request: %w", request.Type, err))
	}
	message, err := executor.nextMessageLocked(ctx)
	if err != nil {
		return macOSProtocolMessage{}, executor.failLocked(fmt.Errorf("reading macOS CUA %s response: %w", request.Type, err))
	}
	if err := validateMacOSResponse(message, expectedType, expectedID, expectedAction, redactions); err != nil {
		if message.Error != nil {
			return macOSProtocolMessage{}, err
		}
		return macOSProtocolMessage{}, executor.failLocked(err)
	}
	return message, nil
}

func (executor *MacOSPreview) writeRequestLocked(ctx context.Context, request macOSRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encoding macOS CUA request: %w", err)
	}
	data = append(data, '\n')
	done := make(chan error, 1)
	go func() {
		_, writeErr := executor.stdinWriter.Write(data)
		done <- writeErr
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("writing JSONL request: %w", err)
		}
		return nil
	case <-ctx.Done():
		executor.closePipes()
		err := <-done
		return errors.Join(ctx.Err(), err)
	case <-executor.ctx.Done():
		executor.closePipes()
		err := <-done
		return errors.Join(executor.ctx.Err(), err)
	}
}

func (executor *MacOSPreview) nextMessageLocked(ctx context.Context) (macOSProtocolMessage, error) {
	if message, ok, err := executor.pendingMessage(); ok || err != nil {
		return message, err
	}
	select {
	case result := <-executor.responses:
		if result.err != nil {
			return macOSProtocolMessage{}, result.err
		}
		return result.message, nil
	case <-ctx.Done():
		return macOSProtocolMessage{}, ctx.Err()
	case <-executor.ctx.Done():
		return macOSProtocolMessage{}, executor.ctx.Err()
	case <-executor.process.done:
		return executor.messageAfterProcessExit()
	}
}

func (executor *MacOSPreview) pendingMessage() (macOSProtocolMessage, bool, error) {
	select {
	case result := <-executor.responses:
		if result.err != nil {
			return macOSProtocolMessage{}, true, result.err
		}
		return result.message, true, nil
	default:
		return macOSProtocolMessage{}, false, nil
	}
}

func (executor *MacOSPreview) messageAfterProcessExit() (macOSProtocolMessage, error) {
	if message, ok, err := executor.pendingMessage(); ok || err != nil {
		return message, err
	}
	_ = executor.stdoutWriter.Close()
	select {
	case result := <-executor.responses:
		if result.err != nil {
			return macOSProtocolMessage{}, result.err
		}
		return result.message, nil
	case <-executor.readerDone:
		if message, ok, err := executor.pendingMessage(); ok || err != nil {
			return message, err
		}
	}
	if err := executor.process.Wait(); err != nil {
		return macOSProtocolMessage{}, fmt.Errorf("macOS CUA helper exited: %w", err)
	}
	return macOSProtocolMessage{}, fmt.Errorf("macOS CUA helper exited")
}

func (executor *MacOSPreview) failLocked(err error) error {
	executor.closed = true
	return executor.abort(err)
}

func (executor *MacOSPreview) abort(err error) error {
	executor.cancel()
	executor.closePipes()
	var stopErr error
	if executor.process != nil {
		stopErr = errors.Join(executor.process.kill(), executor.process.Wait())
	}
	executor.waitReader()
	<-executor.contextDone
	return errors.Join(err, stopErr)
}

func (executor *MacOSPreview) waitProcess(ctx context.Context) error {
	select {
	case <-executor.process.done:
		return executor.process.Wait()
	case <-ctx.Done():
		killErr := executor.process.kill()
		waitErr := executor.process.Wait()
		return errors.Join(ctx.Err(), killErr, waitErr)
	}
}

func (executor *MacOSPreview) startReader() {
	go func() {
		defer close(executor.readerDone)
		scanner := bufio.NewScanner(executor.stdoutReader)
		scanner.Buffer(make([]byte, 0, 64*1024), macOSMaxLineBytes)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			message, err := decodeMacOSProtocolMessage(line)
			if !executor.deliver(macOSReadResult{message: message, err: err}) {
				return
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			_ = executor.deliver(macOSReadResult{err: fmt.Errorf("reading macOS CUA JSONL: %w", err)})
			return
		}
		_ = executor.deliver(macOSReadResult{err: io.EOF})
	}()
}

func (executor *MacOSPreview) watchContext() {
	go func() {
		defer close(executor.contextDone)
		<-executor.ctx.Done()
		executor.closePipes()
	}()
}

func (executor *MacOSPreview) deliver(result macOSReadResult) bool {
	select {
	case executor.responses <- result:
		return true
	case <-executor.ctx.Done():
		return false
	}
}

func (executor *MacOSPreview) closePipes() {
	executor.closePipesOnce.Do(func() {
		_ = executor.stdinWriter.Close()
		_ = executor.stdinReader.Close()
		_ = executor.stdoutReader.Close()
		_ = executor.stdoutWriter.Close()
	})
}

func (executor *MacOSPreview) waitReader() {
	<-executor.readerDone
}

func (executor *MacOSPreview) nextRequestID(prefix string) string {
	executor.requestSeq++
	return prefix + "-" + strconv.FormatUint(executor.requestSeq, 10)
}

type preparedMacOSPreview struct {
	command         CommandSpec
	stdinReader     *io.PipeReader
	stdinWriter     *io.PipeWriter
	stdoutReader    *io.PipeReader
	stdoutWriter    *io.PipeWriter
	startupTimeout  time.Duration
	shutdownTimeout time.Duration
}

func (prepared preparedMacOSPreview) closePipes() {
	_ = prepared.stdinWriter.Close()
	_ = prepared.stdinReader.Close()
	_ = prepared.stdoutReader.Close()
	_ = prepared.stdoutWriter.Close()
}

func prepareMacOSPreview(opts MacOSPreviewOptions) (preparedMacOSPreview, error) {
	helperPath := valueOrDefault(opts.HelperPath, defaultMacOSHelperPath)
	if strings.TrimSpace(helperPath) == "" {
		return preparedMacOSPreview{}, fmt.Errorf("macOS CUA helper path is required")
	}
	startupTimeout := durationOrDefault(opts.StartupTimeout, defaultMacOSStartupTimeout)
	shutdownTimeout := durationOrDefault(opts.ShutdownTimeout, defaultMacOSShutdownTimeout)
	if startupTimeout <= 0 {
		return preparedMacOSPreview{}, fmt.Errorf("macOS CUA startup timeout must be positive")
	}
	if shutdownTimeout <= 0 {
		return preparedMacOSPreview{}, fmt.Errorf("macOS CUA shutdown timeout must be positive")
	}
	envBase := opts.CommandEnvBase
	if envBase == nil {
		envBase = os.Environ()
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	return preparedMacOSPreview{
		command: CommandSpec{
			Path:   helperPath,
			Args:   append([]string(nil), opts.Args...),
			Env:    sanitizedCommandEnv(envBase),
			Dir:    opts.Dir,
			Stdin:  stdinReader,
			Stdout: stdoutWriter,
			Stderr: io.Discard,
		},
		stdinReader:     stdinReader,
		stdinWriter:     stdinWriter,
		stdoutReader:    stdoutReader,
		stdoutWriter:    stdoutWriter,
		startupTimeout:  startupTimeout,
		shutdownTimeout: shutdownTimeout,
	}, nil
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

type macOSRequest struct {
	Version int                 `json:"version"`
	ID      string              `json:"id"`
	Type    string              `json:"type"`
	Token   string              `json:"token"`
	Action  *macOSActionPayload `json:"action,omitempty"`
}

func validateMacOSResponse(
	message macOSProtocolMessage,
	expectedType string,
	expectedID string,
	expectedAction string,
	redactions []string,
) error {
	if message.Error != nil {
		if message.ID != expectedID {
			return fmt.Errorf("macOS CUA helper error response had unexpected id")
		}
		return macOSHelperError{failure: *message.Error, redactions: redactions}
	}
	if message.Type != expectedType {
		return fmt.Errorf("macOS CUA helper sent %q, expected %q", message.Type, expectedType)
	}
	if message.ID != expectedID {
		return fmt.Errorf("macOS CUA helper response had unexpected id")
	}
	if expectedAction != "" && message.Action != expectedAction {
		return fmt.Errorf("macOS CUA helper result had unexpected action")
	}
	return nil
}

type macOSHelperError struct {
	failure    macOSFailure
	redactions []string
}

func (failure macOSHelperError) Error() string {
	message := sanitizeMacOSMessage(failure.failure.Message, failure.redactions)
	if len(failure.failure.Permissions) == 0 {
		return fmt.Sprintf("macOS CUA helper returned %s: %s", failure.failure.Code, message)
	}
	return fmt.Sprintf("macOS CUA helper returned %s for permissions %s: %s",
		failure.failure.Code,
		strings.Join(failure.failure.Permissions, ","),
		message,
	)
}

func sanitizeMacOSMessage(message string, redactions []string) string {
	for _, value := range redactions {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
	}
	return message
}
