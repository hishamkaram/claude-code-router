package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayForwardsImageToOpenAICompatibleProvider(t *testing.T) {
	ctx := context.Background()
	var rawContent json.RawMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var payload struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		if len(payload.Messages) > 0 {
			rawContent = payload.Messages[len(payload.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"looked"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: "env:PROVIDER_KEY"}, store.Model{
		Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "full",
		CapabilityOverrides: modelcap.Values{SupportsVision: modelcap.Bool(true)},
	})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"})
	defer func() { _ = server.Shutdown(ctx) }()

	body := `{"model":"gpt","max_tokens":20,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}` +
		`]}]}`
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}

	var parts []openAIContentPart
	if err := json.Unmarshal(rawContent, &parts); err != nil {
		t.Fatalf("provider content is not a multipart array: %s (%v)", rawContent, err)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "what is this" {
		t.Fatalf("provider content parts = %#v", parts)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("provider image part = %#v", parts[1])
	}
}

func TestGatewayRoutesOpenAICompatibleMessages(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	var gotAPIKey string
	var gotModel string
	var gotContent string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
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

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: "env:PROVIDER_KEY"}, store.Model{
		Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded",
		CapabilityOverrides: modelcap.Values{SupportsThinking: modelcap.Bool(false)},
	})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()
	if !strings.HasPrefix(server.URL(), "http://127.0.0.1:") {
		t.Fatalf("gateway URL = %q, want loopback", server.URL())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","max_tokens":20,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	req.Header.Set("x-api-key", "anthropic-api-key")
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
	if gotAuth != "Bearer provider-secret" {
		t.Fatalf("provider did not receive expected bearer auth")
	}
	if gotAPIKey != "" {
		t.Fatalf("provider received leaked Anthropic x-api-key %q", gotAPIKey)
	}
	if gotModel != "gpt-5" || gotContent != "hello" {
		t.Fatalf("provider received model=%q content=%q", gotModel, gotContent)
	}
}

func TestGatewayRejectsLegacyLiteLLMControlModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalled = true
		http.Error(w, "control model must not reach provider", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL},
		store.Model{Alias: "legacy-control", ProviderName: "litellm", ProviderModel: "all-proxy-models", Status: "degraded"},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages",
		strings.NewReader(`{"model":"legacy-control","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "control model") {
		t.Fatalf("gateway response = status %d body %s", resp.StatusCode, raw)
	}
	if providerCalled {
		t.Fatal("legacy LiteLLM control model reached provider")
	}
}

func TestGatewayRoutesOpenAICompatibleClaudeCodeStreamingShape(t *testing.T) {
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
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"gpt",
		"max_tokens":20,
		"stream":true,
		"metadata":{"user_id":"test"},
		"output_config":{"effort":"xhigh"},
		"thinking":{"type":"enabled","budget_tokens":1024},
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

func TestGatewayTranslatesEnabledThinkingToOpenAIReasoningEffort(t *testing.T) {
	ctx := context.Background()
	var gotReasoningEffort string
	var gotThinking bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		if raw, ok := payload["reasoning_effort"]; ok {
			if err := json.Unmarshal(raw, &gotReasoningEffort); err != nil {
				t.Fatalf("reasoning_effort decode error = %v", err)
			}
		}
		_, gotThinking = payload["thinking"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"routed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`))
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
	if gotReasoningEffort != "high" {
		t.Fatalf("provider reasoning_effort = %q, want high", gotReasoningEffort)
	}
	if gotThinking {
		t.Fatalf("provider received raw Anthropic thinking field")
	}
}

