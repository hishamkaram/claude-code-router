package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

// modelCapabilitiesForRoute narrows advertised capabilities to what the gateway
// can actually deliver over a given provider protocol. The OpenAI-compatible
// adapter can translate Anthropic image blocks into image_url content parts, but
// only an explicit model signal enables that path because URL image sources may
// require gateway-side fetching. PDF and audio remain unsupported adapters.
func modelCapabilitiesForRoute(provider providers.Capabilities, model modelcap.Values) modelcap.Values {
	if provider.Protocol != providers.ProtocolOpenAICompatible {
		return model
	}
	if model.SupportsVision == nil && !slices.Contains(model.InputModalities, "image") {
		model.SupportsVision = modelcap.Bool(false)
	}
	model.SupportsPDFInput = modelcap.Bool(false)
	model.SupportsAudioInput = modelcap.Bool(false)
	return model
}

func validateModelMessageCapabilities(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	if isNonChatModelKind(capabilities.Kind) {
		return unsupportedModelCapability(model, "kind "+capabilities.Kind)
	}
	if validationErr := validateModelOutputLimit(model, capabilities, req); validationErr != nil {
		return validationErr
	}
	if validationErr := validateModelGenerationFeatures(model, capabilities, req); validationErr != nil {
		return validationErr
	}
	if validationErr := validateModelToolFeatures(model, capabilities, req); validationErr != nil {
		return validationErr
	}
	if validationErr := validateModelContentFeatures(model, capabilities, req); validationErr != nil {
		return validationErr
	}
	return validateModelInputModalities(model, capabilities, req)
}

func validateModelOutputLimit(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	if req.MaxTokens > 0 && capabilities.MaxOutputTokens != nil && int64(req.MaxTokens) > *capabilities.MaxOutputTokens {
		return &requestValidationError{
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("model alias %q max_tokens %d exceeds its configured maximum output of %d", model.Alias, req.MaxTokens, *capabilities.MaxOutputTokens),
		}
	}
	return nil
}

func validateModelGenerationFeatures(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	if req.Stream && explicitlyFalse(capabilities.SupportsStreaming) {
		return unsupportedModelCapability(model, "streaming")
	}
	if requestUsesThinkingFeature(req) && explicitlyFalse(capabilities.SupportsThinking) {
		return unsupportedModelCapability(model, "thinking")
	}
	return nil
}

func requestUsesThinkingFeature(req anthropicRequest) bool {
	thinkingType, err := openAIThinkingType(req.Thinking)
	if err == nil && thinkingType != "" && thinkingType != "disabled" {
		return true
	}
	effort, err := openAIReasoningEffortFromOutputConfig(req.OutputConfig)
	return err == nil && effort != ""
}

func validateModelToolFeatures(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	usesTools := anthropicRequestUsesTools(req)
	if usesTools && explicitlyFalse(capabilities.SupportsTools) {
		return unsupportedModelCapability(model, "tools")
	}
	if rawJSONPresent(req.ToolChoice) && explicitlyFalse(capabilities.SupportsToolChoice) {
		return unsupportedModelCapability(model, "tool choice")
	}
	if usesTools && requestExplicitlyAllowsParallelTools(req.ToolChoice) && explicitlyFalse(capabilities.SupportsParallelTools) {
		return unsupportedModelCapability(model, "parallel tool calls")
	}
	return nil
}

func validateModelContentFeatures(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	if requestUsesSystemMessages(req) && explicitlyFalse(capabilities.SupportsSystemMessages) {
		return unsupportedModelCapability(model, "system messages")
	}
	if requestUsesPromptCaching(req) && explicitlyFalse(capabilities.SupportsPromptCaching) {
		return unsupportedModelCapability(model, "prompt caching")
	}
	if outputConfigUsesResponseSchema(req.OutputConfig) && explicitlyFalse(capabilities.SupportsResponseSchema) {
		return unsupportedModelCapability(model, "response schemas")
	}
	return nil
}

func requestUsesSystemMessages(req anthropicRequest) bool {
	if hasSystemContent(req.System) {
		return true
	}
	for _, message := range req.Messages {
		if message.Role == "system" && hasSystemContent(message.Content) {
			return true
		}
	}
	return false
}

func validateModelInputModalities(model store.Model, capabilities modelcap.Values, req anthropicRequest) *requestValidationError {
	blockTypes := requestContentBlockTypes(req)
	// A screenshot returned for a preceding native computer call is translated
	// to computer_call_output by the Responses adapter. It is not a model image
	// input, so it must not require a separate vision capability.
	if requestUsesImageInput(req) && modalityUnsupported(capabilities.InputModalities, "image", capabilities.SupportsVision) {
		return unsupportedModelCapability(model, "image input")
	}
	if blockTypes["document"] && modalityUnsupported(capabilities.InputModalities, "pdf", capabilities.SupportsPDFInput) {
		return unsupportedModelCapability(model, "PDF input")
	}
	if (blockTypes["audio"] || blockTypes["input_audio"]) && modalityUnsupported(capabilities.InputModalities, "audio", capabilities.SupportsAudioInput) {
		return unsupportedModelCapability(model, "audio input")
	}
	return nil
}

func requestUsesImageInput(req anthropicRequest) bool {
	computerCallIDs := nativeComputerCallIDs(req)
	if contentUsesImageInput(req.System, computerCallIDs) {
		return true
	}
	for _, message := range req.Messages {
		if contentUsesImageInput(message.Content, computerCallIDs) {
			return true
		}
	}
	return false
}

