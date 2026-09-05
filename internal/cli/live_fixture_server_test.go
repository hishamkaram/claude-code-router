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

	mu               sync.Mutex
	messageCalls     int
	countTokensCalls int
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

func (f *liveFirstPartyClassifierFixture) AssertUnused(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messageCalls != 0 || f.countTokensCalls != 0 {
		t.Fatalf(
			"auto-mode classifier leaked to first-party Anthropic: messages=%d count_tokens=%d",
			f.messageCalls,
			f.countTokensCalls,
		)
	}
}

func (f *liveFirstPartyClassifierFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/messages/count_tokens":
		f.mu.Lock()
		f.countTokensCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/messages":
		f.mu.Lock()
		f.messageCalls++
		f.mu.Unlock()
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
		writeLiveAnthropicClassifierResponse(w, payload)
	default:
		http.NotFound(w, r)
	}
}

func isLiveAutoClassifierRequest(payload liveOpenAIChatPayload) bool {
	return openAIMessagesContain(payload.Messages, "You are a security monitor for autonomous AI coding agents.")
}

func isLiveAnthropicAutoClassifierRequest(payload liveAnthropicMessagePayload) bool {
	return strings.Contains(liveAnthropicSystemText(payload.System), "You are a security monitor for autonomous AI coding agents.")
}

func liveAnthropicSystemText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var combined strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if combined.Len() > 0 {
			combined.WriteByte('\n')
		}
		combined.WriteString(block.Text)
	}
	return combined.String()
}

func liveClassifierAllowResponse(system string) string {
	if strings.Contains(system, "<severity>N</severity>") {
		return "<severity>0</severity>"
	}
	return "<block>no</block>"
}

func writeLiveOpenAIClassifierResponse(w http.ResponseWriter, payload liveOpenAIChatPayload) {
	text := liveClassifierAllowResponse(openAIMessagesText(payload.Messages))
	writeLiveOpenAITextFixture(w, payload, "chatcmpl-live-classifier", text, 9, 3)
}

func writeLiveOpenAITextFixture(
	w http.ResponseWriter,
	payload liveOpenAIChatPayload,
	id, content string,
	promptTokens, completionTokens int,
) {
	if payload.Stream {
		writeLiveOpenAIStreamFrames(w, []any{
			map[string]any{
				"id": id,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": content},
				}},
			},
			map[string]any{
				"id": id,
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				}},
			},
			map[string]any{
				"id":      id,
				"choices": []any{},
				"usage":   map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens},
			},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id,
		"choices": []any{map[string]any{
			"message":       map[string]string{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens},
	})
}

func writeLiveOpenAIToolFixture(
	w http.ResponseWriter,
	payload liveOpenAIChatPayload,
	id, callID, name string,
	input any,
) {
	arguments, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "fixture tool arguments could not be encoded", http.StatusInternalServerError)
		return
	}
	if payload.Stream {
		writeLiveOpenAIStreamFrames(w, []any{
			map[string]any{
				"id": id,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0,
						"id":    callID,
						"type":  "function",
						"function": map[string]string{
							"name":      name,
							"arguments": string(arguments),
						},
					}}},
				}},
			},
			map[string]any{
				"id": id,
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "tool_calls",
				}},
			},
			map[string]any{
				"id":      id,
				"choices": []any{},
				"usage":   map[string]int{"prompt_tokens": 4, "completion_tokens": 3},
			},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id,
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":       callID,
					"type":     "function",
					"function": map[string]string{"name": name, "arguments": string(arguments)},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 3},
	})
}

func writeLiveOpenAIStreamFrames(w http.ResponseWriter, frames []any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			http.Error(w, "fixture stream frame could not be encoded", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeLiveResponsesTextFixture(
	w http.ResponseWriter,
	payload openairesponses.Request,
	id, text string,
	promptTokens, completionTokens int,
) {
	writeLiveResponsesFixtureResponse(w, payload, map[string]any{
		"id":     id,
		"model":  payload.Model,
		"status": "completed",
		"output": []any{map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]string{
				"type": "output_text",
				"text": text,
			}},
		}},
		"usage": map[string]int{"input_tokens": promptTokens, "output_tokens": completionTokens},
	})
}

func writeLiveResponsesToolFixture(
	w http.ResponseWriter,
	payload openairesponses.Request,
	id, callID, name string,
	input any,
) {
	arguments, err := json.Marshal(input)
	if err != nil {
		http.Error(w, "fixture tool arguments could not be encoded", http.StatusInternalServerError)
		return
	}
	writeLiveResponsesFixtureResponse(w, payload, map[string]any{
		"id":     id,
		"model":  payload.Model,
		"status": "completed",
		"output": []any{map[string]string{
			"type":      "function_call",
			"call_id":   callID,
			"name":      name,
			"arguments": string(arguments),
		}},
		"usage": map[string]int{"input_tokens": 4, "output_tokens": 2},
	})
}

func writeLiveResponsesFixtureResponse(w http.ResponseWriter, payload openairesponses.Request, response map[string]any) {
	if !payload.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	writeLiveResponsesStreamFrames(w, []any{map[string]any{
		"type":     "response.completed",
		"response": response,
	}})
}

func writeLiveResponsesStreamFrames(w http.ResponseWriter, frames []any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			http.Error(w, "fixture stream frame could not be encoded", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func writeLiveAnthropicClassifierResponse(w http.ResponseWriter, payload liveAnthropicMessagePayload) {
	text := liveClassifierAllowResponse(liveAnthropicSystemText(payload.System))
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
	f.writeOpenAIText(w, payload, payload.Model)
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
	writeLiveResponsesTextFixture(w, payload, "resp_fixture", f.responseText(payload.Model), 7, 3)
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

func (f *liveMatrixFixture) writeOpenAIText(w http.ResponseWriter, payload liveOpenAIChatPayload, model string) {
	writeLiveOpenAITextFixture(w, payload, "chatcmpl-fixture", f.responseText(model), 7, 3)
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
