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

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayFirstPartyClaudeModelBeatsDefaultAliasFallback(t *testing.T) {
	ctx := context.Background()
	var gotModel string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"default route"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	anthropicCalled := false
	var gotAnthropicAuth string
	var gotAnthropicBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			anthropicCalled = true
			gotAnthropicAuth = r.Header.Get("Authorization")
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading Anthropic body: %v", err)
			}
			gotAnthropicBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"native"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddProvider(ctx, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}); err != nil {
		t.Fatalf("AddProvider(anthropic) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if decoded.Model != "claude-opus-4-7" {
		t.Fatalf("gateway response model = %q, want first-party Claude model", decoded.Model)
	}
	if gotModel != "" {
		t.Fatalf("OpenAI-compatible default provider model = %q, want no call", gotModel)
	}
	if !anthropicCalled {
		t.Fatalf("Anthropic pass-through was not called for first-party Claude model")
	}
	if gotAnthropicAuth != "Bearer anthropic-session" {
		t.Fatalf("Anthropic auth = %q, want incoming subscription auth", gotAnthropicAuth)
	}
	if !strings.Contains(gotAnthropicBody, `"model":"claude-opus-4-7"`) {
		t.Fatalf("Anthropic body = %s, want requested Claude model", gotAnthropicBody)
	}
}

func TestGatewayRoutesFullClaudeModelIDToFirstParty(t *testing.T) {
	ctx := context.Background()
	openAICalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalled = true
		http.Error(w, "advertised Anthropic model should pass through", http.StatusInternalServerError)
	}))
	defer provider.Close()
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-7","display_name":"Claude Opus 4.7"}]}`)
		case "/v1/messages":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading anthropic body: %v", err)
			}
			gotBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"native"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`))
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
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if decoded.Model != "claude-opus-4-7" {
		t.Fatalf("gateway response model = %q, want full Claude model ID", decoded.Model)
	}
	if openAICalled {
		t.Fatalf("OpenAI-compatible provider was called for advertised Anthropic model")
	}
	if !strings.Contains(gotBody, `"model":"claude-opus-4-7"`) {
		t.Fatalf("Anthropic pass-through body = %s, want original advertised model", gotBody)
	}
}

func TestGatewayModelDiscoveryIncludesFirstPartyWithoutNativeShim(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-7","display_name":"Claude Opus 4.7"}]}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddProvider(ctx, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}); err != nil {
		t.Fatalf("AddProvider(anthropic) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway models request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway model decode error = %v", err)
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		ids = append(ids, item.ID)
	}
	for _, want := range []string{"default", "sonnet", "opus", "haiku", "opusplan", "opusplan[1m]", "anthropic.ccr.gpt"} {
		if !containsString(ids, want) {
			t.Fatalf("discovery ids = %#v, missing %q", ids, want)
		}
	}
	if containsString(ids, "claude-native-claude-opus-4-7") {
		t.Fatalf("discovery ids = %#v, want no stale native-prefixed Anthropic model", ids)
	}
}

func TestGatewayFirstPartyClaudeRouteDoesNotFallBackWhenDiscoveryFails(t *testing.T) {
	ctx := context.Background()
	openAICalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalled = true
		http.Error(w, "must not silently fall back", http.StatusInternalServerError)
	}))
	defer provider.Close()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"native"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}]}`))
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
	if openAICalled {
		t.Fatalf("OpenAI-compatible provider was called after Anthropic discovery failure")
	}
}

func TestGatewayRoutesOpusPlan1MToFirstParty(t *testing.T) {
	ctx := context.Background()
	openAICalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalled = true
		http.Error(w, "opusplan[1m] should pass through", http.StatusInternalServerError)
	}))
	defer provider.Close()
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading anthropic body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"opusplan[1m]","content":[{"type":"text","text":"plan"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"opusplan[1m]","messages":[{"role":"user","content":"hello"}]}`))
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
	if openAICalled {
		t.Fatalf("OpenAI-compatible provider was called for opusplan[1m]")
	}
	if !strings.Contains(gotBody, `"model":"opusplan[1m]"`) {
		t.Fatalf("Anthropic pass-through body = %s, want original opusplan[1m] model", gotBody)
	}
}

func TestGatewayModelsKeepsFirstPartyAliasesWhenAnthropicDiscoveryFails(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway models request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway model decode error = %v", err)
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		ids = append(ids, item.ID)
	}
	for _, want := range []string{"default", "sonnet", "anthropic.ccr.gpt"} {
		if !containsString(ids, want) {
			t.Fatalf("discovery ids = %#v, missing %q", ids, want)
		}
	}
}

func TestGatewayExactAliasWithDiscoveryPrefixWinsOverShim(t *testing.T) {
	ctx := context.Background()
	var gotModel string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddModel(ctx, store.Model{Alias: "claude-ccr-gpt", ProviderName: "litellm", ProviderModel: "prefix-model", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(prefix alias) error = %v", err)
	}
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude-ccr-gpt","messages":[{"role":"user","content":"hello"}]}`))
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
	if gotModel != "prefix-model" {
		t.Fatalf("provider model = %q, want prefix-model", gotModel)
	}
}

func TestGatewayRoutesSelectivelyEscapedDiscoveryAliases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "sonnet-opus-haiku", ProviderName: "litellm", ProviderModel: "third-party-model", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token"}}
	requested := "anthropic.ccr.s%6fnnet-%6fpus-h%61iku"

	route, validationErr := h.selectRoute(ctx, requested)
	if validationErr != nil {
		t.Fatalf("selectRoute(%q) error = %#v", requested, validationErr)
	}
	if route.model.Alias != "sonnet-opus-haiku" {
		t.Fatalf("route alias = %q, want sonnet-opus-haiku", route.model.Alias)
	}
	if route.responseModel != requested {
		t.Fatalf("response model = %q, want %q", route.responseModel, requested)
	}
}

func TestGatewayRejectsUnknownOrMalformedCanonicalDiscoveryAliases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt"}}

	for _, requested := range []string{
		"anthropic.ccr.unknown",
		"anthropic.ccr.sonnet",
		"anthropic.ccr.g%70t",
		"anthropic.ccr.",
	} {
		t.Run(requested, func(t *testing.T) {
			_, validationErr := h.selectRoute(ctx, requested)
			if validationErr == nil {
				t.Fatalf("selectRoute(%q) succeeded, want rejection", requested)
			}
			if validationErr.status != http.StatusBadRequest {
				t.Fatalf("selectRoute(%q) status = %d, want %d", requested, validationErr.status, http.StatusBadRequest)
			}
		})
	}
}

func TestGatewayRejectsUnknownNonClaudeModelWithDefaultAlias(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: "gpt"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"unknown-model","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("gateway status = %d, want 502", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if !strings.Contains(string(raw), "refusing to route it to the startup alias") {
		t.Fatalf("gateway response = %s, want visible startup alias refusal", raw)
	}
	if called {
		t.Fatalf("provider was called for unknown non-Claude model")
	}
}
