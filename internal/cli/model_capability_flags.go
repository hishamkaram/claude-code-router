package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

type modelCapabilityFlags struct {
	kind             string
	contextWindow    int64
	maxInputTokens   int64
	maxOutputTokens  int64
	inputModalities  string
	outputModalities string
	tools            string
	toolChoice       string
	parallelTools    string
	streaming        string
	thinking         string
	promptCaching    string
	systemMessages   string
	vision           string
	pdfInput         string
	audioInput       string
	audioOutput      string
	responseSchema   string
	responses        string
	computerUse      string
	clearAll         bool
}

type capabilityBooleanBinding struct {
	name  string
	value *string
}

func capabilityBooleanBindings(flags *modelCapabilityFlags) []capabilityBooleanBinding {
	return []capabilityBooleanBinding{
		{name: "tools", value: &flags.tools},
		{name: "tool-choice", value: &flags.toolChoice},
		{name: "parallel-tools", value: &flags.parallelTools},
		{name: "streaming", value: &flags.streaming},
		{name: "thinking", value: &flags.thinking},
		{name: "prompt-caching", value: &flags.promptCaching},
		{name: "system-messages", value: &flags.systemMessages},
		{name: "vision", value: &flags.vision},
		{name: "pdf-input", value: &flags.pdfInput},
		{name: "audio-input", value: &flags.audioInput},
		{name: "audio-output", value: &flags.audioOutput},
		{name: "response-schema", value: &flags.responseSchema},
		{name: "responses", value: &flags.responses},
		{name: "computer-use", value: &flags.computerUse},
	}
}

func (flags *modelCapabilityFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&flags.kind, "model-kind", "", "Capability override: chat, completion, responses, embedding, rerank, image, audio, control, unknown, or auto")
	cmd.Flags().Int64Var(&flags.contextWindow, "context-window", 0, "Context-window token override; 0 clears it")
	cmd.Flags().Int64Var(&flags.maxInputTokens, "max-input-tokens", 0, "Maximum input-token override; 0 clears it")
	cmd.Flags().Int64Var(&flags.maxOutputTokens, "max-output-tokens", 0, "Maximum output-token override; 0 clears it")
	cmd.Flags().StringVar(&flags.inputModalities, "input-modalities", "", "Comma-separated input modalities, or auto")
	cmd.Flags().StringVar(&flags.outputModalities, "output-modalities", "", "Comma-separated output modalities, or auto")
	for _, capability := range capabilityBooleanBindings(flags) {
		cmd.Flags().StringVar(capability.value, capability.name, "", "Capability override: true, false, or auto")
	}
	cmd.Flags().BoolVar(&flags.clearAll, "clear-capabilities", false, "Clear all explicit capability overrides before applying other capability flags")
}

func capabilityFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range capabilityFlagNames() {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func capabilityFlagNames() []string {
	bindings := capabilityBooleanBindings(&modelCapabilityFlags{})
	names := make([]string, 0, 7+len(bindings))
	names = append(names,
		"model-kind", "context-window", "max-input-tokens", "max-output-tokens",
		"input-modalities", "output-modalities", "clear-capabilities",
	)
	for _, capability := range bindings {
		names = append(names, capability.name)
	}
	return names
}

func (flags modelCapabilityFlags) apply(cmd *cobra.Command, current modelcap.Values) (modelcap.Values, error) {
	if flags.clearAll {
		current = modelcap.Values{}
	}
	if cmd.Flags().Changed("model-kind") {
		kind := strings.ToLower(strings.TrimSpace(flags.kind))
		if kind == "auto" {
			kind = ""
		}
		current.Kind = kind
	}
	if err := applyTokenOverride(cmd, "context-window", flags.contextWindow, &current.ContextWindowTokens); err != nil {
		return modelcap.Values{}, err
	}
	if err := applyTokenOverride(cmd, "max-input-tokens", flags.maxInputTokens, &current.MaxInputTokens); err != nil {
		return modelcap.Values{}, err
	}
	if err := applyTokenOverride(cmd, "max-output-tokens", flags.maxOutputTokens, &current.MaxOutputTokens); err != nil {
		return modelcap.Values{}, err
	}
	if cmd.Flags().Changed("input-modalities") {
		current.InputModalities = parseModalityOverride(flags.inputModalities)
	}
	if cmd.Flags().Changed("output-modalities") {
		current.OutputModalities = parseModalityOverride(flags.outputModalities)
	}
	booleanTargets := map[string]**bool{
		"tools":           &current.SupportsTools,
		"tool-choice":     &current.SupportsToolChoice,
		"parallel-tools":  &current.SupportsParallelTools,
		"streaming":       &current.SupportsStreaming,
		"thinking":        &current.SupportsThinking,
		"prompt-caching":  &current.SupportsPromptCaching,
		"system-messages": &current.SupportsSystemMessages,
		"vision":          &current.SupportsVision,
		"pdf-input":       &current.SupportsPDFInput,
		"audio-input":     &current.SupportsAudioInput,
		"audio-output":    &current.SupportsAudioOutput,
		"response-schema": &current.SupportsResponseSchema,
		"responses":       &current.SupportsResponses,
		"computer-use":    &current.SupportsComputerUse,
	}
	for _, capability := range capabilityBooleanBindings(&flags) {
		if !cmd.Flags().Changed(capability.name) {
			continue
		}
		value, err := parseBooleanOverride(*capability.value)
		if err != nil {
			return modelcap.Values{}, fmt.Errorf("--%s: %w", capability.name, err)
		}
		*booleanTargets[capability.name] = value
	}
	values, err := modelcap.NormalizeValues(current)
	if err != nil {
		return modelcap.Values{}, err
	}
	return values, nil
}

func applyTokenOverride(cmd *cobra.Command, name string, value int64, destination **int64) error {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	if value < 0 {
		return fmt.Errorf("--%s must be zero or greater", name)
	}
	if value == 0 {
		*destination = nil
		return nil
	}
	*destination = modelcap.Int64(value)
	return nil
}

func parseModalityOverride(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "auto") {
		return nil
	}
	return strings.Split(value, ",")
}

func parseBooleanOverride(value string) (*bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "auto" {
		return nil, nil
	}
	if value != "true" && value != "false" {
		return nil, fmt.Errorf("must be true, false, or auto")
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("parsing boolean: %w", err)
	}
	return modelcap.Bool(parsed), nil
}
