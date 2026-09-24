package gateway

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/agentinput"
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
		if isAgentChildToolName(toolCall.Function.Name) {
			if fields, ok := input.(map[string]any); ok {
				// Normalize at the response boundary as well as while decoding the
				// provider arguments. This keeps the pass-through payload valid even
				// when a future decoder returns the raw object directly.
				input = agentinput.Normalize(fields)
			}
		}
		if message := invalidChildToolInputMessage(toolCall.Function.Name, input); message != "" {
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
	if !isAgentChildToolName(toolName) {
		return ""
	}
	fields, ok := input.(map[string]any)
	if !ok {
		return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid %s tool input. The subagent was not started.", childToolDisplayName(toolName))
	}
	fields = agentinput.Normalize(fields)
	missing := make([]string, 0, 2)
	if trimmedStringField(fields, "prompt") == "" {
		missing = append(missing, "prompt")
	}
	if trimmedStringField(fields, "description") == "" {
		missing = append(missing, "description")
	}
	if len(missing) == 0 {
		if len(agentChildDescriptorsFromToolInput(toolName, fields)) > 0 {
			return ""
		}
		return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid %s tool input. The subagent was not started.", childToolDisplayName(toolName))
	}
	return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid %s tool input (missing required %s). The subagent was not started.", childToolDisplayName(toolName), strings.Join(missing, " and "))
}

func childToolDisplayName(toolName string) string {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "task":
		return "Task"
	default:
		return "Agent"
	}
}
