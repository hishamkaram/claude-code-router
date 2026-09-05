package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func (h *handler) produceOpenAIChatStream(
	ctx context.Context,
	provider store.Provider,
	apiKey string,
	payload openAIChatRequest,
	events chan<- upstreamStreamEvent,
) {
	resp, failure := h.openAIChatStreamResponse(ctx, provider, apiKey, payload)
	if failure != nil {
		sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
			kind:    upstreamStreamFailed,
			failure: failure,
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamReady, status: resp.StatusCode}) {
		return
	}
	forwardOpenAIChatStream(ctx, resp.Body, events)
}

func newOpenAIChatStreamRequest(
	ctx context.Context,
	provider store.Provider,
	apiKey string,
	payload openAIChatRequest,
) (*http.Request, *streamFailure) {
	endpoint, err := providers.ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return nil, &streamFailure{errorClass: "provider_transport", message: err.Error()}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &streamFailure{errorClass: "gateway", message: fmt.Sprintf("encoding OpenAI-compatible request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &streamFailure{errorClass: "gateway", message: fmt.Sprintf("creating OpenAI-compatible request: %v", err)}
	}
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

func (h *handler) openAIChatStreamResponse(
	ctx context.Context,
	provider store.Provider,
	apiKey string,
	payload openAIChatRequest,
) (*http.Response, *streamFailure) {
	req, failure := newOpenAIChatStreamRequest(ctx, provider, apiKey, payload)
	if failure != nil {
		return nil, failure
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return nil, &streamFailure{
			errorClass: "provider_transport",
			message:    fmt.Sprintf("requesting OpenAI-compatible provider %q: %v", provider.Name, err),
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, providerErrorDetailReadLimit(apiKey)))
		_ = resp.Body.Close()
		statusErr := newOpenAIProviderStatusError(provider.Name, resp.StatusCode, sanitizeProviderErrorDetail(raw, apiKey))
		return nil, &streamFailure{
			statusCode: statusErr.SafeStatusCode(),
			errorClass: "provider_http",
			message:    statusErr.Error(),
		}
	}
	if !isOpenAIEventStream(resp.Header.Get("Content-Type")) {
		_ = resp.Body.Close()
		return nil, &streamFailure{
			statusCode: resp.StatusCode,
			errorClass: "provider_protocol",
			message:    "OpenAI-compatible provider did not honor the streaming request",
		}
	}
	return resp, nil
}

func forwardOpenAIChatStream(ctx context.Context, body io.Reader, events chan<- upstreamStreamEvent) {
	scanner := newOpenAISSEScanner(body, maxOpenAIChatResponseBytes)
	completion := newOpenAIChatStreamEOFCompletion()
	for {
		data, ok, scanErr := scanner.next()
		if scanErr != nil {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind:    upstreamStreamFailed,
				failure: &streamFailure{errorClass: "provider_stream", message: fmt.Sprintf("decoding streamed OpenAI-compatible provider response: %v", scanErr)},
			})
			return
		}
		if !ok {
			if completion.complete() {
				sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
				return
			}
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind:    upstreamStreamFailed,
				failure: &streamFailure{errorClass: "provider_stream", message: "provider stream ended before a completion event"},
			})
			return
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
			return
		}
		completion.observe(data)
		if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamData, data: data}) {
			return
		}
	}
}

// openAIChatStreamEOFCompletion preserves the OpenAI stream contract for
// providers that finish by closing the SSE response instead of sending [DONE].
type openAIChatStreamEOFCompletion struct {
	choices map[int]bool
}

func newOpenAIChatStreamEOFCompletion() *openAIChatStreamEOFCompletion {
	return &openAIChatStreamEOFCompletion{choices: make(map[int]bool)}
}

