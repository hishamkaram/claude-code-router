package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

type openAIMessageConversion struct {
	messages      []openAIMessage
	ignoredFields []string
}

func openAIMessagesFromAnthropicWithResolver(ctx context.Context, message anthropicMessage, resolver imageSourceResolver) (openAIMessageConversion, error) {
	switch message.Role {
	case "user":
		messages, err := openAIUserMessagesFromAnthropicWithResolver(ctx, message.Content, resolver)
		if err != nil {
			return openAIMessageConversion{}, err
		}
		return openAIMessageConversion{messages: messages}, nil
	case "assistant":
		return openAIAssistantMessagesFromAnthropic(message.Content)
	case "system":
		messages, err := openAISystemMessagesFromAnthropic(message.Content)
		if err != nil {
			return openAIMessageConversion{}, err
		}
		return openAIMessageConversion{messages: messages}, nil
	default:
		return openAIMessageConversion{}, fmt.Errorf("unsupported message role %q", message.Role)
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

func openAIUserMessagesFromAnthropicWithResolver(ctx context.Context, content any, resolver imageSourceResolver) ([]openAIMessage, error) {
	if text, ok := content.(string); ok {
		return []openAIMessage{{Role: "user", Content: text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported user message content type %T", content)
	}
	return openAIUserBlockMessages(ctx, blocks, resolver)
}

type openAIUserBlockConversion struct {
	part        any
	userParts   []any
	toolMessage *openAIMessage
}

func openAIUserBlockMessages(ctx context.Context, blocks []any, resolver imageSourceResolver) ([]openAIMessage, error) {
	toolMessages := make([]openAIMessage, 0)
	parts := make([]any, 0, len(blocks))
	for _, item := range blocks {
		converted, err := openAIUserBlock(ctx, item, resolver)
		if err != nil {
			return nil, err
		}
		if converted.toolMessage != nil {
			toolMessages = append(toolMessages, *converted.toolMessage)
		}
		if converted.part != nil {
			parts = append(parts, converted.part)
		}
		parts = append(parts, converted.userParts...)
	}

	// OpenAI requires every tool response to directly follow the assistant
	// tool_calls message, contiguous and before any other role. Emit all tool
	// messages first, then one user message carrying the turn's remaining
	// top-level text/images.
	messages := append([]openAIMessage(nil), toolMessages...)
	if len(parts) > 0 {
		messages = append(messages, openAIMessage{Role: "user", Content: openAIContentFromParts(parts)})
	}
	return messages, nil
}

func openAIUserBlock(ctx context.Context, item any, resolver imageSourceResolver) (openAIUserBlockConversion, error) {
	block, ok := item.(map[string]any)
	if !ok {
		return openAIUserBlockConversion{}, fmt.Errorf("user content block is not an object")
	}
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		text, err := anthropicTextBlockText(block)
		if err != nil {
			return openAIUserBlockConversion{}, fmt.Errorf("unsupported user text block: %w", err)
		}
		return openAIUserBlockConversion{part: map[string]any{"type": "text", "text": text}}, nil
	case "image":
		image, err := resolver(ctx, block)
		if err != nil {
			return openAIUserBlockConversion{}, err
		}
		return openAIUserBlockConversion{part: image}, nil
	case "tool_result":
		return openAIToolResultMessage(ctx, block, resolver)
	default:
		return openAIUserBlockConversion{}, fmt.Errorf("user content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
	}
}

func openAIToolResultMessage(ctx context.Context, block map[string]any, resolver imageSourceResolver) (openAIUserBlockConversion, error) {
	toolCallID, _ := block["tool_use_id"].(string)
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return openAIUserBlockConversion{}, fmt.Errorf("tool_result block missing tool_use_id")
	}
	textContent, imageParts, err := splitOpenAIToolResultImages(ctx, block["content"], resolver)
	if err != nil {
		return openAIUserBlockConversion{}, err
	}
	resultContent, err := openAIToolResultContent(textContent)
	if err != nil {
		return openAIUserBlockConversion{}, err
	}
	if len(imageParts) > 0 {
		if text, ok := resultContent.(string); ok && text == "" {
			resultContent = "[image output]"
		}
	}
	message := openAIMessage{Role: "tool", ToolCallID: toolCallID, Content: resultContent}
	return openAIUserBlockConversion{toolMessage: &message, userParts: imageParts}, nil
}

// splitOpenAIToolResultImages keeps image_url parts out of the OpenAI tool
// message, which many Chat Completions providers require to remain text-only.
// The extracted images are emitted in the trailing user message after all tool
// messages so parallel tool results remain contiguous after assistant calls.
func splitOpenAIToolResultImages(ctx context.Context, value any, resolver imageSourceResolver) (remainingContent any, imageParts []any, err error) {
	blocks, ok := value.([]any)
	if !ok {
		return value, nil, nil
	}
	textBlocks := make([]any, 0, len(blocks))
	images := make([]any, 0)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			textBlocks = append(textBlocks, item)
			continue
		}
		blockType, _ := block["type"].(string)
		if strings.EqualFold(strings.TrimSpace(blockType), "image") {
			image, err := resolver(ctx, block)
			if err != nil {
				return nil, nil, err
			}
			images = append(images, image)
			continue
		}
		textBlocks = append(textBlocks, item)
	}
	return textBlocks, images, nil
}

func openAIContentFromParts(parts []any) any {
	text := make([]string, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok || part["type"] != "text" {
			return openAIMultipartContent(parts)
		}
		value, ok := part["text"].(string)
		if !ok {
			return openAIMultipartContent(parts)
		}
		text = append(text, value)
	}
	return strings.Join(text, "\n")
}

func openAIMultipartContent(parts []any) []any {
	kept := make([]any, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if ok && part["type"] == "text" && part["text"] == "" {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func openAIToolResultContent(value any) (any, error) {
	if value == nil {
		return "", nil
	}
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		return openAIToolResultParts(content)
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return nil, fmt.Errorf("encoding tool_result content: %w", err)
		}
		return string(encoded), nil
	}
}

func openAIToolResultParts(content []any) (any, error) {
	parts := make([]any, 0, len(content))
	for _, item := range content {
		part, err := openAIToolResultPart(item)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return openAIContentFromParts(parts), nil
}

func openAIToolResultPart(item any) (any, error) {
	block, ok := item.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool_result content block is not an object")
	}
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		return openAIToolResultTextPart(block)
	case "tool_reference":
		return openAIToolReferencePart(block)
	default:
		return nil, fmt.Errorf("tool_result content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
	}
}

func openAIToolResultTextPart(block map[string]any) (any, error) {
	text, err := anthropicTextBlockText(block)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "text", "text": text}, nil
}

func openAIToolReferencePart(block map[string]any) (any, error) {
	text, err := anthropicToolReferenceBlockText(block)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "text", "text": text}, nil
}

