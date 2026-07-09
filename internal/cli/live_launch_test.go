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
	modelsCalled := false
	toolsSeen := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelsCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled = true
			var payload struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
				Tools           []any  `json:"tools"`
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
			toolsSeen = len(payload.Tools) > 0
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

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled {
		t.Fatalf("fake OpenAI-compatible chat endpoint was not called")
	}
	if !modelsCalled {
		t.Fatalf("fake OpenAI-compatible models endpoint was not called")
	}
	if !toolsSeen {
		t.Fatalf("fake OpenAI-compatible chat endpoint did not receive Claude Code tools")
	}
	if !strings.Contains(out, "live-smoke-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchPassesThroughFakeAnthropicProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	messageCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-7","display_name":"Claude Opus 4.7"}]}`)
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
		case "/v1/messages":
			messageCalled = true
			var payload struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("provider decode error = %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Model == "" {
				t.Errorf("provider model is empty")
				http.Error(w, "bad model", http.StatusBadRequest)
				return
			}
			if payload.Stream {
				writeLiveAnthropicStream(w, payload.Model, "anthropic-pass-ok")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_live","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"anthropic-pass-ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":2}}`, payload.Model)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if out, errOut, err := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "provider", "add", "anthropic", "--base-url", provider.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--print")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !messageCalled {
		t.Fatalf("fake Anthropic messages endpoint was not called")
	}
	if !strings.Contains(out, "anthropic-pass-ok") {
		t.Fatalf("launch output missing pass-through response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func writeLiveAnthropicStream(w http.ResponseWriter, model string, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_live\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n", model)
	_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	_, _ = fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n")
	_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
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
