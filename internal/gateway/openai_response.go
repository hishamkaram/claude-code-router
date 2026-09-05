package gateway

import (
	"fmt"
	"strings"
)

func toAnthropicResponse(alias, messageID string, resp openAIChatResponse, finishReason string) map[string]any {
	blocks, stopReason := anthropicContentBlocksFromOpenAI(resp, finishReason)
	return map[string]any{
		"id":            messageID,
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
