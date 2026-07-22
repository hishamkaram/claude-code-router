package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

const maxDiscoveryResponseBytes = 4 << 20

type optionalInt64 struct {
	Value *int64
}

func (value *optionalInt64) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" || text == "" {
		value.Value = nil
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		unquoted, err := strconv.Unquote(text)
		if err != nil {
			return err
		}
		text = unquoted
	}
	value.Value = parsePositiveInt64(text)
	return nil
}

func parsePositiveInt64(text string) *int64 {
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed <= 0 {
		return nil
	}
	return &parsed
}

type openAIModelItem struct {
	ID                      string        `json:"id"`
	Name                    string        `json:"name"`
	DisplayName             string        `json:"display_name"`
	Mode                    string        `json:"mode"`
	Type                    string        `json:"type"`
	ContextLength           optionalInt64 `json:"context_length"`
	MaxModelLength          optionalInt64 `json:"max_model_len"`
	MaxInputTokens          optionalInt64 `json:"max_input_tokens"`
	MaxTokens               optionalInt64 `json:"max_tokens"`
	MaxOutputTokens         optionalInt64 `json:"max_output_tokens"`
	InputModalities         []string      `json:"input_modalities"`
	OutputModalities        []string      `json:"output_modalities"`
	SupportedParameters     []string      `json:"supported_parameters"`
	SupportedOpenAIParams   []string      `json:"supported_openai_params"`
	SupportsTools           *bool         `json:"supports_tools"`
	SupportsToolChoice      *bool         `json:"supports_tool_choice"`
	SupportsParallelTools   *bool         `json:"supports_parallel_tools"`
	SupportsStreaming       *bool         `json:"supports_streaming"`
	SupportsThinking        *bool         `json:"supports_thinking"`
	SupportsPromptCaching   *bool         `json:"supports_prompt_caching"`
	SupportsSystemMessages  *bool         `json:"supports_system_messages"`
	SupportsVision          *bool         `json:"supports_vision"`
	SupportsPDFInput        *bool         `json:"supports_pdf_input"`
	SupportsAudioInput      *bool         `json:"supports_audio_input"`
	SupportsAudioOutput     *bool         `json:"supports_audio_output"`
	SupportsNativeStreaming *bool         `json:"supports_native_streaming"`
	SupportsResponseSchema  *bool         `json:"supports_response_schema"`
	SupportsResponses       *bool         `json:"supports_responses"`
	SupportsComputerUse     *bool         `json:"supports_computer_use"`
	Architecture            struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens optionalInt64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Capabilities anthropicModelCapabilities `json:"capabilities"`
}

type capabilitySupport struct {
	Supported *bool `json:"supported"`
}

type anthropicModelCapabilities struct {
	ImageInput        capabilitySupport `json:"image_input"`
	PDFInput          capabilitySupport `json:"pdf_input"`
	StructuredOutputs capabilitySupport `json:"structured_outputs"`
	Thinking          struct {
		Supported *bool `json:"supported"`
		Types     struct {
			Adaptive capabilitySupport `json:"adaptive"`
			Enabled  capabilitySupport `json:"enabled"`
		} `json:"types"`
	} `json:"thinking"`
}

type liteLLMModelInfoItem struct {
	ModelName string `json:"model_name"`
	ModelInfo struct {
		ID                              string        `json:"id"`
		Mode                            string        `json:"mode"`
		MaxTokens                       optionalInt64 `json:"max_tokens"`
		MaxInputTokens                  optionalInt64 `json:"max_input_tokens"`
		MaxOutputTokens                 optionalInt64 `json:"max_output_tokens"`
		InputModalities                 []string      `json:"input_modalities"`
		OutputModalities                []string      `json:"output_modalities"`
		SupportedModalities             []string      `json:"supported_modalities"`
		SupportedOutputModalities       []string      `json:"supported_output_modalities"`
		SupportedOpenAIParams           []string      `json:"supported_openai_params"`
		SupportsFunctionCalling         *bool         `json:"supports_function_calling"`
		SupportsToolChoice              *bool         `json:"supports_tool_choice"`
		SupportsParallelFunctionCalling *bool         `json:"supports_parallel_function_calling"`
		SupportsVision                  *bool         `json:"supports_vision"`
		SupportsPDFInput                *bool         `json:"supports_pdf_input"`
		SupportsAudioInput              *bool         `json:"supports_audio_input"`
		SupportsAudioOutput             *bool         `json:"supports_audio_output"`
		SupportsNativeStreaming         *bool         `json:"supports_native_streaming"`
		SupportsPromptCaching           *bool         `json:"supports_prompt_caching"`
		SupportsReasoning               *bool         `json:"supports_reasoning"`
		SupportsSystemMessages          *bool         `json:"supports_system_messages"`
		SupportsResponseSchema          *bool         `json:"supports_response_schema"`
		SupportsNativeStructuredOutput  *bool         `json:"supports_native_structured_output"`
		SupportsResponses               *bool         `json:"supports_responses"`
		SupportsComputerUse             *bool         `json:"supports_computer_use"`
	} `json:"model_info"`
}

