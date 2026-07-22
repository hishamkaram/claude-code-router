package responses

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAnthropicResponseFromResponsesJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want *AnthropicResponse
	}{
		{
			name: "text and function calls",
			raw: `{
  "id":"resp_123",
  "model":"gpt-5.6",
  "output":[
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will check."}]},
    {"type":"function_call","call_id":"call_weather","name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}
  ],
  "usage":{"input_tokens":10,"output_tokens":5}
}`,
			want: &AnthropicResponse{
				ID:    "resp_123",
				Type:  "message",
				Role:  "assistant",
				Model: "gpt-5.6",
				Usage: Usage{InputTokens: 10, OutputTokens: 5},
				Content: []AnthropicContentBlock{
					{Type: "text", Text: "I will check."},
					{
						Type:  "tool_use",
						ID:    "call_weather",
						Name:  "get_weather",
						Input: map[string]any{"city": "Berlin"},
					},
				},
				StopReason: "tool_use",
			},
		},
		{
			name: "output_text fallback",
			raw:  `{"id":"resp_text","model":"gpt","output":[],"output_text":"done","usage":{"input_tokens":1,"output_tokens":2}}`,
			want: &AnthropicResponse{
				ID:         "resp_text",
				Type:       "message",
				Role:       "assistant",
				Model:      "gpt",
				Content:    []AnthropicContentBlock{{Type: "text", Text: "done"}},
				StopReason: "end_turn",
				Usage:      Usage{InputTokens: 1, OutputTokens: 2},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := AnthropicResponseFromResponsesJSON([]byte(test.raw))
			if err != nil {
				t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v", err)
			}
			assertJSONEqual(t, got, test.want)
		})
	}
}

func TestAnthropicResponseFromResponsesRejectsUnmanagedComputerCall(t *testing.T) {
	t.Parallel()

	_, err := AnthropicResponseFromResponsesJSON([]byte(fixture(t, "responses_output_mixed.json")))
	if !errors.Is(err, ErrMalformedProviderOutput) || !strings.Contains(err.Error(), "requires a CCR managed CUA executor") {
		t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v, want managed CUA rejection", err)
	}
}

func TestAnthropicResponseFromResponsesJSONIncludesEmptyTextField(t *testing.T) {
	t.Parallel()

	response, err := AnthropicResponseFromResponsesJSON([]byte(`{"id":"resp_empty","model":"gpt","output":[]}`))
	if err != nil {
		t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal Anthropic response: %v", err)
	}
	var decoded struct {
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal Anthropic response: %v", err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Type != "text" ||
		decoded.Content[0].Text == nil || *decoded.Content[0].Text != "" {
		t.Fatalf("content JSON = %s, want text block with empty text field", encoded)
	}
}

func TestOutputItemNormalizesSingularComputerAction(t *testing.T) {
	t.Parallel()

	var item OutputItem
	if err := json.Unmarshal([]byte(`{"type":"computer_call","call_id":"call_1","action":{"type":"screenshot"}}`), &item); err != nil {
		t.Fatalf("unmarshal OutputItem: %v", err)
	}
	if len(item.Actions) != 1 || string(item.Actions[0]) != `{"type":"screenshot"}` {
		t.Fatalf("computer call actions = %s, want one screenshot action", item.Actions)
	}
}

func TestAnthropicResponseFromResponsesNormalizesAgentInput(t *testing.T) {
	t.Parallel()

	response, err := AnthropicResponseFromResponsesJSON([]byte(`{
  "id":"resp_agent",
  "model":"gpt",
  "output":[{
    "type":"function_call",
    "call_id":"call_agent",
    "name":"Agent",
    "arguments":"{\"prompt\":\"find latest chatgpt news\",\"agent_type\":\"general-purpose\",\"run_in_background\":\"false\",\"extra\":\"ignored\"}"
  }]
}`))
	if err != nil {
		t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v", err)
	}
	if len(response.Content) != 1 {
		t.Fatalf("content = %#v", response.Content)
	}
	input, ok := response.Content[0].Input.(map[string]any)
	if !ok {
		t.Fatalf("Agent input = %#v, want object", response.Content[0].Input)
	}
	if input["prompt"] != "find latest chatgpt news" ||
		input["description"] != "find latest chatgpt news" ||
		input["subagent_type"] != "general-purpose" ||
		input["run_in_background"] != false {
		t.Fatalf("Agent input = %#v", input)
	}
	if _, found := input["extra"]; found {
		t.Fatalf("Agent input retained unknown field: %#v", input)
	}
}

func TestAnthropicResponseFromResponsesTurnsInvalidAgentInputIntoCompatibilityText(t *testing.T) {
	t.Parallel()

	response, err := AnthropicResponseFromResponsesJSON([]byte(`{
  "id":"resp_agent",
  "model":"gpt",
  "output":[{
    "type":"function_call",
    "call_id":"call_agent",
    "name":"Agent",
    "arguments":"{}"
  }]
}`))
	if err != nil {
		t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v", err)
	}
	if response.StopReason != "end_turn" || len(response.Content) != 1 || response.Content[0].Type != "text" {
		t.Fatalf("response = %#v", response)
	}
	if !strings.Contains(response.Content[0].Text, "invalid Agent tool input") ||
		!strings.Contains(response.Content[0].Text, "prompt and description") {
		t.Fatalf("compatibility text = %q", response.Content[0].Text)
	}
}

func TestFunctionToolUseBlockWrapsNonObjectArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments string
		want      any
	}{
		{name: "string", arguments: `"search"`, want: "search"},
		{name: "number", arguments: `42`, want: float64(42)},
		{name: "array", arguments: `["one",2]`, want: []any{"one", float64(2)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			block, toolUse, err := functionToolUseBlock(OutputItem{
				CallID: "call_1", Name: "lookup", Arguments: test.arguments,
			})
			if err != nil {
				t.Fatalf("functionToolUseBlock() error = %v", err)
			}
			if !toolUse {
				t.Fatal("functionToolUseBlock() did not produce tool use")
			}
			assertJSONEqual(t, block.Input, map[string]any{"value": test.want})
		})
	}
}

