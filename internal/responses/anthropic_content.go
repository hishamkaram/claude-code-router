package responses

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

func toolUseInputItem(raw json.RawMessage, state *convertState) (InputItem, error) {
	var block struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return InputItem{}, fmt.Errorf("decode tool_use block: %w", err)
	}
	block.ID = strings.TrimSpace(block.ID)
	block.Name = strings.TrimSpace(block.Name)
	if block.ID == "" || block.Name == "" {
		return InputItem{}, fmt.Errorf("tool_use block requires id and name")
	}
	if strings.EqualFold(block.Name, "computer") && state.hasNativeComputer && !state.functionToolNames["computer"] {
		state.computerCallIDs[block.ID] = true
		actions, err := openAIComputerActionsFromAnthropic(block.Input)
		if err != nil {
			return InputItem{}, err
		}
		return InputItem{
			Type:    "computer_call",
			CallID:  block.ID,
			Status:  "completed",
			Actions: actions,
		}, nil
	}
	if len(block.Input) == 0 || string(block.Input) == "null" {
		block.Input = json.RawMessage(`{}`)
	}
	arguments, err := canonicalJSON(block.Input)
	if err != nil {
		return InputItem{}, fmt.Errorf("encode tool_use input for %q: %w", block.Name, err)
	}
	return InputItem{
		Type:      "function_call",
		CallID:    block.ID,
		Name:      block.Name,
		Arguments: string(arguments),
	}, nil
}

func toolResultInputItems(raw json.RawMessage, state *convertState) ([]InputItem, error) {
	var block struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("decode tool_result block: %w", err)
	}
	block.ToolUseID = strings.TrimSpace(block.ToolUseID)
	if block.ToolUseID == "" {
		return nil, fmt.Errorf("tool_result block missing tool_use_id")
	}
	output, isImage, err := toolResultOutput(block.Content)
	if err != nil {
		return nil, err
	}
	if isImage {
		if !state.computerCallIDs[block.ToolUseID] {
			return nil, fmt.Errorf("%w: image tool_result %q needs a computer tool", ErrNativeCUARequired, block.ToolUseID)
		}
		return []InputItem{{
			Type:   "computer_call_output",
			CallID: block.ToolUseID,
			Output: ComputerScreenshot{
				Type:     "computer_screenshot",
				ImageURL: output,
				Detail:   "original",
			},
		}}, nil
	}
	return []InputItem{{Type: "function_call_output", CallID: block.ToolUseID, Output: output}}, nil
}

func toolResultOutput(raw json.RawMessage) (output string, isImage bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	if text, ok, stringErr := rawString(raw); ok || stringErr != nil {
		return text, false, stringErr
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return "", false, fmt.Errorf("tool_result content must be a string or array: %w", err)
	}
	return toolResultOutputFromBlocks(blocks)
}

func toolResultOutputFromBlocks(blocks []json.RawMessage) (output string, isImage bool, resultErr error) {
	texts := make([]string, 0, len(blocks))
	var imageURL string
	for _, rawBlock := range blocks {
		text, image, isImage, err := toolResultOutputBlock(rawBlock)
		if err != nil {
			return "", false, err
		}
		if isImage {
			if imageURL != "" || len(texts) > 0 {
				return "", false, fmt.Errorf("tool_result content must not mix images with other blocks")
			}
			imageURL = image
			continue
		}
		if imageURL != "" {
			return "", false, fmt.Errorf("tool_result content must not mix images with other blocks")
		}
		texts = append(texts, text)
	}
	if imageURL != "" {
		return imageURL, true, nil
	}
	return strings.Join(texts, "\n"), false, nil
}

