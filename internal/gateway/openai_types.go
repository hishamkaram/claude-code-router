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
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Stream          bool            `json:"stream"`
	User            string          `json:"user,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           []openAITool    `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	ParallelTools   *bool           `json:"parallel_tool_calls,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
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
