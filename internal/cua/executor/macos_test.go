package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	macOSTestToken = "test-session-token"
	macOSTestReady = `{"version":1,"type":"ready","preview":true,"token":"` + macOSTestToken + `","actions":["screenshot","click","double_click","drag","move","type","keypress","scroll","wait"]}`
)

func TestMacOSPreviewExecutesScreenshotWithTypedJSONL(t *testing.T) {
	t.Parallel()

	runner := newMacOSScriptRunner(func(_ context.Context, spec CommandSpec, _ <-chan struct{}) error {
		if err := writeMacOSTestLine(spec.Stdout, macOSTestReady); err != nil {
			return err
		}
		scanner := bufio.NewScanner(spec.Stdin)
		start, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if assertErr := assertMacOSTestRequest(start, "start"); assertErr != nil {
			return assertErr
		}
		if start.Action != nil {
			return fmt.Errorf("start request included action")
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+start.ID+`","type":"started","preview":true,"permissions":{"accessibility":true,"screen_recording":true}}`); writeErr != nil {
			return writeErr
		}

		action, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if assertErr := assertMacOSTestRequest(action, "action"); assertErr != nil {
			return assertErr
		}
		if got := action.Action["kind"]; got != "screenshot" || len(action.Action) != 1 {
			return fmt.Errorf("screenshot action payload did not match expected shape")
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+action.ID+`","type":"result","preview":true,"action":"screenshot","result":{"content_type":"image/png","data_base64":"UE5H","width":2,"height":1}}`); writeErr != nil {
			return writeErr
		}

		closeReq, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if assertErr := assertMacOSTestRequest(closeReq, "close"); assertErr != nil {
			return assertErr
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+closeReq.ID+`","type":"closed","preview":true}`); writeErr != nil {
			return writeErr
		}
		return nil
	})

	executor, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{
		Runner:         runner,
		HelperPath:     "/usr/local/bin/ccr-cua-macos",
		Args:           []string{"--fixture"},
		CommandEnvBase: []string{"PATH=/usr/bin", "CCR_TOKEN=not-forwarded", "OPENAI_API_KEY=not-forwarded"},
	}, "darwin")
	if err != nil {
		t.Fatalf("newMacOSPreview() error = %v", err)
	}
	if checkErr := executor.Check(context.Background()); checkErr != nil {
		t.Fatalf("Check() error = %v", checkErr)
	}
	observation, err := executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionScreenshot})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(observation.Screenshot) != "PNG" || observation.ContentType != "image/png" || len(observation.Raw) != 0 {
		t.Fatal("screenshot observation did not match expected in-memory result")
	}
	if closeErr := executor.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	specs := runner.Specs()
	if len(specs) != 1 {
		t.Fatalf("runner recorded %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Path != "/usr/local/bin/ccr-cua-macos" {
		t.Fatalf("command path = %q", spec.Path)
	}
	if strings.Join(spec.Args, "\x00") != "--fixture" {
		t.Fatal("command args did not match expected fixture shape")
	}
	assertOnlyPATHEnv(t, spec.Env)
	joinedSpec := strings.Join(append(append([]string{spec.Path}, spec.Args...), spec.Env...), "\x00")
	for index, forbidden := range []string{macOSTestToken, "CCR_TOKEN", "OPENAI_API_KEY", "not-forwarded"} {
		if strings.Contains(joinedSpec, forbidden) {
			t.Fatalf("command spec exposed forbidden value at index %d", index)
		}
	}
	process := runner.Process()
	if process == nil {
		t.Fatal("runner did not create process")
	}
	if process.KillCount() != 0 {
		t.Fatalf("KillCount() = %d, want 0", process.KillCount())
	}
	waitFor(t, func() bool {
		return process.WaitCount() == 1
	})
}

func TestNewMacOSPreviewRejectsMalformedReadyAndStopsProcess(t *testing.T) {
	t.Parallel()

	const secretToken = "secret-ready-token"
	runner := newMacOSScriptRunner(func(_ context.Context, spec CommandSpec, killed <-chan struct{}) error {
		if err := writeMacOSTestLine(spec.Stdout, `{"version":1,"type":"ready","preview":true,"token":"`+secretToken+`","actions":["screenshot"],"debug":true}`); err != nil {
			return err
		}
		<-killed
		return nil
	})
	_, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{
		Runner:          runner,
		StartupTimeout:  time.Second,
		ShutdownTimeout: time.Second,
	}, "darwin")
	if err == nil {
		t.Fatal("newMacOSPreview() error = nil")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatal("error exposed ready token")
	}
	process := runner.Process()
	if process == nil {
		t.Fatal("runner did not create process")
	}
	waitFor(t, func() bool {
		return process.KillCount() == 1 && process.WaitCount() == 1
	})
}