func parseOpenAIModels(body io.Reader) ([]DiscoveredModel, error) {
	var payload struct {
		Data []openAIModelItem `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(body, maxDiscoveryResponseBytes))
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for index := range payload.Data {
		item := &payload.Data[index]
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		values := capabilitiesFromOpenAIItem(*item)
		snapshot, err := modelcap.SnapshotFrom(values, modelcap.SourceOpenAIModels)
		if err != nil {
			return nil, fmt.Errorf("model %q capabilities: %w", id, err)
		}
		applyOpenAIAdapterCapabilities(&snapshot)
		displayName := strings.TrimSpace(firstNonEmpty(item.DisplayName, item.Name))
		if displayName == "" {
			displayName = id
		}
		models = append(models, DiscoveredModel{
			ID: id, DisplayName: displayName, Routable: true, Capabilities: snapshot,
		})
	}
	return models, nil
}

func parseLiteLLMModelInfo(body io.Reader) ([]DiscoveredModel, error) {
	var payload struct {
		Data []liteLLMModelInfoItem `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(body, maxDiscoveryResponseBytes))
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(payload.Data))
	for index := range payload.Data {
		item := &payload.Data[index]
		id := strings.TrimSpace(item.ModelName)
		if id == "" {
			id = strings.TrimSpace(item.ModelInfo.ID)
		}
		if id == "" {
			continue
		}
		contextWindow := maxOptionalInt64(item.ModelInfo.MaxTokens.Value, item.ModelInfo.MaxInputTokens.Value)
		inputModalities := firstNonEmptySlice(item.ModelInfo.InputModalities, item.ModelInfo.SupportedModalities)
		outputModalities := firstNonEmptySlice(item.ModelInfo.OutputModalities, item.ModelInfo.SupportedOutputModalities)
		values := modelcap.Values{
			Kind:                   kindFromProviderMode(item.ModelInfo.Mode),
			ContextWindowTokens:    contextWindow,
			MaxInputTokens:         positiveOptionalInt64(item.ModelInfo.MaxInputTokens.Value),
			MaxOutputTokens:        positiveOptionalInt64(item.ModelInfo.MaxOutputTokens.Value),
			InputModalities:        inputModalities,
			OutputModalities:       outputModalities,
			SupportsTools:          item.ModelInfo.SupportsFunctionCalling,
			SupportsToolChoice:     item.ModelInfo.SupportsToolChoice,
			SupportsParallelTools:  item.ModelInfo.SupportsParallelFunctionCalling,
			SupportsThinking:       item.ModelInfo.SupportsReasoning,
			SupportsPromptCaching:  item.ModelInfo.SupportsPromptCaching,
			SupportsSystemMessages: item.ModelInfo.SupportsSystemMessages,
			SupportsVision:         item.ModelInfo.SupportsVision,
			SupportsPDFInput:       item.ModelInfo.SupportsPDFInput,
			SupportsAudioInput:     item.ModelInfo.SupportsAudioInput,
			SupportsAudioOutput:    item.ModelInfo.SupportsAudioOutput,
			SupportsResponseSchema: firstBool(item.ModelInfo.SupportsResponseSchema, item.ModelInfo.SupportsNativeStructuredOutput),
			SupportsResponses:      item.ModelInfo.SupportsResponses,
			SupportsComputerUse:    item.ModelInfo.SupportsComputerUse,
		}
		normalizeProviderModalities(&values)
		applySupportedParameters(&values, item.ModelInfo.SupportedOpenAIParams)
		applyModalityCapabilities(&values)
		snapshot, err := modelcap.SnapshotFrom(values, modelcap.SourceLiteLLMInfo)
		if err != nil {
			return nil, fmt.Errorf("model %q capabilities: %w", id, err)
		}
		applyOpenAIAdapterCapabilities(&snapshot)
		models = append(models, DiscoveredModel{ID: id, DisplayName: id, Routable: true, Capabilities: snapshot})
	}
	return models, nil
}

