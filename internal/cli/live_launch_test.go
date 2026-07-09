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
	"sync"
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
				Model string `json:"model"`
				Tools []any  `json:"tools"`
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

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
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
	if !strings.Contains(out, "live-smoke-ok") {
		t.Fatalf("launch output missing routed response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func TestLiveLaunchOpenAIProviderStreamsAgentToolInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	provider, state := newLiveAgentToolProvider(t)
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)

	prompt := `Spawn a research subagent now. The subagent prompt must be: "Return exactly CCR_LIVE_CHILD_OK and nothing else." After the subagent finishes, reply exactly CCR_LIVE_PARENT_OK if it succeeded. Do not use web or shell.`
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_PARENT_OK") {
		t.Fatalf("launch output missing parent response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	state.assertComplete(t, out, errOut)
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

	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "glm", "--print")
	if err != nil {
		if strings.Contains(out+errOut, "Not logged in") {
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

type liveOpenAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type liveOpenAIChatPayload struct {
	Model string `json:"model"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []liveOpenAIChatMessage `json:"messages"`
}

type liveAgentToolProviderState struct {
	mu                       sync.Mutex
	chatCalls                int
	firstRequestHadAgentTool bool
	childPromptSeen          bool
	parentToolResultSeen     bool
}

func newLiveAgentToolProvider(t *testing.T) (*httptest.Server, *liveAgentToolProviderState) {
	t.Helper()
	state := &liveAgentToolProviderState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.handle(t, w, r)
	}))
	return server, state
}

func (s *liveAgentToolProviderState) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
	case "/v1/chat/completions":
		s.handleChat(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *liveAgentToolProviderState) handleChat(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatCalls++
	w.Header().Set("Content-Type", "application/json")
	switch {
	case s.chatCalls == 1:
		s.firstRequestHadAgentTool = liveToolsContainAgent(payload.Tools)
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-tool","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu_agent_live","type":"function","function":{"name":"Agent","arguments":"{\"description\":\"return child sentinel\",\"prompt\":\"Return exactly CCR_LIVE_CHILD_OK and nothing else.\",\"subagent_type\":\"general-purpose\",\"run_in_background\":false}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`)
	case s.chatCalls == 2:
		s.handleChildRequest(t, w, payload.Messages)
	case openAIMessagesContainRole(payload.Messages, "tool", "CCR_LIVE_CHILD_OK"):
		s.parentToolResultSeen = true
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-parent","choices":[{"message":{"content":"CCR_LIVE_PARENT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	default:
		t.Errorf("unexpected provider request after Agent tool call: %#v", payload.Messages)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func decodeLiveOpenAIChatPayload(t *testing.T, w http.ResponseWriter, r *http.Request) (liveOpenAIChatPayload, bool) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("provider decode error = %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return payload, false
	}
	if payload.Model != "gpt-5" {
		t.Errorf("provider model = %q, want gpt-5", payload.Model)
		http.Error(w, "bad model", http.StatusBadRequest)
		return payload, false
	}
	return payload, true
}

func (s *liveAgentToolProviderState) handleChildRequest(t *testing.T, w http.ResponseWriter, messages []liveOpenAIChatMessage) {
	t.Helper()
	if openAIMessagesContainRole(messages, "tool", "") || !openAIMessagesContain(messages, "Return exactly CCR_LIVE_CHILD_OK") {
		t.Errorf("second provider request is not the child request: %#v", messages)
		http.Error(w, "bad child request", http.StatusBadRequest)
		return
	}
	s.childPromptSeen = true
	_, _ = fmt.Fprint(w, `{"id":"chatcmpl-agent-child","choices":[{"message":{"content":"CCR_LIVE_CHILD_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
}

func (s *liveAgentToolProviderState) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.firstRequestHadAgentTool || !s.childPromptSeen || !s.parentToolResultSeen {
		t.Fatalf("Agent live route incomplete: firstRequestHadAgentTool=%v childPromptSeen=%v parentToolResultSeen=%v chatCalls=%d\nstdout:\n%s\nstderr:\n%s", s.firstRequestHadAgentTool, s.childPromptSeen, s.parentToolResultSeen, s.chatCalls, out, errOut)
	}
}

func liveToolsContainAgent(tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}) bool {
	for _, tool := range tools {
		if tool.Function.Name == "Agent" {
			return true
		}
	}
	return false
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

func openAIMessagesContainRole(messages []liveOpenAIChatMessage, role, needle string) bool {
	for _, message := range messages {
		if message.Role == role && strings.Contains(message.Content, needle) {
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
