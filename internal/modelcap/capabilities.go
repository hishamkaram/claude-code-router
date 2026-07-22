package modelcap

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	KindUnknown    = "unknown"
	KindChat       = "chat"
	KindCompletion = "completion"
	KindResponses  = "responses"
	KindEmbedding  = "embedding"
	KindRerank     = "rerank"
	KindImage      = "image"
	KindAudio      = "audio"
	KindControl    = "control"

	SourceOverride      = "override"
	SourceModelIDHint   = "model_id_hint"
	SourceOpenAIModels  = "openai:/v1/models"
	SourceLiteLLMInfo   = "litellm:/model/info"
	SourceOpenAIAdapter = "gateway:openai-adapter"
)

// Values is the normalized, provider-independent model capability contract.
// Pointer booleans distinguish an explicit false value from unknown metadata.
type Values struct {
	Kind                   string   `json:"kind,omitempty"`
	ContextWindowTokens    *int64   `json:"context_window_tokens,omitempty"`
	MaxInputTokens         *int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens        *int64   `json:"max_output_tokens,omitempty"`
	InputModalities        []string `json:"input_modalities,omitempty"`
	OutputModalities       []string `json:"output_modalities,omitempty"`
	SupportsTools          *bool    `json:"supports_tools,omitempty"`
	SupportsToolChoice     *bool    `json:"supports_tool_choice,omitempty"`
	SupportsParallelTools  *bool    `json:"supports_parallel_tools,omitempty"`
	SupportsStreaming      *bool    `json:"supports_streaming,omitempty"`
	SupportsThinking       *bool    `json:"supports_thinking,omitempty"`
	SupportsPromptCaching  *bool    `json:"supports_prompt_caching,omitempty"`
	SupportsSystemMessages *bool    `json:"supports_system_messages,omitempty"`
	SupportsVision         *bool    `json:"supports_vision,omitempty"`
	SupportsPDFInput       *bool    `json:"supports_pdf_input,omitempty"`
	SupportsAudioInput     *bool    `json:"supports_audio_input,omitempty"`
	SupportsAudioOutput    *bool    `json:"supports_audio_output,omitempty"`
	SupportsResponseSchema *bool    `json:"supports_response_schema,omitempty"`
	SupportsResponses      *bool    `json:"supports_responses,omitempty"`
	SupportsComputerUse    *bool    `json:"supports_computer_use,omitempty"`
	presentFields          map[string]struct{}
}

// Snapshot stores normalized values and per-field provenance from discovery.
type Snapshot struct {
	Values  Values            `json:"values,omitempty"`
	Sources map[string]string `json:"sources,omitempty"`
}

func (v *Values) UnmarshalJSON(data []byte) error {
	fields, err := decodeValueFields(data)
	if err != nil {
		return err
	}
	type wire Values
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*v = Values(decoded)
	for _, field := range []string{"supports_responses", "supports_computer_use"} {
		if _, ok := fields[field]; ok {
			if v.presentFields == nil {
				v.presentFields = make(map[string]struct{}, 2)
			}
			v.presentFields[field] = struct{}{}
		}
	}
	return nil
}