func nativeComputerCallIDs(req anthropicRequest) map[string]struct{} {
	if !cua.UsesComputerTool(req.Tools) || requestHasFunctionNamedComputer(req.Tools) {
		return nil
	}
	callIDs := make(map[string]struct{})
	for _, message := range req.Messages {
		if message.Role != "assistant" {
			continue
		}
		collectNativeComputerCallIDs(message.Content, callIDs)
	}
	return callIDs
}

func requestHasFunctionNamedComputer(tools []json.RawMessage) bool {
	for _, raw := range tools {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		if !cua.IsNativeComputerTool(tool.Type, tool.Name, tool.InputSchema) && strings.EqualFold(strings.TrimSpace(tool.Name), "computer") {
			return true
		}
	}
	return false
}

func collectNativeComputerCallIDs(content any, callIDs map[string]struct{}) {
	blocks, ok := content.([]any)
	if !ok {
		return
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || !isNativeComputerCall(block) {
			continue
		}
		callIDs[strings.TrimSpace(stringValue(block["id"]))] = struct{}{}
	}
}

func isNativeComputerCall(block map[string]any) bool {
	if !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") ||
		!strings.EqualFold(strings.TrimSpace(stringValue(block["name"])), "computer") ||
		strings.TrimSpace(stringValue(block["id"])) == "" {
		return false
	}
	return true
}

func contentUsesImageInput(content any, computerCallIDs map[string]struct{}) bool {
	switch value := content.(type) {
	case []any:
		for _, item := range value {
			block, ok := item.(map[string]any)
			if ok && contentBlockUsesImageInput(block, computerCallIDs) {
				return true
			}
		}
	case map[string]any:
		return contentBlockUsesImageInput(value, computerCallIDs)
	}
	return false
}

func contentBlockUsesImageInput(block map[string]any, computerCallIDs map[string]struct{}) bool {
	switch strings.ToLower(strings.TrimSpace(stringValue(block["type"]))) {
	case "image":
		return true
	case "tool_result":
		if _, isNativeComputerResult := computerCallIDs[strings.TrimSpace(stringValue(block["tool_use_id"]))]; isNativeComputerResult {
			return false
		}
		return contentUsesImageInput(block["content"], computerCallIDs)
	default:
		return false
	}
}

func unsupportedModelCapability(model store.Model, capability string) *requestValidationError {
	return &requestValidationError{
		status:  http.StatusNotImplemented,
		message: fmt.Sprintf("model alias %q does not support %s", model.Alias, capability),
	}
}

func explicitlyFalse(value *bool) bool {
	return value != nil && !*value
}

func isNonChatModelKind(kind string) bool {
	return !modelcap.IsRoutableKind(kind)
}

func modalityUnsupported(modalities []string, modality string, support *bool) bool {
	if explicitlyFalse(support) {
		return true
	}
	return len(modalities) > 0 && !slices.Contains(modalities, modality)
}

func requestExplicitlyAllowsParallelTools(raw json.RawMessage) bool {
	if !rawJSONPresent(raw) {
		return false
	}
	var payload struct {
		DisableParallel *bool `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.DisableParallel == nil {
		return false
	}
	return !*payload.DisableParallel
}

func hasSystemContent(system any) bool {
	switch value := system.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

func outputConfigUsesResponseSchema(raw json.RawMessage) bool {
	if !rawJSONPresent(raw) {
		return false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	format, ok := payload["format"]
	return ok && rawJSONPresent(format)
}

func requestUsesPromptCaching(req anthropicRequest) bool {
	if contentUsesPromptCaching(req.System) {
		return true
	}
	for _, message := range req.Messages {
		if contentUsesPromptCaching(message.Content) {
			return true
		}
	}
	for _, tool := range req.Tools {
		var value map[string]json.RawMessage
		if json.Unmarshal(tool, &value) == nil && rawJSONPresent(value["cache_control"]) {
			return true
		}
	}
	return false
}

func contentUsesPromptCaching(value any) bool {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			block, ok := item.(map[string]any)
			if ok && contentBlockUsesPromptCaching(block) {
				return true
			}
		}
	case map[string]any:
		return contentBlockUsesPromptCaching(value)
	}
	return false
}

func contentBlockUsesPromptCaching(block map[string]any) bool {
	if cacheControl, exists := block["cache_control"]; exists && cacheControl != nil {
		return true
	}
	blockType, _ := block["type"].(string)
	return strings.EqualFold(strings.TrimSpace(blockType), "tool_result") && contentUsesPromptCaching(block["content"])
}

func requestContentBlockTypes(req anthropicRequest) map[string]bool {
	types := make(map[string]bool)
	collectContentBlockTypes(req.System, types)
	for _, message := range req.Messages {
		collectContentBlockTypes(message.Content, types)
	}
	return types
}

func collectContentBlockTypes(value any, types map[string]bool) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			collectContentBlockType(block, types)
		}
	case map[string]any:
		collectContentBlockType(value, types)
	}
}

func collectContentBlockType(block map[string]any, types map[string]bool) {
	blockType, ok := block["type"].(string)
	if !ok {
		return
	}
	blockType = strings.ToLower(strings.TrimSpace(blockType))
	types[blockType] = true
	if blockType == "tool_result" {
		collectContentBlockTypes(block["content"], types)
	}
}
