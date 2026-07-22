package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func writeAnthropicStream(w http.ResponseWriter, alias string, resp openAIChatResponse, finishReason string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	blocks, finishReason := anthropicContentBlocksFromOpenAI(resp, finishReason)
	id := firstNonEmpty(resp.ID, "msg_ccr")
	writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         alias,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  resp.Usage.PromptTokens,
				"output_tokens": 0,
			},
		},
	})
	for index, block := range blocks {
		writeSSEEvent(w, flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": streamStartBlock(block),
		})
		if delta, ok := streamBlockDelta(block); ok {
			writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": delta,
			})
		}
		writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})
	}
	writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   finishReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": resp.Usage.CompletionTokens},
	})
	writeSSEEvent(w, flusher, "message_stop", map[string]string{"type": "message_stop"})
}

func writeSSEEvent(w io.Writer, flusher http.Flusher, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

func toAnthropicResponse(alias string, resp openAIChatResponse, finishReason string) map[string]any {
	blocks, stopReason := anthropicContentBlocksFromOpenAI(resp, finishReason)
	return map[string]any{
		"id":            firstNonEmpty(resp.ID, "msg_ccr"),
		"type":          "message",
		"role":          "assistant",
		"model":         alias,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
}

func anthropicContentBlocksFromOpenAI(resp openAIChatResponse, finishReason string) (blocks []map[string]any, stopReason string) {
	if len(resp.Choices) == 0 {
		return []map[string]any{{"type": "text", "text": ""}}, finishReason
	}
	message := resp.Choices[0].Message
	toolCalls := message.ToolCalls
	if len(toolCalls) == 0 && message.FunctionCall != nil {
		toolCalls = []openAIToolCall{{
			ID:       "toolu_ccr_function_call",
			Type:     "function",
			Function: *message.FunctionCall,
		}}
	}
	blocks = make([]map[string]any, 0, 1+len(toolCalls))
	textContent := message.textContent()
	if textContent != "" || len(toolCalls) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": textContent})
	}
	for _, toolCall := range toolCalls {
		input := openAIToolArgumentsForTool(toolCall.Function.Name, toolCall.Function.Arguments)
		if message := invalidAgentToolInputMessage(toolCall.Function.Name, input); message != "" {
			return []map[string]any{{"type": "text", "text": message}}, "end_turn"
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(toolCall.ID, "toolu_ccr"),
			"name":  toolCall.Function.Name,
			"input": input,
		})
	}
	return blocks, finishReason
}

func invalidAgentToolInputMessage(toolName string, input any) string {
	if !strings.EqualFold(strings.TrimSpace(toolName), "Agent") {
		return ""
	}
	fields, ok := input.(map[string]any)
	if !ok {
		return "CCR provider compatibility error: external provider returned invalid Agent tool input. The subagent was not started."
	}
	missing := make([]string, 0, 2)
	if trimmedStringField(fields, "prompt") == "" {
		missing = append(missing, "prompt")
	}
	if trimmedStringField(fields, "description") == "" {
		missing = append(missing, "description")
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid Agent tool input (missing required %s). The subagent was not started.", strings.Join(missing, " and "))
}

func streamStartBlock(block map[string]any) map[string]any {
	if block["type"] == "text" {
		return map[string]any{"type": "text", "text": ""}
	}
	if block["type"] == "tool_use" {
		start := make(map[string]any, len(block))
		for key, value := range block {
			if key != "input" {
				start[key] = value
			}
		}
		start["input"] = map[string]any{}
		return start
	}
	return block
}

func streamBlockDelta(block map[string]any) (map[string]string, bool) {
	if text, ok := block["text"].(string); ok && text != "" {
		return map[string]string{"type": "text_delta", "text": text}, true
	}
	if block["type"] != "tool_use" {
		return nil, false
	}
	input := firstNonNil(block["input"], map[string]any{})
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, false
	}
	return map[string]string{"type": "input_json_delta", "partial_json": string(encoded)}, true
}
