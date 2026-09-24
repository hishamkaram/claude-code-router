//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func newJobOwnerCommand(ctx context.Context, deps Dependencies) *cobra.Command {
	return &cobra.Command{Use: "_job-owner", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return runJobOwner(ctx, cmd, deps) }}
}

func runJobOwner(ctx context.Context, cmd *cobra.Command, deps Dependencies) error {
	input, ack, lease := os.NewFile(3, "job-admission"), os.NewFile(4, "job-ack"), os.NewFile(5, "job-lease")
	defer func() { _ = input.Close() }()
	defer func() { _ = ack.Close() }()
	defer func() { _ = lease.Close() }()
	for fd := 3; fd <= 6; fd++ {
		unix.CloseOnExec(fd)
	}
	var admission jobAdmission
	if err := json.NewDecoder(io.LimitReader(input, 32<<20)).Decode(&admission); err != nil {
		return fmt.Errorf("reading job admission: %w", err)
	}
	_ = input.Close()
	s := jobs.Store{Root: admission.Root}
	r, err := s.Read(admission.ID)
	if err != nil {
		return err
	}
	if r.Terminal() {
		return fmt.Errorf("cannot restart a terminal job")
	}
	registry, sessionLease, err := adoptJobAdmission(ctx, s, r, admission)
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	defer func() { _ = sessionLease.Close() }()

	if reapErr := jobs.EnableSubreaper(); reapErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Job orphan reaping unavailable")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if socketErr := s.PrepareControlSocket(r.JobID, lease); socketErr != nil {
		return socketErr
	}
	owner := jobs.NewOwner(s, r, cancel)
	listener, err := owner.Listen()
	if err != nil {
		return err
	}
	server := &http.Server{Handler: owner, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-served }()
	if err := s.Write(r); err != nil {
		return err
	}
	// Receipt delivery cannot revoke an admitted owner when its caller vanished.
	_ = json.NewEncoder(ack).Encode(jobReceipt{JobID: r.JobID, SessionID: r.SessionID, SubmissionID: r.SubmissionID})
	_ = ack.Close()
	return executeOwnedJob(runCtx, cmd, deps, admission, r, owner, registry, sessionLease)
}

func executeOwnedJob(ctx context.Context, cmd *cobra.Command, deps Dependencies, admission jobAdmission, record jobs.Record, owner *jobs.Owner, registry *jobs.Registry, lease *jobs.SessionLease) error {
	invocation, err := parseLaunchInvocation(admission.Args)
	if err != nil {
		return owner.FinishAdmission(context.WithoutCancel(ctx), registry, jobs.FinalOutcome{RunError: err})
	}
	sessionFlag := "--session-id"
	if record.RequestedResumeSession != "" {
		sessionFlag = "--resume"
	}
	invocation.claudeArgs = append([]string{sessionFlag, record.SessionID}, invocation.claudeArgs...)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	accounting := gateway.NewRequestAccounting()
	deps.RequestAccounting = accounting
	launcher := &jobClaudeLauncher{id: record.JobID, owner: owner, out: os.Stdout, errOut: os.Stderr}
	launcher.beforeRelease = func(gateCtx context.Context) error {
		digest, digestErr := executionFingerprint(gateCtx, &options{dbPath: admission.DB}, invocation)
		if digestErr != nil {
			return digestErr
		}
		return registry.CommitExecution(gateCtx, lease, admission.SubmissionID, digest)
	}
	deps.Launcher = launcher
	cmd.SetIn(bytes.NewReader(admission.Prompt))
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	observed := streamJSONJob(invocation.claudeArgs)
	var observer *jobs.OutputObserver
	var observationErr error
	deps.launchPrepared = func(resolved resolvedLaunch) error {
		record.ExpectedModel = resolved.claudeModelID
		if err := owner.ExpectedModel(record.ExpectedModel); err != nil {
			return err
		}
		if observed {
			observer, observationErr = jobs.StartOutputObserver(context.WithoutCancel(ctx), record.Log, record.SessionID, record.ExpectedModel, cancel)
		}
		return observationErr
	}
	runErr := runLaunch(runCtx, cmd, &options{dbPath: admission.DB}, deps, invocation)
	if runErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Detached job execution failed:", runErr)
	}
	outcome := collectJobOutcome(ctx, launcher, accounting, observer, record, observed, runErr)
	outcome.ObservationError = errors.Join(outcome.ObservationError, observationErr)
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	return owner.FinishAdmission(finishCtx, registry, outcome)
}

type jobClaudeLauncher struct {
	id            string
	owner         *jobs.Owner
	out           *os.File
	errOut        *os.File
	mu            sync.Mutex
	process       *jobs.Process
	beforeRelease func(context.Context) error
}

func (l *jobClaudeLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, in io.Reader, _, _ io.Writer) (ClaudeProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving job executable: %w", err)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("resolving Claude Code: %w", err)
	}
	prompt, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("reading admitted job prompt: %w", err)
	}
	p, err := jobs.StartProcess(ctx, jobs.ProcessConfig{BeforeRelease: l.beforeRelease, JobID: l.id, Executable: executable, Path: claude, Args: args, Env: applyClaudeEnvironment(os.Environ(), env), Input: prompt, Out: l.out, Err: l.errOut})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.process = p
	l.mu.Unlock()
	if err := l.owner.Backend(p.Backend()); err != nil {
		_ = p.Stop()
		return nil, errors.Join(err, <-p.Done())
	}
	return p, nil
}

func (l *jobClaudeLauncher) result() (cleanup jobs.Cleanup, code *int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.process == nil {
		return jobs.Cleanup{Coverage: "unknown", Reason: "workload not started"}, nil
	}
	// A foreground shutdown timeout must not publish a detached terminal receipt
	// while the owned subprocess/reaper is still active.
	_ = l.process.Stop()
	<-l.process.Done()
	return l.process.Result()
}