func toolResultOutputBlock(raw json.RawMessage) (text, image string, isImage bool, err error) {
	blockType, err := blockType(raw)
	if err != nil {
		return "", "", false, err
	}
	switch blockType {
	case "text":
		text, err := textBlock(raw)
		return text, "", false, err
	case "tool_reference":
		text, err := toolReferenceOutputText(raw)
		return text, "", false, err
	case "image":
		image, err := imageURLFromBlock(raw)
		return "", image, true, err
	case "document":
		return "", "", false, fmt.Errorf("%w: document blocks are not supported", ErrUnsupportedPDF)
	case "audio":
		return "", "", false, fmt.Errorf("%w: audio blocks are not supported", ErrUnsupportedAudio)
	default:
		return "", "", false, fmt.Errorf("unsupported tool_result content block type %q", blockType)
	}
}

func toolReferenceOutputText(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", fmt.Errorf("decode tool_reference block")
	}
	for key := range fields {
		if key != "type" && key != "tool_name" {
			return "", fmt.Errorf("tool_reference block field %q is not supported", key)
		}
	}
	nameRaw, ok := fields["tool_name"]
	if !ok {
		return "", fmt.Errorf("tool_reference block tool_name must be a string")
	}
	var name string
	if err := json.Unmarshal(nameRaw, &name); err != nil {
		return "", fmt.Errorf("tool_reference block tool_name must be a string")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("tool_reference block tool_name is required")
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("tool_reference block tool_name must not contain control characters")
	}
	return "[Loaded tool: " + name + "]", nil
}

func textFromAnthropicContent(raw json.RawMessage) (string, error) {
	if text, ok, err := rawString(raw); ok || err != nil {
		return text, err
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		blockType, err := blockType(rawBlock)
		if err != nil {
			return "", err
		}
		switch blockType {
		case "text":
			text, err := textBlock(rawBlock)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		case "document":
			return "", fmt.Errorf("%w: document blocks are not supported", ErrUnsupportedPDF)
		case "audio":
			return "", fmt.Errorf("%w: audio blocks are not supported", ErrUnsupportedAudio)
		default:
			return "", fmt.Errorf("system content block type %q is not supported", blockType)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func textBlock(raw json.RawMessage) (string, error) {
	var block struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", fmt.Errorf("decode text block: %w", err)
	}
	return block.Text, nil
}

func imageURLFromBlock(raw json.RawMessage) (string, error) {
	var block struct {
		Source json.RawMessage `json:"source"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", fmt.Errorf("decode image block: %w", err)
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(block.Source, &source); err != nil {
		return "", fmt.Errorf("decode image source: %w", err)
	}
	if strings.EqualFold(source.MediaType, "application/pdf") {
		return "", fmt.Errorf("%w: image source media_type is application/pdf", ErrUnsupportedPDF)
	}
	if strings.HasPrefix(strings.ToLower(source.MediaType), "audio/") {
		return "", fmt.Errorf("%w: image source media_type is audio", ErrUnsupportedAudio)
	}
	switch strings.ToLower(strings.TrimSpace(source.Type)) {
	case "base64":
		if !strings.HasPrefix(strings.ToLower(source.MediaType), "image/") {
			return "", fmt.Errorf("base64 image source media_type %q is not supported", source.MediaType)
		}
		if strings.TrimSpace(source.Data) == "" {
			return "", fmt.Errorf("base64 image source data is required")
		}
		return "data:" + source.MediaType + ";base64," + source.Data, nil
	case "url":
		parsed, err := url.Parse(source.URL)
		if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
			return "", fmt.Errorf("image URL source must be an absolute HTTPS URL")
		}
		return source.URL, nil
	default:
		return "", fmt.Errorf("image source type %q is not supported", source.Type)
	}
}

func blockType(raw json.RawMessage) (string, error) {
	var block struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", fmt.Errorf("decode content block: %w", err)
	}
	block.Type = strings.TrimSpace(block.Type)
	if block.Type == "" {
		return "", fmt.Errorf("content block missing type")
	}
	return block.Type, nil
}

func rawString(raw json.RawMessage) (value string, isString bool, resultErr error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true, nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return json.Marshal(value)
}
