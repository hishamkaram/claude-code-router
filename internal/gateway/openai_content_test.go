package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestAnthropicToolResultTextConvertsToolReferences(t *testing.T) {
	t.Parallel()

	got, err := anthropicToolResultText([]any{
		map[string]any{"type": "text", "text": "Loaded:"},
		map[string]any{"type": "tool_reference", "tool_name": "Agent"},
		map[string]any{"type": "text", "text": "Also loaded:"},
		map[string]any{"type": "tool_reference", "tool_name": "Workflow"},
	})
	if err != nil {
		t.Fatalf("anthropicToolResultText() error = %v", err)
	}
	want := "Loaded:\n[Loaded tool: Agent]\nAlso loaded:\n[Loaded tool: Workflow]"
	if got != want {
		t.Fatalf("anthropicToolResultText() = %q, want %q", got, want)
	}
}

func TestAnthropicToolResultTextRejectsInvalidToolReferences(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		block map[string]any
		want  string
	}{
		"missing name": {
			block: map[string]any{"type": "tool_reference"},
			want:  "tool_name must be a string",
		},
		"empty name": {
			block: map[string]any{"type": "tool_reference", "tool_name": " \t "},
			want:  "tool_name is required",
		},
		"non-string name": {
			block: map[string]any{"type": "tool_reference", "tool_name": 12},
			want:  "tool_name must be a string",
		},
		"control character": {
			block: map[string]any{"type": "tool_reference", "tool_name": "Tool\nSearch"},
			want:  "control characters",
		},
		"unknown field": {
			block: map[string]any{"type": "tool_reference", "tool_name": "Agent", "extra": true},
			want:  "field",
		},
		"unsupported type": {
			block: map[string]any{"type": "image", "source": "x"},
			want:  "not supported",
		},
	}
	for name, tt := range tests {
		tt := tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := anthropicToolResultText([]any{tt.block})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("anthropicToolResultText() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestToolReferenceOnlyAllowedInsideToolResult(t *testing.T) {
	t.Parallel()

	_, err := anthropicContentText([]any{
		map[string]any{"type": "tool_reference", "tool_name": "Agent"},
	})
	if err == nil || !strings.Contains(err.Error(), `content block field "tool_name" is not supported`) {
		t.Fatalf("anthropicContentText() error = %v", err)
	}
}

func TestGatewayRoutesToolSearchToolReferenceResultToOpenAIProvider(t *testing.T) {
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
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
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
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_toolsearch","name":"ToolSearch","input":{"query":"agent tools"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_toolsearch","content":[
					{"type":"text","text":"Loaded matching tools:"},
					{"type":"tool_reference","tool_name":"Agent"},
					{"type":"text","text":"Use it for child work."}
				]}
			]},
			{"role":"user","content":"start the child agent"}
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
	if len(gotMessages) < 2 {
		t.Fatalf("provider messages = %#v, want assistant and tool messages", gotMessages)
	}
	if gotMessages[1].Role != "tool" || gotMessages[1].ToolCallID != "toolu_toolsearch" {
		t.Fatalf("tool result message = %#v", gotMessages[1])
	}
	wantContent := "Loaded matching tools:\n[Loaded tool: Agent]\nUse it for child work."
	if gotMessages[1].Content != wantContent {
		t.Fatalf("tool result content = %q, want %q", gotMessages[1].Content, wantContent)
	}
}
