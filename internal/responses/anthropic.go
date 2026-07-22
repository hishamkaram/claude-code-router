package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

type anthropicRequest struct {
	Model         string            `json:"model"`
	System        json.RawMessage   `json:"system,omitempty"`
	Messages      []anthropicMsg    `json:"messages"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage   `json:"metadata,omitempty"`
	Thinking      json.RawMessage   `json:"thinking,omitempty"`
	OutputConfig  json.RawMessage   `json:"output_config,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type convertState struct {
	hasNativeComputer bool
	computerCallIDs   map[string]bool
	functionToolNames map[string]bool
}

// RequestFromAnthropicMessagesJSON converts raw Anthropic Messages JSON into a
// typed OpenAI Responses request.
func RequestFromAnthropicMessagesJSON(raw []byte) (*Request, error) {
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode Anthropic Messages request: %w", err)
	}
	tools, hasComputer, functionNames, err := responsesTools(req.Tools)
	if err != nil {
		return nil, err
	}
	metadata, err := responsesMetadata(req.Metadata)
	if err != nil {
		return nil, err
	}
	reasoning, text, err := responsesOutputOptions(req.OutputConfig, req.Thinking)
	if err != nil {
		return nil, err
	}
	state := convertState{
		hasNativeComputer: hasComputer,
		computerCallIDs:   make(map[string]bool),
		functionToolNames: functionNames,
	}
	input := make([]InputItem, 0, len(req.Messages))
	for _, msg := range req.Messages {
		items, conversionErr := inputItemsFromAnthropicMessage(msg, &state)
		if conversionErr != nil {
			return nil, conversionErr
		}
		input = append(input, items...)
	}
	converted := &Request{
		Model:           req.Model,
		Input:           input,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		Stop:            req.StopSequences,
		Tools:           tools,
		Metadata:        metadata,
		Reasoning:       reasoning,
		Text:            text,
	}
	if len(req.System) > 0 && string(req.System) != "null" {
		instructions, instructionErr := textFromAnthropicContent(req.System)
		if instructionErr != nil {
			return nil, fmt.Errorf("convert system content: %w", instructionErr)
		}
		converted.Instructions = instructions
	}
	toolChoice, parallelToolCalls, err := responsesToolChoice(req.ToolChoice, hasComputer)
	if err != nil {
		return nil, err
	}
	converted.ToolChoice = toolChoice
	converted.ParallelToolCalls = parallelToolCalls
	return converted, nil
}

func responsesMetadata(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("metadata must be an object with string values")
	}
	if len(metadata) == 0 {
		return nil, nil
	}
	return metadata, nil
}

func responsesOutputOptions(outputConfig, thinking json.RawMessage) (*Reasoning, *Text, error) {
	effort, text, err := responsesOutputConfig(outputConfig)
	if err != nil {
		return nil, nil, err
	}
	thinkingEffort, err := responsesThinkingEffort(thinking)
	if err != nil {
		return nil, nil, err
	}
	if effort == "" {
		effort = thinkingEffort
	}
	if effort == "" {
		return nil, text, nil
	}
	return &Reasoning{Effort: effort}, text, nil
}

func responsesOutputConfig(raw json.RawMessage) (string, *Text, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var config struct {
		Effort string          `json:"effort"`
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", nil, fmt.Errorf("unsupported output_config: %w", err)
	}
	effort, err := responsesReasoningEffort(config.Effort)
	if err != nil {
		return "", nil, err
	}
	if len(config.Format) == 0 || string(config.Format) == "null" {
		return effort, nil, nil
	}
	var format struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(config.Format, &format); err != nil {
		return "", nil, fmt.Errorf("output_config.format must be an object")
	}
	if format.Type != "json_schema" {
		return "", nil, fmt.Errorf("output_config.format.type %q is not supported by the OpenAI Responses gateway path", format.Type)
	}
	var schema map[string]any
	if len(format.Schema) == 0 || string(format.Schema) == "null" || json.Unmarshal(format.Schema, &schema) != nil || schema == nil {
		return "", nil, fmt.Errorf("output_config.format.schema must be a JSON object")
	}
	return effort, &Text{Format: &TextFormat{
		Type: "json_schema", Name: "claude_output", Schema: format.Schema, Strict: true,
	}}, nil
}

func responsesThinkingEffort(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var thinking struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &thinking); err != nil {
		return "", fmt.Errorf("unsupported thinking field: %w", err)
	}
	switch strings.TrimSpace(thinking.Type) {
	case "", "adaptive", "disabled":
		return "", nil
	case "enabled":
		return "high", nil
	default:
		return "", fmt.Errorf("thinking mode %q is not supported by the OpenAI Responses gateway path", thinking.Type)
	}
}

