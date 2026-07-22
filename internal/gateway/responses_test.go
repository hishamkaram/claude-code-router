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
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRoutesResponsesModelWithoutChatFallback(t *testing.T) {
	ctx := context.Background()
	var chatCalls int
	var responsesCalls int
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			http.Error(w, "Chat fallback is forbidden", http.StatusBadGateway)
		case "/v1/responses":
			responsesCalls++
			if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
				t.Fatalf("decode Responses request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses-routed"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsThinking: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsThinking: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"metadata":{"user_id":"fixture-user"},"thinking":{"type":"enabled"},"output_config":{"effort":"medium"},"context_management":{"edits":[{"type":"clear_tool_uses"}]},"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	var payload struct {
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if chatCalls != 0 || responsesCalls != 1 {
		t.Fatalf("upstream calls chat=%d responses=%d, want 0 and 1", chatCalls, responsesCalls)
	}
	if providerRequest.Model != "gpt-responses" || providerRequest.Metadata["user_id"] != "fixture-user" || providerRequest.Reasoning == nil || providerRequest.Reasoning.Effort != "medium" || len(providerRequest.Input) != 1 ||
		len(providerRequest.Input[0].Content) != 1 || providerRequest.Input[0].Content[0].Text != "hello" {
		t.Fatalf("provider request = %#v", providerRequest)
	}
	if payload.Model != "responses" || len(payload.Content) != 1 || payload.Content[0].Text != "responses-routed" {
		t.Fatalf("gateway response = %#v", payload)
	}
}

func TestGatewayResponsesRouteInjectsIdentityInstructionsForModelQuestion(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"glm"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsSystemMessages: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"anthropic.ccr.responses",
		"system":"Be concise.",
		"messages":[
			{"role":"user","content":"which model were you earlier?"},
			{"role":"assistant","content":"I am Sonnet."},
			{"role":"user","content":"which model are you now?"}
		]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if !strings.Contains(providerRequest.Instructions, "Be concise.") ||
		!strings.Contains(providerRequest.Instructions, `CCR alias "responses"`) ||
		!strings.Contains(providerRequest.Instructions, `provider "openai"`) ||
		!strings.Contains(providerRequest.Instructions, `provider model "gpt-responses"`) ||
		!strings.Contains(providerRequest.Instructions, `Claude Code requested model ID "anthropic.ccr.responses"`) {
		t.Fatalf("Responses instructions = %q", providerRequest.Instructions)
	}
	if len(providerRequest.Input) != 3 || providerRequest.Input[1].Role != "assistant" {
		t.Fatalf("Responses input = %#v, want original transcript preserved", providerRequest.Input)
	}
}

func TestGatewayResponsesRouteSuppressesIdentityWhenSystemMessagesUnsupported(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"unknown"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsSystemMessages: modelcap.Bool(false),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"anthropic.ccr.responses","messages":[{"role":"user","content":"which model are you now?"}]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if providerRequest.Instructions != "" {
		t.Fatalf("Responses instructions = %q, want no injected route identity", providerRequest.Instructions)
	}
}

func TestGatewayResponsesRouteForcesSerialToolCallsWhenModelDisallowsParallelTools(t *testing.T) {
	ctx := context.Background()
	var providerRequest openairesponses.Request
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Fatalf("decode Responses request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsParallelTools: modelcap.Bool(false),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"responses","max_tokens":16,
		"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"bash"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway status = %d, want 200: %s", response.StatusCode, body)
	}
	if providerRequest.ParallelToolCalls == nil || *providerRequest.ParallelToolCalls {
		t.Fatalf("Responses provider saw parallel_tool_calls=%v, want false", providerRequest.ParallelToolCalls)
	}
}

