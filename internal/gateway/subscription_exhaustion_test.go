package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayFirstPartyAnthropic429ReportsSubscriptionExhaustion(t *testing.T) {
	ctx := context.Background()
	const headerSecret = "header-secret-value"
	const bodySecret = "body-secret-value"
	upstreamBody := `{"type":"error","error":{"type":"rate_limit_error","message":"` + bodySecret + `"}}`
	events := make(chan AnthropicSubscriptionExhaustionEvent, 1)
	var upstreamCalls atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("anthropic path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer anthropic-session" {
			t.Fatalf("anthropic auth = %q, want incoming subscription auth", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Upstream-Secret", headerSecret)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, upstreamBody)
	}))
	defer anthropic.Close()

	server := startFirstPartySubscriptionGateway(t, ctx, anthropic.URL, events)
	defer shutdownGateway(t, ctx, server)

	resp := postSubscriptionGatewayMessage(t, ctx, server, "claude-opus-4-7")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gateway status = %d, want 429", resp.StatusCode)
	}
	if string(raw) != upstreamBody {
		t.Fatalf("gateway body = %q, want exact upstream body %q", raw, upstreamBody)
	}
	if resp.Header.Get("Retry-After") != "7" || resp.Header.Get("X-Upstream-Secret") != headerSecret {
		t.Fatalf("gateway headers Retry-After=%q X-Upstream-Secret=%q", resp.Header.Get("Retry-After"), resp.Header.Get("X-Upstream-Secret"))
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}

	event := receiveSubscriptionEvent(t, events)
	if event.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("event status = %d, want 429", event.StatusCode)
	}
	if event.RetryAfterDuration != 7*time.Second || !event.RetryAfterTime.IsZero() {
		t.Fatalf("event retry metadata = duration %s time %s, want 7s and zero time", event.RetryAfterDuration, event.RetryAfterTime)
	}
	assertSubscriptionEventHasNoRawData(t, event, []string{
		headerSecret,
		bodySecret,
		"anthropic-session",
		"local-token",
		"Authorization",
		"X-Upstream-Secret",
		"Retry-After",
	})
}

func TestGatewayFirstPartyAnthropic429DropsUnreadSubscriptionSink(t *testing.T) {
	ctx := context.Background()
	const upstreamBody = `{"type":"error","error":{"type":"rate_limit_error","message":"subscription exhausted"}}`
	unread := make(chan AnthropicSubscriptionExhaustionEvent)
	var upstreamCalls atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, upstreamBody)
	}))
	defer anthropic.Close()

	server := startFirstPartySubscriptionGateway(t, ctx, anthropic.URL, unread)
	defer shutdownGateway(t, ctx, server)

	resp := postSubscriptionGatewayMessage(t, ctx, server, "claude-opus-4-7")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || string(raw) != upstreamBody {
		t.Fatalf("gateway response status=%d body=%q, want preserved 429 body", resp.StatusCode, raw)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
	select {
	case event := <-unread:
		t.Fatalf("unexpected event delivered to unread sink: %#v", event)
	default:
	}
}

func TestGatewaySubscriptionExhaustionEventOnlyFiresForFirstPartyAnthropic429(t *testing.T) {
	t.Run("registered Anthropic provider 429", func(t *testing.T) {
		ctx := context.Background()
		events := make(chan AnthropicSubscriptionExhaustionEvent, 1)
		var upstreamCalls atomic.Int32
		anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":"registered provider 429"}`)
		}))
		defer anthropic.Close()
		s := newGatewayStore(t,
			store.Provider{Name: "registered-anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: "env:ANTHROPIC_API_KEY"},
			store.Model{Alias: "registered", ProviderName: "registered-anthropic", ProviderModel: "claude-opus", Status: "full"},
		)
		server := startGatewayWithConfig(t, ctx, Config{
			Store:                           s,
			Secrets:                         fakeGatewaySecrets{"env:ANTHROPIC_API_KEY": "registered-secret"},
			Token:                           "local-token",
			AnthropicSubscriptionExhaustion: events,
		})
		defer shutdownGateway(t, ctx, server)

		resp := postSubscriptionGatewayMessage(t, ctx, server, "registered")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("gateway status = %d, want 429", resp.StatusCode)
		}
		if upstreamCalls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
		}
		assertNoSubscriptionEvent(t, events)
	})
}

