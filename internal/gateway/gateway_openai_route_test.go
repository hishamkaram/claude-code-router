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

	var parts []map[string]any
	if err := json.Unmarshal(rawContent, &parts); err != nil {
		t.Fatalf("provider content is not a multipart array: %s (%v)", rawContent, err)
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[0]["text"] != "what is this" {
		t.Fatalf("provider content parts = %#v", parts)
	}
	imageURL, ok := parts[1]["image_url"].(map[string]any)
	if !ok || parts[1]["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,AAAA" {
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
