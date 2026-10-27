//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchForwardsChromeOption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, errOut, err := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "launch", "--auth-mode", "preserve", "--chrome", "--version")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out+errOut, "(Claude Code)") {
		t.Fatalf("launch output missing Claude Code version:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchMetadataCommandSkipsRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, errOut, err := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "launch", "--", "--version")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out+errOut, "(Claude Code)") {
		t.Fatalf("launch output missing Claude Code version:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if strings.Contains(out+errOut, "Claude Code launched through") {
		t.Fatalf("launch output contains CCR summary:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after Claude metadata command: stat err=%v", statErr)
	}
}

func TestLiveLaunchRoutesThroughFakeOpenAIProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	chatCalled := false
	modelsCalled := false
	toolsSeen := false
	promptSeen := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelsCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled = true
			var payload liveOpenAIChatPayload
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
			toolsSeen = len(payload.Tools) > 0
			promptSeen = openAIMessagesContain(payload.Messages, "--dangerously-skip-permissions explain this option")
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-live-smoke", "live-smoke-ok", 4, 2)
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

	out, errOut, err := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token", "--", "--", "--dangerously-skip-permissions explain this option")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled {
		t.Fatalf("fake OpenAI-compatible chat endpoint was not called\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !modelsCalled {
		t.Fatalf("fake OpenAI-compatible models endpoint was not called\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !toolsSeen {
		t.Fatalf("fake OpenAI-compatible chat endpoint did not receive Claude Code tools\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !promptSeen {
		t.Fatalf("fake OpenAI-compatible chat endpoint did not receive dash-prefixed prompt text\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, "live-smoke-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchNoStartupModelCanSelectConfiguredOpenAIAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatalf("required live Claude Code unavailable: %v", err)
	}
	isolateLiveClaudeAuth(t)
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	t.Chdir(t.TempDir())
	var chatCalls, firstPartyCalls atomic.Int64
	var capturedModel atomic.Pointer[string]
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
			if !ok {
				return
			}
			chatCalls.Add(1)
			capturedModel.Store(&payload.Model)
			if payload.Model != "gpt-5" {
				t.Errorf("provider model = %q, want gpt-5", payload.Model)
				http.Error(w, "bad model", http.StatusBadRequest)
				return
			}
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-no-startup", "no-startup-selection-ok", 4, 2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	firstParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstPartyCalls.Add(1)
		http.Error(w, "unexpected first-party request", http.StatusBadRequest)
	}))
	defer firstParty.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	deps := Dependencies{StartGateway: func(startCtx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		cfg.AnthropicBaseURL = firstParty.URL
		return gateway.Start(startCtx, cfg)
	}}
	run := startLiveCompactionCommand(t, ctx, deps, []string{
		"--db", dbPath, "launch", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	})
	selection, err := liveRealTurn(ctx, run, liveStreamInput(t, "/model anthropic.ccr.gpt"))
	normalizedSelection := normalizeLiveClaudeText(selection)
	acknowledgedModel := strings.HasPrefix(normalizedSelection, normalizeLiveClaudeText("Set model to anthropic.ccr.gpt")) ||
		strings.HasPrefix(normalizedSelection, normalizeLiveClaudeText("Set model to CCR gpt (gpt-5)"))
	if err != nil || !acknowledgedModel {
		t.Fatalf("launch missing native model switch confirmation: error=%v; %s", err, liveRealSelectionSummary(selection, "anthropic.ccr.gpt"))
	}
	wantSelectionCalls := int64(1)
	if liveNativeVersionAtLeast(t, ctx, [3]int{2, 1, 242}) {
		wantSelectionCalls = 0
	}
	if got := chatCalls.Load(); got != wantSelectionCalls {
		t.Fatalf("native model selection inference requests=%d, want=%d", got, wantSelectionCalls)
	}
	result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with the no-startup synthetic provider response."))
	if err != nil || strings.TrimSpace(result) != "no-startup-selection-ok" {
		t.Fatalf("selected alias did not complete a real turn: result_matches=%t error=%v", strings.TrimSpace(result) == "no-startup-selection-ok", err)
	}
	model := capturedModel.Load()
	if chatCalls.Load() <= wantSelectionCalls || model == nil || *model != "gpt-5" {
		t.Fatal("real no-startup turn did not route through the captured gpt-5 provider model")
	}
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatalf("finish no-startup native launch: %v", err)
	}
	if !strings.Contains(run.errOut.String(), "No ccr startup model selected") {
		t.Fatal("launch stderr missing no-startup summary")
	}
	if firstPartyCalls.Load() != 0 {
		t.Fatal("no-startup selection or real turn contacted the first-party trap")
	}
}