func TestGatewayOpenAI429DoesNotReportSubscriptionExhaustion(t *testing.T) {
	t.Run("OpenAI compatible provider 429", func(t *testing.T) {
		ctx := context.Background()
		events := make(chan AnthropicSubscriptionExhaustionEvent, 1)
		var upstreamCalls atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"message":"OpenAI-compatible provider exhausted"}}`)
		}))
		defer provider.Close()
		s := newGatewayStore(t,
			store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""},
			store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
		)
		server := startGatewayWithConfig(t, ctx, Config{
			Store:                           s,
			Secrets:                         fakeGatewaySecrets{},
			Token:                           "local-token",
			AnthropicSubscriptionExhaustion: events,
		})
		defer shutdownGateway(t, ctx, server)

		resp := postSubscriptionGatewayMessage(t, ctx, server, "gpt")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("gateway status = %d, want 429", resp.StatusCode)
		}
		if upstreamCalls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
		}
		assertNoSubscriptionEvent(t, events)
	})
}

func TestGatewayFirstPartyNon429DoesNotReportSubscriptionExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "first-party Anthropic 529",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
		},
		{
			name:   "first-party Anthropic 200",
			status: http.StatusOK,
			body:   `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			events := make(chan AnthropicSubscriptionExhaustionEvent, 1)
			var upstreamCalls atomic.Int32
			anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer anthropic.Close()
			server := startFirstPartySubscriptionGateway(t, ctx, anthropic.URL, events)
			defer shutdownGateway(t, ctx, server)

			resp := postSubscriptionGatewayMessage(t, ctx, server, "claude-opus-4-7")
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("gateway status = %d, want %d", resp.StatusCode, tc.status)
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
			}
			assertNoSubscriptionEvent(t, events)
		})
	}
}

func TestAnthropicSubscriptionExhaustionEventParsesRetryAfterDate(t *testing.T) {
	retryAt := time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{retryAt.Format(http.TimeFormat)}},
	}
	event := newAnthropicSubscriptionExhaustionEvent(resp)
	if event.StatusCode != http.StatusTooManyRequests || !event.RetryAfterTime.Equal(retryAt) || event.RetryAfterDuration != 0 {
		t.Fatalf("event = %#v, want status 429 and retry-at %s", event, retryAt)
	}
}

func startFirstPartySubscriptionGateway(
	t *testing.T,
	ctx context.Context,
	anthropicBaseURL string,
	sink chan<- AnthropicSubscriptionExhaustionEvent,
) *Server {
	t.Helper()
	s := newGatewayStoreWithContext(t, ctx,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	return startGatewayWithConfig(t, ctx, Config{
		Store:                           s,
		Secrets:                         fakeGatewaySecrets{},
		Token:                           "local-token",
		DefaultModelAlias:               "gpt",
		AnthropicBaseURL:                anthropicBaseURL,
		AnthropicSubscriptionExhaustion: sink,
	})
}

func postSubscriptionGatewayMessage(t *testing.T, ctx context.Context, server *Server, model string) *http.Response {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	t.Cleanup(cancel)
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	return resp
}

func receiveSubscriptionEvent(t *testing.T, events <-chan AnthropicSubscriptionExhaustionEvent) AnthropicSubscriptionExhaustionEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatalf("subscription event was not delivered")
		return AnthropicSubscriptionExhaustionEvent{}
	}
}

func assertNoSubscriptionEvent(t *testing.T, events <-chan AnthropicSubscriptionExhaustionEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected subscription exhaustion event: %#v", event)
	default:
	}
}

func assertSubscriptionEventHasNoRawData(t *testing.T, event AnthropicSubscriptionExhaustionEvent, forbidden []string) {
	t.Helper()
	eventType := reflect.TypeOf(event)
	headerType := reflect.TypeOf(http.Header{})
	bodyType := reflect.TypeOf([]byte{})
	for i := range eventType.NumField() {
		field := eventType.Field(i)
		if field.Type.Kind() == reflect.String || field.Type == headerType || field.Type == bodyType {
			t.Fatalf("event field %s has raw-data-capable type %s", field.Name, field.Type)
		}
	}
	rendered := fmt.Sprintf("%#v", event)
	for _, value := range forbidden {
		if strings.Contains(rendered, value) {
			t.Fatalf("event leaked %q in %#v", value, event)
		}
	}
}

func shutdownGateway(t *testing.T, ctx context.Context, server *Server) {
	t.Helper()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}