func TestGatewayTranslatesSystemMessageRoleOnOpenAIPath(t *testing.T) {
	ctx := context.Background()
	var gotMessages []openAIMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []openAIMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotMessages = payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"routed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"gpt",
		"system":"top-level system",
		"messages":[
			{"role":"system","content":[{"type":"text","text":"message system","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"hello"}
		]
	}`
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if len(gotMessages) != 3 {
		t.Fatalf("provider messages = %#v, want 3 messages", gotMessages)
	}
	if gotMessages[0].Role != "system" || gotMessages[0].Content != "top-level system" ||
		gotMessages[1].Role != "system" || gotMessages[1].Content != "message system" ||
		gotMessages[2].Role != "user" || gotMessages[2].Content != "hello" {
		t.Fatalf("provider messages = %#v", gotMessages)
	}
}

func TestGatewayDiscoveryShimRoutesConfiguredRequestAlias(t *testing.T) {
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
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	for _, requested := range []string{"anthropic.ccr.other", "claude-ccr-other"} {
		gotModel = ""
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, requested)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest(%q) error = %v", requested, err)
		}
		req.Header.Set("X-CCR-Session-Token", "local-token")
		req.Header.Set("Authorization", "Bearer anthropic-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("gateway request %q error = %v", requested, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("gateway status for %q = %d, want 200", requested, resp.StatusCode)
		}
		if gotModel != "other-model" {
			t.Fatalf("provider model for %q = %q, want other-model", requested, gotModel)
		}
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

func TestGatewayPreservesOpenAICompatibleProviderErrorDetail(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"GLM rejected tool_choice for workflow","x-api-key":"provider-secret","authorization":"Bearer provider-secret"}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: "env:PROVIDER_KEY"}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "glm-5-2", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"})
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
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gateway status = %d, want 429", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading error body: %v", err)
	}
	errorBody := string(raw)
	if !strings.Contains(errorBody, "GLM rejected tool_choice for workflow") {
		t.Fatalf("gateway error body missing provider detail: %q", errorBody)
	}
	if strings.Contains(errorBody, "provider-secret") {
		t.Fatalf("gateway error body leaked provider secret: %q", errorBody)
	}
	if !strings.Contains(errorBody, "[redacted]") {
		t.Fatalf("gateway error body = %q, want redaction marker", errorBody)
	}
}

func TestSanitizeProviderErrorDetailBoundsAndRedacts(t *testing.T) {
	raw := []byte(`{"error":{"message":"` + strings.Repeat("x", providerErrorDetailLimit+64) + `","authorization":"Bearer provider-secret","api_key":"provider-secret"}}`)
	got := sanitizeProviderErrorDetail(raw, "provider-secret")
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("sanitizeProviderErrorDetail() = %q, want truncated marker", got)
	}
	if len(got) > providerErrorDetailLimit+len("...") {
		t.Fatalf("sanitizeProviderErrorDetail() length = %d, want bounded", len(got))
	}
	if strings.Contains(got, "provider-secret") {
		t.Fatalf("sanitizeProviderErrorDetail() leaked provider secret: %q", got)
	}
}

func TestSanitizeProviderErrorDetailRedactsSecretAcrossVisibleBoundary(t *testing.T) {
	secretValue := "provider-secret-crossing-boundary"
	raw := []byte(strings.Repeat("x", providerErrorDetailLimit-4) + secretValue + " tail")
	got := sanitizeProviderErrorDetail(raw, secretValue)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("sanitizeProviderErrorDetail() = %q, want truncated marker", got)
	}
	if strings.Contains(got, secretValue) || strings.Contains(got, secretValue[:len(secretValue)-4]) {
		t.Fatalf("sanitizeProviderErrorDetail() leaked partial provider secret: %q", got)
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

func TestGatewayAcceptsCCRSessionTokenHeaderAndRejectsWrongToken(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	okReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(ok) error = %v", err)
	}
	okReq.Header.Set("X-CCR-Session-Token", "local-token")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("gateway ok request error = %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("gateway ok status = %d, want 200", okResp.StatusCode)
	}

	badReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(bad) error = %v", err)
	}
	badReq.Header.Set("X-CCR-Session-Token", "wrong-token")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("gateway bad request error = %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("gateway bad status = %d, want 401", badResp.StatusCode)
	}
}

func TestGatewayRejectsUnsupportedProviderWithoutFallback(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t, store.Provider{Name: "unsupported", Type: "unsupported", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "unsupported", ProviderModel: "claude-opus", Status: "degraded"})
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
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
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

func TestGatewayRejectsToolsForChatOnlyAliasWithoutProviderCall(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "chat-only"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`))
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
		t.Fatalf("provider was called for chat-only tool request")
	}
}

