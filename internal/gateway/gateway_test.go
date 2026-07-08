package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRoutesOpenAICompatibleMessages(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	var gotModel string
	var gotContent string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotModel = payload.Model
		if len(payload.Messages) > 0 {
			gotContent = payload.Messages[len(payload.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"routed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: "env:PROVIDER_KEY"}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()
	if !strings.HasPrefix(server.URL(), "http://127.0.0.1:") {
		t.Fatalf("gateway URL = %q, want loopback", server.URL())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
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
	var decoded struct {
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if decoded.Model != "gpt" || len(decoded.Content) != 1 || decoded.Content[0].Text != "routed" {
		t.Fatalf("gateway response = %#v", decoded)
	}
	if gotAuth != "Bearer provider-secret" || gotModel != "gpt-5" || gotContent != "hello" {
		t.Fatalf("provider received auth=%q model=%q content=%q", gotAuth, gotModel, gotContent)
	}
}

func TestGatewayRoutesForcedAliasClaudeCodeStreamingShape(t *testing.T) {
	ctx := context.Background()
	var gotModel string
	var gotSystem string
	var gotContent string
	var gotUser string
	var gotReasoningEffort string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var payload struct {
			Model           string `json:"model"`
			User            string `json:"user"`
			ReasoningEffort string `json:"reasoning_effort"`
			Messages        []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotModel = payload.Model
		gotUser = payload.User
		gotReasoningEffort = payload.ReasoningEffort
		if len(payload.Messages) > 0 {
			gotSystem = payload.Messages[0].Content
			gotContent = payload.Messages[len(payload.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"streamed route"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ForcedModelAlias: "gpt"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"claude-opus-4-7",
		"max_tokens":20,
		"stream":true,
		"metadata":{"user_id":"test"},
		"output_config":{"effort":"xhigh"},
		"thinking":{"type":"adaptive"},
		"system":[
			{"type":"text","text":"system one"},
			{"type":"text","text":"system two","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hello"},
			{"type":"text","text":"world","cache_control":{"type":"ephemeral"}}
		]}]
	}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages?beta=true", strings.NewReader(body))
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading stream error = %v", err)
	}
	stream := string(raw)
	for _, want := range []string{"event: message_start", "event: content_block_delta", "streamed route", "event: message_stop"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
	if gotModel != "gpt-5" || gotSystem != "system one\nsystem two" || gotContent != "hello\nworld" || gotUser != "test" || gotReasoningEffort != "high" {
		t.Fatalf("provider received model=%q system=%q content=%q user=%q effort=%q", gotModel, gotSystem, gotContent, gotUser, gotReasoningEffort)
	}
}

func TestGatewayForcedAliasPreservesConfiguredRequestAlias(t *testing.T) {
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
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"other route"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddModel(ctx, store.Model{Alias: "other", ProviderName: "litellm", ProviderModel: "other-model", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(other) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", ForcedModelAlias: "gpt"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"other","messages":[{"role":"user","content":"hello"}]}`))
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
	if gotModel != "other-model" {
		t.Fatalf("provider model = %q, want other-model", gotModel)
	}
}

func TestGatewayRejectsUnsupportedProviderFinishReason(t *testing.T) {
	ctx := context.Background()

	for _, stream := range []bool{false, true} {
		stream := stream
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"filtered"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer provider.Close()

			s := newGatewayStoreWithContext(t, ctx, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			body := fmt.Sprintf(`{"model":"gpt","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer local-token")
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
				t.Fatalf("reading error body: %v", err)
			}
			errorBody := string(raw)
			if !strings.Contains(errorBody, "unsupported finish_reason") || !strings.Contains(errorBody, "content_filter") {
				t.Fatalf("gateway error body = %q", raw)
			}
		})
	}
}

func TestGatewayRejectsMissingLocalToken(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	resp, err := http.Post(server.URL()+"/v1/messages", "application/json", strings.NewReader(`{"model":"gpt","messages":[]}`))
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("gateway status = %d, want 401", resp.StatusCode)
	}
}

func TestGatewayRejectsUnsupportedProviderWithoutFallback(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: "https://api.anthropic.com", SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("x-api-key", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
	}
}

func TestGatewayRejectsBlockedAliasWithoutProviderCall(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "blocked"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gateway status = %d, want 403", resp.StatusCode)
	}
	if called {
		t.Fatalf("provider was called for blocked alias")
	}
}

func TestGatewayRejectsUnsupportedFeatures(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","tools":[{"name":"bash"}],"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
	}
}

func TestGatewayRejectsUnknownAnthropicFields(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","temperature":0.2,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
	}
	if called {
		t.Fatalf("provider was called for unsupported Anthropic field")
	}
}

func TestGatewayRejectsUnsupportedAcceptedOptionFields(t *testing.T) {
	ctx := context.Background()

	tests := []string{
		`{"model":"gpt","metadata":{"trace_id":"abc"},"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"gpt","output_config":{"verbosity":"high"},"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"gpt","output_config":{"effort":"extreme"},"messages":[{"role":"user","content":"hello"}]}`,
	}
	for _, body := range tests {
		body := body
		t.Run(body, func(t *testing.T) {
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.Error(w, "should not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()
			s := newGatewayStoreWithContext(t, ctx, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer local-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("gateway request error = %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
			}
			if called {
				t.Fatalf("provider was called for unsupported option field")
			}
		})
	}
}

func TestGatewayRejectsUnsupportedNestedTextBlockFields(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"gpt","messages":[{"role":"user","content":[{"type":"text","text":"hello","citations":[]}]}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", resp.StatusCode)
	}
	if called {
		t.Fatalf("provider was called for unsupported nested text block field")
	}
}

func newGatewayStore(t *testing.T, provider store.Provider, model store.Model) *store.Store {
	t.Helper()
	return newGatewayStoreWithContext(t, context.Background(), provider, model)
}

func newGatewayStoreWithContext(t *testing.T, ctx context.Context, provider store.Provider, model store.Model) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	return s
}

func startGateway(t *testing.T, ctx context.Context, s *store.Store, secrets fakeGatewaySecrets) *Server {
	t.Helper()
	return startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: secrets, Token: "local-token"})
}

func startGatewayWithConfig(t *testing.T, ctx context.Context, cfg Config) *Server {
	t.Helper()
	server, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return server
}

type fakeGatewaySecrets map[string]string

func (f fakeGatewaySecrets) Available(ctx context.Context) error {
	return ctx.Err()
}

func (f fakeGatewaySecrets) Store(ctx context.Context, ref string, value string) error {
	return ctx.Err()
}

func (f fakeGatewaySecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, ok := f[ref]
	if !ok {
		return "", fmt.Errorf("missing secret ref")
	}
	return value, nil
}
