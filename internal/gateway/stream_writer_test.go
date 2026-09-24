package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayStreamsOpenAIContentBeforeProviderCompletion(t *testing.T) {
	ctx := context.Background()
	firstChunk := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-first\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first response\"}}]}\r\n\r\n")
		w.(http.Flusher).Flush()
		close(firstChunk)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" after completion\"},\"finish_reason\":\"stop\"}]}\r\n\r\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\r\n\r\ndata: [DONE]\r\n\r\n")
		w.(http.Flusher).Flush()
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

	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	select {
	case <-firstChunk:
	case <-requestContext.Done():
		t.Fatalf("provider never produced the first chunk: %v", requestContext.Err())
	}

	reader := bufio.NewReader(resp.Body)
	var initial strings.Builder
	for !strings.Contains(initial.String(), "first response") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("reading first gateway stream event: %v; got %q", readErr, initial.String())
		}
		initial.WriteString(line)
	}
	if !strings.Contains(initial.String(), "event: message_start") {
		t.Fatalf("gateway did not start an Anthropic stream before completion: %q", initial.String())
	}
	if strings.Contains(initial.String(), `"input_tokens":0`) {
		t.Fatalf("gateway emitted zero input usage before provider completion: %q", initial.String())
	}

	close(release)
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading completed gateway stream: %v", err)
	}
	stream := initial.String() + string(remainder)
	if !strings.Contains(stream, "after completion") || !strings.Contains(stream, "event: message_stop") {
		t.Fatalf("completed stream missing terminal content: %q", stream)
	}
}

func TestGatewayMarksOpenAIStreamWithoutFinishReasonAsError(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-incomplete\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial response\"}}]}\n\ndata: [DONE]\n\n")
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader("{\"model\":\"gpt\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway stream: %v", err)
	}
	stream := string(body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(stream, "partial response") ||
		!strings.Contains(stream, "event: error") || !strings.Contains(stream, "ended without a finish reason") ||
		strings.Contains(stream, "event: message_stop") {
		t.Fatalf("gateway status/stream = %d %q", resp.StatusCode, stream)
	}
}

func TestGatewayAssignsUniqueMessageIDsToTranslatedStreams(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-constant\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"response\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
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

	first := translatedStreamMessageID(t, ctx, server.URL())
	second := translatedStreamMessageID(t, ctx, server.URL())
	if first == second {
		t.Fatalf("translated stream message ids must be unique, got %q twice", first)
	}
	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "msg_ccr_") || id == "chatcmpl-constant" {
			t.Fatalf("translated stream message id = %q, want a gateway-generated id", id)
		}
	}
}

func translatedStreamMessageID(t *testing.T, ctx context.Context, baseURL string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway stream: %v", err)
	}
	return anthropicMessageStartID(t, string(raw))
}

func anthropicMessageStartID(t *testing.T, stream string) string {
	t.Helper()
	const start = "event: message_start\ndata: "
	offset := strings.Index(stream, start)
	if offset < 0 {
		t.Fatalf("stream is missing message_start: %q", stream)
	}
	data, _, _ := strings.Cut(stream[offset+len(start):], "\n")
	var event struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("decode message_start event: %v; data=%q", err, data)
	}
	if event.Type != "message_start" || event.Message.ID == "" {
		t.Fatalf("message_start event = %#v", event)
	}
	return event.Message.ID
}

func TestGatewayRejectsNonStreamingOpenAIProviderForStreamRequest(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-buffered","choices":[{"message":{"content":"buffered"},"finish_reason":"stop"}]}`)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "did not honor the streaming request") {
		t.Fatalf("gateway status/body = %d %q", resp.StatusCode, body)
	}
}

func TestTranslatedProviderStreamStartsBeforeSlowUpstreamHeaders(t *testing.T) {
	recorder := &flushingResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()
	result := runTranslatedProviderStreamWithTiming(
		context.Background(),
		recorder,
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			time.Sleep(25 * time.Millisecond)
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind:    upstreamStreamFailed,
				failure: &streamFailure{errorClass: "provider_transport", message: "provider connection failed"},
			})
		},
		newOpenAIChatStreamAdapter("gpt", "", "msg_test"),
		5*time.Millisecond,
		5*time.Millisecond,
	)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("stream startup took %s, expected early downstream commitment", elapsed)
	}
	if !result.Committed || result.ErrorClass != "provider_transport" {
		t.Fatalf("stream result = %#v", result)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: error") {
		t.Fatalf("early stream response = %q", body)
	}
}

