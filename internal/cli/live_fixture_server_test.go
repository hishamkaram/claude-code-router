//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
)

type liveFirstPartyClassifierFixture struct {
	server *httptest.Server

	mu    sync.Mutex
	calls int
}

func newLiveFirstPartyClassifierFixture(t *testing.T) *liveFirstPartyClassifierFixture {
	t.Helper()
	fixture := &liveFirstPartyClassifierFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveFirstPartyClassifierFixture) Close() {
	f.server.Close()
}

func (f *liveFirstPartyClassifierFixture) StartGateway(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
	cfg.AnthropicBaseURL = f.server.URL
	return gateway.Start(ctx, cfg)
}

func (f *liveFirstPartyClassifierFixture) Seen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls > 0
}

func (f *liveFirstPartyClassifierFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/messages":
		var payload liveAnthropicMessagePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding first-party classifier request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !isLiveAnthropicAutoClassifierRequest(payload) {
			t.Errorf("unexpected first-party request for model %q", payload.Model)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		writeLiveAnthropicClassifierResponse(w, payload)
	default:
		http.NotFound(w, r)
	}
}

func isLiveAutoClassifierRequest(payload liveOpenAIChatPayload) bool {
	return openAIMessagesContain(payload.Messages, "You are a security monitor for autonomous AI coding agents.")
}

func isLiveAnthropicAutoClassifierRequest(payload liveAnthropicMessagePayload) bool {
	return strings.Contains(string(payload.System), "You are a security monitor for autonomous AI coding agents.")
}

func liveClassifierAllowResponse(system string) string {
	if strings.Contains(system, "<severity>N</severity>") {
		return "<severity>0</severity>"
	}
	return "<block>no</block>"
}

func writeLiveOpenAIClassifierResponse(w http.ResponseWriter, payload liveOpenAIChatPayload) {
	text := liveClassifierAllowResponse(openAIMessagesText(payload.Messages))
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-live-classifier","choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`, text)
}

func writeLiveAnthropicClassifierResponse(w http.ResponseWriter, payload liveAnthropicMessagePayload) {
	text := liveClassifierAllowResponse(string(payload.System))
	if payload.Stream {
		writeLiveAnthropicStream(w, payload.Model, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_live_classifier","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":3}}`, payload.Model, text)
}

func openAIMessagesText(messages []liveOpenAIChatMessage) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(message.Content)
		text.WriteByte('\n')
	}
	return text.String()
}

type liveMatrixFixture struct {
	protocol string
	server   *httptest.Server

	mu                  sync.Mutex
	aliasCalls          map[string]int
	firstPartyCalls     int
	requestIncludedTool map[string]bool
}

func newLiveMatrixFixture(t *testing.T, protocol string) *liveMatrixFixture {
	t.Helper()
	fixture := &liveMatrixFixture{
		protocol:            protocol,
		aliasCalls:          make(map[string]int),
		requestIncludedTool: make(map[string]bool),
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveMatrixFixture) URL() string {
	return f.server.URL
}

func (f *liveMatrixFixture) Close() {
	f.server.Close()
}

func (f *liveMatrixFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-full-model"},{"id":"fixture-degraded-model"},{"id":"fixture-chat-model"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	case "/v1/chat/completions":
		f.handleOpenAI(t, w, r)
	case "/v1/responses":
		f.handleResponses(t, w, r)
	case "/v1/messages":
		f.handleAnthropic(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveMatrixFixture) handleOpenAI(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding OpenAI fixture request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if f.protocol != "openai-chat" || !strings.HasPrefix(payload.Model, "fixture-") {
		t.Errorf("unexpected OpenAI fixture model %q for protocol %q", payload.Model, f.protocol)
		http.Error(w, "unexpected model", http.StatusBadRequest)
		return
	}
	f.recordAliasCall(payload.Model, len(payload.Tools) > 0)
	f.writeOpenAIText(w, payload.Model)
}

func (f *liveMatrixFixture) handleResponses(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload openairesponses.Request
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding Responses fixture request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if f.protocol != "openai-responses" || !strings.HasPrefix(payload.Model, "fixture-") {
		t.Errorf("unexpected Responses fixture model %q for protocol %q", payload.Model, f.protocol)
		http.Error(w, "unexpected model", http.StatusBadRequest)
		return
	}
	f.recordAliasCall(payload.Model, len(payload.Tools) > 0)
	writeLiveResponsesText(w, payload.Model, f.responseText(payload.Model))
}

func (f *liveMatrixFixture) handleAnthropic(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding Anthropic fixture request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if isLiveAnthropicAutoClassifierRequest(payload) {
		f.mu.Lock()
		f.firstPartyCalls++
		f.mu.Unlock()
		writeLiveAnthropicClassifierResponse(w, payload)
		return
	}
	if strings.HasPrefix(payload.Model, "fixture-") {
		if f.protocol != "anthropic-native" {
			t.Errorf("unexpected Anthropic alias model %q for protocol %q", payload.Model, f.protocol)
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		f.recordAliasCall(payload.Model, len(payload.Tools) > 0)
	} else {
		f.mu.Lock()
		f.firstPartyCalls++
		f.mu.Unlock()
	}
	text := f.responseText(payload.Model)
	if payload.Stream {
		writeLiveAnthropicStream(w, payload.Model, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`, payload.Model, text)
}

func (f *liveMatrixFixture) recordAliasCall(model string, tools bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliasCalls[model]++
	f.requestIncludedTool[model] = f.requestIncludedTool[model] || tools
}

func (f *liveMatrixFixture) writeOpenAIText(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-fixture","choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`, f.responseText(model))
}

func writeLiveResponsesText(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"resp_fixture","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":7,"output_tokens":3}}`, model, text)
}

func (f *liveMatrixFixture) responseText(model string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch model {
	case "fixture-full-model":
		return "CCR_LIVE_ALIAS"
	case "fixture-degraded-model":
		return "CCR_LIVE_DEGRADED"
	case "fixture-chat-model":
		return "CCR_LIVE_CHAT"
	default:
		return "CCR_LIVE_ANTHROPIC"
	}
}

func (f *liveMatrixFixture) toolsSeen(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestIncludedTool[model]
}

func (f *liveMatrixFixture) assertSwitching(t *testing.T, out, errOut string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aliasCalls["fixture-full-model"] < 2 || f.firstPartyCalls < 1 || !f.requestIncludedTool["fixture-full-model"] {
		t.Fatalf("live model switching incomplete: aliasCalls=%v firstPartyCalls=%d tools=%v\nstdout:\n%s\nstderr:\n%s", f.aliasCalls, f.firstPartyCalls, f.requestIncludedTool, out, errOut)
	}
}