func decodeValueFields(data []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	unknown := make([]string, 0)
	for field := range fields {
		if !isValueField(field) {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, fmt.Errorf("json: unknown field %q", unknown[0])
	}
	return fields, nil
}

func isValueField(field string) bool {
	switch field {
	case "kind",
		"context_window_tokens",
		"max_input_tokens",
		"max_output_tokens",
		"input_modalities",
		"output_modalities",
		"supports_tools",
		"supports_tool_choice",
		"supports_parallel_tools",
		"supports_streaming",
		"supports_thinking",
		"supports_prompt_caching",
		"supports_system_messages",
		"supports_vision",
		"supports_pdf_input",
		"supports_audio_input",
		"supports_audio_output",
		"supports_response_schema",
		"supports_responses",
		"supports_computer_use":
		return true
	default:
		return false
	}
}

// Effective merges discovered values, explicit overrides, and safe model-ID hints.
func Effective(discovered Snapshot, overrides Values, providerModel string) (Snapshot, error) {
	normalizedDiscovered, err := NormalizeSnapshot(discovered)
	if err != nil {
		return Snapshot{}, err
	}
	normalizedOverrides, err := NormalizeValues(overrides)
	if err != nil {
		return Snapshot{}, err
	}
	effective := normalizedDiscovered
	if effective.Sources == nil {
		effective.Sources = make(map[string]string)
	}
	applyValues(&effective.Values, normalizedOverrides, effective.Sources, SourceOverride)
	if effective.Values.ContextWindowTokens == nil && hasOneMillionHint(providerModel) {
		value := int64(1_000_000)
		effective.Values.ContextWindowTokens = &value
		effective.Sources["context_window_tokens"] = SourceModelIDHint
	}
	return effective, nil
}

// SnapshotFrom creates a normalized snapshot whose populated fields share one source.
func SnapshotFrom(values Values, source string) (Snapshot, error) {
	values, err := NormalizeValues(values)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Values: values, Sources: make(map[string]string)}
	for _, field := range PopulatedFields(values) {
		snapshot.Sources[field] = source
	}
	return snapshot, nil
}

// MergeSnapshots overlays populated update fields while preserving unknown base fields.
func MergeSnapshots(base, update Snapshot) (Snapshot, error) {
	base, err := NormalizeSnapshot(base)
	if err != nil {
		return Snapshot{}, err
	}
	update, err = NormalizeSnapshot(update)
	if err != nil {
		return Snapshot{}, err
	}
	if base.Sources == nil {
		base.Sources = make(map[string]string)
	}
	applySnapshotValues(&base.Values, update.Values)
	for _, field := range PopulatedFields(update.Values) {
		if source := update.Sources[field]; source != "" {
			base.Sources[field] = source
		} else {
			delete(base.Sources, field)
		}
	}
	return base, nil
}

func NormalizeSnapshot(snapshot Snapshot) (Snapshot, error) {
	values, err := NormalizeValues(snapshot.Values)
	if err != nil {
		return Snapshot{}, err
	}
	populated := PopulatedFields(values)
	sources := make(map[string]string, len(populated))
	for _, field := range populated {
		if source := strings.TrimSpace(snapshot.Sources[field]); source != "" {
			sources[field] = source
		}
	}
	return Snapshot{Values: values, Sources: sources}, nil
}

func NormalizeValues(values Values) (Values, error) {
	values.Kind = strings.ToLower(strings.TrimSpace(values.Kind))
	if values.Kind != "" && !validKind(values.Kind) {
		return Values{}, fmt.Errorf("invalid model capability kind %q", values.Kind)
	}
	positiveIntegers := []struct {
		name  string
		value *int64
	}{
		{name: "context_window_tokens", value: values.ContextWindowTokens},
		{name: "max_input_tokens", value: values.MaxInputTokens},
		{name: "max_output_tokens", value: values.MaxOutputTokens},
	}
	for _, capability := range positiveIntegers {
		name, value := capability.name, capability.value
		if value != nil && *value <= 0 {
			return Values{}, fmt.Errorf("model capability %s must be greater than zero", name)
		}
	}
	var err error
	values.InputModalities, err = normalizeModalities(values.InputModalities)
	if err != nil {
		return Values{}, fmt.Errorf("input modalities: %w", err)
	}
	values.OutputModalities, err = normalizeModalities(values.OutputModalities)
	if err != nil {
		return Values{}, fmt.Errorf("output modalities: %w", err)
	}
	return values, nil
}

