package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRoutesZAIAnthropicCompatibleProvider(t *testing.T) {
	ctx := context.Background()
	var gotAPIKey string
	var gotBody string
	zai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			t.Fatalf("Z.AI provider model discovery should not be called")
		}
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("Z.AI path = %q, want /v1/messages", r.URL.Path)
		}
		gotAPIKey = r.Header.Get("x-api-key")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading Z.AI body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer zai.Close()

	s := newGatewayStore(t, store.Provider{Name: "zai", Type: "zai", BaseURL: zai.URL, SecretRef: "env:ZAI_API_KEY"}, store.Model{Alias: "glm", ProviderName: "zai", ProviderModel: "glm-4.7", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:ZAI_API_KEY": "zai-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	modelsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(models) error = %v", err)
	}
	modelsReq.Header.Set("Authorization", "Bearer local-token")
	modelsResp, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatalf("gateway models request error = %v", err)
	}
	defer modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("gateway models status = %d, want 200", modelsResp.StatusCode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"glm","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}],"future_field":{"kept":true}}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway Z.AI request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if gotAPIKey != "zai-secret" {
		t.Fatalf("Z.AI provider did not receive API key")
	}
	if !strings.Contains(gotBody, `"model":"glm-4.7"`) || !strings.Contains(gotBody, `"future_field"`) {
		t.Fatalf("Z.AI body = %s", gotBody)
	}
}

func TestGatewayFirstPartyClaudeRouteDoesNotUseZAI(t *testing.T) {
	ctx := context.Background()
	called := false
	zai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer zai.Close()
	anthropicCalled := false
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalled = true
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("Anthropic path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "zai", Type: "zai", BaseURL: zai.URL, SecretRef: "env:ZAI_API_KEY"}, store.Model{Alias: "glm", ProviderName: "zai", ProviderModel: "glm-4.7", Status: "full"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{"env:ZAI_API_KEY": "zai-secret"}, Token: "local-token", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if called {
		t.Fatalf("Z.AI provider was called for unconfigured Claude pass-through")
	}
	if !anthropicCalled {
		t.Fatalf("first-party Anthropic provider was not called")
	}
}

func TestGatewayFirstPartyClaudeRouteIgnoresConfiguredAnthropicProviders(t *testing.T) {
	ctx := context.Background()
	otherCalled := false
	otherAnthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer otherAnthropic.Close()
	canonicalCalled := false
	canonicalAnthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonicalCalled = true
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("canonical Anthropic path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer canonicalAnthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "aaa", Type: "anthropic", BaseURL: otherAnthropic.URL, SecretRef: ""}, store.Model{Alias: "dummy", ProviderName: "aaa", ProviderModel: "claude-opus", Status: "full"})
	if err := s.AddProvider(ctx, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: canonicalAnthropic.URL, SecretRef: ""}); err != nil {
		t.Fatalf("AddProvider(anthropic) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", AnthropicBaseURL: canonicalAnthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-opus","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if !canonicalCalled {
		t.Fatalf("canonical Anthropic provider was not called")
	}
	if otherCalled {
		t.Fatalf("alphabetically first Anthropic provider was called instead of canonical")
	}
}

func TestGatewayRejectsCountTokensWhenProviderCapabilityMissing(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "generic", Type: "anthropic-compatible", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "glm", ProviderName: "generic", ProviderModel: "glm-4.7", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages/count_tokens", strings.NewReader(`{"model":"glm","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway count_tokens request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
	}
	if called {
		t.Fatalf("provider was called for unsupported count_tokens")
	}
}