func TestLiveLaunchPreserveAuthRoutesThroughFakeAnthropicCompatibleProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	chatCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
		case "/v1/messages":
			chatCalled = true
			var payload struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("provider decode error = %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.Model != "glm-4.7" {
				t.Errorf("provider model = %q, want glm-4.7", payload.Model)
				http.Error(w, "bad model", http.StatusBadRequest)
				return
			}
			if payload.Stream {
				writeLiveAnthropicStream(w, payload.Model, "preserve-live-ok")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"msg_live","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"preserve-live-ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "zai", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "glm", "--provider", "zai", "--model", "glm-4.7"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "glm", "--print", "--auth-mode", "preserve")
	if err != nil {
		if liveAnthropicAuthUnavailable(out + errOut) {
			t.Skipf("live Claude Code Anthropic auth unavailable:\nstdout:\n%s\nstderr:\n%s", out, errOut)
		}
		t.Fatalf("launch error = %v (chatCalled=%v)\nstdout:\n%s\nstderr:\n%s", err, chatCalled, out, errOut)
	}
	if !chatCalled {
		t.Fatalf("fake Anthropic-compatible chat endpoint was not called\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, "preserve-live-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchRoutesThroughFakeAnthropicCompatibleAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	messageCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
			if payload.Model != "glm-4.7" {
				t.Errorf("provider model = %q, want glm-4.7", payload.Model)
				http.Error(w, "bad model", http.StatusBadRequest)
				return
			}
			if payload.Stream {
				writeLiveAnthropicStream(w, payload.Model, "anthropic-alias-ok")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"msg_live","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"anthropic-alias-ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "zai", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "glm", "--provider", "zai", "--model", "glm-4.7"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "glm", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !messageCalled {
		t.Fatalf("fake Anthropic-compatible alias endpoint was not called\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, "anthropic-alias-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchPassesThroughFakeAnthropicAlias(t *testing.T) {
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
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "anthropic", "--base-url", provider.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "claude-fake", "--provider", "anthropic", "--model", "claude-opus-4-7"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "claude-fake", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !messageCalled {
		t.Fatalf("fake Anthropic messages endpoint was not called\nstdout:\n%s\nstderr:\n%s", out, errOut)
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

func liveAnthropicAuthUnavailable(output string) bool {
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "not logged in") ||
		strings.Contains(normalized, "failed to authenticate") ||
		strings.Contains(normalized, "oauth session expired") ||
		strings.Contains(normalized, "oauth refresh token is no longer valid") ||
		strings.Contains(normalized, "run /login to re-authenticate")
}

func liveAutoLaunchAuthUnavailable(err error) bool {
	return err != nil && strings.Contains(
		strings.ToLower(err.Error()),
		"claude subscription authentication was not found",
	)
}

type liveOpenAIChatMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

type liveOpenAIChatPayload struct {
	Model         string   `json:"model"`
	MaxTokens     int      `json:"max_tokens"`
	Temperature   *float64 `json:"temperature"`
	Stop          []string `json:"stop"`
	Stream        bool     `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []liveOpenAIChatMessage `json:"messages"`
}

func addLiveOpenAIModel(t *testing.T, ctx context.Context, dbPath, baseURL string) {
	t.Helper()
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", baseURL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
}

func openAIMessagesContain(messages []liveOpenAIChatMessage, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func openAIMessagesContainToolRole(messages []liveOpenAIChatMessage, needle string) bool {
	for _, message := range messages {
		if message.Role == "tool" && strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
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

func openAIMessagesContainAgentResult(messages []liveOpenAIChatMessage, callID, needle string) bool {
	for _, message := range messages {
		if message.Role != "tool" || message.ToolCallID != callID {
			continue
		}
		if strings.Contains(message.Content, needle) {
			return true
		}
		// Claude can deliver SubagentHandback reports separately, leaving a
		// positive delivery acknowledgment in the matching Agent tool result.
		if strings.HasPrefix(strings.TrimSpace(message.Content), "This agent's report was delivered to you as a message from ") &&
			strings.Contains(message.Content, "(its SubagentHandback call).") {
			return true
		}
	}
	return false
}
