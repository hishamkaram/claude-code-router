package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputObserverCancelsContradictoryIdentityAndJoins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	workloadCtx, cancelWorkload := context.WithCancel(t.Context())
	defer cancelWorkload()
	observer, err := StartOutputObserver(t.Context(), path, "session", "model", cancelWorkload)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = observer.Stop() }()
	if writeErr := os.WriteFile(path, []byte("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"contradiction\",\"model\":\"model\"}\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	select {
	case <-workloadCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("identity mismatch did not cancel the admitted workload")
	}
	_, err = observer.Stop()
	var outputErr *OutputError
	if !errors.As(err, &outputErr) || outputErr.ReasonCode != ReasonSessionIdentityMismatch || outputErr.ObservedSessionID != "contradiction" {
		t.Fatalf("observer lost mismatch: %v", err)
	}
}

func TestOutputObserverStopDoesNotInventSuccessfulCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	observer, err := StartOutputObserver(t.Context(), path, "session", "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := observer.Stop()
	if err != nil || result.Successful || result.Initialized {
		t.Fatalf("empty observer fabricated completion: %+v %v", result, err)
	}
	if _, _, commitErr := CommitOutput(t.Context(), path, "session", "model"); commitErr == nil {
		t.Fatal("empty final log accepted")
	}
}
