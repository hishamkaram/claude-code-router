//go:build linux || darwin

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type ExecRequest struct {
	Path string
	Args []string
}

// ExecGated is invoked only by the internal child entrypoint. EOF means abort.
func ExecGated() error {
	f := os.NewFile(3, "job-exec-gate")
	if f == nil {
		return fmt.Errorf("missing execution gate")
	}
	var request ExecRequest
	err := json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&request)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("reading execution gate: %w", err)
	}
	if request.Path == "" {
		return fmt.Errorf("missing workload executable")
	}
	if err := syscall.Exec(request.Path, append([]string{request.Path}, request.Args...), os.Environ()); err != nil {
		return fmt.Errorf("executing job workload: %w", err)
	}
	return nil
}

type Process struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan error
	mu       sync.Mutex
	cleanup  Cleanup
	backend  string
	exitCode *int
	input    *processInput
}

type ProcessConfig struct {
	JobID      string
	Executable string
	Path       string
	Args       []string
	Env        []string
	Input      []byte
	Out        *os.File
	Err        *os.File
}

func StartProcess(ctx context.Context, cfg ProcessConfig) (*Process, error) {
	return startProcess(ctx, cfg, true)
}

func startProcess(ctx context.Context, cfg ProcessConfig, useScope bool) (*Process, error) {
	if err := ValidateID(cfg.JobID); err != nil {
		return nil, err
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()
	var s *scope
	var capabilityErr error
	if useScope {
		s, capabilityErr = openScope(setupCtx, cfg.JobID)
	}
	read, write, err := os.Pipe()
	if err != nil {
		if s != nil {
			s.Close(ctx)
		}
		return nil, fmt.Errorf("creating workload gate: %w", err)
	}
	defer func() { _ = read.Close() }()
	defer func() { _ = write.Close() }()
	input, err := newProcessInput(cfg.Input)
	if err != nil {
		if s != nil {
			s.Close(ctx)
		}
		return nil, err
	}
	defer input.closeRead()
	// Explicit streams and cancellation permit releasing a migrated child
	// without retaining any os/exec pipe-copy or cancellation goroutines.
	cmd := exec.Command(cfg.Executable, "_job-exec")
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Env, input.read, cfg.Out, cfg.Err
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		input.close()
		if s != nil {
			s.Close(ctx)
		}
		return nil, fmt.Errorf("starting gated workload: %w", err)
	}
	if s != nil {
		if err := s.Admit(setupCtx, cmd.Process.Pid); err != nil {
			input.close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			s.Close(ctx)
			return nil, fmt.Errorf("admitting workload scope: %w", err)
		}
	}
	if err := setupCtx.Err(); err != nil {
		input.close()
		return nil, abortGated(ctx, cmd, s, err)
	}
	if err := json.NewEncoder(write).Encode(ExecRequest{Path: cfg.Path, Args: cfg.Args}); err != nil {
		input.close()
		return nil, abortGated(ctx, cmd, s, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	p := &Process{cmd: cmd, cancel: cancel, done: make(chan error, 1), backend: "process-group", input: input}
	if s != nil {
		p.backend = "systemd-scope"
	}
	p.cleanup = Cleanup{Coverage: "unknown", Reason: "workload has not finished"}
	go p.run(runCtx, s, capabilityErr)
	return p, nil
}

func abortGated(ctx context.Context, cmd *exec.Cmd, s *scope, cause error) error {
	if s != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cause = errors.Join(cause, s.Stop(stopCtx))
		s.Close(ctx)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return fmt.Errorf("releasing workload gate: %w", cause)
}

func (p *Process) run(ctx context.Context, s *scope, capabilityErr error) {
	defer p.cancel()
	defer p.input.close()
	observerCtx, stopObserver := context.WithCancel(context.WithoutCancel(ctx))
	defer stopObserver()
	exited := make(chan error, 1)
	go func() { exited <- observeChildExit(observerCtx, p.cmd.Process.Pid) }()
	var observationErr error
	select {
	case observationErr = <-exited:
		exited = nil
	case <-ctx.Done():
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if s == nil && errors.Is(observationErr, unix.ECHILD) {
		// Another reaper would invalidate the pinned-PID invariant. Withhold all
		// signaling rather than attempting to reconstruct process ownership.
		p.mu.Lock()
		p.cleanup = Cleanup{Coverage: "unknown", Reason: "owned child identity no longer waitable; no signal attempted"}
		p.mu.Unlock()
		p.done <- errors.Join(observationErr, p.cmd.Wait())
		close(p.done)
		return
	}
	cleanup, stopErr := p.stopOwned(cleanupCtx, s, capabilityErr)
	if exited != nil {
		select {
		case observationErr = <-exited:
		case <-cleanupCtx.Done():
			stopObserver()
			observationErr = <-exited
		}
	}
	if observationErr != nil {
		p.releaseUnconfirmed(observationErr, stopErr)
		return
	}
	// No group operation is permitted after Wait releases the child PID.
	waitErr := p.cmd.Wait()
	cleanup.Normalize()
	p.mu.Lock()
	p.cleanup = cleanup
	if p.cmd.ProcessState != nil {
		code := p.cmd.ProcessState.ExitCode()
		p.exitCode = &code
	}
	p.mu.Unlock()
	p.done <- errors.Join(waitErr, stopErr, observationErr)
	close(p.done)
}

func (p *Process) releaseUnconfirmed(observationErr, stopErr error) {
	p.mu.Lock()
	p.cleanup = Cleanup{Coverage: "unknown", Reason: "owned child exit unconfirmed after cleanup; no additional signal attempted", Survivors: []Survivor{{PID: p.cmd.Process.Pid}}, Observed: []string{"owned_child_exit_unconfirmed"}}
	p.cleanup.Normalize()
	p.mu.Unlock()
	// D-Bus migration may leave this child outside the scope. Relinquish the
	// handle rather than hang the owner or signal an external unit.
	err := p.cmd.Process.Release()
	p.done <- errors.Join(observationErr, stopErr, err)
	close(p.done)
}

func (p *Process) stopOwned(ctx context.Context, s *scope, capabilityErr error) (Cleanup, error) {
	if s != nil {
		defer s.Close(ctx)
		err := s.Stop(ctx)
		cleanup := s.Observe(ctx)
		if err != nil {
			cleanup = Cleanup{Coverage: "unknown", Reason: "scope stop could not be confirmed"}
		}
		return cleanup, err
	}
	// The direct child remains unreaped here, pinning its dedicated group ID.
	pid := p.cmd.Process.Pid
	if pid <= 1 {
		return Cleanup{Coverage: "unknown", Reason: "invalid owned child identity"}, fmt.Errorf("invalid owned child identity")
	}
	err := unix.Kill(-pid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		err = nil
	}
	reason := "process-group cleanup cannot exclude escaped descendants"
	if capabilityErr != nil {
		reason += "; systemd scope unavailable"
	}
	cleanup := observeGroup(ctx, pid)
	cleanup.Unobservable = []string{"escaped_descendants"}
	if cleanup.Coverage == "partial" {
		cleanup.Reason = reason
	}
	return cleanup, err
}

func (p *Process) PID() int           { return p.cmd.Process.Pid }
func (p *Process) Done() <-chan error { return p.done }
func (p *Process) Stop() error        { p.cancel(); return nil }
func (p *Process) Backend() string    { return p.backend }
func (p *Process) Result() (cleanup Cleanup, code *int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.cleanup
	c.Normalize()
	return c, p.exitCode
}
