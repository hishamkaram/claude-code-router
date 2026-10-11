package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIToolChangesActivateInlineDefinitionAbsentFromTopLevel(t *testing.T) {
	t.Parallel()

	tools, err := openAIToolsFromAnthropic([]json.RawMessage{
		json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
	})
	if err != nil {
		t.Fatalf("openAIToolsFromAnthropic() error = %v", err)
	}
	active, err := openAIToolsAfterAnthropicChanges(tools, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type": "tool_definition",
				"definition": map[string]any{
					"name":        "read",
					"description": "Read a file",
					"input_schema": map[string]any{
						"type": "object",
					},
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("openAIToolsAfterAnthropicChanges() inline addition error = %v", err)
	}
	if len(active) != 2 || active[1].Function.Name != "read" || active[1].Function.Description != "Read a file" {
		t.Fatalf("active tools after inline addition = %#v, want bash and read", active)
	}
}

func TestOpenAIToolChangesReplaceDeferredToolWithInlineDefinition(t *testing.T) {
	t.Parallel()

	tools, err := openAIToolsFromAnthropic([]json.RawMessage{
		json.RawMessage(`{"name":"bash","input_schema":{"type":"object"}}`),
		json.RawMessage(`{"name":"read","description":"old","defer_loading":true,"input_schema":{"type":"object"}}`),
	})
	if err != nil {
		t.Fatalf("openAIToolsFromAnthropic() error = %v", err)
	}
	active, err := openAIToolsAfterAnthropicChanges(tools, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type": "tool_definition",
				"definition": map[string]any{
					"name":          "read",
					"description":   "new",
					"defer_loading": true,
					"input_schema": map[string]any{
						"type": "object",
					},
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("openAIToolsAfterAnthropicChanges() inline replacement error = %v", err)
	}
	if len(active) != 2 || active[1].Function.Name != "read" || active[1].Function.Description != "new" {
		t.Fatalf("active tools after inline replacement = %#v, want one active read at its original position", active)
	}
	readCount := 0
	for _, tool := range active {
		if tool.Function.Name == "read" {
			readCount++
		}
	}
	if readCount != 1 {
		t.Fatalf("active tools after inline replacement = %#v, want exactly one read", active)
	}
}

func TestOpenAIToolChangesRejectMissingInlineDefinition(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type": "tool_definition",
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "requires a definition") {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want missing definition rejection", err)
	}
}

func TestOpenAIToolChangesRejectNameOnToolDefinition(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type":       "tool_definition",
				"name":       "read",
				"definition": map[string]any{"name": "read"},
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), `field "name"`) {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want illegal name rejection", err)
	}
}

func TestOpenAIToolChangesRejectDefinitionOnToolReference(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type":       "tool_reference",
				"name":       "read",
				"definition": map[string]any{"name": "read"},
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), `field "definition"`) {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want illegal definition rejection", err)
	}
}

func TestOpenAIToolChangesRejectDefinitionOnRemoval(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_removal",
			"tool": map[string]any{
				"type":       "tool_definition",
				"definition": map[string]any{"name": "read"},
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "only supported for tool_addition") {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want removal definition rejection", err)
	}
}

func TestOpenAIToolChangesRejectMalformedInlineDefinition(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type":       "tool_definition",
				"definition": "not an object",
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "definition must be an object") {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want malformed definition rejection", err)
	}
}

func TestOpenAIToolChangesInlineAdditionThenRemoval(t *testing.T) {
	t.Parallel()

	active, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{
		{Role: "system", Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type": "tool_definition",
				"definition": map[string]any{
					"name":         "read",
					"input_schema": map[string]any{"type": "object"},
				},
			},
		}}},
		{Role: "system", Content: []any{map[string]any{
			"type": "tool_removal",
			"tool": map[string]any{"type": "tool_reference", "name": "read"},
		}}},
	})
	if err != nil {
		t.Fatalf("openAIToolsAfterAnthropicChanges() add/remove error = %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active tools after inline add/remove = %#v, want none", active)
	}
}

func TestOpenAIToolChangesRejectUnknownNestedReferenceField(t *testing.T) {
	t.Parallel()

	_, err := openAIToolsAfterAnthropicChanges(nil, nil, []anthropicMessage{{
		Role: "system",
		Content: []any{map[string]any{
			"type": "tool_addition",
			"tool": map[string]any{
				"type":  "tool_reference",
				"name":  "read",
				"extra": true,
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), `field "extra"`) {
		t.Fatalf("openAIToolsAfterAnthropicChanges() error = %v, want unknown nested field rejection", err)
	}
}
