//go:build linux || darwin

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmissionGateExecutesOnlyOnce(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	marker := filepath.Join(t.TempDir(), "executions")
	cfg := testProcessConfig(t, `printf x >> "$1"`)
	cfg.Args = append(cfg.Args, "workload", marker)
	cfg.JobID = a.JobID
	cfg.BeforeRelease = func(ctx context.Context) error {
		return r.CommitExecution(ctx, lease, a.SubmissionID, a.ExecutionDigest)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	p, err := startProcess(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if waitErr := <-p.Done(); waitErr != nil {
		t.Fatal(waitErr)
	}
	if _, replayErr := startProcess(ctx, cfg, false); !errors.Is(replayErr, ErrAdmissionClosed) {
		t.Fatalf("repeated execution was not rejected: %v", replayErr)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "x" {
		t.Fatalf("execution count: %q, %v", got, err)
	}
}

func TestAdmissionGateWithholdsExecutionAfterCancellation(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	cfg := testProcessConfig(t, `printf x > "$1"`)
	cfg.Args = append(cfg.Args, "workload", marker)
	cfg.JobID = a.JobID
	called := false
	cfg.BeforeRelease = func(ctx context.Context) error {
		called = true
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			return errors.New("workload executed before execution intent")
		}
		if _, err := r.AbortPrepared(ctx, a.SubmissionID); err != nil {
			return err
		}
		return r.CommitExecution(ctx, lease, a.SubmissionID, a.ExecutionDigest)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := startProcess(ctx, cfg, false); !errors.Is(err, ErrAdmissionClosed) || !called {
		t.Fatalf("gate cancellation: called=%v err=%v", called, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled admission executed: %v", err)
	}
}
