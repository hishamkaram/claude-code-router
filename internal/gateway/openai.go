package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func validateThinking(raw json.RawMessage) error {
	thinkingType, err := openAIThinkingType(raw)
	if err != nil {
		return err
	}
	switch thinkingType {
	case "", "adaptive", "disabled", "enabled":
		return nil
	default:
		return fmt.Errorf("thinking mode %q is not supported by the OpenAI-compatible gateway path", thinkingType)
	}
}

func (h *handler) callOpenAICompatible(ctx context.Context, provider store.Provider, apiKey string, payload openAIChatRequest) (openAIChatResponse, error) {
	endpoint, err := providers.ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return openAIChatResponse{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("encoding OpenAI-compatible request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("creating OpenAI-compatible request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("requesting OpenAI-compatible provider %q: %w", provider.Name, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return openAIChatResponse{}, fmt.Errorf("OpenAI-compatible provider %q returned HTTP %d %s", provider.Name, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var decoded openAIChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return openAIChatResponse{}, fmt.Errorf("decoding OpenAI-compatible provider response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return openAIChatResponse{}, fmt.Errorf("OpenAI-compatible provider %q returned no choices", provider.Name)
	}
	return decoded, nil
}

type anthropicRequest struct {
	Model        string                     `json:"model"`
	System       any                        `json:"system,omitempty"`
	Messages     []anthropicMessage         `json:"messages"`
	MaxTokens    int                        `json:"max_tokens,omitempty"`
	Stream       bool                       `json:"stream,omitempty"`
	Tools        []json.RawMessage          `json:"tools,omitempty"`
	ToolChoice   json.RawMessage            `json:"tool_choice,omitempty"`
	Thinking     json.RawMessage            `json:"thinking,omitempty"`
	Metadata     json.RawMessage            `json:"metadata,omitempty"`
	OutputConfig json.RawMessage            `json:"output_config,omitempty"`
	Fields       map[string]json.RawMessage `json:"-"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAIChatRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
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
}

func toOpenAIChatRequest(req anthropicRequest, providerModel string) (openAIChatRequest, error) {
	options, err := openAIOptionsFromAnthropic(req)
	if err != nil {
		return openAIChatRequest{}, err
	}
	tools, err := openAIToolsFromAnthropic(req.Tools)
	if err != nil {
		return openAIChatRequest{}, err
	}
	toolChoice, parallelTools, err := openAIToolChoiceFromAnthropic(req.ToolChoice)
	if err != nil {
		return openAIChatRequest{}, err
	}
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != nil {
		text, err := anthropicContentText(req.System)
		if err != nil {
			return openAIChatRequest{}, fmt.Errorf("unsupported system content: %w", err)
		}
		if text != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: text})
		}
	}
	for _, message := range req.Messages {
		converted, err := openAIMessagesFromAnthropic(message)
		if err != nil {
			return openAIChatRequest{}, err
		}
		messages = append(messages, converted...)
	}
	return openAIChatRequest{
		Model:           providerModel,
		Messages:        messages,
		MaxTokens:       req.MaxTokens,
		Stream:          false,
		User:            options.user,
		ReasoningEffort: options.reasoningEffort,
		Tools:           tools,
		ToolChoice:      toolChoice,
		ParallelTools:   parallelTools,
	}, nil
}

func openAIToolsFromAnthropic(rawTools []json.RawMessage) ([]openAITool, error) {
	if len(rawTools) == 0 {
		return nil, nil
	}
	tools := make([]openAITool, 0, len(rawTools))
	for _, raw := range rawTools {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("unsupported tool definition: %w", err)
		}
		name, _ := payload["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("tool definition missing name")
		}
		description, _ := payload["description"].(string)
		parameters := payload["input_schema"]
		if parameters == nil {
			parameters = map[string]any{"type": "object"}
		}
		var strict *bool
		if value, ok := payload["strict"].(bool); ok {
			strict = &value
		}
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        name,
				Description: description,
				Parameters:  parameters,
				Strict:      strict,
			},
		})
	}
	return tools, nil
}

func openAIToolChoiceFromAnthropic(raw json.RawMessage) (choice any, parallelTools *bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var payload map[string]json.RawMessage
	err = json.Unmarshal(raw, &payload)
	if err != nil {
		return nil, nil, fmt.Errorf("unsupported tool_choice: %w", err)
	}
	parallelTools, err = openAIParallelToolCallsFromAnthropic(payload)
	if err != nil {
		return nil, nil, err
	}
	var choiceType string
	if rawType, ok := payload["type"]; ok {
		if err := json.Unmarshal(rawType, &choiceType); err != nil {
			return nil, nil, fmt.Errorf("tool_choice.type must be a string")
		}
	}
	switch choiceType {
	case "", "auto":
		return "auto", parallelTools, nil
	case "none":
		return "none", parallelTools, nil
	case "any":
		return "required", parallelTools, nil
	case "tool":
		var name string
		if rawName, ok := payload["name"]; ok {
			if err := json.Unmarshal(rawName, &name); err != nil {
				return nil, nil, fmt.Errorf("tool_choice.name must be a string")
			}
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, nil, fmt.Errorf("tool_choice type %q requires name", choiceType)
		}
		return map[string]any{
			"type": "function",
			"function": map[string]string{
				"name": name,
			},
		}, parallelTools, nil
	default:
		return nil, nil, fmt.Errorf("tool_choice type %q is not supported by the OpenAI-compatible gateway path", choiceType)
	}
}

func openAIParallelToolCallsFromAnthropic(payload map[string]json.RawMessage) (*bool, error) {
	raw, ok := payload["disable_parallel_tool_use"]
	if !ok {
		return nil, nil
	}
	var disabled bool
	if err := json.Unmarshal(raw, &disabled); err != nil {
		return nil, fmt.Errorf("tool_choice.disable_parallel_tool_use must be a boolean")
	}
	if !disabled {
		return nil, nil
	}
	parallel := false
	return &parallel, nil
}

func openAIMessagesFromAnthropic(message anthropicMessage) ([]openAIMessage, error) {
	switch message.Role {
	case "user":
		return openAIUserMessagesFromAnthropic(message.Content)
	case "assistant":
		return openAIAssistantMessagesFromAnthropic(message.Content)
	case "system":
		return openAISystemMessagesFromAnthropic(message.Content)
	default:
		return nil, fmt.Errorf("unsupported message role %q", message.Role)
	}
}

func openAISystemMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	text, err := anthropicContentText(content)
	if err != nil {
		return nil, fmt.Errorf("unsupported system message content: %w", err)
	}
	if text == "" {
		return nil, nil
	}
	return []openAIMessage{{Role: "system", Content: text}}, nil
}

func openAIUserMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	if text, ok := content.(string); ok {
		return []openAIMessage{{Role: "user", Content: text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported user message content type %T", content)
	}
	messages := make([]openAIMessage, 0, len(blocks))
	textParts := make([]string, 0)
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		messages = append(messages, openAIMessage{Role: "user", Content: strings.Join(textParts, "\n")})
		textParts = textParts[:0]
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("user content block is not an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return nil, fmt.Errorf("unsupported user text block: %w", err)
			}
			textParts = append(textParts, text)
		case "tool_result":
			flushText()
			toolCallID, _ := block["tool_use_id"].(string)
			toolCallID = strings.TrimSpace(toolCallID)
			if toolCallID == "" {
				return nil, fmt.Errorf("tool_result block missing tool_use_id")
			}
			text, err := anthropicToolResultText(block["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, openAIMessage{Role: "tool", ToolCallID: toolCallID, Content: text})
		default:
			return nil, fmt.Errorf("user content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	flushText()
	return messages, nil
}

func openAIAssistantMessagesFromAnthropic(content any) ([]openAIMessage, error) {
	if text, ok := content.(string); ok {
		return []openAIMessage{{Role: "assistant", Content: text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported assistant message content type %T", content)
	}
	var textParts []string
	var toolCalls []openAIToolCall
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("assistant content block is not an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return nil, fmt.Errorf("unsupported assistant text block: %w", err)
			}
			textParts = append(textParts, text)
		case "tool_use":
			toolCall, err := openAIToolCallFromAnthropic(block)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, toolCall)
		case "thinking", "redacted_thinking":
			continue
		default:
			return nil, fmt.Errorf("assistant content block type %q is not supported by the OpenAI-compatible gateway path", blockType)
		}
	}
	if len(textParts) == 0 && len(toolCalls) == 0 {
		return nil, nil
	}
	return []openAIMessage{{Role: "assistant", Content: strings.Join(textParts, "\n"), ToolCalls: toolCalls}}, nil
}

func openAIToolCallFromAnthropic(block map[string]any) (openAIToolCall, error) {
	id, _ := block["id"].(string)
	name, _ := block["name"].(string)
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return openAIToolCall{}, fmt.Errorf("tool_use block requires id and name")
	}
	arguments, err := json.Marshal(firstNonNil(block["input"], map[string]any{}))
	if err != nil {
		return openAIToolCall{}, fmt.Errorf("encoding tool_use input for %q: %w", name, err)
	}
	return openAIToolCall{
		ID:   id,
		Type: "function",
		Function: openAIFunctionCall{
			Name:      name,
			Arguments: string(arguments),
		},
	}, nil
}

func anthropicToolResultText(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		return anthropicContentText(content)
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return "", fmt.Errorf("encoding tool_result content: %w", err)
		}
		return string(encoded), nil
	}
}

type openAIRequestOptions struct {
	user            string
	reasoningEffort string
}

func openAIOptionsFromAnthropic(req anthropicRequest) (openAIRequestOptions, error) {
	user, err := openAIUserFromMetadata(req.Metadata)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	reasoningEffort, err := openAIReasoningEffortFromOutputConfig(req.OutputConfig)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	if reasoningEffort == "" {
		reasoningEffort, err = openAIReasoningEffortFromThinking(req.Thinking)
		if err != nil {
			return openAIRequestOptions{}, err
		}
	}
	return openAIRequestOptions{user: user, reasoningEffort: reasoningEffort}, nil
}

func openAIUserFromMetadata(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported metadata: %w", err)
	}
	var user string
	for key, value := range payload {
		if key == "user_id" {
			if err := json.Unmarshal(value, &user); err != nil {
				return "", fmt.Errorf("metadata.user_id must be a string")
			}
		}
	}
	return strings.TrimSpace(user), nil
}

func openAIReasoningEffortFromOutputConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported output_config: %w", err)
	}
	var effort string
	for key, value := range payload {
		if key == "effort" {
			if err := json.Unmarshal(value, &effort); err != nil {
				return "", fmt.Errorf("output_config.effort must be a string")
			}
		}
	}
	return openAIReasoningEffortFromClaudeEffort(effort)
}

func openAIReasoningEffortFromThinking(raw json.RawMessage) (string, error) {
	thinkingType, err := openAIThinkingType(raw)
	if err != nil {
		return "", err
	}
	if thinkingType == "enabled" {
		return "high", nil
	}
	return "", nil
}

func openAIThinkingType(raw json.RawMessage) (string, error) {
	if !rawJSONPresent(raw) {
		return "", nil
	}
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported thinking field: %w", err)
	}
	return strings.TrimSpace(payload.Type), nil
}

func openAIReasoningEffortFromClaudeEffort(effort string) (string, error) {
	trimmed := strings.TrimSpace(effort)
	switch trimmed {
	case "":
		return "", nil
	case "low", "medium", "high":
		return trimmed, nil
	case "xhigh", "max":
		return "high", nil
	default:
		return "", fmt.Errorf("output_config.effort %q is not supported by the OpenAI-compatible gateway path", effort)
	}
}

func anthropicContentText(value any) (string, error) {
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		var parts []string
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content block is not an object")
			}
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("content type %T is not supported", value)
	}
}

func anthropicTextBlockText(block map[string]any) (string, error) {
	for key := range block {
		if key != "type" && key != "text" && key != "cache_control" {
			return "", fmt.Errorf("content block field %q is not supported", key)
		}
	}
	blockType, _ := block["type"].(string)
	if blockType != "text" {
		return "", fmt.Errorf("content block type %q is not supported", blockType)
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", fmt.Errorf("content block text must be a string")
	}
	return text, nil
}

func writeAnthropicStream(w http.ResponseWriter, alias string, resp openAIChatResponse, finishReason string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	blocks := anthropicContentBlocksFromOpenAI(resp)
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
		if text, ok := block["text"].(string); ok && text != "" {
			writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]string{"type": "text_delta", "text": text},
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
	return map[string]any{
		"id":            firstNonEmpty(resp.ID, "msg_ccr"),
		"type":          "message",
		"role":          "assistant",
		"model":         alias,
		"content":       anthropicContentBlocksFromOpenAI(resp),
		"stop_reason":   finishReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
}

func anthropicContentBlocksFromOpenAI(resp openAIChatResponse) []map[string]any {
	if len(resp.Choices) == 0 {
		return []map[string]any{{"type": "text", "text": ""}}
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
	blocks := make([]map[string]any, 0, 1+len(toolCalls))
	if message.Content != "" || len(toolCalls) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
	}
	for _, toolCall := range toolCalls {
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(toolCall.ID, "toolu_ccr"),
			"name":  toolCall.Function.Name,
			"input": openAIToolArguments(toolCall.Function.Arguments),
		})
	}
	return blocks
}

func streamStartBlock(block map[string]any) map[string]any {
	if block["type"] == "text" {
		return map[string]any{"type": "text", "text": ""}
	}
	return block
}

func openAIToolArguments(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return map[string]any{"arguments": raw}
	}
	if decoded == nil {
		return map[string]any{}
	}
	if _, ok := decoded.(map[string]any); !ok {
		return map[string]any{"value": decoded}
	}
	return decoded
}

func anthropicStopReasonFromOpenAI(resp openAIChatResponse) (string, error) {
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("OpenAI-compatible provider returned no choices")
	}
	switch finishReason := resp.Choices[0].FinishReason; finishReason {
	case "", "stop":
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	case "tool_calls":
		return "tool_use", nil
	case "function_call":
		if resp.Choices[0].Message.FunctionCall == nil {
			return "", fmt.Errorf("OpenAI-compatible provider returned function_call finish_reason without function_call")
		}
		return "tool_use", nil
	default:
		return "", fmt.Errorf("OpenAI-compatible provider returned unsupported finish_reason %q", finishReason)
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