func TestMacOSPreviewHelperErrorRedactsTokenAndActionText(t *testing.T) {
	t.Parallel()

	const typedSecret = "typed-secret"
	runner := newMacOSScriptRunner(func(_ context.Context, spec CommandSpec, _ <-chan struct{}) error {
		if err := writeMacOSTestLine(spec.Stdout, macOSTestReady); err != nil {
			return err
		}
		scanner := bufio.NewScanner(spec.Stdin)
		start, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+start.ID+`","type":"started","preview":true,"permissions":{"accessibility":true,"screen_recording":true}}`); writeErr != nil {
			return writeErr
		}
		action, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if assertErr := assertMacOSTestRequest(action, "action"); assertErr != nil {
			return assertErr
		}
		if action.Action["kind"] != "type" || action.Action["text"] != typedSecret {
			return fmt.Errorf("type action payload did not match expected shape")
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+action.ID+`","type":"error","preview":true,"error":{"code":"action_failed","message":"bad `+macOSTestToken+` `+typedSecret+`","preview_only":true}}`); writeErr != nil {
			return writeErr
		}
		closeReq, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+closeReq.ID+`","type":"closed","preview":true}`); writeErr != nil {
			return writeErr
		}
		return nil
	})
	executor, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{Runner: runner}, "darwin")
	if err != nil {
		t.Fatalf("newMacOSPreview() error = %v", err)
	}
	_, err = executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionType, Text: typedSecret})
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if strings.Contains(err.Error(), macOSTestToken) || strings.Contains(err.Error(), typedSecret) {
		t.Fatal("Execute() error exposed secret data")
	}
	if closeErr := executor.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
}

func TestMacOSPreviewDragEndpointsRejectPath(t *testing.T) {
	t.Parallel()

	_, _, err := buildMacOSActionPayload(cua.Action{
		CallID: "call_drag",
		Kind:   cua.ActionDrag,
		Raw:    json.RawMessage(`{"type":"drag","path":[{"x":1,"y":2},{"x":10,"y":20},{"x":3,"y":4}]}`),
	})
	if !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("buildMacOSActionPayload() error = %v, want ErrUnsupportedAction", err)
	}
	if !strings.Contains(err.Error(), "path-based drags") {
		t.Fatalf("buildMacOSActionPayload() error = %v, want path-based drag rejection", err)
	}
}

func TestMacOSPreviewDragEndpointsPreserveFromTo(t *testing.T) {
	t.Parallel()

	payload, redacted, err := buildMacOSActionPayload(cua.Action{
		CallID: "call_drag",
		Kind:   cua.ActionDrag,
		Raw:    json.RawMessage(`{"type":"drag","from":{"x":1,"y":2},"to":{"x":3,"y":4}}`),
	})
	if err != nil {
		t.Fatalf("buildMacOSActionPayload() error = %v", err)
	}
	if len(redacted) != 0 {
		t.Fatalf("redacted values = %v, want none", redacted)
	}
	if payload.From == nil || payload.To == nil || payload.From.X != 1 || payload.From.Y != 2 || payload.To.X != 3 || payload.To.Y != 4 {
		t.Fatalf("drag payload = %#v, want from/to endpoints", payload)
	}
}

func TestMacOSPreviewExecuteCancellationStopsProcess(t *testing.T) {
	t.Parallel()

	actionReceived := make(chan struct{})
	runner := newMacOSScriptRunner(func(_ context.Context, spec CommandSpec, killed <-chan struct{}) error {
		if err := writeMacOSTestLine(spec.Stdout, macOSTestReady); err != nil {
			return err
		}
		scanner := bufio.NewScanner(spec.Stdin)
		start, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+start.ID+`","type":"started","preview":true,"permissions":{"accessibility":true,"screen_recording":true}}`); writeErr != nil {
			return writeErr
		}
		if _, actionErr := readMacOSTestRequest(scanner); actionErr != nil {
			return actionErr
		}
		close(actionReceived)
		<-killed
		return nil
	})
	executor, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{
		Runner:          runner,
		ShutdownTimeout: time.Second,
	}, "darwin")
	if err != nil {
		t.Fatalf("newMacOSPreview() error = %v", err)
	}
	actionCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, executeErr := executor.Execute(actionCtx, cua.Action{CallID: "call_1", Kind: cua.ActionScreenshot})
		done <- executeErr
	}()
	select {
	case <-actionReceived:
	case <-time.After(time.Second):
		t.Fatal("helper did not receive action")
	}
	cancel()
	select {
	case executeErr := <-done:
		if !errors.Is(executeErr, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", executeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not return after cancellation")
	}
	process := runner.Process()
	if process == nil {
		t.Fatal("runner did not create process")
	}
	waitFor(t, func() bool {
		return process.KillCount() == 1 && process.WaitCount() == 1
	})
	if closeErr := executor.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("Close() error = %v, want nil or context.Canceled", closeErr)
	}
}