func (c *openAIChatStreamEOFCompletion) observe(data []byte) {
	var chunk struct {
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	for _, choice := range chunk.Choices {
		if _, observed := c.choices[choice.Index]; !observed {
			c.choices[choice.Index] = false
		}
		if choice.FinishReason != "" {
			c.choices[choice.Index] = true
		}
	}
}

func (c *openAIChatStreamEOFCompletion) complete() bool {
	if len(c.choices) == 0 {
		return false
	}
	for _, finished := range c.choices {
		if !finished {
			return false
		}
	}
	return true
}

type openAIChatStreamAdapter struct {
	alias        string
	apiKey       string
	messageID    string
	inputTokens  int
	usage        observability.TokenUsage
	sawChoice    bool
	finishReason string
	textStarted  bool
	textStopped  bool
	toolCalls    map[int]*openAIToolCallAccumulator
	functionCall *openAIFunctionCall
	functionArgs strings.Builder
}

func newOpenAIChatStreamAdapter(alias, apiKey, messageID string) *openAIChatStreamAdapter {
	return &openAIChatStreamAdapter{
		alias:     alias,
		apiKey:    apiKey,
		messageID: messageID,
		toolCalls: make(map[int]*openAIToolCallAccumulator),
	}
}

func (a *openAIChatStreamAdapter) Start(writer *anthropicSSEWriter) error {
	if strings.TrimSpace(a.messageID) == "" {
		return fmt.Errorf("translated stream is missing a response message id")
	}
	return writer.Event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            a.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         a.alias,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  a.inputTokens,
				"output_tokens": 0,
			},
		},
	})
}

func (a *openAIChatStreamAdapter) Consume(writer *anthropicSSEWriter, event upstreamStreamEvent) error {
	var chunk openAIChatStreamChunk
	if err := json.Unmarshal(event.data, &chunk); err != nil {
		return fmt.Errorf("decoding provider stream event: %w", err)
	}
	if raw := bytes.TrimSpace(chunk.Error); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		detail := sanitizeProviderErrorDetail(raw, a.apiKey)
		if detail == "" {
			return fmt.Errorf("provider stream reported an error")
		}
		return fmt.Errorf("provider stream reported an error: %s", detail)
	}
	if chunk.Usage != nil {
		a.usage = observability.TokenUsage{
			Observed:     true,
			InputTokens:  int64(chunk.Usage.PromptTokens),
			OutputTokens: int64(chunk.Usage.CompletionTokens),
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		a.sawChoice = true
		if err := a.consumeChoice(writer, choice); err != nil {
			return err
		}
	}
	return nil
}

func (a *openAIChatStreamAdapter) consumeChoice(writer *anthropicSSEWriter, choice openAIChatStreamChoice) error {
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		if err := a.writeTextDelta(writer, *choice.Delta.Content); err != nil {
			return err
		}
	}
	for _, streamed := range choice.Delta.ToolCalls {
		toolCall := a.toolCalls[streamed.Index]
		if toolCall == nil {
			toolCall = &openAIToolCallAccumulator{}
			a.toolCalls[streamed.Index] = toolCall
		}
		if toolCall.call.ID == "" {
			toolCall.call.ID = streamed.ID
		}
		if toolCall.call.Type == "" {
			toolCall.call.Type = streamed.Type
		}
		toolCall.call.Function.Name = appendStableStreamValue(toolCall.call.Function.Name, streamed.Function.Name)
		toolCall.arguments.WriteString(streamed.Function.Arguments)
	}
	if streamed := choice.Delta.FunctionCall; streamed != nil {
		if a.functionCall == nil {
			a.functionCall = &openAIFunctionCall{}
		}
		a.functionCall.Name = appendStableStreamValue(a.functionCall.Name, streamed.Name)
		a.functionArgs.WriteString(streamed.Arguments)
	}
	if choice.FinishReason != "" {
		a.finishReason = choice.FinishReason
	}
	return nil
}

func (a *openAIChatStreamAdapter) writeTextDelta(writer *anthropicSSEWriter, text string) error {
	if !a.textStarted {
		if err := writer.Event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
		a.textStarted = true
	}
	return writer.Event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": text},
	})
}

func (a *openAIChatStreamAdapter) stopText(writer *anthropicSSEWriter) error {
	if !a.textStarted || a.textStopped {
		return nil
	}
	a.textStopped = true
	return writer.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
}

