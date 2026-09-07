package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestRouteCacheHintsRequireSupportOnlyForPassthrough(t *testing.T) {
	t.Parallel()
	for _, kind := range []routeKind{routeOpenAI, routeOpenAIResponses, routeAnthropic} {
		route := messageRoute{kind: kind, model: store.Model{Alias: "no-cache"}, modelCapabilities: modelcap.Values{
			SupportsPromptCaching: modelcap.Bool(false),
		}}
		req := anthropicRequest{System: []any{map[string]any{
			"type": "text", "text": "retain me", "cache_control": map[string]any{"type": "ephemeral"},
		}}}
		err := validateRouteModelCapabilities(route, req)
		if (err != nil) != (kind == routeAnthropic) {
			t.Fatalf("route %v: validation error = %v", kind, err)
		}
		if !explicitlyFalse(route.modelCapabilities.SupportsPromptCaching) {
			t.Fatal("validation mutated the advertised capability")
		}
	}
}

func TestGatewayCountsTokensWithoutPromptCacheSupport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "provider", Type: "openai-compatible", BaseURL: "https://unused.invalid"},
		store.Model{Alias: "no-cache", ProviderName: "provider", ProviderModel: "upstream", Status: "degraded", CapabilityOverrides: modelcap.Values{
			SupportsPromptCaching: modelcap.Bool(false),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages/count_tokens", strings.NewReader(`{"model":"no-cache","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("token count status = %d, want 200", response.StatusCode)
	}
}

func TestGatewayAnthropicCacheCapabilityRemainsExplicit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		supported *bool
		cached    bool
		status    int
	}{
		{"supported", modelcap.Bool(true), true, http.StatusOK},
		{"unknown", nil, true, http.StatusOK},
		{"unsupported", modelcap.Bool(false), true, http.StatusNotImplemented},
		{"uncached", modelcap.Bool(false), false, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			captured := make(chan string, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "read failed", http.StatusBadRequest)
					return
				}
				captured <- string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"msg_cache","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer provider.Close()
			s := newGatewayStore(t, store.Provider{Name: "provider", Type: "anthropic", BaseURL: provider.URL},
				store.Model{Alias: "native", ProviderName: "provider", ProviderModel: "upstream", Status: "full", CapabilityOverrides: modelcap.Values{
					SupportsPromptCaching: test.supported,
				}},
			)
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() { _ = server.Shutdown(ctx) }()
			cache := ""
			if test.cached {
				cache = `,"cache_control":{"type":"ephemeral"}`
			}
			response := postGatewayMessage(t, ctx, server.URL(), `{"model":"native","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"retain-me"`+cache+`}]}]}`)
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if test.status != http.StatusOK {
				if len(captured) != 0 {
					t.Fatal("unsupported cache feature reached upstream")
				}
				return
			}
			body := <-captured
			if !strings.Contains(body, "retain-me") || strings.Contains(body, "cache_control") != test.cached {
				t.Fatalf("Anthropic passthrough content changed: %s", body)
			}
		})
	}
}

func TestGatewayTranslatesCacheHintsWithoutProviderCacheSupport(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{modelcap.KindChat, modelcap.KindResponses} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			captured := make(chan string, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/v1/chat/completions"
				if kind == modelcap.KindResponses {
					wantPath = "/v1/responses"
				}
				if r.URL.Path != wantPath {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, wantPath)
					http.Error(w, "wrong protocol", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "read failed", http.StatusBadRequest)
					return
				}
				captured <- string(body)
				w.Header().Set("Content-Type", "application/json")
				if kind == modelcap.KindResponses {
					_, _ = fmt.Fprint(w, `{"id":"resp_cache","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
				} else {
					_, _ = fmt.Fprint(w, `{"id":"chat_cache","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				}
			}))
			defer provider.Close()
			s := newGatewayStore(t,
				store.Provider{Name: "provider", Type: "openai-compatible", BaseURL: provider.URL, SupportsResponses: true},
				store.Model{Alias: "no-cache", ProviderName: "provider", ProviderModel: "upstream", Status: "degraded", CapabilityOverrides: modelcap.Values{
					Kind: kind, SupportsResponses: modelcap.Bool(kind == modelcap.KindResponses), SupportsPromptCaching: modelcap.Bool(false),
				}},
			)
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() { _ = server.Shutdown(ctx) }()
			response := postGatewayMessage(t, ctx, server.URL(), `{"model":"no-cache","max_tokens":16,"system":[{"type":"text","text":"retain-system","cache_control":{"type":"ephemeral"}}],"context_management":{"edits":[]},"messages":[{"role":"user","content":[{"type":"text","text":"retain-user","cache_control":{"type":"ephemeral"}}]}]}`)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d: %s", response.StatusCode, body)
			}
			for _, field := range []string{"cache_control", "context_management"} {
				if !strings.Contains(response.Header.Get(ccrIgnoredFieldsHeader), field) {
					t.Fatalf("missing ignored field %q: %v", field, response.Header)
				}
			}
			body := <-captured
			if !json.Valid([]byte(body)) || strings.Contains(body, "cache_control") || !strings.Contains(body, "retain-system") || !strings.Contains(body, "retain-user") {
				t.Fatalf("upstream request did not preserve content without cache hints: %s", body)
			}
		})
	}
}