func TestMacOSPreviewCloseWithoutActionsSendsCloseAndWaits(t *testing.T) {
	t.Parallel()

	closeReceived := make(chan struct{})
	runner := newMacOSScriptRunner(func(_ context.Context, spec CommandSpec, _ <-chan struct{}) error {
		if err := writeMacOSTestLine(spec.Stdout, macOSTestReady); err != nil {
			return err
		}
		scanner := bufio.NewScanner(spec.Stdin)
		start, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if writeErr := writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+start.ID+`","type":"started","preview":true,"permissions":{"accessibility":true,"screen_recording":true}}`); writeErr != nil {
			return writeErr
		}
		closeReq, err := readMacOSTestRequest(scanner)
		if err != nil {
			return err
		}
		if assertErr := assertMacOSTestRequest(closeReq, "close"); assertErr != nil {
			return assertErr
		}
		close(closeReceived)
		return writeMacOSTestLine(spec.Stdout, `{"version":1,"id":"`+closeReq.ID+`","type":"closed","preview":true}`)
	})
	executor, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{Runner: runner}, "darwin")
	if err != nil {
		t.Fatalf("newMacOSPreview() error = %v", err)
	}
	if closeErr := executor.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	select {
	case <-closeReceived:
	default:
		t.Fatal("helper did not receive close request")
	}
	process := runner.Process()
	if process == nil {
		t.Fatal("runner did not create process")
	}
	if process.KillCount() != 0 {
		t.Fatalf("KillCount() = %d, want 0", process.KillCount())
	}
	waitFor(t, func() bool {
		return process.WaitCount() == 1
	})
}

func TestNewMacOSPreviewRejectsNonDarwinWithoutStartingProcess(t *testing.T) {
	t.Parallel()

	runner := newMacOSScriptRunner(func(context.Context, CommandSpec, <-chan struct{}) error {
		return fmt.Errorf("runner should not start")
	})
	_, err := newMacOSPreview(context.Background(), MacOSPreviewOptions{Runner: runner}, "linux")
	if err == nil || !strings.Contains(err.Error(), "requires darwin") {
		t.Fatalf("newMacOSPreview() error = %v, want darwin rejection", err)
	}
	if specs := runner.Specs(); len(specs) != 0 {
		t.Fatalf("runner recorded %d specs, want 0", len(specs))
	}
}

func TestMacOSPreviewRejectsPointerModifiers(t *testing.T) {
	t.Parallel()

	_, _, err := buildMacOSActionPayload(cua.Action{
		CallID: "call_1", Kind: cua.ActionClick, X: 1, Y: 2, Keys: []string{"SHIFT"},
	})
	if !IsUnsupportedAction(err) || !strings.Contains(err.Error(), "pointer modifiers") {
		t.Fatalf("buildMacOSActionPayload() error = %v", err)
	}
}

func TestMacOSPreviewNormalizesResponsesKeyAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		keys []string
		want []string
	}{
		{name: "control shortcut", keys: []string{"CTRL", "L"}, want: []string{"control", "l"}},
		{name: "command arrow", keys: []string{"CMD", "ARROWLEFT"}, want: []string{"command", "left"}},
		{name: "meta arrow", keys: []string{"META", "ARROWRIGHT"}, want: []string{"command", "right"}},
		{name: "option arrow", keys: []string{"ALT", "ARROWUP"}, want: []string{"option", "up"}},
		{name: "enter", keys: []string{"ENTER"}, want: []string{"return"}},
		{name: "escape", keys: []string{"ESC"}, want: []string{"escape"}},
		{name: "backspace", keys: []string{"BACKSPACE"}, want: []string{"delete"}},
		{name: "forward delete", keys: []string{"DELETE"}, want: []string{"forward_delete"}},
		{name: "page navigation", keys: []string{"PAGEDOWN"}, want: []string{"page_down"}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload, _, err := buildMacOSActionPayload(cua.Action{
				CallID: "call_1", Kind: cua.ActionKeypress, Keys: test.keys,
			})
			if err != nil {
				t.Fatalf("buildMacOSActionPayload() error = %v", err)
			}
			if payload.Keys == nil || !slices.Equal(*payload.Keys, test.want) {
				t.Fatalf("payload keys = %#v, want %#v", payload.Keys, test.want)
			}
		})
	}
}

