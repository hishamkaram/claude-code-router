package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestDecodeOpenAIChatStreamAggregatesTextToolsAndUsage(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		": keepalive",
		"",
		`data: {"id":"chatcmpl-stream","model":"provider-model","choices":[{"index":0,"delta":{"content":"hello "}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"world","tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"write","arguments":"{\"path\":"}},{"index":0,"id":"call_1","type":"function","function":{"name":"look","arguments":"{\"q\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"up","arguments":"\"status\"}"}},{"index":1,"function":{"arguments":"\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":4}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	got, err := decodeOpenAIChatStream(strings.NewReader(stream), 1<<20, "")
	if err != nil {
		t.Fatalf("decodeOpenAIChatStream() error = %v", err)
	}
	if got.ID != "chatcmpl-stream" || got.Model != "provider-model" || len(got.Choices) != 1 {
		t.Fatalf("response identity/choices = %#v", got)
	}
	choice := got.Choices[0]
	if choice.Message.textContent() != "hello world" || choice.FinishReason != "tool_calls" {
		t.Fatalf("choice = %#v", choice)
	}
	if len(choice.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %#v", choice.Message.ToolCalls)
	}
	toolCall := choice.Message.ToolCalls[0]
	if toolCall.ID != "call_1" || toolCall.Type != "function" || toolCall.Function.Name != "lookup" || toolCall.Function.Arguments != `{"q":"status"}` {
		t.Fatalf("tool call = %#v", toolCall)
	}
	secondToolCall := choice.Message.ToolCalls[1]
	if secondToolCall.ID != "call_2" || secondToolCall.Function.Name != "write" || secondToolCall.Function.Arguments != `{"path":"notes.md"}` {
		t.Fatalf("second tool call = %#v", secondToolCall)
	}
	if !got.usageObserved || got.Usage.PromptTokens != 17 || got.Usage.CompletionTokens != 4 {
		t.Fatalf("usage = %#v observed=%t", got.Usage, got.usageObserved)
	}
}

func TestDecodeOpenAIChatStreamAggregatesLegacyFunctionCall(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"function_call":{"name":"get_","arguments":"{\"id\":"}}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"function_call":{"name":"get_item","arguments":"7}"}},"finish_reason":"function_call"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	got, err := decodeOpenAIChatStream(strings.NewReader(stream), 1024, "")
	if err != nil {
		t.Fatalf("decodeOpenAIChatStream() error = %v", err)
	}
	call := got.Choices[0].Message.FunctionCall
	if call == nil || call.Name != "get_item" || call.Arguments != `{"id":7}` {
		t.Fatalf("legacy function call = %#v", call)
	}
}

func TestDecodeOpenAIChatStreamStopsAtDoneSentinel(t *testing.T) {
	t.Parallel()
	reader := io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"complete\"}}]}\n\ndata: [DONE]\n\n"),
		errorReader{err: errors.New("reader must not be called after done")},
	)
	got, err := decodeOpenAIChatStream(reader, 1024, "")
	if err != nil {
		t.Fatalf("decodeOpenAIChatStream() error = %v", err)
	}
	if got.Choices[0].Message.textContent() != "complete" {
		t.Fatalf("content = %q", got.Choices[0].Message.textContent())
	}
}

func TestOpenAIChatRequestPreservesStreamingPreference(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		request, _, err := toOpenAIChatRequestWithResolver(
			context.Background(),
			anthropicRequest{Stream: stream, Messages: []anthropicMessage{{Role: "user", Content: "hello"}}},
			openAIModelRoute{providerModel: "provider-model"},
			nil,
		)
		if err != nil {
			t.Fatalf("toOpenAIChatRequestWithResolver(stream=%t) error = %v", stream, err)
		}
		if request.Stream != stream {
			t.Fatalf("request.Stream = %t, want %t", request.Stream, stream)
		}
		if got := request.StreamOptions != nil; got != stream {
			t.Fatalf("stream options present = %t, want %t", got, stream)
		}
		if request.StreamOptions != nil && !request.StreamOptions.IncludeUsage {
			t.Fatal("stream request did not request terminal usage")
		}
	}
}

func TestDecodeOpenAIChatStreamAcceptsFinishWithoutDoneSentinel(t *testing.T) {
	t.Parallel()
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}]}\n\n"
	got, err := decodeOpenAIChatStream(strings.NewReader(stream), 1024, "")
	if err != nil {
		t.Fatalf("decodeOpenAIChatStream() error = %v", err)
	}
	if got.Choices[0].Message.textContent() != "complete" {
		t.Fatalf("content = %q", got.Choices[0].Message.textContent())
	}
}

func TestOpenAIChatStreamEOFCompletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events []string
		want   bool
	}{
		{
			name:   "single completed choice",
			events: []string{`{"choices":[{"index":0,"finish_reason":"stop"}]}`},
			want:   true,
		},
		{
			name:   "unfinished choice",
			events: []string{`{"choices":[{"index":0}]}`},
			want:   false,
		},
		{
			name:   "partially finished choices",
			events: []string{`{"choices":[{"index":0,"finish_reason":"stop"},{"index":1}]}`},
			want:   false,
		},
		{
			name: "all choices finish in separate events",
			events: []string{
				`{"choices":[{"index":0,"finish_reason":"stop"}]}`,
				`{"choices":[{"index":1,"finish_reason":"length"}]}`,
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			completion := newOpenAIChatStreamEOFCompletion()
			for _, event := range test.events {
				completion.observe([]byte(event))
			}
			if got := completion.complete(); got != test.want {
				t.Fatalf("complete() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDecodeOpenAIChatStreamRejectsIncompleteMalformedAndOversizedStreams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stream    string
		maxBytes  int64
		wantError string
	}{
		{name: "incomplete", stream: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n", maxBytes: 1024, wantError: "ended before"},
		{name: "partially finished choices", stream: `data: {"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"}}]}` + "\n\n", maxBytes: 1024, wantError: "ended before"},
		{name: "malformed", stream: "data: {not-json}\n\n", maxBytes: 1024, wantError: "decoding provider stream event"},
		{name: "oversized", stream: "data: " + strings.Repeat("x", 128) + "\n\n", maxBytes: 32, wantError: "exceeds the 32 byte limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeOpenAIChatStream(strings.NewReader(test.stream), test.maxBytes, "")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("decodeOpenAIChatStream() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestDecodeOpenAIChatStreamRedactsProviderError(t *testing.T) {
	t.Parallel()
	const secret = "stream-provider-secret"
	stream := `data: {"error":{"message":"denied","authorization":"Bearer ` + secret + `"}}` + "\n\n"
	_, err := decodeOpenAIChatStream(strings.NewReader(stream), 1024, secret)
	if err == nil || !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("decodeOpenAIChatStream() error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("decodeOpenAIChatStream() leaked provider secret: %v", err)
	}
}

func TestDecodeOpenAIChatStreamPropagatesReadError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("fixture read failure")
	reader := io.MultiReader(
		strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n"),
		errorReader{err: wantErr},
	)
	_, err := decodeOpenAIChatStream(reader, 1024, "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("decodeOpenAIChatStream() error = %v, want wrapping %v", err, wantErr)
	}
}

func TestGatewayRequestsUpstreamStreamAndCompletesWithoutDoneSentinel(t *testing.T) {
	ctx := context.Background()
	var sawStream atomic.Bool
	var sawUsageOption atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode provider request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !payload.Stream {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("fixture response writer cannot hijack connection")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack provider connection: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		sawStream.Store(true)
		sawUsageOption.Store(payload.StreamOptions != nil && payload.StreamOptions.IncludeUsage)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-eof-proof\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"stream survived\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"compact now"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "stream survived") || !strings.Contains(string(raw), "event: message_stop") || strings.Contains(string(raw), "event: error") {
		t.Fatalf("gateway status/body = %d %s", resp.StatusCode, raw)
	}
	if !sawStream.Load() || !sawUsageOption.Load() {
		t.Fatalf("provider stream=%t include_usage=%t", sawStream.Load(), sawUsageOption.Load())
	}
}

func TestCallOpenAICompatibleCancellationInterruptsStreamRead(t *testing.T) {
	started := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer provider.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := (&handler{}).callOpenAICompatible(ctx, store.Provider{
			Name: "fixture", BaseURL: provider.URL,
		}, "", openAIChatRequest{Model: "fixture-model", Stream: true})
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("provider stream did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("callOpenAICompatible() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callOpenAICompatible() did not stop after cancellation")
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