func TestGatewayRejectsResponsesComputerUseWithoutManagedExecutor(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		secretRef string
		secrets   fakeGatewaySecrets
		content   string
	}{
		{
			name:      "before secret resolution",
			secretRef: "env:PROVIDER_KEY",
			secrets:   fakeGatewaySecrets{},
			content:   `"take a screenshot"`,
		},
		{
			name:      "before image normalization",
			secretRef: "env:PROVIDER_KEY",
			secrets:   fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"},
			content: `[{"type":"text","text":"take a screenshot"},
				{"type":"image","source":{"type":"url","url":"https://127.0.0.1/private.png"}}]`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			providerCalled := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalled = true
				http.Error(w, "must not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()

			s := newGatewayStoreWithContext(t, ctx,
				store.Provider{
					Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SecretRef: test.secretRef,
					SupportsTools: true, SupportsStreaming: true, SupportsResponses: true,
				},
				store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
					Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(true),
					SupportsVision: modelcap.Bool(true),
				}},
			)
			server := startGatewayWithConfig(t, ctx, Config{
				Store: s, Secrets: test.secrets, Token: "local-token",
			})
			defer func() { _ = server.Shutdown(ctx) }()

			response := postGatewayMessage(t, ctx, server.URL(), `{
				"model":"cua","max_tokens":16,
				"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768,"display_number":1}],
				"messages":[{"role":"user","content":`+test.content+`}]
			}`)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read gateway response: %v", err)
			}
			if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "requires a CCR managed CUA executor") {
				t.Fatalf("gateway status=%d body=%s, want managed CUA rejection", response.StatusCode, body)
			}
			if strings.Contains(string(body), "provider secret") || strings.Contains(string(body), "image URL") {
				t.Fatalf("gateway performed work before managed CUA rejection: %s", body)
			}
			if providerCalled {
				t.Fatal("Responses provider was called for unmanaged computer use")
			}
		})
	}
}

func TestGatewayRejectsResponsesOnlyAliasOnAnthropicCompatibleProvider(t *testing.T) {
	ctx := context.Background()
	providerCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "anthropic", Type: "anthropic-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "responses", ProviderName: "anthropic", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "requires the OpenAI Responses API") {
		t.Fatalf("gateway status=%d body=%s, want Responses route rejection", response.StatusCode, body)
	}
	if providerCalled {
		t.Fatal("Anthropic-compatible provider was called for a Responses-only alias")
	}
}

func TestGatewayModelDiscoveryExcludesResponsesAliasWithoutProviderSupport(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", SupportsTools: true, SupportsStreaming: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway discovery request error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("gateway discovery status=%d body=%s, want 200", response.StatusCode, body)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode gateway discovery response: %v", err)
	}
	for _, model := range payload.Data {
		if model.ID == "anthropic.ccr.responses" {
			t.Fatalf("gateway discovery advertised a Responses alias whose provider lacks Responses support: %#v", payload.Data)
		}
	}
}

func TestGatewayReturnsBadGatewayForUnknownResponsesOutput(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_unknown","output":[{"type":"future_item"}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "unsupported Responses output item type") {
		t.Fatalf("gateway status=%d body=%s, want 502 malformed provider output", response.StatusCode, body)
	}
}

func TestGatewayReturnsBadGatewayForUnsuccessfulResponsesStatus(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_failed","status":"failed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"must not be surfaced"}]}]}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "responses-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "status") || !strings.Contains(string(body), "failed") || strings.Contains(string(body), "must not be surfaced") {
		t.Fatalf("gateway status=%d body=%s, want visible unsuccessful provider status", response.StatusCode, body)
	}
}

func TestGatewayRejectsComputerUseOnChatCompletionsRoute(t *testing.T) {
	ctx := context.Background()
	called := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL},
		store.Model{Alias: "chat", ProviderName: "openai", ProviderModel: "chat-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
			SupportsComputerUse: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessage(t, ctx, server.URL(), `{
		"model":"chat","max_tokens":16,
		"tools":[{"type":"computer_20250124","name":"computer"}],
		"messages":[{"role":"user","content":"take a screenshot"}]
	}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("gateway status = %d, want 501", response.StatusCode)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if !strings.Contains(string(raw), "Chat Completions fallback is disabled") {
		t.Fatalf("gateway response = %s", raw)
	}
	if called {
		t.Fatal("chat provider was called for computer use")
	}
}

func postGatewayMessage(t *testing.T, ctx context.Context, gatewayURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	return response
}