func TestMacOSPreviewScrollPayloadRetainsCoordinates(t *testing.T) {
	t.Parallel()

	payload, _, err := buildMacOSActionPayload(cua.Action{
		CallID: "call_1", Kind: cua.ActionScroll, X: 640, Y: 480,
		Raw: json.RawMessage(`{"type":"scroll","x":640,"y":480,"delta_x":12,"delta_y":-24}`),
	})
	if err != nil {
		t.Fatalf("buildMacOSActionPayload() error = %v", err)
	}
	if payload.X == nil || payload.Y == nil || payload.DeltaX == nil || payload.DeltaY == nil {
		t.Fatalf("scroll payload omitted required fields: %#v", payload)
	}
	if *payload.X != 640 || *payload.Y != 480 || *payload.DeltaX != 12 || *payload.DeltaY != -24 {
		t.Fatalf("scroll payload = %#v", payload)
	}
}

func TestMacOSPreviewRejectsNonLeftPointerButtons(t *testing.T) {
	t.Parallel()

	for _, button := range []string{"right", "middle"} {
		button := button
		t.Run(button, func(t *testing.T) {
			t.Parallel()
			_, _, err := buildMacOSActionPayload(cua.Action{
				CallID: "call_1", Kind: cua.ActionClick, X: 1, Y: 2,
				Raw: json.RawMessage(`{"type":"click","x":1,"y":2,"button":"` + button + `"}`),
			})
			if !IsUnsupportedAction(err) || !strings.Contains(err.Error(), "left mouse button") {
				t.Fatalf("buildMacOSActionPayload() error = %v", err)
			}
		})
	}
}

type macOSTestRequest struct {
	Version int            `json:"version"`
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Token   string         `json:"token"`
	Action  map[string]any `json:"action"`
}

func readMacOSTestRequest(scanner *bufio.Scanner) (macOSTestRequest, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return macOSTestRequest{}, err
		}
		return macOSTestRequest{}, io.EOF
	}
	var request macOSTestRequest
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		return macOSTestRequest{}, err
	}
	return request, nil
}

func assertMacOSTestRequest(request macOSTestRequest, requestType string) error {
	if request.Version != macOSProtocolVersion {
		return fmt.Errorf("version = %d", request.Version)
	}
	if request.ID == "" {
		return fmt.Errorf("id is empty")
	}
	if request.Type != requestType {
		return fmt.Errorf("type = %q, want %q", request.Type, requestType)
	}
	if request.Token != macOSTestToken {
		return fmt.Errorf("token was not forwarded in memory")
	}
	return nil
}

func writeMacOSTestLine(writer io.Writer, line string) error {
	_, err := fmt.Fprintln(writer, line)
	return err
}

type macOSScriptRunner struct {
	handler macOSScriptHandler

	mu        sync.Mutex
	specs     []CommandSpec
	processes []*macOSScriptProcess
}

type macOSScriptHandler func(context.Context, CommandSpec, <-chan struct{}) error

func newMacOSScriptRunner(handler macOSScriptHandler) *macOSScriptRunner {
	return &macOSScriptRunner{handler: handler}
}

func (runner *macOSScriptRunner) Start(ctx context.Context, spec CommandSpec) (Process, error) {
	process := newMacOSScriptProcess()
	runner.mu.Lock()
	runner.specs = append(runner.specs, spec.clone())
	runner.processes = append(runner.processes, process)
	runner.mu.Unlock()
	go func() {
		err := runner.handler(ctx, spec, process.killed)
		process.finish(err)
	}()
	return process, nil
}

func (runner *macOSScriptRunner) Specs() []CommandSpec {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	specs := make([]CommandSpec, len(runner.specs))
	for index := range runner.specs {
		specs[index] = runner.specs[index].clone()
	}
	return specs
}

func (runner *macOSScriptRunner) Process() *macOSScriptProcess {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.processes) == 0 {
		return nil
	}
	return runner.processes[0]
}

type macOSScriptProcess struct {
	done     chan struct{}
	killed   chan struct{}
	killOnce sync.Once
	doneOnce sync.Once

	mu        sync.Mutex
	waitCount int
	killCount int
	waitErr   error
}

func newMacOSScriptProcess() *macOSScriptProcess {
	return &macOSScriptProcess{
		done:   make(chan struct{}),
		killed: make(chan struct{}),
	}
}

func (process *macOSScriptProcess) PID() int {
	return 4321
}

func (process *macOSScriptProcess) Wait() error {
	process.mu.Lock()
	process.waitCount++
	process.mu.Unlock()
	<-process.done
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.waitErr
}

func (process *macOSScriptProcess) Kill() error {
	process.mu.Lock()
	process.killCount++
	process.mu.Unlock()
	process.killOnce.Do(func() {
		close(process.killed)
	})
	return nil
}

func (process *macOSScriptProcess) finish(err error) {
	process.mu.Lock()
	process.waitErr = err
	process.mu.Unlock()
	process.doneOnce.Do(func() {
		close(process.done)
	})
}

func (process *macOSScriptProcess) KillCount() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.killCount
}

func (process *macOSScriptProcess) WaitCount() int {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.waitCount
}
