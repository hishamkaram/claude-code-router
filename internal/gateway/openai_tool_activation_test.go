package gateway

import (
	"context"
	"encoding/json"
	"testing"
)

func TestOpenAIChatRequestActivatesDeferredToolFromToolSearchResult(t *testing.T) {
	t.Parallel()

	req := anthropicRequest{
		Model:     "gpt",
		MaxTokens: 32,
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
			json.RawMessage(`{"name":"gsheets","defer_loading":true,"input_schema":{"type":"object"}}`),
		},
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "id": "toolu_toolsearch", "name": "ToolSearch",
				"input": map[string]any{"query": "sheets"},
			}}},
			{Role: "user", Content: []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_toolsearch",
				"content": []any{map[string]any{"type": "tool_reference", "tool_name": "gsheets"}},
			}}},
		},
	}

	converted, _, err := toOpenAIChatRequestWithResolver(
		context.Background(), req, openAIModelRoute{providerModel: "gpt"}, testImageSourceResolver,
	)
	if err != nil {
		t.Fatalf("toOpenAIChatRequestWithResolver() error = %v", err)
	}
	if len(converted.Tools) != 2 || converted.Tools[1].Function.Name != "gsheets" {
		t.Fatalf("provider tools = %#v, want bash and gsheets", converted.Tools)
	}
}
