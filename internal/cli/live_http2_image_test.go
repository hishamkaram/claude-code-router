//go:build live

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"testing"
)

// Require the image returned by the called fixture tool, not an input image or
// a final success marker that the model can emit after a failed tool call.
func hasHTTP2ImageToolResult(raw []byte) bool {
	calls := make(map[string]bool)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					ToolUseID string          `json:"tool_use_id"`
					IsError   bool            `json:"is_error"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		for _, block := range event.Message.Content {
			if event.Type == "assistant" && block.Type == "tool_use" && block.ID != "" && hasHTTP2Tool([]string{block.Name}, "image") {
				calls[block.ID] = true
			}
			if event.Type != "user" || block.Type != "tool_result" || block.IsError || !calls[block.ToolUseID] {
				continue
			}
			var content []struct {
				Type   string `json:"type"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			}
			if json.Unmarshal(block.Content, &content) != nil {
				continue
			}
			for _, image := range content {
				if image.Type == "image" && image.Source.Type == "base64" && image.Source.MediaType == "image/png" && validHTTP2ReturnedPNG(image.Source.Data) {
					return true
				}
			}
		}
	}
	return false
}

func validHTTP2ReturnedPNG(data string) bool {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return false
	}
	image, err := png.Decode(bytes.NewReader(raw))
	return err == nil && !image.Bounds().Empty()
}

func TestHTTP2ImageAcceptanceRequiresSuccessfulReturnedImage(t *testing.T) {
	call := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"image-1","name":"mcp__fixture__image"}]}}` + "\n"
	result := `{"type":"result","result":"CCR_HTTP2_BINARY_OK","is_error":false}` + "\n"
	image := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + http2ImageData(t) + `"}}]`
	for _, tc := range []struct {
		name, content, id string
		failed, accepted  bool
	}{
		{name: "failed tool", content: `"tool failed"`, id: "image-1", failed: true},
		{name: "text only", content: `[{"type":"text","text":"no image"}]`, id: "image-1"},
		{name: "unrelated image", content: image, id: "other"},
		{name: "invalid image", content: `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"fixture"}}]`, id: "image-1"},
		{name: "returned image", content: image, id: "image-1", accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "tool_use_id": tc.id, "is_error": tc.failed, "content": json.RawMessage(tc.content)}}}})
			if err != nil {
				t.Fatal(err)
			}
			output := []byte(call + string(raw) + "\n" + result)
			if hasHTTP2ImageToolResult(output) != tc.accepted {
				t.Fatal("incorrect image acceptance")
			}
			_, _, _, failed := inspectHTTP2BinaryOutput(output)
			if failed != tc.failed {
				t.Fatal("tool-result error was not preserved")
			}
		})
	}
}
