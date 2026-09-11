//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func adoptJobAdmission(ctx context.Context, s jobs.Store, record jobs.Record, admission jobAdmission) (*jobs.Registry, *jobs.SessionLease, error) {
	file := os.NewFile(6, "session-lease")
	if record.SchemaVersion != 2 || record.SubmissionID != admission.SubmissionID {
		_ = file.Close()
		return nil, nil, fmt.Errorf("invalid owner admission identity")
	}
	registry, err := jobs.OpenRegistry(ctx, s.Root)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	lease, err := registry.AdoptSessionLease(file, record.SessionID)
	if err != nil {
		_ = registry.Close()
		return nil, nil, err
	}
	bound, err := registry.Lookup(ctx, admission.SubmissionID)
	if err != nil || bound.JobID != record.JobID || bound.SessionID != record.SessionID || bound.ExecutionDigest != admission.ExecutionDigest || bound.State != jobs.AdmissionPrepared {
		_ = lease.Close()
		_ = registry.Close()
		return nil, nil, fmt.Errorf("owner handoff no longer matches prepared admission")
	}
	return registry, lease, nil
}

func streamJSONJob(args []string) bool {
	for i, arg := range args {
		if arg == "--output-format=stream-json" || (arg == "--output-format" && i+1 < len(args) && args[i+1] == "stream-json") {
			return true
		}
	}
	return false
}

func collectJobOutcome(ctx context.Context, launcher *jobClaudeLauncher, accounting *gateway.RequestAccounting, observer *jobs.OutputObserver, record jobs.Record, observed bool, runErr error) jobs.FinalOutcome {
	cleanup, code := launcher.result()
	if reapErr := jobs.ReapOrphans(); reapErr != nil {
		cleanup = jobs.Cleanup{Coverage: "unknown", Reason: "adopted child reaping failed"}
		runErr = errors.Join(runErr, reapErr)
	}
	outcome := jobs.FinalOutcome{RunError: runErr, Cleanup: cleanup, ExitCode: code, ObservationRequired: observed}
	if observer != nil {
		outcome.Stream, outcome.ObservationError = observer.Stop()
	}
	barrierCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := accounting.SealAndWait(barrierCtx); err != nil {
		outcome.ObservationError = errors.Join(outcome.ObservationError, err)
	}
	outcome.RequestEntries = accounting.Entered()
	if observed {
		evidence, result, err := jobs.CommitOutput(barrierCtx, record.Log, record.SessionID, record.ExpectedModel)
		// Keep the live observer's contradiction even if later output changed.
		outcome.ObservationError = errors.Join(outcome.ObservationError, err)
		if result.Initialized {
			outcome.Stream = result
		}
		if err == nil {
			outcome.Evidence = &evidence
		}
	}
	return outcome
}

func resolveOwnedModel(ctx context.Context, deps Dependencies, admission jobAdmission, invocation launchInvocation) string {
	s, _, err := openMigratedStore(ctx, &options{dbPath: admission.DB})
	if err != nil {
		return ""
	}
	defer closeStore(s)
	resolved, err := resolveLaunch(ctx, deps, s, invocation)
	if err != nil {
		return ""
	} // runLaunch publishes the actual startup failure.
	return resolved.claudeModelID
}