func PopulatedFields(values Values) []string {
	fields := make([]string, 0, 20)
	if values.Kind != "" {
		fields = append(fields, "kind")
	}
	if values.ContextWindowTokens != nil {
		fields = append(fields, "context_window_tokens")
	}
	if values.MaxInputTokens != nil {
		fields = append(fields, "max_input_tokens")
	}
	if values.MaxOutputTokens != nil {
		fields = append(fields, "max_output_tokens")
	}
	if len(values.InputModalities) > 0 {
		fields = append(fields, "input_modalities")
	}
	if len(values.OutputModalities) > 0 {
		fields = append(fields, "output_modalities")
	}
	boolFields := []struct {
		name  string
		value *bool
	}{
		{name: "supports_tools", value: values.SupportsTools},
		{name: "supports_tool_choice", value: values.SupportsToolChoice},
		{name: "supports_parallel_tools", value: values.SupportsParallelTools},
		{name: "supports_streaming", value: values.SupportsStreaming},
		{name: "supports_thinking", value: values.SupportsThinking},
		{name: "supports_prompt_caching", value: values.SupportsPromptCaching},
		{name: "supports_system_messages", value: values.SupportsSystemMessages},
		{name: "supports_vision", value: values.SupportsVision},
		{name: "supports_pdf_input", value: values.SupportsPDFInput},
		{name: "supports_audio_input", value: values.SupportsAudioInput},
		{name: "supports_audio_output", value: values.SupportsAudioOutput},
		{name: "supports_response_schema", value: values.SupportsResponseSchema},
		{name: "supports_responses", value: values.SupportsResponses},
		{name: "supports_computer_use", value: values.SupportsComputerUse},
	}
	for _, field := range boolFields {
		if field.value != nil {
			fields = append(fields, field.name)
		}
	}
	return fields
}

func IsZeroValues(values Values) bool {
	return len(PopulatedFields(values)) == 0
}

func IsZeroSnapshot(snapshot Snapshot) bool {
	return IsZeroValues(snapshot.Values) && len(snapshot.Sources) == 0
}

func UsesSchemaV3Fields(values Values) bool {
	return valueFieldPresent(values, "supports_responses", values.SupportsResponses) ||
		valueFieldPresent(values, "supports_computer_use", values.SupportsComputerUse)
}

func valueFieldPresent(values Values, field string, value *bool) bool {
	if value != nil {
		return true
	}
	_, ok := values.presentFields[field]
	return ok
}

func SupportsOneMillion(values Values) bool {
	return values.ContextWindowTokens != nil && *values.ContextWindowTokens >= 1_000_000
}

// IsRoutableKind reports whether CCR can safely use a model through a
// conversational Anthropic-compatible gateway route. Empty and unknown kinds
// remain routable because discovery frequently omits this field.
func IsRoutableKind(kind string) bool {
	switch kind {
	case "", KindUnknown, KindChat, KindCompletion, KindResponses:
		return true
	default:
		return false
	}
}

func Bool(value bool) *bool {
	return &value
}

func Int64(value int64) *int64 {
	return &value
}

