package gateway

import (
	"bytes"
	"encoding/json"
)

type anthropicRequest struct {
	Model         string                     `json:"model"`
	System        any                        `json:"system,omitempty"`
	Messages      []anthropicMessage         `json:"messages"`
	MaxTokens     int                        `json:"max_tokens,omitempty"`
	Temperature   *float64                   `json:"temperature,omitempty"`
	StopSequences []string                   `json:"stop_sequences,omitempty"`
	Stream        bool                       `json:"stream,omitempty"`
	Tools         []json.RawMessage          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage            `json:"tool_choice,omitempty"`
	Thinking      json.RawMessage            `json:"thinking,omitempty"`
	Metadata      json.RawMessage            `json:"metadata,omitempty"`
	OutputConfig  json.RawMessage            `json:"output_config,omitempty"`
	Fields        map[string]json.RawMessage `json:"-"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAIChatRequest struct {
	Model           string                `json:"model"`
	Messages        []openAIMessage       `json:"messages"`
	MaxTokens       int                   `json:"max_tokens,omitempty"`
	Temperature     *float64              `json:"temperature,omitempty"`
	Stop            []string              `json:"stop,omitempty"`
	Stream          bool                  `json:"stream"`
	User            string                `json:"user,omitempty"`
	ReasoningEffort string                `json:"reasoning_effort,omitempty"`
	ResponseFormat  *openAIResponseFormat `json:"response_format,omitempty"`
	Tools           []openAITool          `json:"tools,omitempty"`
	ToolChoice      any                   `json:"tool_choice,omitempty"`
	ParallelTools   *bool                 `json:"parallel_tool_calls,omitempty"`
}

type openAIResponseFormat struct {
	Type       string           `json:"type"`
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type openAIMessage struct {
	Role       string              `json:"role"`
	Content    string              `json:"content"`
	Parts      []openAIContentPart `json:"-"`
	ToolCalls  []openAIToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
}

// openAIContentPart is one element of a multipart OpenAI chat message content
// array. Text-only messages continue to serialize Content as a plain string;
// Parts is used only when a message carries non-text input such as an image.
type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

// MarshalJSON emits the OpenAI `content` field as a multipart array when Parts
// is populated, otherwise as the plain Content string. This keeps the common
// text-only wire format unchanged while supporting image_url content parts.
func (m openAIMessage) MarshalJSON() ([]byte, error) {
	type wireMessage struct {
		Role       string           `json:"role"`
		Content    any              `json:"content"`
		ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
	}
	wire := wireMessage{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if len(m.Parts) > 0 {
		wire.Content = m.Parts
	} else {
		wire.Content = m.Content
	}
	return json.Marshal(wire)
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content      string              `json:"content"`
			ToolCalls    []openAIToolCall    `json:"tool_calls"`
			FunctionCall *openAIFunctionCall `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	usageObserved bool
}

func (r *openAIChatResponse) UnmarshalJSON(data []byte) error {
	type wireResponse openAIChatResponse
	var decoded wireResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var presence struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		return err
	}
	*r = openAIChatResponse(decoded)
	trimmed := bytes.TrimSpace(presence.Usage)
	r.usageObserved = len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
	return nil
}
