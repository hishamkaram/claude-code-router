package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

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
			name: "vision", overrides: modelcap.Values{SupportsVision: modelcap.Bool(false)},
			body:       `{"model":"gpt","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`,
			wantStatus: http.StatusNotImplemented,
		},
		{
			name:       "OpenAI route with unknown vision support",
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

func TestGatewayAppliesSystemToolRemovalForOpenAIProvider(t *testing.T) {
	ctx := context.Background()
	var gotToolNames []string
	var gotMessages []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		for _, tool := range payload.Tools {
			gotToolNames = append(gotToolNames, tool.Function.Name)
		}
		gotMessages = payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-tool-change","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{
		Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded",
	})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	body := `{
		"model":"gpt",
		"tools":[
			{"name":"bash","input_schema":{"type":"object"}},
			{"name":"read","input_schema":{"type":"object"}}
		],
		"messages":[
			{"role":"user","content":"start"},
			{"role":"system","content":[
				{"type":"tool_removal","tool":{"type":"tool_reference","name":"read"}}
			]},
			{"role":"user","content":"continue"}
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
	if len(gotToolNames) != 1 || gotToolNames[0] != "bash" {
		t.Fatalf("provider tools = %#v, want only bash", gotToolNames)
	}
	for _, message := range gotMessages {
		if message["role"] == "system" {
			t.Fatalf("provider received a translated tool-change system message: %#v", message)
		}
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

func TestGatewayDropsSafeguardsOnTranslatedOpenAIPath(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		if _, present := payload["safeguards"]; present {
			t.Fatalf("provider received Anthropic safeguards field")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-safeguards","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	body := `{"model":"gpt","safeguards":{"mode":"auto"},"messages":[{"role":"user","content":"hello"}]}`
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
	if got := resp.Header.Get(ccrIgnoredFieldsHeader); got != "safeguards" {
		t.Fatalf("%s = %q, want safeguards", ccrIgnoredFieldsHeader, got)
	}
}

func TestResponsesPathAcceptsSafeguardsAsIgnored(t *testing.T) {
	fields := map[string]json.RawMessage{"safeguards": json.RawMessage(`{"mode":"auto"}`)}
	req := anthropicRequest{Fields: fields}
	if validationErr := (&handler{}).validateOpenAIMessageRequest(&req); validationErr != nil {
		t.Fatalf("validateOpenAIMessageRequest() = %#v, want accepted field", validationErr)
	}
	if validationErr := validateResponsesMessageRequest(&req); validationErr != nil {
		t.Fatalf("validateResponsesMessageRequest() = %#v, want accepted field", validationErr)
	}
	if got := ignoredOpenAIRequestFields(req); len(got) != 1 || got[0] != "safeguards" {
		t.Fatalf("ignoredOpenAIRequestFields() = %#v, want safeguards", got)
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
