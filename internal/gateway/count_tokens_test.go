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

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const countTokensTestBody = `{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`

func TestGatewayCountTokensUsesLiteLLMProviderEndpoint(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	var gotAPIKey string
	var gotCCRToken string
	var gotAnthropicVersion string
	var gotBody string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("provider path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotCCRToken = r.Header.Get(ccrSessionTokenHeader)
		gotAnthropicVersion = r.Header.Get("anthropic-version")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading provider body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":11}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: "env:LITELLM_KEY"}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "glm-5.2", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:LITELLM_KEY": "provider-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages/count_tokens", strings.NewReader(countTokensTestBody))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	req.Header.Set("x-api-key", "anthropic-api-key")
	req.Header.Set(ccrSessionTokenHeader, "local-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway count_tokens request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	assertCountTokensResponse(t, resp, 11, tokenCountModeProvider, "")
	if gotAuth != "Bearer provider-secret" {
		t.Fatalf("provider Authorization = %q, want provider bearer", gotAuth)
	}
	if gotAPIKey != "" || gotCCRToken != "" || gotAnthropicVersion != "" {
		t.Fatalf("provider received leaked headers x-api-key=%q ccr=%q anthropic-version=%q", gotAPIKey, gotCCRToken, gotAnthropicVersion)
	}
	if !strings.Contains(gotBody, `"model":"glm-5.2"`) {
		t.Fatalf("provider body = %s, want provider model rewrite", gotBody)
	}
}

func TestGatewayCountTokensTreatsStoredLiteLLMAsProviderBacked(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	}))
	defer provider.Close()

	stored := store.Provider{
		Name:                   "litellm",
		Type:                   "litellm",
		BaseURL:                provider.URL,
		Protocol:               providers.ProtocolOpenAICompatible,
		SupportsTools:          true,
		SupportsStreaming:      true,
		SupportsThinking:       true,
		SupportsModelDiscovery: true,
		SupportsCountTokens:    false,
		Mode:                   providers.ModeDegraded,
	}
	s := newGatewayStore(t, stored, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	resp := postCountTokens(t, ctx, server.URL())
	defer resp.Body.Close()
	assertCountTokensResponse(t, resp, 7, tokenCountModeProvider, "")
	if !called {
		t.Fatalf("stored LiteLLM provider was not called for exact count_tokens")
	}
}

func TestGatewayCountTokensEstimatesWhenExactUnavailable(t *testing.T) {
	ctx := context.Background()
	tests := map[string]store.Provider{
		"generic openai-compatible": {Name: "provider", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		"openrouter":                {Name: "provider", Type: "openrouter", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		"local":                     {Name: "provider", Type: "local", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		"anthropic-compatible":      {Name: "provider", Type: "anthropic-compatible", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
	}
	for name, provider := range tests {
		provider := provider
		t.Run(name, func(t *testing.T) {
			s := newGatewayStoreWithContext(t, ctx, provider, store.Model{Alias: "gpt", ProviderName: "provider", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			resp := postCountTokens(t, ctx, server.URL())
			defer resp.Body.Close()
			assertCountTokensResponse(t, resp, len(countTokensTestBody), tokenCountModeEstimated, "")
		})
	}
}

func TestGatewayCountTokensFallbackHeaders(t *testing.T) {
	ctx := context.Background()
	tests := map[string]struct {
		secretRef string
		secrets   fakeGatewaySecrets
		handler   http.HandlerFunc
		want      string
	}{
		"secret": {
			secretRef: "env:MISSING",
			secrets:   fakeGatewaySecrets{},
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("provider should not be called when secret resolution fails")
			},
			want: tokenCountFallbackSecret,
		},
		"upstream status": {
			secrets: fakeGatewaySecrets{"env:LITELLM_KEY": "secret-value"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			want: tokenCountFallbackUpstreamStatus,
		},
		"malformed json": {
			secrets: fakeGatewaySecrets{"env:LITELLM_KEY": "secret-value"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{`)
			},
			want: tokenCountFallbackInvalidResponse,
		},
		"invalid count": {
			secrets: fakeGatewaySecrets{"env:LITELLM_KEY": "secret-value"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{"input_tokens":-1}`)
			},
			want: tokenCountFallbackInvalidResponse,
		},
	}
	for name, tt := range tests {
		tt := tt
		t.Run(name, func(t *testing.T) {
			provider := httptest.NewServer(tt.handler)
			defer provider.Close()
			secretRef := tt.secretRef
			if secretRef == "" {
				secretRef = "env:LITELLM_KEY"
			}
			s := newGatewayStoreWithContext(t, ctx, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: secretRef}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, tt.secrets)
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			resp := postCountTokens(t, ctx, server.URL())
			defer resp.Body.Close()
			raw := assertCountTokensResponse(t, resp, len(countTokensTestBody), tokenCountModeEstimated, tt.want)
			if strings.Contains(raw, "secret-value") {
				t.Fatalf("response leaked provider secret: %q", raw)
			}
		})
	}
}

func TestGatewayCountTokensTransportFallback(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := provider.URL
	provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: baseURL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	resp := postCountTokens(t, ctx, server.URL())
	defer resp.Body.Close()
	assertCountTokensResponse(t, resp, len(countTokensTestBody), tokenCountModeEstimated, tokenCountFallbackTransport)
}

func TestOpenAICountTokensCancellationDoesNotReturnFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &handler{}
	_, fallback, ok := h.callOpenAICompatibleCountTokens(ctx, store.Provider{Name: "litellm", BaseURL: "http://127.0.0.1:1"}, "", []byte(`{"model":"gpt"}`))
	if ok || fallback != "" {
		t.Fatalf("callOpenAICompatibleCountTokens() ok=%v fallback=%q, want canceled without fallback", ok, fallback)
	}
}

func postCountTokens(t *testing.T, ctx context.Context, gatewayURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages/count_tokens", strings.NewReader(countTokensTestBody))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway count_tokens request error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("gateway status = %d, want 200: %s", resp.StatusCode, raw)
	}
	return resp
}

func assertCountTokensResponse(t *testing.T, resp *http.Response, wantTokens int, wantMode, wantFallback string) string {
	t.Helper()
	if got := resp.Header.Get(ccrTokenCountModeHeader); got != wantMode {
		t.Fatalf("%s = %q, want %q", ccrTokenCountModeHeader, got, wantMode)
	}
	if got := resp.Header.Get(ccrTokenCountFallbackHeader); got != wantFallback {
		t.Fatalf("%s = %q, want %q", ccrTokenCountFallbackHeader, got, wantFallback)
	}
	var decoded struct {
		InputTokens int `json:"input_tokens"`
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading count_tokens response: %v", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding count_tokens response: %v", err)
	}
	if decoded.InputTokens != wantTokens {
		t.Fatalf("input_tokens = %d, want %d", decoded.InputTokens, wantTokens)
	}
	return string(raw)
}
