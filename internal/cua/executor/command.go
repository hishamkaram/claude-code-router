package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CommandSpec describes one subprocess launch without inheriting stdio by default.
type CommandSpec struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (spec CommandSpec) clone() CommandSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	return spec
}

// CommandRunner starts subprocesses for launch-scoped executors.
type CommandRunner interface {
	Start(context.Context, CommandSpec) (Process, error)
}

// Process is the owned child process returned by a CommandRunner.
type Process interface {
	PID() int
	Wait() error
	Kill() error
}

// OSCommandRunner starts real operating-system processes.
type OSCommandRunner struct{}

func (OSCommandRunner) Start(ctx context.Context, spec CommandSpec) (Process, error) {
	if ctx == nil {
		return nil, fmt.Errorf("executor command context is required")
	}
	if strings.TrimSpace(spec.Path) == "" {
		return nil, fmt.Errorf("executor command path is required")
	}
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = append([]string(nil), spec.Env...)
	cmd.Dir = spec.Dir
	cmd.Stdin = spec.Stdin
	cmd.Stdout = writerOrDiscard(spec.Stdout)
	cmd.Stderr = writerOrDiscard(spec.Stderr)
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting executor command %q: %w", spec.Path, err)
	}
	return osProcess{cmd: cmd}, nil
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer != nil {
		return writer
	}
	return io.Discard
}

type osProcess struct {
	cmd *exec.Cmd
}

func (process osProcess) PID() int {
	if process.cmd == nil || process.cmd.Process == nil {
		return 0
	}
	return process.cmd.Process.Pid
}

func (process osProcess) Wait() error {
	if process.cmd == nil {
		return nil
	}
	if err := process.cmd.Wait(); err != nil {
		return fmt.Errorf("waiting for executor command: %w", err)
	}
	return nil
}

func (process osProcess) Kill() error {
	if process.cmd == nil || process.cmd.Process == nil {
		return nil
	}
	if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing executor command: %w", err)
	}
	return nil
}

type managedProcess struct {
	process   Process
	done      chan struct{}
	watchDone chan struct{}

	mu        sync.Mutex
	waitErr   error
	killErr   error
	cancelErr error
	killed    bool
	killOnce  sync.Once
}

func startManagedProcess(ctx context.Context, runner CommandRunner, spec CommandSpec) (*managedProcess, error) {
	if ctx == nil {
		return nil, fmt.Errorf("managed executor process context is required")
	}
	process, err := runner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return newManagedProcess(ctx, process), nil
}

func newManagedProcess(ctx context.Context, process Process) *managedProcess {
	managed := &managedProcess{
		process:   process,
		done:      make(chan struct{}),
		watchDone: make(chan struct{}),
	}
	go managed.wait()
	go managed.watchContext(ctx)
	return managed
}

func (managed *managedProcess) wait() {
	err := managed.process.Wait()
	managed.mu.Lock()
	managed.waitErr = err
	managed.mu.Unlock()
	close(managed.done)
}

func (managed *managedProcess) watchContext(ctx context.Context) {
	defer close(managed.watchDone)
	select {
	case <-ctx.Done():
		managed.mu.Lock()
		managed.cancelErr = ctx.Err()
		managed.mu.Unlock()
		_ = managed.kill()
	case <-managed.done:
	}
}

func (managed *managedProcess) Close() error {
	killErr := managed.kill()
	waitErr := managed.Wait()
	return errors.Join(killErr, waitErr)
}

func (managed *managedProcess) Wait() error {
	<-managed.done
	<-managed.watchDone

	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.cancelErr != nil {
		return managed.cancelErr
	}
	if managed.killErr != nil {
		return managed.killErr
	}
	if managed.killed {
		return nil
	}
	return managed.waitErr
}

func (managed *managedProcess) kill() error {
	managed.killOnce.Do(func() {
		select {
		case <-managed.done:
			return
		default:
		}
		managed.mu.Lock()
		managed.killed = true
		managed.mu.Unlock()
		killErr := managed.process.Kill()
		managed.mu.Lock()
		managed.killErr = killErr
		managed.mu.Unlock()
	})
	managed.mu.Lock()
	defer managed.mu.Unlock()
	return managed.killErr
}

func runCommandToExit(ctx context.Context, runner CommandRunner, spec CommandSpec) error {
	managed, err := startManagedProcess(ctx, runner, spec)
	if err != nil {
		return err
	}
	return managed.Wait()
}

func sanitizedCommandEnv(base []string) []string {
	return allowlistedCommandEnv(base, map[string]struct{}{"PATH": {}})
}

func dockerCommandEnv(base []string) []string {
	return allowlistedCommandEnv(base, map[string]struct{}{
		"PATH":                    {},
		"HOME":                    {},
		"XDG_CONFIG_HOME":         {},
		"XDG_RUNTIME_DIR":         {},
		"DOCKER_API_VERSION":      {},
		"DOCKER_CERT_PATH":        {},
		"DOCKER_CONFIG":           {},
		"DOCKER_CONTEXT":          {},
		"DOCKER_CLI_EXPERIMENTAL": {},
		"DOCKER_HOST":             {},
		"DOCKER_TLS_VERIFY":       {},
		"SSH_AUTH_SOCK":           {},
		"HTTP_PROXY":              {},
		"HTTPS_PROXY":             {},
		"NO_PROXY":                {},
		"ALL_PROXY":               {},
		"http_proxy":              {},
		"https_proxy":             {},
		"no_proxy":                {},
		"all_proxy":               {},
	})
}

func allowlistedCommandEnv(base []string, allowed map[string]struct{}) []string {
	env := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		if _, ok := allowed[name]; !ok {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		env = append(env, entry)
		seen[name] = struct{}{}
	}
	return env
}