func TestAnthropicResponseFromResponsesPreservesIncompleteAndRefusalSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		raw            string
		wantStopReason string
		wantText       string
	}{
		{
			name:           "max output tokens",
			raw:            `{"id":"resp_limit","model":"gpt","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`,
			wantStopReason: "max_tokens",
			wantText:       "partial",
		},
		{
			name:           "refusal content",
			raw:            `{"id":"resp_refusal","model":"gpt","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot assist with that."}]}]}`,
			wantStopReason: "end_turn",
			wantText:       "I cannot assist with that.",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response, err := AnthropicResponseFromResponsesJSON([]byte(test.raw))
			if err != nil {
				t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v", err)
			}
			if response.StopReason != test.wantStopReason || len(response.Content) != 1 || response.Content[0].Text != test.wantText {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

//nolint:misspell // Responses provider status spelling can be "cancelled".
func TestAnthropicResponseFromResponsesRejectsUnsuccessfulStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "failed status",
			raw:     `{"id":"resp_failed","model":"gpt","status":"failed","output":[]}`,
			wantErr: `status "failed"`,
		},
		{
			name:    "cancelled status",
			raw:     `{"id":"resp_cancelled","model":"gpt","status":"cancelled","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hidden"}]}]}`,
			wantErr: `status "cancelled"`,
		},
		{
			name:    "non max token incomplete status",
			raw:     `{"id":"resp_incomplete","model":"gpt","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hidden"}]}]}`,
			wantErr: `reason "content_filter"`,
		},
		{
			name:    "missing incomplete reason",
			raw:     `{"id":"resp_incomplete","model":"gpt","status":"incomplete","output_text":"hidden"}`,
			wantErr: `reason "unknown"`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := AnthropicResponseFromResponsesJSON([]byte(test.raw))
			if !errors.Is(err, ErrUnsuccessfulProviderStatus) || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("AnthropicResponseFromResponsesJSON() error = %v, want unsuccessful status containing %q", err, test.wantErr)
			}
		})
	}
}

func TestAnthropicResponseFromResponsesJSONMalformedProviderOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "invalid response JSON",
			raw:  `{`,
		},
		{
			name: "function arguments not JSON",
			raw: `{"id":"resp","model":"gpt","output":[
				{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"not-json"}
			]}`,
		},
		{
			name: "computer call missing actions",
			raw: `{"id":"resp","model":"gpt","output":[
				{"type":"computer_call","call_id":"call_1","status":"completed"}
			]}`,
		},
		{
			name: "message role is not assistant",
			raw: `{"id":"resp","model":"gpt","output":[
				{"type":"message","role":"user","content":[{"type":"output_text","text":"bad"}]}
			]}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := AnthropicResponseFromResponsesJSON([]byte(test.raw))
			if !errors.Is(err, ErrMalformedProviderOutput) {
				t.Fatalf("error = %v, want ErrMalformedProviderOutput", err)
			}
		})
	}
}

func TestAnthropicResponseFromResponsesRejectsUnknownOutputItem(t *testing.T) {
	t.Parallel()

	_, err := AnthropicResponseFromResponsesJSON([]byte(`{"id":"resp","model":"gpt","output":[{"type":"future_item"}]}`))
	if !errors.Is(err, ErrMalformedProviderOutput) {
		t.Fatalf("error = %v, want ErrMalformedProviderOutput", err)
	}
	if !strings.Contains(err.Error(), "future_item") {
		t.Fatalf("error = %v, want output item type", err)
	}
}

func TestOutputItemKeepsRawJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"type":"reasoning","summary":[{"text":"hidden"}]}`)
	var item OutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("UnmarshalJSON() error = %v", err)
	}
	if string(item.Raw) != string(raw) {
		t.Fatalf("Raw = %s, want %s", item.Raw, raw)
	}
}
