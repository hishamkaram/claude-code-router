//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchRoutesThroughFakeOpenAIProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	chatCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled = true
			var payload struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("provider decode error = %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Model != "gpt-5" {
				t.Errorf("provider model = %q, want gpt-5", payload.Model)
				http.Error(w, "bad model", http.StatusBadRequest)
				return
			}
			if payload.ReasoningEffort != "high" {
				t.Errorf("provider reasoning_effort = %q, want high", payload.ReasoningEffort)
				http.Error(w, "bad reasoning effort", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-live-smoke","choices":[{"message":{"content":"live-smoke-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "gpt")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled {
		t.Fatalf("fake OpenAI-compatible chat endpoint was not called")
	}
	if !strings.Contains(out, "live-smoke-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func runLiveCommand(ctx context.Context, deps Dependencies, args ...string) (string, string, error) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	if deps.In == nil {
		deps.In = strings.NewReader("")
	}
	deps.Out = &out
	deps.Err = &errOut
	cmd := NewRootCommand(ctx, deps)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}
