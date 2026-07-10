package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

func openAIMessagesFromAnthropic(message anthropicMessage) ([]openAIMessage, error) {
	switch message.Role {
	case "user":
		return openAIUserMessagesFromAnthropic(message.Content)
	case "assistant":
		return openAIAssistantMessagesFromAnthropic(message.Content)
	case "system":
		return openAISystemMessagesFromAnthropic(message.Content)
	default:
		return nil, fmt.Errorf("unsupported message role %q", message.Role)
	}
}

func openAISystemMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	text, err := anthropicContentText(content)
	if err != nil {
		return nil, fmt.Errorf("unsupported system message content: %w", err)
	}
	if text == "" {
		return nil, nil
	}
	return []openAIMessage{{Role: "system", Content: text}}, nil
}

func openAIUserMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	if text, ok := content.(string); ok {
		return []openAIMessage{{Role: "user", Content: text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported user message content type %T", content)
	}
	messages := make([]openAIMessage, 0, len(blocks))
	textParts := make([]string, 0)
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		messages = append(messages, openAIMessage{Role: "user", Content: strings.Join(textParts, "\n")})
		textParts = textParts[:0]
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("user content block is not an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return nil, fmt.Errorf("unsupported user text block: %w", err)
			}
			textParts = append(textParts, text)
		case "tool_result":
			flushText()
			toolCallID, _ := block["tool_use_id"].(string)
			toolCallID = strings.TrimSpace(toolCallID)
			if toolCallID == "" {
				return nil, fmt.Errorf("tool_result block missing tool_use_id")
			}
			text, err := anthropicToolResultText(block["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, openAIMessage{Role: "tool", ToolCallID: toolCallID, Content: text})
		default:
			return nil, fmt.Errorf("user content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	flushText()
	return messages, nil
}

func openAIAssistantMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	if text, ok := content.(string); ok {
		return []openAIMessage{{Role: "assistant", Content: text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported assistant message content type %T", content)
	}
	var textParts []string
	var toolCalls []openAIToolCall
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("assistant content block is not an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return nil, fmt.Errorf("unsupported assistant text block: %w", err)
			}
			textParts = append(textParts, text)
		case "tool_use":
			toolCall, err := openAIToolCallFromAnthropic(block)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, toolCall)
		case "thinking", "redacted_thinking":
			continue
		default:
			return nil, fmt.Errorf("assistant content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	if len(textParts) == 0 && len(toolCalls) == 0 {
		return nil, nil
	}
	return []openAIMessage{{Role: "assistant", Content: strings.Join(textParts, "\n"), ToolCalls: toolCalls}}, nil
}

func openAIToolCallFromAnthropic(block map[string]any) (openAIToolCall, error) {
	id, _ := block["id"].(string)
	name, _ := block["name"].(string)
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return openAIToolCall{}, fmt.Errorf("tool_use block requires id and name")
	}
	arguments, err := json.Marshal(firstNonNil(block["input"], map[string]any{}))
	if err != nil {
		return openAIToolCall{}, fmt.Errorf("encoding tool_use input for %q: %w", name, err)
	}
	return openAIToolCall{
		ID:   id,
		Type: "function",
		Function: openAIFunctionCall{
			Name:      name,
			Arguments: string(arguments),
		},
	}, nil
}

func anthropicToolResultText(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		return anthropicToolResultContentText(content)
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return "", fmt.Errorf("encoding tool_result content: %w", err)
		}
		return string(encoded), nil
	}
}

func anthropicToolResultContentText(content []any) (string, error) {
	parts := make([]string, 0, len(content))
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("tool_result content block is not an object")
		}
		text, err := anthropicToolResultBlockText(block)
		if err != nil {
			return "", err
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n"), nil
}

func anthropicToolResultBlockText(block map[string]any) (string, error) {
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		return anthropicTextBlockText(block)
	case "tool_reference":
		return anthropicToolReferenceBlockText(block)
	default:
		return "", fmt.Errorf("tool_result content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
	}
}

func anthropicToolReferenceBlockText(block map[string]any) (string, error) {
	for key := range block {
		if key != "type" && key != "tool_name" {
			return "", fmt.Errorf("tool_reference block field %q is not supported", key)
		}
	}
	toolName, ok := block["tool_name"].(string)
	if !ok {
		return "", fmt.Errorf("tool_reference block tool_name must be a string")
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "", fmt.Errorf("tool_reference block tool_name is required")
	}
	if strings.IndexFunc(toolName, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("tool_reference block tool_name must not contain control characters")
	}
	return "[Loaded tool: " + toolName + "]", nil
}

func anthropicContentText(value any) (string, error) {
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content block is not an object")
			}
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("content type %T is not supported", value)
	}
}

func anthropicTextBlockText(block map[string]any) (string, error) {
	for key := range block {
		if key != "type" && key != "text" && key != "cache_control" {
			return "", fmt.Errorf("content block field %q is not supported", key)
		}
	}
	blockType, _ := block["type"].(string)
	if blockType != "text" {
		return "", fmt.Errorf("content block type %q is not supported", blockType)
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", fmt.Errorf("content block text must be a string")
	}
	return text, nil
}