func TestGatewayEnforcesExplicitModelCapabilityRestrictions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		overrides  modelcap.Values
		body       string
		wantStatus int
	}{
		{
			name: "tools", overrides: modelcap.Values{SupportsTools: modelcap.Bool(false)},
			body:       `{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "explicit parallel tools", overrides: modelcap.Values{SupportsParallelTools: modelcap.Bool(false)},
			body:       `{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":false},"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "thinking", overrides: modelcap.Values{SupportsThinking: modelcap.Bool(false)},
			body:       `{"model":"gpt","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "reasoning effort", overrides: modelcap.Values{SupportsThinking: modelcap.Bool(false)},
			body:       `{"model":"gpt","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "max output", overrides: modelcap.Values{MaxOutputTokens: modelcap.Int64(32)},
			body:       `{"model":"gpt","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "vision", overrides: modelcap.Values{SupportsVision: modelcap.Bool(false)},
			body:       `{"model":"gpt","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			// The OpenAI adapter can translate images, so a model that declares
			// PDF support is still gated because the adapter cannot serialize
			// document input over the OpenAI-compatible path.
			name: "OpenAI adapter PDF gap", overrides: modelcap.Values{SupportsPDFInput: modelcap.Bool(true)},
			body:       `{"model":"gpt","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="}}]}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "system messages", overrides: modelcap.Values{SupportsSystemMessages: modelcap.Bool(false)},
			body:       `{"model":"gpt","system":"instructions","messages":[{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "system role messages", overrides: modelcap.Values{SupportsSystemMessages: modelcap.Bool(false)},
			body:       `{"model":"gpt","messages":[{"role":"system","content":"instructions"},{"role":"user","content":"hello"}]}`,
			wantStatus: http.StatusNotImplemented,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.Error(w, "should not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()
			model := store.Model{
				Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded",
				CapabilityOverrides: test.overrides,
			}
			s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL}, model)
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() { _ = server.Shutdown(ctx) }()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(test.body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer local-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("gateway request error = %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("gateway status = %d, want %d", resp.StatusCode, test.wantStatus)
			}
			if called {
				t.Fatal("provider was called for unsupported model capability")
			}
		})
	}
}

func TestGatewayTranslatesToolsForOpenAICompatibleProviders(t *testing.T) {
	ctx := context.Background()
	var gotToolName string
	var gotToolChoice any
	var gotParallelTools *bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			ToolChoice    any   `json:"tool_choice"`
			ParallelTools *bool `json:"parallel_tool_calls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		if len(payload.Tools) > 0 {
			gotToolName = payload.Tools[0].Function.Name
		}
		gotToolChoice = payload.ToolChoice
		gotParallelTools = payload.ParallelTools
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{
		Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded",
		CapabilityOverrides: modelcap.Values{SupportsParallelTools: modelcap.Bool(false)},
	})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"bash"},"messages":[{"role":"user","content":"hello"}]}`))
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
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if gotToolName != "bash" || gotToolChoice == nil {
		t.Fatalf("provider saw tool=%q choice=%#v", gotToolName, gotToolChoice)
	}
	if gotParallelTools == nil || *gotParallelTools {
		t.Fatalf("provider saw parallel_tool_calls=%v, want false", gotParallelTools)
	}
	if decoded.StopReason != "tool_use" || len(decoded.Content) != 1 || decoded.Content[0].Type != "tool_use" || decoded.Content[0].Name != "bash" || decoded.Content[0].Input["cmd"] != "pwd" {
		t.Fatalf("gateway tool response = %#v", decoded)
	}
}

func TestGatewayTranslatesLegacyFunctionCallResponse(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"","function_call":{"name":"bash","arguments":"{\"cmd\":\"pwd\"}"}},"finish_reason":"function_call"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`))
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
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if decoded.StopReason != "tool_use" || len(decoded.Content) != 1 || decoded.Content[0].Type != "tool_use" || decoded.Content[0].Name != "bash" || decoded.Content[0].Input["cmd"] != "pwd" {
		t.Fatalf("gateway function_call response = %#v", decoded)
	}
}

func TestOpenAIToolArgumentsRepairsTruncatedJSONObject(t *testing.T) {
	got, ok := openAIToolArguments(`{"description":"latest news","prompt":"find latest chatgpt news","subagent_type":"general-purpose"`).(map[string]any)
	if !ok {
		t.Fatalf("openAIToolArguments() = %#v, want object", got)
	}
	if got["description"] != "latest news" || got["prompt"] != "find latest chatgpt news" || got["subagent_type"] != "general-purpose" {
		t.Fatalf("openAIToolArguments() = %#v", got)
	}
}

func TestOpenAIToolArgumentsKeepsUnsafeMalformedJSONVisible(t *testing.T) {
	raw := `{"prompt":"unterminated}`
	got, ok := openAIToolArguments(raw).(map[string]any)
	if !ok {
		t.Fatalf("openAIToolArguments() = %#v, want object", got)
	}
	if got["arguments"] != raw {
		t.Fatalf("openAIToolArguments() = %#v, want raw arguments fallback", got)
	}
}

func TestOpenAIToolArgumentsNormalizesAgentInput(t *testing.T) {
	raw := `{"prompt":"find latest chatgpt news","agent_type":"general-purpose","model":"glm","run_in_background":"false","extra":"ignored"}`
	got, ok := openAIToolArgumentsForTool("Agent", raw).(map[string]any)
	if !ok {
		t.Fatalf("openAIToolArgumentsForTool() = %#v, want object", got)
	}
	if got["prompt"] != "find latest chatgpt news" ||
		got["description"] != "find latest chatgpt news" ||
		got["subagent_type"] != "general-purpose" ||
		got["run_in_background"] != false {
		t.Fatalf("openAIToolArgumentsForTool() = %#v", got)
	}
	if _, ok := got["model"]; ok {
		t.Fatalf("openAIToolArgumentsForTool() kept invalid model: %#v", got)
	}
	if _, ok := got["extra"]; ok {
		t.Fatalf("openAIToolArgumentsForTool() kept extra field: %#v", got)
	}
}

func TestOpenAIToolArgumentsDoesNotNormalizeNonAgentInput(t *testing.T) {
	raw := `{"prompt":"find latest chatgpt news","agent_type":"general-purpose","extra":"kept"}`
	got, ok := openAIToolArgumentsForTool("Bash", raw).(map[string]any)
	if !ok {
		t.Fatalf("openAIToolArgumentsForTool() = %#v, want object", got)
	}
	if got["agent_type"] != "general-purpose" || got["extra"] != "kept" {
		t.Fatalf("openAIToolArgumentsForTool() = %#v, want original non-Agent fields", got)
	}
	if _, ok := got["description"]; ok {
		t.Fatalf("openAIToolArgumentsForTool() normalized non-Agent input: %#v", got)
	}
}

func TestGatewayRejectsUnsupportedAnthropicFieldsOnOpenAIPath(t *testing.T) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","top_p":0.2,"messages":[{"role":"user","content":"hello"}]}`))
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

func TestGatewayDropsContextManagementOnOpenAIPath(t *testing.T) {
	ctx := context.Background()
	var gotContextManagement bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		_, gotContextManagement = payload["context_management"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"gpt","context_management":{"edits":[{"type":"clear_tool_uses"}]},"messages":[{"role":"user","content":"hello"}]}`
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if gotContextManagement {
		t.Fatalf("provider received Anthropic context_management field")
	}
	if got := resp.Header.Get(ccrIgnoredFieldsHeader); got != "context_management" {
		t.Fatalf("%s = %q, want context_management", ccrIgnoredFieldsHeader, got)
	}
}

func TestGatewayIgnoresUnknownAcceptedOptionFields(t *testing.T) {
	ctx := context.Background()

	tests := []string{
		`{"model":"gpt","metadata":{"trace_id":"abc"},"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"gpt","output_config":{"verbosity":"high"},"messages":[{"role":"user","content":"hello"}]}`,
	}
	for _, body := range tests {
		body := body
		t.Run(body, func(t *testing.T) {
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
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
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
			}
			if !called {
				t.Fatalf("provider was not called")
			}
		})
	}
}

func TestGatewayRejectsUnsupportedEffortValue(t *testing.T) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","output_config":{"effort":"extreme"},"messages":[{"role":"user","content":"hello"}]}`))
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
		t.Fatalf("provider was called for unsupported effort")
	}
}

func TestGatewayRejectsUnsupportedThinkingBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		provider store.Provider
		body     string
	}{
		{
			name: "unsupported capability",
			provider: store.Provider{
				Name:                   "litellm",
				Type:                   "litellm",
				BaseURL:                "http://127.0.0.1:1",
				Protocol:               "openai-compatible",
				SupportsTools:          true,
				SupportsStreaming:      true,
				SupportsThinking:       false,
				SupportsModelDiscovery: true,
				Mode:                   "degraded",
			},
			body: `{"model":"gpt","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:     "malformed",
			provider: store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
			body:     `{"model":"gpt","thinking":true,"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:     "unknown type",
			provider: store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
			body:     `{"model":"gpt","thinking":{"type":"budget_tokens"},"messages":[{"role":"user","content":"hello"}]}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.Error(w, "should not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()
			tt.provider.BaseURL = provider.URL
			s := newGatewayStoreWithContext(t, ctx, tt.provider, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(tt.body))
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
				t.Fatalf("provider was called for unsupported thinking")
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

func TestGatewayModelDiscoveryIncludesConfiguredAliases(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("limit") != "1000" {
			t.Fatalf("anthropic discovery path = %q rawQuery=%q", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6"}]}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	if err := s.AddProvider(ctx, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}); err != nil {
		t.Fatalf("AddProvider(anthropic) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "claude-custom", ProviderName: "litellm", ProviderModel: "claude-compatible", Status: "full"}); err != nil {
		t.Fatalf("AddModel(claude-custom) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "sonnet", ProviderName: "litellm", ProviderModel: "third-party-sonnet", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(sonnet) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "glm", ProviderName: "litellm", ProviderModel: "glm-5.2[1m]", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(glm) error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "blocked", ProviderName: "litellm", ProviderModel: "blocked-model", Status: "blocked"}); err != nil {
		t.Fatalf("AddModel(blocked) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", AnthropicBaseURL: anthropic.URL})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models?limit=1000", http.NoBody)
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
	for _, want := range []string{"default", "sonnet", "opus", "haiku", "anthropic.ccr.gpt", "anthropic.ccr.claude-custom", "anthropic.ccr.s%6fnnet", "anthropic.ccr.glm[1m]"} {
		if !containsString(ids, want) {
			t.Fatalf("discovery ids = %#v, missing %q", ids, want)
		}
	}
	for _, hidden := range []string{"gpt", "claude-custom", "claude-native-claude-sonnet-4-6", "anthropic.ccr.blocked", "claude-ccr-gpt"} {
		if containsString(ids, hidden) {
			t.Fatalf("discovery ids = %#v, should hide %q", ids, hidden)
		}
	}
}

func TestGatewayAnthropicPassThroughPreservesToolsAndHeaders(t *testing.T) {
	ctx := context.Background()
	var gotBeta string
	var gotSession string
	var gotLocalAuth string
	var gotAPIKey string
	var gotCCRToken string
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "beta=true" {
			t.Fatalf("anthropic path = %q rawQuery=%q", r.URL.Path, r.URL.RawQuery)
		}
		gotBeta = r.Header.Get("anthropic-beta")
		gotSession = r.Header.Get("x-claude-code-session-id")
		gotLocalAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotCCRToken = r.Header.Get("X-CCR-Session-Token")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading pass-through body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: "env:ANTHROPIC_API_KEY"}, store.Model{Alias: "gpt", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{"env:ANTHROPIC_API_KEY": "upstream-secret"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"gpt","tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}],"future_field":{"kept":true}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages?beta=true", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	req.Header.Set("Authorization", "Bearer anthropic-session")
	req.Header.Set("anthropic-beta", "tools-2026")
	req.Header.Set("x-claude-code-session-id", "session-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway pass-through error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if gotAPIKey != "upstream-secret" {
		t.Fatalf("pass-through did not receive expected API key header")
	}
	if gotBeta != "tools-2026" || gotSession != "session-1" || gotLocalAuth != "" || gotCCRToken != "" {
		t.Fatalf("pass-through headers beta=%q session=%q auth=%q ccr=%q", gotBeta, gotSession, gotLocalAuth, gotCCRToken)
	}
	if !strings.Contains(gotBody, `"model":"claude-opus"`) || !strings.Contains(gotBody, `"future_field"`) {
		t.Fatalf("pass-through body = %s, want provider model rewrite with future field", gotBody)
	}
}

func TestGatewayAnthropicPassThroughFlushesEventStream(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message_start\n")
		_, _ = fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(750 * time.Millisecond)
		_, _ = fmt.Fprint(w, "event: message_stop\n")
		w.(http.Flusher).Flush()
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway stream request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading first stream line: %v", err)
	}
	if line != "event: message_start\n" {
		t.Fatalf("first stream line = %q, want message_start event", line)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading stream data line: %v", err)
	}
	if !strings.Contains(line, `"model":"claude"`) {
		t.Fatalf("stream data line = %q, want alias model", line)
	}
}

func TestGatewayCountTokensUsesAnthropicAliasProviderModel(t *testing.T) {
	ctx := context.Background()
	var gotBody string
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("anthropic path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading count_tokens body: %v", err)
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	}))
	defer anthropic.Close()

	s := newGatewayStore(t, store.Provider{Name: "anthropic", Type: "anthropic", BaseURL: anthropic.URL, SecretRef: ""}, store.Model{Alias: "claude", ProviderName: "anthropic", ProviderModel: "claude-opus", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages/count_tokens", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway count_tokens error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(gotBody, `"model":"claude-opus"`) {
		t.Fatalf("count_tokens body = %s, want provider model rewrite", gotBody)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
