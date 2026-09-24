//go:build live && (linux || darwin)

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func TestLiveDetachedClaudeContinuation(t *testing.T) {
	proveLiveDetachedClaudeContinuation(t, "")
}

func TestLiveDetachedLegacyContinuation(t *testing.T) {
	legacy := os.Getenv("CCR_LIVE_LEGACY_CCR")
	if legacy == "" {
		t.Fatal("CCR_LIVE_LEGACY_CCR must name a real CCR 0.5.1 binary")
	}
	version, err := exec.CommandContext(t.Context(), legacy, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "ccr 0.5.1 ") {
		t.Fatalf("required CCR 0.5.1 predecessor: %s %v", version, err)
	}
	t.Logf("legacy predecessor binary: %s", strings.TrimSpace(string(version)))
	proveLiveDetachedClaudeContinuation(t, legacy)
}

func proveLiveDetachedClaudeContinuation(t *testing.T, legacy string) {
	t.Helper()
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("required real Claude CLI unavailable")
	}
	version, err := exec.CommandContext(t.Context(), claude, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real CLI: %s", strings.TrimSpace(string(version)))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	binary := buildLiveCCRExecutable(t, ctx)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	marker := "CCR_HISTORY_" + uuid.NewString()
	var calls atomic.Int64
	server := continuationFixture(t, marker, &calls)
	defer server.Close()
	database := filepath.Join(t.TempDir(), "ccr.db")
	configureLiveMatrixModels(t, ctx, Dependencies{}, database, server.URL, "openai-chat")
	directory := t.TempDir()
	var head jobs.Record
	for index, phase := range []string{"original", "review consultation", "residual resolution", "debate"} {
		prompt := "Recall the original unpredictable marker for this " + phase + " round."
		if index == 0 {
			prompt = "Remember this marker for later rounds: " + marker
		}
		before := calls.Load()
		launchBinary := binary
		legacyRound := index == 0 && legacy != ""
		if legacyRound {
			launchBinary = legacy
		}
		head = launchLiveContinuation(t, ctx, launchBinary, database, directory, prompt, head, legacyRound)
		if legacyRound {
			if head.SchemaVersion != 1 {
				t.Fatalf("predecessor is not a genuine schema-1 record: %+v", head)
			}
			evidence, _, err := jobs.CommitOutput(ctx, head.Log, head.SessionID, "anthropic.ccr.fixture-full[1m]")
			if err != nil {
				t.Fatal(err)
			}
			// Validate independently without rewriting the genuine old record.
			head.ResultEvidence = &evidence
		}
		if head.Status != "completed" || !head.Stopped() || head.ResultEvidence == nil || head.Containment != expectedLiveContainment(t) {
			diagnostic, _ := os.ReadFile(head.ErrorLog)
			t.Fatalf("%s: %+v\n%s", phase, head, diagnostic)
		}
		result, err := jobs.ReadCommittedOutput(ctx, head.Log, *head.ResultEvidence)
		if err != nil || result.Text != marker || calls.Load() <= before {
			t.Fatalf("%s lost history or did not call provider: %+v %v", phase, result, err)
		}
		t.Logf("%s job=%s session=%s parent=%s coverage=%s", phase, head.JobID, head.SessionID, head.ResumedFrom, head.Cleanup.Coverage)
	}
	// An isolated empty profile creates a real missing-history failure without
	// production code inspecting Claude's transcript storage layout.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	missing := launchLiveContinuation(t, ctx, binary, database, directory, "Continue the original session.", head, false)
	if missing.Status != "failed" || missing.ReasonCode != jobs.ReasonStartupFailed || !missing.Stopped() || missing.RequestedResumeSession != head.SessionID {
		diagnostic, _ := os.ReadFile(missing.Log)
		t.Fatalf("missing history classification: %+v\n%s", missing, diagnostic)
	}
}

func continuationFixture(t *testing.T, marker string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "fixture-full-model"}, {"id": "fixture-degraded-model"}, {"id": "fixture-chat-model"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var payload liveOpenAIChatPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		history, err := json.Marshal(payload.Messages)
		if err != nil {
			t.Errorf("encode observed history: %v", err)
			return
		}
		calls.Add(1)
		reply := "ORIGINAL_MARKER_ABSENT"
		if strings.Contains(string(history), marker) {
			reply = marker
		}
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-history", reply, 7, 3)
	}))
}

func launchLiveContinuation(t *testing.T, ctx context.Context, binary, database, directory, prompt string, parent jobs.Record, legacy bool) jobs.Record {
	t.Helper()
	path := filepath.Join(directory, "prompt.txt")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", database, "launch", "--model=fixture-full", "--auth-mode=provider-only", "--detach", "-p", "--prompt-file=" + path, "--no-lifecycle", "--no-statusline", "--output-format=stream-json", "--verbose", "--max-turns=1", "--tools=", "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}"}
	if !legacy {
		args = append(args, "--submission-id="+uuid.NewString())
	}
	if parent.JobID != "" {
		args = append(args, "--resume="+parent.SessionID, "--expected-parent-job="+parent.JobID)
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("continuation admission: %v %s", err, output)
	}
	var receipt jobReceipt
	if err := json.Unmarshal(output, &receipt); err != nil {
		t.Fatal(err)
	}
	registerJobCleanup(t, ctx, binary, receipt.JobID)
	for ctx.Err() == nil {
		record := readBuiltJobStatus(t, ctx, binary, receipt.JobID)
		if record.Terminal() {
			return record
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("continuation did not finalize")
	return jobs.Record{}
}
