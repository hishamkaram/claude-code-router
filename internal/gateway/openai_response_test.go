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

func TestGatewayStreamsOpenAICompatibleToolUseInputJSONDelta(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token"})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"gpt","max_tokens":20,"stream":true,"tools":[{"name":"bash","input_schema":{"type":"object","required":["cmd"],"properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":"run pwd"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
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
	for _, want := range []string{
		`"type":"tool_use"`,
		`"input":{}`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"cmd\":\"pwd\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %q:\n%s", want, stream)
		}
	}
}

func TestGatewayStopsInvalidAgentToolResponseFromOpenAICompatibleProvider(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"Agent","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"gpt","tools":[{"name":"Agent","description":"spawn an agent","input_schema":{"type":"object","required":["description","prompt"],"properties":{"description":{"type":"string"},"prompt":{"type":"string"}}}}],"messages":[{"role":"user","content":"spawn agent"}]}`))
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
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("gateway decode error = %v", err)
	}
	if decoded.StopReason != "end_turn" || len(decoded.Content) != 1 || decoded.Content[0].Type != "text" || decoded.Content[0].Name != "" {
		t.Fatalf("gateway invalid Agent response = %#v", decoded)
	}
	if !strings.Contains(decoded.Content[0].Text, "invalid Agent tool input") ||
		!strings.Contains(decoded.Content[0].Text, "missing required prompt and description") {
		t.Fatalf("gateway invalid Agent text = %q", decoded.Content[0].Text)
	}
}

func TestAnthropicContentBlocksRejectsInvalidAgentToolInput(t *testing.T) {
	var resp openAIChatResponse
	raw := `{"id":"chatcmpl-test","choices":[{"message":{"tool_calls":[{"id":"call-empty-agent","type":"function","function":{"name":"Agent","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	blocks, stopReason := anthropicContentBlocksFromOpenAI(resp, "tool_use")
	if stopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", stopReason)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "text" {
		t.Fatalf("blocks = %#v, want one text block", blocks)
	}
	text, _ := blocks[0]["text"].(string)
	if !strings.Contains(text, "invalid Agent tool input") ||
		!strings.Contains(text, "missing required prompt and description") ||
		!strings.Contains(text, "subagent was not started") {
		t.Fatalf("text = %q, want visible Agent compatibility error", text)
	}
}

func TestAnthropicContentBlocksKeepsValidAgentToolInput(t *testing.T) {
	var resp openAIChatResponse
	raw := `{"id":"chatcmpl-test","choices":[{"message":{"tool_calls":[{"id":"call-valid-agent","type":"function","function":{"name":"Agent","arguments":"{\"prompt\":\"find latest chatgpt news\",\"subagent_type\":\"general-purpose\"}"}}]},"finish_reason":"tool_calls"}]}`
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	blocks, stopReason := anthropicContentBlocksFromOpenAI(resp, "tool_use")
	if stopReason != "tool_use" {
		t.Fatalf("stopReason = %q, want tool_use", stopReason)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "tool_use" || blocks[0]["name"] != "Agent" {
		t.Fatalf("blocks = %#v, want Agent tool_use", blocks)
	}
	input, ok := blocks[0]["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v, want object", blocks[0]["input"])
	}
	if input["prompt"] != "find latest chatgpt news" || input["description"] != "find latest chatgpt news" || input["subagent_type"] != "general-purpose" {
		t.Fatalf("input = %#v", input)
	}
}

func TestAnthropicContentBlocksSkipsNullOpenAIContentForToolCalls(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"id":"chatcmpl-null","choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"status\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-omitted","choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"status\"}"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		var response openAIChatResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}

		blocks, stopReason := anthropicContentBlocksFromOpenAI(response, "tool_use")
		if stopReason != "tool_use" {
			t.Fatalf("stopReason = %q, want tool_use", stopReason)
		}
		if len(blocks) != 1 || blocks[0]["type"] != "tool_use" {
			t.Fatalf("blocks = %#v, want only a tool_use block", blocks)
		}
		if _, found := blocks[0]["text"]; found {
			t.Fatalf("tool-use block unexpectedly included text: %#v", blocks[0])
		}
	}
}
