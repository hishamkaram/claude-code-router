//go:build linux || darwin

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

// An abrupt process exit bypasses Go defers and exercises actual lease release
// and durable SQLite recovery, without production environment-controlled hooks.
func TestAdmissionPrepareCrashHelper(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		return
	}
	phase, submission := args[1], args[2]
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	h := admissionBinaryHarness{prompt: filepath.Join(root, "prompt")}
	invocation, err := parseLaunchInvocation(h.args(submission)[1:])
	if err != nil {
		t.Fatal(err)
	}
	database, err := resolveDBPath(&options{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	execution, err := canonicalAdmissionContext(database, store.Root)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(h.prompt)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := requestFingerprint(invocation, prompt, execution)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := jobs.OpenRegistry(t.Context(), execution.JobRoot)
	if err != nil {
		t.Fatal(err)
	}
	admitted, lease, err := prepareDetachedAdmission(t.Context(), registry, &options{dbPath: execution.Database}, Dependencies{}, invocation, digest)
	if err != nil || lease == nil {
		t.Fatalf("prepare crash fixture: %v", err)
	}
	if phase != "reservation" {
		if _, _, err := registry.MaterializePrepared(t.Context(), lease, admitted.SubmissionID); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "execution-intent" {
		if err := registry.CommitExecution(t.Context(), lease, admitted.SubmissionID, admitted.ExecutionDigest); err != nil {
			t.Fatal(err)
		}
	}
	os.Exit(71)
}

func TestAdmissionBuiltBinaryRecoversAcrossAbruptOwnerExit(t *testing.T) {
	h := newAdmissionBinaryHarness(t)
	if err := os.WriteFile(h.prompt, []byte("crash-boundary"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"reservation", "materialized", "execution-intent", "cancel-prepared", "changed-config"} {
		t.Run(phase, func(t *testing.T) {
			child := exec.CommandContext(t.Context(), helper, "-test.run=^TestAdmissionPrepareCrashHelper$", "--", phase, phase)
			child.Dir, child.Env = h.root, h.env
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 71 {
				t.Fatalf("did not crash at boundary: %v %s", err, output)
			}
			var before jobs.Record
			if err := json.Unmarshal(h.run("status", "--submission-id="+phase, "--json"), &before); err != nil {
				t.Fatal(err)
			}
			executionsBefore, _ := os.ReadFile(filepath.Join(h.root, "history", "executions"))
			if phase == "cancel-prepared" {
				h.run("cancel", before.JobID)
			}
			savedEnv := h.env
			if phase == "changed-config" {
				h.env = append(append([]string{}, h.env...), "CCR_TEST_EXECUTION_SETTING=changed")
			}
			replay := h.receipt(h.args(phase))
			h.env = savedEnv
			if replay.JobID != before.JobID || replay.SessionID != before.SessionID {
				t.Fatal("recovery changed logical admission")
			}
			final := h.terminal(replay.JobID)
			executionsAfter, _ := os.ReadFile(filepath.Join(h.root, "history", "executions"))
			delta := len(strings.Fields(string(executionsAfter))) - len(strings.Fields(string(executionsBefore)))
			switch phase {
			case "reservation", "materialized":
				if final.Status != "completed" || delta != 1 {
					t.Fatalf("prepared recovery failed: delta=%d %+v", delta, final)
				}
			case "execution-intent":
				if delta != 0 || final.ReasonCode != jobs.ReasonOwnerLost || final.WorkloadDisposition != jobs.DispositionUnknown {
					t.Fatalf("replayed possible execution: delta=%d %+v", delta, final)
				}
			default:
				if delta != 0 || final.WorkloadDisposition != jobs.DispositionNotStarted {
					t.Fatalf("revived closed prepared admission: delta=%d %+v", delta, final)
				}
			}
		})
	}
}
