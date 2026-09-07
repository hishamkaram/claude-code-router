package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Text is a pointer so empty input_text blocks retain their required text field.
type functionOutputContent struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

func functionToolResultOutput(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if text, ok, err := rawString(raw); ok || err != nil {
		return text, err
	}
	blocks, err := rawArray(raw)
	if err != nil {
		return nil, fmt.Errorf("tool_result content must be a string or array: %w", err)
	}
	content := make([]functionOutputContent, 0, len(blocks))
	texts := make([]string, 0, len(blocks))
	hasImage := false
	for _, block := range blocks {
		text, image, isImage, err := toolResultOutputBlock(block)
		if err != nil {
			return nil, err
		}
		if isImage {
			hasImage = true
			content = append(content, functionOutputContent{Type: "input_image", ImageURL: image, Detail: "auto"})
		} else {
			texts = append(texts, text)
			content = append(content, functionOutputContent{Type: "input_text", Text: &text})
		}
	}
	if hasImage {
		return content, nil
	}
	return strings.Join(texts, "\n"), nil
}
