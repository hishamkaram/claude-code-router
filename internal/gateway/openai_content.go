package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
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
	var toolMessages []openAIMessage
	parts := make([]openAIContentPart, 0, len(blocks))
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
			parts = append(parts, openAIContentPart{Type: "text", Text: text})
		case "image":
			imagePart, err := openAIImagePartFromAnthropic(block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, imagePart)
		case "tool_result":
			toolMessage, images, err := openAIToolResultMessage(block)
			if err != nil {
				return nil, err
			}
			toolMessages = append(toolMessages, toolMessage)
			parts = append(parts, images...)
		default:
			return nil, fmt.Errorf("user content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	// OpenAI requires every tool response to directly follow the assistant
	// tool_calls message, contiguous and before any other role. Emit all tool
	// messages first, then a single user message carrying the turn's remaining
	// content: top-level text/images plus any images extracted from tool_result
	// blocks (which the tool role cannot carry). This keeps tool responses
	// contiguous even when a user turn mixes parallel tool_results with images.
	messages := toolMessages
	if len(parts) > 0 {
		messages = append(messages, userMessageFromParts(parts))
	}
	return messages, nil
}

// openAIToolResultMessage converts an Anthropic tool_result block into an OpenAI
// tool-role message plus any image content parts. The tool role cannot carry
// image_url content on the OpenAI-compatible path, so images are returned
// separately for the caller to place in the trailing user message (after every
// tool message, preserving tool-response contiguity). When a tool_result
// carries only images, a short placeholder keeps the tool content non-empty for
// backends that reject empty tool messages.
func openAIToolResultMessage(block map[string]any) (openAIMessage, []openAIContentPart, error) {
	toolCallID, _ := block["tool_use_id"].(string)
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return openAIMessage{}, nil, fmt.Errorf("tool_result block missing tool_use_id")
	}
	textContent, images, err := splitToolResultImages(block["content"])
	if err != nil {
		return openAIMessage{}, nil, err
	}
	text, err := anthropicToolResultText(textContent)
	if err != nil {
		return openAIMessage{}, nil, err
	}
	if text == "" && len(images) > 0 {
		text = "[image output]"
	}
	return openAIMessage{Role: "tool", ToolCallID: toolCallID, Content: text}, images, nil
}

// splitToolResultImages separates image blocks from the rest of a tool_result
// content value. Non-array content (string, nil, object) has no images and is
// returned unchanged. Non-image blocks are left untouched so the existing text
// path validates and renders them exactly as before.
func splitToolResultImages(value any) (any, []openAIContentPart, error) {
	blocks, ok := value.([]any)
	if !ok {
		return value, nil, nil
	}
	textBlocks := make([]any, 0, len(blocks))
	images := make([]openAIContentPart, 0)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			textBlocks = append(textBlocks, item)
			continue
		}
		blockType, _ := block["type"].(string)
		if strings.EqualFold(strings.TrimSpace(blockType), "image") {
			image, err := openAIImagePartFromAnthropic(block)
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

// userMessageFromParts collapses a buffered run of user content parts into one
// OpenAI user message. A run without images keeps the plain-string content form
// so text-only requests are byte-for-byte unchanged; any image forces the
// multipart representation.
func userMessageFromParts(parts []openAIContentPart) openAIMessage {
	hasNonText := slices.ContainsFunc(parts, func(part openAIContentPart) bool {
		return part.Type != "text"
	})
	if !hasNonText {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			texts = append(texts, part.Text)
		}
		return openAIMessage{Role: "user", Content: strings.Join(texts, "\n")}
	}
	// Multipart form: drop empty text parts. An empty text part serializes as
	// {"type":"text"} without the required text field (Text has omitempty) and
	// strict OpenAI-compatible providers reject it.
	kept := make([]openAIContentPart, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" && part.Text == "" {
			continue
		}
		kept = append(kept, part)
	}
	return openAIMessage{Role: "user", Parts: kept}
}

// openAIImagePartFromAnthropic converts an Anthropic image block into an OpenAI
// image_url content part. Base64 sources become inline data URLs; url sources
// pass through directly.
func openAIImagePartFromAnthropic(block map[string]any) (openAIContentPart, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return openAIContentPart{}, fmt.Errorf("image block requires a source object")
	}
	sourceType, _ := source["type"].(string)
	switch strings.TrimSpace(sourceType) {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		mediaType = strings.TrimSpace(mediaType)
		data = strings.TrimSpace(data)
		if mediaType == "" || data == "" {
			return openAIContentPart{}, fmt.Errorf("base64 image source requires media_type and data")
		}
		url := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
		return openAIContentPart{Type: "image_url", ImageURL: &openAIImageURL{URL: url}}, nil
	case "url":
		url, _ := source["url"].(string)
		url = strings.TrimSpace(url)
		if url == "" {
			return openAIContentPart{}, fmt.Errorf("url image source requires a url")
		}
		return openAIContentPart{Type: "image_url", ImageURL: &openAIImageURL{URL: url}}, nil
	default:
		return openAIContentPart{}, fmt.Errorf("image source type %q is not supported by the OpenAI-compatible gateway path", sourceType)
	}
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
