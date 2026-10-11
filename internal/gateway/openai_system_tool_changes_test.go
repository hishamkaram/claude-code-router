package gateway

import (
	"encoding/json"
	"testing"
)

func TestOpenAIChatRequestAppliesTopLevelToolChangesInChronologicalOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tools    []json.RawMessage
		system   any
		messages []anthropicMessage
		want     []string
	}{
		{
			name: "top-level addition activates deferred tool",
			tools: []json.RawMessage{
				json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
				json.RawMessage(`{"name":"read","defer_loading":true,"input_schema":{"type":"object"}}`),
			},
			system: []any{map[string]any{
				"type": "tool_addition",
				"tool": map[string]any{"type": "tool_reference", "name": "read"},
			}},
			want: []string{"bash", "read"},
		},
		{
			name: "top-level removal filters active tool",
			tools: []json.RawMessage{
				json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
				json.RawMessage(`{"name":"read","input_schema":{"type":"object"}}`),
			},
			system: []any{map[string]any{
				"type": "tool_removal",
				"tool": map[string]any{"type": "tool_reference", "name": "read"},
			}},
			want: []string{"bash"},
		},
		{
			name: "message tool result follows top-level removal",
			tools: []json.RawMessage{
				json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
				json.RawMessage(`{"name":"read","input_schema":{"type":"object"}}`),
			},
			system: []any{map[string]any{
				"type": "tool_removal",
				"tool": map[string]any{"type": "tool_reference", "name": "read"},
			}},
			messages: []anthropicMessage{{
				Role: "user",
				Content: []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_search",
					"content": []any{map[string]any{
						"type":      "tool_reference",
						"tool_name": "read",
					}},
				}},
			}},
			want: []string{"bash", "read"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertOpenAITopLevelToolChanges(t, tt.tools, tt.system, tt.messages, tt.want)
		})
	}
}

func assertOpenAITopLevelToolChanges(t *testing.T, tools []json.RawMessage, system any, messages []anthropicMessage, want []string) {
	t.Helper()
	req := anthropicRequest{
		Model: "gpt", MaxTokens: 32, System: system, Messages: messages, Tools: tools,
	}
	converted, _, err := toOpenAIChatRequest(req, openAIModelRoute{providerModel: "gpt"})
	if err != nil {
		t.Fatalf("toOpenAIChatRequest() error = %v", err)
	}
	if len(converted.Tools) != len(want) {
		t.Fatalf("provider tools = %#v, want %v", converted.Tools, want)
	}
	for i, name := range want {
		if got := converted.Tools[i].Function.Name; got != name {
			t.Fatalf("provider tool %d name = %q, want %q", i, got, name)
		}
	}
}
