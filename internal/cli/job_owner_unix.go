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
	"path/filepath"
	"strings"
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
	invocation, err := prepareOwnedClaudeInvocation(ctx, admission, &record)
	if err != nil {
		return owner.FinishAdmission(context.WithoutCancel(ctx), registry, jobs.FinalOutcome{RunError: err})
	}
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
	deps.launchProfilePrepared = func(profileDir string) error {
		record.ClaudeProfileDir = profileDir
		return owner.ClaudeProfileDir(profileDir)
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

func prepareOwnedClaudeInvocation(ctx context.Context, admission jobAdmission, record *jobs.Record) (launchInvocation, error) {
	invocation, err := parseLaunchInvocation(admission.Args)
	if err != nil {
		return launchInvocation{}, err
	}
	sessionFlag := "--session-id"
	if record.RequestedResumeSession != "" {
		sessionFlag = "--resume"
		invocation.resumeSession = record.RequestedResumeSession
	}
	invocation.claudeArgs = append([]string{sessionFlag, record.SessionID}, invocation.claudeArgs...)
	// The detached session ID is the durable Claude profile identity. The profile
	// path is persisted in the job record after profile initialization; continuations must
	// reuse it, or migrate that owned profile before a broad native source can
	// contain it. Recomputing a path from current environment state can strand
	// the JSONL session that --resume needs.
	source, err := currentClaudeConfigDir()
	if err != nil {
		return launchInvocation{}, err
	}
	persistedProfile := strings.TrimSpace(record.ClaudeProfileDir)
	requireExistingProfile := record.RequestedResumeSession != "" && persistedProfile != ""
	if record.RequestedResumeSession != "" && record.ResumedFrom != "" {
		// The session authority has already validated and stopped the predecessor
		// in ResumeHead. Read through the store so a genuine schema-1 predecessor
		// can be migrated as well; Registry.JobStatus intentionally accepts only
		// schema-2 records and would reject the compatibility case here.
		predecessor, predecessorErr := (jobs.Store{Root: admission.Root}).StatusContext(ctx, record.ResumedFrom)
		if predecessorErr != nil {
			return launchInvocation{}, fmt.Errorf("reading predecessor job %s for detached Claude profile: %w", record.ResumedFrom, predecessorErr)
		}
		if predecessorProfile := strings.TrimSpace(predecessor.ClaudeProfileDir); predecessorProfile != "" {
			persistedProfile = predecessorProfile
			requireExistingProfile = true
		}
	}
	invocation.claudeProfileDir, err = resolveDetachedClaudeProfileDestination(
		admission.Root, source, record.SessionID, persistedProfile, requireExistingProfile,
	)
	if err != nil {
		return launchInvocation{}, err
	}
	return invocation, nil
}

func detachedClaudeProfileDestination(root, sessionID string) string {
	return filepath.Join(root, "claude-profiles", sessionID)
}

func detachedClaudeProfileDestinationForSource(root, source, sessionID string) (string, error) {
	candidate := detachedClaudeProfileDestination(root, sessionID)
	if !claudePathContains(source, candidate) && claudeProfileBaseWritable(root) {
		return candidate, nil
	}
	for _, base := range claudePersistentProfileStorageBases() {
		candidate = filepath.Join(base, "claude-code-router", "claude-profiles", "detached", sessionID)
		if claudePathContains(source, candidate) || !claudeProfileBaseWritable(base) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("locating detached CCR Claude profile outside source profile %s: no writable profile root is available", source)
}

func resolveDetachedClaudeProfileDestination(root, source, sessionID, persisted string, requireExisting bool) (string, error) {
	persisted = strings.TrimSpace(persisted)
	if persisted == "" {
		return detachedClaudeProfileDestinationForSource(root, source, sessionID)
	}
	if !claudePathContains(source, persisted) {
		return validatePersistedDetachedClaudeProfile(persisted, requireExisting)
	}
	return resolveDetachedClaudeProfileMigration(root, source, sessionID, persisted, requireExisting)
}

func validatePersistedDetachedClaudeProfile(persisted string, requireExisting bool) (string, error) {
	if !requireExisting {
		return persisted, nil
	}
	exists, err := validateClaudeProfileMigrationSource(persisted)
	if err != nil {
		return "", fmt.Errorf("reusing detached CCR Claude profile %s: %w", persisted, err)
	}
	if !exists {
		return "", fmt.Errorf("reusing detached CCR Claude profile %s: profile is missing", persisted)
	}
	return persisted, nil
}

func resolveDetachedClaudeProfileMigration(root, source, sessionID, persisted string, requireExisting bool) (string, error) {
	candidate, err := detachedClaudeProfileDestinationForSource(root, source, sessionID)
	if err != nil {
		return "", err
	}
	if filepath.Clean(persisted) == filepath.Clean(candidate) {
		return candidate, nil
	}
	destinationExists, err := inspectClaudeProfileMigrationDestination(candidate)
	if err != nil {
		return "", err
	}
	if destinationExists {
		return recoverDetachedClaudeProfileMigration(persisted, candidate)
	}
	sourceExists, sourceErr := validateClaudeProfileMigrationSource(persisted)
	if sourceErr != nil {
		return "", sourceErr
	}
	if !sourceExists {
		if requireExisting {
			return "", fmt.Errorf("reusing detached CCR Claude profile %s: profile is missing", persisted)
		}
		return candidate, nil
	}
	if err := migrateClaudeProfileDirectory(persisted, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func recoverDetachedClaudeProfileMigration(source, destination string) (string, error) {
	// Migration commits the destination before the owner can persist its new
	// path. If the owner crashes in that window, the next attempt must adopt
	// the verified CCR-owned destination instead of treating the old record as
	// unrecoverable. When both trees exist, the destination is already the
	// committed copy; remove only the validated old CCR source.
	sourceExists, err := validateClaudeProfileMigrationSource(source)
	if err != nil {
		return "", err
	}
	if sourceExists {
		if removeErr := os.RemoveAll(source); removeErr != nil {
			return "", fmt.Errorf("finishing detached Claude profile migration: %w", removeErr)
		}
	}
	return destination, nil
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
