//go:build linux || darwin

package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func TestDetachedModelExpectationUsesLaunchResolution(t *testing.T) {
	h := newAdmissionBinaryHarness(t)
	var calls atomic.Int32
	var failFirst atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 && failFirst.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()
	h.run("provider", "add", "discovery", "--type=openai-compatible", "--base-url="+server.URL, "--no-api-key")
	h.run("model", "add", "discovery", "--provider=discovery", "--model=gpt-4o", "--compat=full")
	if err := os.WriteFile(h.prompt, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnv := h.env
	for _, tc := range []struct {
		name           string
		fail           bool
		output         string
		status, reason string
		executions     int
	}{
		{"transient-preflight", true, "wrong-model", "failed", jobs.ReasonStartupFailed, 0},
		{"wrong-model", false, "wrong-model", "failed", jobs.ReasonObservationFailed, 1},
		{"valid-model", false, "", "completed", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			failFirst.Store(tc.fail)
			h.env = append(append([]string{}, baseEnv...), "CCR_ADMISSION_OUTPUT_MODEL="+tc.output)
			before, _ := os.ReadFile(filepath.Join(h.root, "history", "executions"))
			args := h.args(tc.name)
			for i, arg := range args {
				if strings.HasPrefix(arg, "--model=") {
					args[i] = "--model=discovery"
				}
			}
			receipt := h.receipt(args)
			final := h.terminal(receipt.JobID)
			after, _ := os.ReadFile(filepath.Join(h.root, "history", "executions"))
			delta := len(strings.Fields(string(after))) - len(strings.Fields(string(before)))
			if final.Status != tc.status || final.ReasonCode != tc.reason || delta != tc.executions || calls.Load() != 1 {
				t.Fatalf("status=%s reason=%s executions=%d discovery calls=%d: %+v", final.Status, final.ReasonCode, delta, calls.Load(), final)
			}
			if tc.fail {
				if final.WorkloadDisposition != jobs.DispositionNotStarted {
					t.Fatalf("preflight failure may have executed: %+v", final)
				}
			} else if final.ExpectedModel != "anthropic.ccr.discovery" {
				t.Fatalf("unbound model expectation: %+v", final)
			}
			if tc.status == "completed" && (final.ResultEvidence == nil || final.ResultEvidence.Model != final.ExpectedModel) {
				t.Fatalf("unbound successful evidence: %+v", final)
			}
		})
	}
}