func capabilitiesFromOpenAIItem(item openAIModelItem) modelcap.Values {
	contextWindow := maxOptionalInt64(item.ContextLength.Value, item.MaxModelLength.Value, item.MaxInputTokens.Value)
	maxOutput := firstPositiveOptionalInt64(item.MaxOutputTokens.Value, item.MaxTokens.Value, item.TopProvider.MaxCompletionTokens.Value)
	inputModalities := item.InputModalities
	if len(inputModalities) == 0 {
		inputModalities = item.Architecture.InputModalities
	}
	outputModalities := item.OutputModalities
	if len(outputModalities) == 0 {
		outputModalities = item.Architecture.OutputModalities
	}
	values := modelcap.Values{
		Kind:                   kindFromProviderMode(firstNonEmpty(item.Mode, item.Type)),
		ContextWindowTokens:    contextWindow,
		MaxInputTokens:         positiveOptionalInt64(item.MaxInputTokens.Value),
		MaxOutputTokens:        maxOutput,
		InputModalities:        inputModalities,
		OutputModalities:       outputModalities,
		SupportsTools:          item.SupportsTools,
		SupportsToolChoice:     item.SupportsToolChoice,
		SupportsParallelTools:  item.SupportsParallelTools,
		SupportsThinking:       firstBool(item.SupportsThinking, item.Capabilities.Thinking.Supported, item.Capabilities.Thinking.Types.Enabled.Supported),
		SupportsPromptCaching:  item.SupportsPromptCaching,
		SupportsSystemMessages: item.SupportsSystemMessages,
		SupportsVision:         firstBool(item.SupportsVision, item.Capabilities.ImageInput.Supported),
		SupportsPDFInput:       firstBool(item.SupportsPDFInput, item.Capabilities.PDFInput.Supported),
		SupportsAudioInput:     item.SupportsAudioInput,
		SupportsAudioOutput:    item.SupportsAudioOutput,
		SupportsResponseSchema: firstBool(item.SupportsResponseSchema, item.Capabilities.StructuredOutputs.Supported),
		SupportsResponses:      item.SupportsResponses,
		SupportsComputerUse:    item.SupportsComputerUse,
	}
	normalizeProviderModalities(&values)
	applySupportedParameters(&values, item.SupportedParameters)
	applySupportedParameters(&values, item.SupportedOpenAIParams)
	applyModalityCapabilities(&values)
	return values
}

func normalizeProviderModalities(values *modelcap.Values) {
	values.InputModalities = normalizeProviderModalityList(values.InputModalities)
	values.OutputModalities = normalizeProviderModalityList(values.OutputModalities)
}

func normalizeProviderModalityList(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		canonical := canonicalProviderModality(value)
		if canonical == "" {
			continue
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return normalized
}

func canonicalProviderModality(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "text", "image", "audio", "pdf", "video":
		return strings.ToLower(strings.TrimSpace(value))
	case "texts":
		return "text"
	case "images", "image_url":
		return "image"
	case "audios":
		return "audio"
	case "document", "documents":
		return "pdf"
	case "videos":
		return "video"
	default:
		// Generic files and vendor-specific modalities are not safe to narrow.
		return ""
	}
}

func applyOpenAIAdapterCapabilities(snapshot *modelcap.Snapshot) {
	snapshot.Values.SupportsStreaming = modelcap.Bool(true)
	if snapshot.Sources == nil {
		snapshot.Sources = make(map[string]string)
	}
	snapshot.Sources["supports_streaming"] = modelcap.SourceOpenAIAdapter
	if snapshot.Values.Kind == modelcap.KindResponses && snapshot.Values.SupportsResponses == nil {
		snapshot.Values.SupportsResponses = modelcap.Bool(true)
		snapshot.Sources["supports_responses"] = modelcap.SourceOpenAIAdapter
	}
}