func applyValues(target *Values, values Values, sources map[string]string, source string) {
	if values.Kind != "" {
		target.Kind = values.Kind
		sources["kind"] = source
	}
	applyInt64 := func(name string, value *int64, destination **int64) {
		if value != nil {
			copyValue := *value
			*destination = &copyValue
			sources[name] = source
		}
	}
	applyBool := func(name string, value *bool, destination **bool) {
		if value != nil {
			copyValue := *value
			*destination = &copyValue
			sources[name] = source
		}
	}
	applyInt64("context_window_tokens", values.ContextWindowTokens, &target.ContextWindowTokens)
	applyInt64("max_input_tokens", values.MaxInputTokens, &target.MaxInputTokens)
	applyInt64("max_output_tokens", values.MaxOutputTokens, &target.MaxOutputTokens)
	if len(values.InputModalities) > 0 {
		target.InputModalities = slices.Clone(values.InputModalities)
		sources["input_modalities"] = source
	}
	if len(values.OutputModalities) > 0 {
		target.OutputModalities = slices.Clone(values.OutputModalities)
		sources["output_modalities"] = source
	}
	applyBool("supports_tools", values.SupportsTools, &target.SupportsTools)
	applyBool("supports_tool_choice", values.SupportsToolChoice, &target.SupportsToolChoice)
	applyBool("supports_parallel_tools", values.SupportsParallelTools, &target.SupportsParallelTools)
	applyBool("supports_streaming", values.SupportsStreaming, &target.SupportsStreaming)
	applyBool("supports_thinking", values.SupportsThinking, &target.SupportsThinking)
	applyBool("supports_prompt_caching", values.SupportsPromptCaching, &target.SupportsPromptCaching)
	applyBool("supports_system_messages", values.SupportsSystemMessages, &target.SupportsSystemMessages)
	applyBool("supports_vision", values.SupportsVision, &target.SupportsVision)
	applyBool("supports_pdf_input", values.SupportsPDFInput, &target.SupportsPDFInput)
	applyBool("supports_audio_input", values.SupportsAudioInput, &target.SupportsAudioInput)
	applyBool("supports_audio_output", values.SupportsAudioOutput, &target.SupportsAudioOutput)
	applyBool("supports_response_schema", values.SupportsResponseSchema, &target.SupportsResponseSchema)
	applyBool("supports_responses", values.SupportsResponses, &target.SupportsResponses)
	applyBool("supports_computer_use", values.SupportsComputerUse, &target.SupportsComputerUse)
}

func applySnapshotValues(target *Values, source Values) {
	if source.Kind != "" {
		target.Kind = source.Kind
	}
	copyInt64 := func(value *int64, destination **int64) {
		if value != nil {
			copied := *value
			*destination = &copied
		}
	}
	copyBool := func(value *bool, destination **bool) {
		if value != nil {
			copied := *value
			*destination = &copied
		}
	}
	copyInt64(source.ContextWindowTokens, &target.ContextWindowTokens)
	copyInt64(source.MaxInputTokens, &target.MaxInputTokens)
	copyInt64(source.MaxOutputTokens, &target.MaxOutputTokens)
	if len(source.InputModalities) > 0 {
		target.InputModalities = slices.Clone(source.InputModalities)
	}
	if len(source.OutputModalities) > 0 {
		target.OutputModalities = slices.Clone(source.OutputModalities)
	}
	copyBool(source.SupportsTools, &target.SupportsTools)
	copyBool(source.SupportsToolChoice, &target.SupportsToolChoice)
	copyBool(source.SupportsParallelTools, &target.SupportsParallelTools)
	copyBool(source.SupportsStreaming, &target.SupportsStreaming)
	copyBool(source.SupportsThinking, &target.SupportsThinking)
	copyBool(source.SupportsPromptCaching, &target.SupportsPromptCaching)
	copyBool(source.SupportsSystemMessages, &target.SupportsSystemMessages)
	copyBool(source.SupportsVision, &target.SupportsVision)
	copyBool(source.SupportsPDFInput, &target.SupportsPDFInput)
	copyBool(source.SupportsAudioInput, &target.SupportsAudioInput)
	copyBool(source.SupportsAudioOutput, &target.SupportsAudioOutput)
	copyBool(source.SupportsResponseSchema, &target.SupportsResponseSchema)
	copyBool(source.SupportsResponses, &target.SupportsResponses)
	copyBool(source.SupportsComputerUse, &target.SupportsComputerUse)
}

func normalizeModalities(values []string) ([]string, error) {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		switch value {
		case "text", "image", "audio", "pdf", "video":
		default:
			return nil, fmt.Errorf("unsupported modality %q", value)
		}
		if !slices.Contains(normalized, value) {
			normalized = append(normalized, value)
		}
	}
	slices.Sort(normalized)
	return normalized, nil
}

func validKind(kind string) bool {
	switch kind {
	case KindUnknown, KindChat, KindCompletion, KindResponses, KindEmbedding, KindRerank, KindImage, KindAudio, KindControl:
		return true
	default:
		return false
	}
}

func hasOneMillionHint(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasSuffix(model, "[1m]")
}