func TestTranslatedProviderStreamSendsAnthropicPingDuringUpstreamIdle(t *testing.T) {
	recorder := &flushingResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	result := runTranslatedProviderStreamWithTiming(
		context.Background(),
		recorder,
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamReady, status: http.StatusOK}) {
				return
			}
			time.Sleep(25 * time.Millisecond)
			if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamData, data: []byte(`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`)}) {
				return
			}
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
		},
		newOpenAIChatStreamAdapter("gpt", "", "msg_test"),
		5*time.Millisecond,
		5*time.Millisecond,
	)
	if result.ErrorClass != "" || !result.Committed {
		t.Fatalf("stream result = %#v", result)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: ping") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("idle stream response = %q", body)
	}
}

func TestTranslatedProviderStreamKeepsNativeAnthropicStreamAliveBeforeMessageStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder := &heartbeatResponseRecorder{
		flushingResponseRecorder: &flushingResponseRecorder{ResponseRecorder: httptest.NewRecorder()},
		keepalive:                make(chan struct{}, 1), ping: make(chan struct{}, 1),
	}
	result := runTranslatedProviderStreamWithTiming(
		ctx,
		recorder,
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamReady, status: http.StatusOK}) {
				return
			}
			select {
			case <-recorder.keepalive:
			case <-ctx.Done():
				return
			}
			if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind:     upstreamStreamData,
				sseEvent: "message_start",
				data:     []byte(`{"type":"message_start","message":{"id":"msg_upstream","type":"message","role":"assistant","model":"provider","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
			}) {
				return
			}
			select {
			case <-recorder.ping:
			case <-ctx.Done():
				return
			}
			if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind:     upstreamStreamData,
				sseEvent: "message_stop",
				data:     []byte(`{"type":"message_stop"}`),
			}) {
				return
			}
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
		},
		&nativeAnthropicStreamAdapter{responseModel: "claude"},
		5*time.Millisecond,
		5*time.Millisecond,
	)
	if result.ErrorClass != "" || !result.Committed {
		t.Fatalf("stream result = %#v", result)
	}
	body := recorder.Body.String()
	messageStart := strings.Index(body, "event: message_start")
	if messageStart < 0 || !strings.Contains(body, ": ccr-keepalive") {
		t.Fatalf("native idle stream = %q", body)
	}
	if ping := strings.Index(body, "event: ping"); ping >= 0 && ping < messageStart {
		t.Fatalf("native stream emitted ping before message_start: %q", body)
	}
	if !strings.Contains(body, "event: ping") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("native idle stream did not resume protocol heartbeats: %q", body)
	}
}

type heartbeatResponseRecorder struct {
	*flushingResponseRecorder
	keepalive chan struct{}
	ping      chan struct{}
}

func (r *heartbeatResponseRecorder) Write(data []byte) (int, error) {
	n, err := r.flushingResponseRecorder.Write(data)
	var observed chan struct{}
	switch {
	case strings.Contains(string(data), ": ccr-keepalive"):
		observed = r.keepalive
	case strings.Contains(string(data), "event: ping"):
		observed = r.ping
	}
	select {
	case observed <- struct{}{}:
	default:
	}
	return n, err
}

func TestTranslatedProviderStreamRejectsDataBeforeUpstreamReady(t *testing.T) {
	recorder := &flushingResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	result := runTranslatedProviderStreamWithTiming(
		context.Background(),
		recorder,
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamData, data: []byte(`{"choices":[]}`)})
		},
		newOpenAIChatStreamAdapter("gpt", "", "msg_test"),
		time.Second,
		time.Second,
	)
	if result.Committed || result.ErrorClass != "provider_protocol" || result.TerminalPhase != "failed" {
		t.Fatalf("stream result = %#v", result)
	}
	if body := recorder.Body.String(); body != "" {
		t.Fatalf("stream body = %q, want empty", body)
	}
}

type flushingResponseRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *flushingResponseRecorder) Flush() {
	w.flushes++
}
