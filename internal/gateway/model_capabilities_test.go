package gateway

import (
	"encoding/json"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestModelInputModalitiesIgnoreTypesInsideToolInput(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{Messages: []anthropicMessage{{
		Role: "assistant",
		Content: []any{map[string]any{
			"type": "tool_use",
			"input": map[string]any{
				"type":   "image",
				"format": map[string]any{"type": "document"},
			},
		}},
	}}}
	capabilities := modelcap.Values{
		InputModalities:  []string{"text"},
		SupportsVision:   modelcap.Bool(false),
		SupportsPDFInput: modelcap.Bool(false),
	}
	if validationErr := validateModelInputModalities(store.Model{Alias: "text-only"}, capabilities, req); validationErr != nil {
		t.Fatalf("validateModelInputModalities() error = %v", validationErr)
	}
}

func TestModelCapabilitiesForRouteMasksOnlyOpenAIMultimodalInput(t *testing.T) {
	t.Parallel()
	values := modelcap.Values{
		SupportsVision: modelcap.Bool(true), SupportsPDFInput: modelcap.Bool(true),
		SupportsAudioInput: modelcap.Bool(true), SupportsTools: modelcap.Bool(true),
	}
	openAI := modelCapabilitiesForRoute(providers.Capabilities{Protocol: providers.ProtocolOpenAICompatible}, values)
	if !explicitlyFalse(openAI.SupportsVision) || !explicitlyFalse(openAI.SupportsPDFInput) ||
		!explicitlyFalse(openAI.SupportsAudioInput) || explicitlyFalse(openAI.SupportsTools) {
		t.Fatalf("OpenAI route capabilities = %#v", openAI)
	}
	anthropic := modelCapabilitiesForRoute(providers.Capabilities{Protocol: providers.ProtocolAnthropicCompatible}, values)
	if anthropic.SupportsVision == nil || !*anthropic.SupportsVision ||
		anthropic.SupportsPDFInput == nil || !*anthropic.SupportsPDFInput ||
		anthropic.SupportsAudioInput == nil || !*anthropic.SupportsAudioInput {
		t.Fatalf("Anthropic route capabilities = %#v", anthropic)
	}
}

func TestRequestContentBlockTypesIncludeToolResultContent(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{Messages: []anthropicMessage{{
		Role: "user",
		Content: []any{map[string]any{
			"type": "tool_result",
			"content": []any{map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "base64"},
			}},
		}},
	}}}
	types := requestContentBlockTypes(req)
	if !types["tool_result"] || !types["image"] || types["base64"] {
		t.Fatalf("requestContentBlockTypes() = %#v", types)
	}
}

func TestPromptCacheDetectionIgnoresToolInputSchemaProperties(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{Tools: []json.RawMessage{json.RawMessage(`{
		"name":"search",
		"input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}
	}`)}}
	if requestUsesPromptCaching(req) {
		t.Fatal("tool input schema property was treated as Anthropic cache metadata")
	}

	req.Tools[0] = json.RawMessage(`{
		"name":"search",
		"input_schema":{"type":"object"},
		"cache_control":{"type":"ephemeral"}
	}`)
	if !requestUsesPromptCaching(req) {
		t.Fatal("top-level tool cache metadata was not detected")
	}
}

func TestPromptCacheDetectionIncludesNestedToolResultBlocks(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{Messages: []anthropicMessage{{
		Role: "user",
		Content: []any{map[string]any{
			"type": "tool_result",
			"content": []any{map[string]any{
				"type":          "text",
				"text":          "cached result",
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
		}},
	}}}
	if !requestUsesPromptCaching(req) {
		t.Fatal("tool result content cache metadata was not detected")
	}
}

func TestProviderThinkingCapabilityAllowsDisabledModeAndRejectsEffort(t *testing.T) {
	t.Parallel()
	route := messageRoute{
		model: store.Model{Alias: "gpt"},
		capabilities: providers.Capabilities{
			SupportsStreaming: true,
			SupportsThinking:  false,
			SupportsTools:     true,
		},
	}
	disabled := anthropicRequest{Thinking: json.RawMessage(`{"type":"disabled"}`)}
	if validationErr := validateRouteMessageCapabilities(route, disabled); validationErr != nil {
		t.Fatalf("disabled thinking rejected: %v", validationErr)
	}

	effort := anthropicRequest{OutputConfig: json.RawMessage(`{"effort":"high"}`)}
	if validationErr := validateRouteMessageCapabilities(route, effort); validationErr == nil {
		t.Fatal("reasoning effort accepted for a provider without thinking support")
	}
}

func TestOpenAIRequestTranslatesAnthropicResponseSchema(t *testing.T) {
	t.Parallel()
	req := anthropicRequest{
		Messages: []anthropicMessage{{Role: "user", Content: "return JSON"}},
		OutputConfig: json.RawMessage(`{
			"effort":"high",
			"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}
		}`),
	}
	translated, err := toOpenAIChatRequest(req, openAIModelRoute{providerModel: "gpt-5"})
	if err != nil {
		t.Fatalf("toOpenAIChatRequest() error = %v", err)
	}
	if translated.ReasoningEffort != "high" || translated.ResponseFormat == nil {
		t.Fatalf("translated request = %#v", translated)
	}
	responseFormat := translated.ResponseFormat
	if responseFormat.Type != "json_schema" || responseFormat.JSONSchema.Name != "claude_output" || !responseFormat.JSONSchema.Strict {
		t.Fatalf("response format = %#v", responseFormat)
	}
	var schema map[string]any
	if decodeErr := json.Unmarshal(responseFormat.JSONSchema.Schema, &schema); decodeErr != nil {
		t.Fatalf("response schema decode error = %v", decodeErr)
	}
	if schema["type"] != "object" {
		t.Fatalf("response schema = %#v", schema)
	}
	encoded, err := json.Marshal(translated)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("translated wire decode error = %v", err)
	}
	if !rawJSONPresent(wire["response_format"]) {
		t.Fatalf("translated wire has no response_format: %s", encoded)
	}
}

func TestOpenAIResponseFormatRejectsUntranslatableSchemas(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"unsupported type": `{"format":{"type":"grammar","schema":{}}}`,
		"missing schema":   `{"format":{"type":"json_schema"}}`,
		"scalar schema":    `{"format":{"type":"json_schema","schema":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := openAIResponseFormatFromOutputConfig(json.RawMessage(raw)); err == nil {
				t.Fatal("openAIResponseFormatFromOutputConfig() succeeded")
			}
		})
	}
}
