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

func openAIToolArguments(raw string) any {
	return openAIToolArgumentsForTool("", raw)
}

func openAIToolArgumentsForTool(toolName, raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	decoded, ok := decodeOpenAIToolArguments(raw)
	if !ok {
		return map[string]any{"arguments": raw}
	}
	if decoded == nil {
		return map[string]any{}
	}
	decodedObject, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{"value": decoded}
	}
	if strings.EqualFold(strings.TrimSpace(toolName), "Agent") {
		return normalizeAgentToolInput(decodedObject)
	}
	return decoded
}

func normalizeAgentToolInput(input map[string]any) map[string]any {
	normalized := make(map[string]any, 6)
	prompt := trimmedStringField(input, "prompt")
	if prompt != "" {
		normalized["prompt"] = prompt
	}
	description := trimmedStringField(input, "description")
	if description == "" {
		description = agentDescriptionFromPrompt(prompt)
	}
	if description != "" {
		normalized["description"] = description
	}
	if subagentType := firstTrimmedStringField(input, "subagent_type", "agent_type"); subagentType != "" {
		normalized["subagent_type"] = subagentType
	}
	if isolation := allowedStringField(input, "isolation", "worktree", "remote"); isolation != "" {
		normalized["isolation"] = isolation
	}
	if model := allowedStringField(input, "model", "sonnet", "opus", "haiku", "fable"); model != "" {
		normalized["model"] = model
	}
	if runInBackground, ok := boolField(input, "run_in_background"); ok {
		normalized["run_in_background"] = runInBackground
	}
	return normalized
}

func agentDescriptionFromPrompt(prompt string) string {
	words := strings.Fields(prompt)
	if len(words) == 0 {
		return ""
	}
	if len(words) > 5 {
		words = words[:5]
	}
	return strings.Join(words, " ")
}

func firstTrimmedStringField(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := trimmedStringField(input, key); value != "" {
			return value
		}
	}
	return ""
}

func trimmedStringField(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func allowedStringField(input map[string]any, key string, allowed ...string) string {
	value := trimmedStringField(input, key)
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

func boolField(input map[string]any, key string) (value, ok bool) {
	switch value := input[key].(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func decodeOpenAIToolArguments(raw string) (any, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
		return decoded, true
	}
	suffix, ok := missingJSONClosingSuffix(raw)
	if !ok {
		return nil, false
	}
	if err := json.Unmarshal([]byte(raw+suffix), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func missingJSONClosingSuffix(raw string) (string, bool) {
	state := jsonClosingSuffixState{stack: make([]byte, 0, 4)}
	for i := 0; i < len(raw); i++ {
		if !state.consume(raw[i]) {
			return "", false
		}
	}
	if state.inString || len(state.stack) == 0 {
		return "", false
	}
	suffix := make([]byte, 0, len(state.stack))
	for i := len(state.stack) - 1; i >= 0; i-- {
		suffix = append(suffix, state.stack[i])
	}
	return string(suffix), true
}

type jsonClosingSuffixState struct {
	stack    []byte
	inString bool
	escaped  bool
}

func (s *jsonClosingSuffixState) consume(b byte) bool {
	if s.inString {
		s.consumeStringByte(b)
		return true
	}
	switch b {
	case '"':
		s.inString = true
	case '{':
		s.stack = append(s.stack, '}')
	case '[':
		s.stack = append(s.stack, ']')
	case '}', ']':
		return s.consumeClosingDelimiter(b)
	}
	return true
}

func (s *jsonClosingSuffixState) consumeStringByte(b byte) {
	if s.escaped {
		s.escaped = false
		return
	}
	if b == '\\' {
		s.escaped = true
		return
	}
	if b == '"' {
		s.inString = false
	}
}

func (s *jsonClosingSuffixState) consumeClosingDelimiter(b byte) bool {
	if len(s.stack) == 0 || s.stack[len(s.stack)-1] != b {
		return false
	}
	s.stack = s.stack[:len(s.stack)-1]
	return true
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
