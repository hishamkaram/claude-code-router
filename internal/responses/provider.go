package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/agentinput"
)

const fallbackMessageID = "msg_ccr_responses"

// AnthropicResponseFromResponsesJSON decodes a non-stream Responses payload and
// converts supported output items to an Anthropic-compatible message.
func AnthropicResponseFromResponsesJSON(raw []byte) (*AnthropicResponse, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode Responses payload: %w", ErrMalformedProviderOutput, err)
	}
	return AnthropicResponseFromResponses(&resp)
}

// AnthropicResponseFromResponses converts a typed non-stream Responses payload
// to an Anthropic-compatible assistant message.
func AnthropicResponseFromResponses(resp *Response) (*AnthropicResponse, error) {
	if err := ValidateStatus(resp); err != nil {
		return nil, err
	}
	blocks := make([]AnthropicContentBlock, 0, len(resp.Output))
	hasToolUse := false
	for index := range resp.Output {
		converted, toolUse, err := anthropicBlocksFromOutputItem(resp.Output[index])
		if err != nil {
			return nil, err
		}
		if toolUse {
			hasToolUse = true
		}
		blocks = append(blocks, converted...)
	}
	if len(blocks) == 0 && resp.OutputText != "" {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: resp.OutputText})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: ""})
	}
	return &AnthropicResponse{
		ID:           firstNonEmpty(resp.ID, fallbackMessageID),
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		Content:      blocks,
		StopReason:   anthropicStopReason(resp, hasToolUse),
		StopSequence: nil,
		Usage:        resp.Usage,
	}, nil
}

// ValidateStatus rejects unsuccessful Responses statuses before callers consume output items.
func ValidateStatus(resp *Response) error {
	if resp == nil {
		return fmt.Errorf("%w: nil Responses payload", ErrMalformedProviderOutput)
	}
	return validateResponsesStatus(resp)
}

func validateResponsesStatus(resp *Response) error {
	status := strings.ToLower(strings.TrimSpace(resp.Status))
	switch status {
	case "", "completed":
		return nil
	case "incomplete":
		reason := ""
		if resp.IncompleteDetails != nil {
			reason = strings.ToLower(strings.TrimSpace(resp.IncompleteDetails.Reason))
		}
		if reason == "max_output_tokens" {
			return nil
		}
		if reason == "" {
			reason = "unknown"
		}
		return fmt.Errorf("%w: OpenAI Responses provider returned incomplete status with reason %q", ErrUnsuccessfulProviderStatus, reason)
	default:
		return fmt.Errorf("%w: OpenAI Responses provider returned status %q", ErrUnsuccessfulProviderStatus, status)
	}
}

func anthropicStopReason(resp *Response, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	if strings.EqualFold(strings.TrimSpace(resp.Status), "incomplete") &&
		resp.IncompleteDetails != nil &&
		strings.EqualFold(strings.TrimSpace(resp.IncompleteDetails.Reason), "max_output_tokens") {
		return "max_tokens"
	}
	return "end_turn"
}

func anthropicBlocksFromOutputItem(item OutputItem) ([]AnthropicContentBlock, bool, error) {
	switch item.Type {
	case "message":
		blocks, err := textBlocksFromResponseMessage(item)
		return blocks, false, err
	case "function_call":
		block, toolUse, err := functionToolUseBlock(item)
		return []AnthropicContentBlock{block}, toolUse, err
	case "computer_call":
		return nil, false, fmt.Errorf("%w: OpenAI Responses computer_call requires a CCR managed CUA executor", ErrMalformedProviderOutput)
	case "reasoning", "function_call_output", "computer_call_output", "tool_search_call", "tool_search_output":
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("%w: unsupported Responses output item type %q", ErrMalformedProviderOutput, item.Type)
	}
}

func textBlocksFromResponseMessage(item OutputItem) ([]AnthropicContentBlock, error) {
	if item.Role != "" && item.Role != "assistant" {
		return nil, fmt.Errorf("%w: output message role %q is not assistant", ErrMalformedProviderOutput, item.Role)
	}
	blocks := make([]AnthropicContentBlock, 0, len(item.Content))
	for _, part := range item.Content {
		switch part.Type {
		case "output_text", "text":
			blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: part.Text})
		case "refusal":
			if part.Refusal != "" {
				blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: part.Refusal})
			}
		default:
			return nil, fmt.Errorf("%w: unsupported message content type %q", ErrMalformedProviderOutput, part.Type)
		}
	}
	return blocks, nil
}

func functionToolUseBlock(item OutputItem) (AnthropicContentBlock, bool, error) {
	if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
		return AnthropicContentBlock{}, false, fmt.Errorf("%w: function_call missing call_id or name", ErrMalformedProviderOutput)
	}
	input := map[string]any{}
	if strings.TrimSpace(item.Arguments) != "" {
		var decoded any
		if err := json.Unmarshal([]byte(item.Arguments), &decoded); err != nil {
			return AnthropicContentBlock{}, false, fmt.Errorf("%w: function_call %q arguments are not valid JSON: %w", ErrMalformedProviderOutput, item.CallID, err)
		}
		switch value := decoded.(type) {
		case map[string]any:
			input = value
		case nil:
			// Anthropic tool input must be an object. JSON null is an empty object.
		default:
			input = map[string]any{"value": value}
		}
	}
	if strings.EqualFold(strings.TrimSpace(item.Name), "Agent") {
		input = agentinput.Normalize(input)
		if message := invalidAgentToolInputMessage(input); message != "" {
			return AnthropicContentBlock{Type: "text", Text: message}, false, nil
		}
	}
	return AnthropicContentBlock{
		Type:  "tool_use",
		ID:    item.CallID,
		Name:  item.Name,
		Input: input,
	}, true, nil
}

func invalidAgentToolInputMessage(input map[string]any) string {
	missing := make([]string, 0, 2)
	if trimmedStringField(input, "prompt") == "" {
		missing = append(missing, "prompt")
	}
	if trimmedStringField(input, "description") == "" {
		missing = append(missing, "description")
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid Agent tool input (missing required %s). The subagent was not started.", strings.Join(missing, " and "))
}

func trimmedStringField(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
