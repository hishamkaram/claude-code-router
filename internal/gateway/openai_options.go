package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

type openAIRequestOptions struct {
	user            string
	reasoningEffort string
	responseFormat  *openAIResponseFormat
}

func openAIOptionsFromAnthropic(req anthropicRequest) (openAIRequestOptions, error) {
	user, err := openAIUserFromMetadata(req.Metadata)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	reasoningEffort, err := openAIReasoningEffortFromOutputConfig(req.OutputConfig)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	if reasoningEffort == "" {
		reasoningEffort, err = openAIReasoningEffortFromThinking(req.Thinking)
		if err != nil {
			return openAIRequestOptions{}, err
		}
	}
	responseFormat, err := openAIResponseFormatFromOutputConfig(req.OutputConfig)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	return openAIRequestOptions{
		user:            user,
		reasoningEffort: reasoningEffort,
		responseFormat:  responseFormat,
	}, nil
}

func openAIUserFromMetadata(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported metadata: %w", err)
	}
	var user string
	for key, value := range payload {
		if key == "user_id" {
			if err := json.Unmarshal(value, &user); err != nil {
				return "", fmt.Errorf("metadata.user_id must be a string")
			}
		}
	}
	return strings.TrimSpace(user), nil
}

func openAIReasoningEffortFromOutputConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported output_config: %w", err)
	}
	var effort string
	for key, value := range payload {
		if key == "effort" {
			if err := json.Unmarshal(value, &effort); err != nil {
				return "", fmt.Errorf("output_config.effort must be a string")
			}
		}
	}
	return openAIReasoningEffortFromClaudeEffort(effort)
}

func openAIResponseFormatFromOutputConfig(raw json.RawMessage) (*openAIResponseFormat, error) {
	if !rawJSONPresent(raw) {
		return nil, nil
	}
	var outputConfig struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &outputConfig); err != nil {
		return nil, fmt.Errorf("unsupported output_config: %w", err)
	}
	if !rawJSONPresent(outputConfig.Format) {
		return nil, nil
	}
	var format struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(outputConfig.Format, &format); err != nil {
		return nil, fmt.Errorf("output_config.format must be an object")
	}
	if format.Type != "json_schema" {
		return nil, fmt.Errorf("output_config.format.type %q is not supported by the OpenAI-compatible gateway path", format.Type)
	}
	var schema map[string]any
	if !rawJSONPresent(format.Schema) || json.Unmarshal(format.Schema, &schema) != nil || schema == nil {
		return nil, fmt.Errorf("output_config.format.schema must be a JSON object")
	}
	return &openAIResponseFormat{
		Type: "json_schema",
		JSONSchema: openAIJSONSchema{
			Name:   "claude_output",
			Schema: format.Schema,
			Strict: true,
		},
	}, nil
}

func openAIReasoningEffortFromThinking(raw json.RawMessage) (string, error) {
	thinkingType, err := openAIThinkingType(raw)
	if err != nil {
		return "", err
	}
	if thinkingType == "enabled" {
		return "high", nil
	}
	return "", nil
}

func openAIThinkingType(raw json.RawMessage) (string, error) {
	if !rawJSONPresent(raw) {
		return "", nil
	}
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported thinking field: %w", err)
	}
	return strings.TrimSpace(payload.Type), nil
}

func openAIReasoningEffortFromClaudeEffort(effort string) (string, error) {
	trimmed := strings.TrimSpace(effort)
	switch trimmed {
	case "":
		return "", nil
	case "low", "medium", "high":
		return trimmed, nil
	case "xhigh", "max":
		return "high", nil
	default:
		return "", fmt.Errorf("output_config.effort %q is not supported by the OpenAI-compatible gateway path", effort)
	}
}