func responsesReasoningEffort(effort string) (string, error) {
	switch strings.TrimSpace(effort) {
	case "":
		return "", nil
	case "low", "medium", "high":
		return strings.TrimSpace(effort), nil
	case "xhigh", "max":
		return "high", nil
	default:
		return "", fmt.Errorf("output_config.effort %q is not supported by the OpenAI Responses gateway path", effort)
	}
}

func inputItemsFromAnthropicMessage(msg anthropicMsg, state *convertState) ([]InputItem, error) {
	switch msg.Role {
	case "user":
		return userInputItems(msg.Content, state)
	case "assistant":
		return assistantInputItems(msg.Content, state)
	case "system":
		return developerInputItems(msg.Content)
	default:
		return nil, fmt.Errorf("unsupported Anthropic message role %q", msg.Role)
	}
}

func developerInputItems(raw json.RawMessage) ([]InputItem, error) {
	text, err := textFromAnthropicContent(raw)
	if err != nil {
		return nil, fmt.Errorf("convert system message content: %w", err)
	}
	if text == "" {
		return nil, nil
	}
	return []InputItem{messageInput("developer", []Content{{Type: "input_text", Text: text}})}, nil
}

func userInputItems(raw json.RawMessage, state *convertState) ([]InputItem, error) {
	if text, ok, err := rawString(raw); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return []InputItem{messageInput("user", []Content{{Type: "input_text", Text: text}})}, nil
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return nil, fmt.Errorf("user content must be a string or array: %w", err)
	}
	items := make([]InputItem, 0, len(blocks))
	parts := make([]Content, 0, len(blocks))
	flush := func() {
		if len(parts) == 0 {
			return
		}
		items = append(items, messageInput("user", parts))
		parts = nil
	}
	for _, rawBlock := range blocks {
		blockType, err := blockType(rawBlock)
		if err != nil {
			return nil, err
		}
		if blockType == "tool_result" {
			flush()
			toolItems, toolErr := toolResultInputItems(rawBlock, state)
			if toolErr != nil {
				return nil, toolErr
			}
			items = append(items, toolItems...)
			continue
		}
		part, err := userInputContentPart(blockType, rawBlock)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	flush()
	return items, nil
}

func userInputContentPart(blockType string, raw json.RawMessage) (Content, error) {
	switch blockType {
	case "text":
		text, err := textBlock(raw)
		if err != nil {
			return Content{}, err
		}
		return Content{Type: "input_text", Text: text}, nil
	case "image":
		imageURL, err := imageURLFromBlock(raw)
		if err != nil {
			return Content{}, err
		}
		return Content{Type: "input_image", ImageURL: imageURL}, nil
	case "document":
		return Content{}, fmt.Errorf("%w: document blocks are not supported", ErrUnsupportedPDF)
	case "audio":
		return Content{}, fmt.Errorf("%w: audio blocks are not supported", ErrUnsupportedAudio)
	default:
		return Content{}, fmt.Errorf("unsupported user content block type %q", blockType)
	}
}

func assistantInputItems(raw json.RawMessage, state *convertState) ([]InputItem, error) {
	if text, ok, err := rawString(raw); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return []InputItem{messageInput("assistant", []Content{{Type: "output_text", Text: text}})}, nil
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return nil, fmt.Errorf("assistant content must be a string or array: %w", err)
	}
	items := make([]InputItem, 0, len(blocks))
	parts := make([]Content, 0, len(blocks))
	flush := func() {
		if len(parts) == 0 {
			return
		}
		items = append(items, messageInput("assistant", parts))
		parts = nil
	}
	for _, rawBlock := range blocks {
		blockType, err := blockType(rawBlock)
		if err != nil {
			return nil, err
		}
		switch blockType {
		case "text":
			text, err := textBlock(rawBlock)
			if err != nil {
				return nil, err
			}
			parts = append(parts, Content{Type: "output_text", Text: text})
		case "tool_use":
			flush()
			item, err := toolUseInputItem(rawBlock, state)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		case "thinking", "redacted_thinking":
			continue
		default:
			return nil, fmt.Errorf("unsupported assistant content block type %q", blockType)
		}
	}
	flush()
	return items, nil
}

func messageInput(role string, parts []Content) InputItem {
	return InputItem{Type: "message", Role: role, Content: parts}
}