func (a *openAIChatStreamAdapter) Finish(writer *anthropicSSEWriter) (observability.TokenUsage, error) {
	if !a.sawChoice {
		return observability.TokenUsage{}, fmt.Errorf("provider stream returned no choices")
	}
	if a.finishReason == "" {
		return observability.TokenUsage{}, fmt.Errorf("provider stream ended without a finish reason")
	}
	tools := a.streamTools()
	if message := invalidStreamAgentToolInput(tools); message != "" {
		return a.finishInvalidAgentToolInput(writer, message)
	}
	if err := a.ensureVisibleContent(writer, tools); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.stopText(writer); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.writeToolBlocks(writer, tools); err != nil {
		return observability.TokenUsage{}, err
	}
	stopReason, err := a.stopReason()
	if err != nil {
		return observability.TokenUsage{}, err
	}
	return a.finishMessage(writer, stopReason)
}

func (a *openAIChatStreamAdapter) streamTools() []openAIToolCall {
	tools := a.orderedTools()
	if len(tools) != 0 || a.functionCall == nil {
		return tools
	}
	return append(tools, openAIToolCall{
		ID:   "toolu_ccr_function_call",
		Type: "function",
		Function: openAIFunctionCall{
			Name:      a.functionCall.Name,
			Arguments: a.functionArgs.String(),
		},
	})
}

func (a *openAIChatStreamAdapter) finishInvalidAgentToolInput(writer *anthropicSSEWriter, message string) (observability.TokenUsage, error) {
	if err := a.writeTextDelta(writer, message); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.stopText(writer); err != nil {
		return observability.TokenUsage{}, err
	}
	return a.finishMessage(writer, "end_turn")
}

func (a *openAIChatStreamAdapter) ensureVisibleContent(writer *anthropicSSEWriter, tools []openAIToolCall) error {
	if a.textStarted || len(tools) != 0 {
		return nil
	}
	return a.writeTextDelta(writer, "")
}

func (a *openAIChatStreamAdapter) writeToolBlocks(writer *anthropicSSEWriter, tools []openAIToolCall) error {
	index := 0
	if a.textStarted {
		index = 1
	}
	for _, tool := range tools {
		if err := writeOpenAIToolStreamBlock(writer, index, tool); err != nil {
			return err
		}
		index++
	}
	return nil
}

func (a *openAIChatStreamAdapter) orderedTools() []openAIToolCall {
	indexes := make([]int, 0, len(a.toolCalls))
	for index := range a.toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	tools := make([]openAIToolCall, 0, len(indexes))
	for _, index := range indexes {
		tool := a.toolCalls[index]
		tool.call.Function.Arguments = tool.arguments.String()
		tools = append(tools, tool.call)
	}
	return tools
}

func invalidStreamAgentToolInput(tools []openAIToolCall) string {
	for _, tool := range tools {
		input := openAIToolArgumentsForTool(tool.Function.Name, tool.Function.Arguments)
		if message := invalidAgentToolInputMessage(tool.Function.Name, input); message != "" {
			return message
		}
	}
	return ""
}

func writeOpenAIToolStreamBlock(writer *anthropicSSEWriter, index int, tool openAIToolCall) error {
	input := openAIToolArgumentsForTool(tool.Function.Name, tool.Function.Arguments)
	if err := writer.Event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(tool.ID, "toolu_ccr"),
			"name":  tool.Function.Name,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding streamed tool input: %w", err)
	}
	if err := writer.Event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{"type": "input_json_delta", "partial_json": string(encoded)},
	}); err != nil {
		return err
	}
	return writer.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (a *openAIChatStreamAdapter) stopReason() (string, error) {
	switch a.finishReason {
	case "", "stop":
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	case "tool_calls":
		return "tool_use", nil
	case "function_call":
		if a.functionCall == nil {
			return "", fmt.Errorf("provider stream returned function_call finish_reason without function_call")
		}
		return "tool_use", nil
	default:
		return "", fmt.Errorf("provider stream returned unsupported finish_reason %q", a.finishReason)
	}
}

func (a *openAIChatStreamAdapter) finishMessage(writer *anthropicSSEWriter, stopReason string) (observability.TokenUsage, error) {
	if err := writer.Event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": int(a.usage.OutputTokens)},
	}); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := writer.Event("message_stop", map[string]string{"type": "message_stop"}); err != nil {
		return observability.TokenUsage{}, err
	}
	writer.terminal = true
	return a.usage, nil
}