func openAIAssistantMessagesFromAnthropic(content any) (openAIMessageConversion, error) {
	if text, ok := content.(string); ok {
		return openAIMessageConversion{messages: []openAIMessage{{Role: "assistant", Content: text}}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return openAIMessageConversion{}, fmt.Errorf("unsupported assistant message content type %T", content)
	}
	var textParts []string
	var toolCalls []openAIToolCall
	var ignoredFields []string
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return openAIMessageConversion{}, fmt.Errorf("assistant content block is not an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, citationsPresent, err := anthropicAssistantTextBlockText(block)
			if err != nil {
				return openAIMessageConversion{}, fmt.Errorf("unsupported assistant text block: %w", err)
			}
			textParts = append(textParts, text)
			if citationsPresent {
				ignoredFields = appendIgnoredAnthropicField(ignoredFields, ccrIgnoredAssistantCitationsField)
			}
		case "tool_use":
			toolCall, err := openAIToolCallFromAnthropic(block)
			if err != nil {
				return openAIMessageConversion{}, err
			}
			toolCalls = append(toolCalls, toolCall)
		case "thinking", "redacted_thinking":
			continue
		default:
			return openAIMessageConversion{}, fmt.Errorf("assistant content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	if len(textParts) == 0 && len(toolCalls) == 0 {
		return openAIMessageConversion{ignoredFields: ignoredFields}, nil
	}
	return openAIMessageConversion{
		messages:      []openAIMessage{{Role: "assistant", Content: strings.Join(textParts, "\n"), ToolCalls: toolCalls}},
		ignoredFields: ignoredFields,
	}, nil
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
	text, _, err := anthropicTextBlockTextWithCitations(block, false)
	return text, err
}

func anthropicAssistantTextBlockText(block map[string]any) (text string, citationsPresent bool, err error) {
	return anthropicTextBlockTextWithCitations(block, true)
}

func anthropicTextBlockTextWithCitations(block map[string]any, allowCitations bool) (text string, citationsPresent bool, err error) {
	for key := range block {
		switch key {
		case "type", "text", "cache_control":
		case "citations":
			if !allowCitations {
				return "", false, fmt.Errorf("content block field %q is not supported", key)
			}
			citationsPresent = true
		default:
			return "", false, fmt.Errorf("content block field %q is not supported", key)
		}
	}
	blockType, _ := block["type"].(string)
	if blockType != "text" {
		return "", false, fmt.Errorf("content block type %q is not supported", blockType)
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", false, fmt.Errorf("content block text must be a string")
	}
	return text, citationsPresent, nil
}