func applySupportedParameters(values *modelcap.Values, parameters []string) {
	for _, parameter := range parameters {
		switch strings.ToLower(strings.TrimSpace(parameter)) {
		case "tools", "tool_use", "functions":
			setTrueIfUnknown(&values.SupportsTools)
		case "tool_choice":
			setTrueIfUnknown(&values.SupportsToolChoice)
		case "parallel_tool_calls":
			setTrueIfUnknown(&values.SupportsParallelTools)
		case "stream":
			setTrueIfUnknown(&values.SupportsStreaming)
		case "reasoning", "thinking":
			setTrueIfUnknown(&values.SupportsThinking)
		case "response_format", "structured_outputs":
			setTrueIfUnknown(&values.SupportsResponseSchema)
		case "responses", "response_api":
			setTrueIfUnknown(&values.SupportsResponses)
		case "computer", "computer_use":
			setTrueIfUnknown(&values.SupportsComputerUse)
		}
	}
}

func applyModalityCapabilities(values *modelcap.Values) {
	if slices.Contains(values.InputModalities, "image") && values.SupportsVision == nil {
		values.SupportsVision = modelcap.Bool(true)
	}
	if slices.Contains(values.InputModalities, "pdf") && values.SupportsPDFInput == nil {
		values.SupportsPDFInput = modelcap.Bool(true)
	}
	if slices.Contains(values.InputModalities, "audio") && values.SupportsAudioInput == nil {
		values.SupportsAudioInput = modelcap.Bool(true)
	}
	if slices.Contains(values.OutputModalities, "audio") && values.SupportsAudioOutput == nil {
		values.SupportsAudioOutput = modelcap.Bool(true)
	}
}

func mergeDiscoveredModels(base, metadata []DiscoveredModel) ([]DiscoveredModel, error) {
	byID := make(map[string]int, len(base))
	for index := range base {
		byID[base[index].ID] = index
	}
	for updateIndex := range metadata {
		update := &metadata[updateIndex]
		index, ok := byID[update.ID]
		if !ok {
			continue
		}
		merged, err := modelcap.MergeSnapshots(base[index].Capabilities, update.Capabilities)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", update.ID, err)
		}
		base[index].Capabilities = merged
		base[index].CapabilityMetadataComplete = true
	}
	return base, nil
}

func classifyDiscoveredModels(models []DiscoveredModel) {
	for index := range models {
		model := &models[index]
		if reason, control := liteLLMControlModelReason(model.ID); control {
			model.Routable = false
			model.SkipReason = reason
			continue
		}
		if !modelcap.IsRoutableKind(model.Capabilities.Values.Kind) {
			model.Routable = false
			model.SkipReason = "non-chat model (kind=" + model.Capabilities.Values.Kind + ")"
			continue
		}
		model.Routable = true
		model.SkipReason = ""
	}
}

func IsProviderControlModel(providerType, modelID string) bool {
	if providerType != "litellm" {
		return false
	}
	_, found := liteLLMControlModelReason(modelID)
	return found
}

func liteLLMControlModelReason(modelID string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "all-proxy-models", "all-team-models", "no-default-models":
		return "LiteLLM control model", true
	default:
		return "", false
	}
}

func setTrueIfUnknown(value **bool) {
	if *value == nil {
		*value = modelcap.Bool(true)
	}
}

func firstBool(values ...*bool) *bool {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func maxOptionalInt64(values ...*int64) *int64 {
	var maximum *int64
	for _, value := range values {
		if value == nil || *value <= 0 || maximum != nil && *value <= *maximum {
			continue
		}
		copyValue := *value
		maximum = &copyValue
	}
	return maximum
}

func firstPositiveOptionalInt64(values ...*int64) *int64 {
	for _, value := range values {
		if positive := positiveOptionalInt64(value); positive != nil {
			return positive
		}
	}
	return nil
}

func positiveOptionalInt64(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func firstNonEmptySlice(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func kindFromProviderMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "chat", "messages":
		return modelcap.KindChat
	case "completion", "completions", "text_completion":
		return modelcap.KindCompletion
	case "responses", "response":
		return modelcap.KindResponses
	case "embedding", "embeddings":
		return modelcap.KindEmbedding
	case "rerank", "reranking":
		return modelcap.KindRerank
	case "image", "image_generation":
		return modelcap.KindImage
	case "audio", "audio_speech", "audio_transcription":
		return modelcap.KindAudio
	case "control":
		return modelcap.KindControl
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
