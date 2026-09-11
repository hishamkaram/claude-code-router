//go:build linux || darwin

package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmissionGateCrashHelper(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 4 {
		return
	}
	root, submission, phase := args[1], args[2], args[3]
	registry, err := OpenRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := registry.Lookup(t.Context(), submission)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.LockSession(admitted.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := registry.MaterializePrepared(t.Context(), lease, submission)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "helper-output"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := ProcessConfig{Executable: executable, Path: "/bin/sh", Args: []string{"-c", `printf x >> "$1"`}, Env: os.Environ(), Out: output, Err: output}
	cfg.Args = append(cfg.Args, "workload", filepath.Join(root, "executions"))
	cfg.JobID = admitted.JobID
	cfg.BeforeRelease = func(ctx context.Context) error {
		if commitErr := registry.CommitExecution(ctx, lease, submission, admitted.ExecutionDigest); commitErr != nil {
			return commitErr
		}
		if phase == "before-gate-release" {
			os.Exit(72)
		}
		return nil
	}
	process, err := startProcess(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-process.Done(); err != nil {
		t.Fatal(err)
	}
	if err := process.Stop(); err != nil {
		t.Fatal(err)
	}
	if phase == "after-record-publication" {
		cleanup, code := process.Result()
		record = finalizeExecution(record, FinalOutcome{Cleanup: cleanup, ExitCode: code})
		if !record.Stopped() || record.Status != "completed" {
			t.Fatalf("no terminal proof: %+v", record)
		}
		if err := (Store{Root: root}).Write(record); err != nil {
			t.Fatal(err)
		}
	}
	os.Exit(72)
}

func TestAdmissionCrashAtExecutionAndPublicationBoundaries(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before-gate-release", "after-workload-exit", "after-record-publication"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			registry := testRegistry(t, root)
			admitted := testAdmission()
			lease := reserveAdmission(t, registry, admitted)
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestAdmissionGateCrashHelper$", "--", root, admitted.SubmissionID, phase)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 72 {
				t.Fatalf("crash boundary not reached: %v %s", err, output)
			}
			recovered, err := registry.LockSession(admitted.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = recovered.Close() }()
			if commitErr := registry.CommitExecution(ctx, recovered, admitted.SubmissionID, admitted.ExecutionDigest); !errors.Is(commitErr, ErrAdmissionClosed) {
				t.Fatalf("execution replayed: %v", commitErr)
			}
			record, err := registry.JobStatus(ctx, admitted.JobID)
			if err != nil {
				t.Fatal(err)
			}
			executions, readErr := os.ReadFile(filepath.Join(root, "executions"))
			if phase == "before-gate-release" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("unreleased gate executed: %q %v", executions, readErr)
				}
			} else if readErr != nil || string(executions) != "x" {
				t.Fatalf("wrong execution count: %q %v", executions, readErr)
			}
			if phase == "after-record-publication" {
				if !record.Stopped() || record.Status != "completed" {
					t.Fatalf("lost durable terminal proof: %+v", record)
				}
			} else if record.WorkloadDisposition != DispositionUnknown || record.ReasonCode != ReasonOwnerLost {
				t.Fatalf("invented owner outcome: %+v", record)
			}
		})
	}
}
