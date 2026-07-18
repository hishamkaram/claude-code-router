package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type launchFinalizer struct {
	store    *store.Store
	tracker  *session.Tracker
	launchID int64
	finished bool
}

func (f *launchFinalizer) Finish(parent context.Context, state, reason string, exitCode *int) error {
	if f == nil || f.finished {
		return nil
	}
	f.finished = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	var trackerErr error
	if f.tracker != nil {
		if err := f.tracker.Finalize(ctx); err != nil {
			trackerErr = fmt.Errorf("finalizing launch runtime: %w", err)
		}
	}
	finishErr := f.store.FinishLaunch(ctx, f.launchID, state, reason, exitCode)
	if finishErr != nil {
		finishErr = fmt.Errorf("finalizing launch record: %w", finishErr)
	}
	return errors.Join(trackerErr, finishErr)
}

func launchExitState(ctx context.Context, waitErr error) (state, reason string) {
	if ctx.Err() != nil {
		return "canceled", "context_canceled"
	}
	if waitErr != nil {
		return "failed", "process_error"
	}
	return "completed", "process_exit"
}

func launchExitCode(waitErr error) *int {
	if waitErr == nil {
		code := 0
		return &code
	}
	var exitCoder interface{ ExitCode() int }
	if errors.As(waitErr, &exitCoder) {
		code := exitCoder.ExitCode()
		return &code
	}
	return nil
}
