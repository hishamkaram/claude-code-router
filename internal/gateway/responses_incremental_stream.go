package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
)

func produceResponsesStream(
	ctx context.Context,
	client *openairesponses.Client,
	providerName string,
	request *openairesponses.Request,
	events chan<- upstreamStreamEvent,
) {
	stream, err := client.StartStream(ctx, request)
	if err != nil {
		failure := responsesStreamStartFailure(providerName, err)
		sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
		return
	}
	defer func() { _ = stream.Close() }()
	if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
		kind:    upstreamStreamReady,
		status:  stream.StatusCode(),
		headers: stream.Header(),
	}) {
		return
	}
	for {
		data, ok, streamErr := stream.Next()
		if streamErr != nil {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind: upstreamStreamFailed,
				failure: &streamFailure{
					errorClass: "provider_stream",
					message:    "reading OpenAI Responses provider stream failed",
				},
			})
			return
		}
		if !ok {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
			return
		}
		eventType, typeErr := responsesStreamEventType(data)
		if typeErr != nil {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind: upstreamStreamFailed,
				failure: &streamFailure{
					errorClass: "provider_protocol",
					message:    "OpenAI Responses provider stream contained an invalid event",
				},
			})
			return
		}
		if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
			kind:     upstreamStreamData,
			sseEvent: eventType,
			data:     data,
		}) {
			return
		}
		if responsesStreamTerminal(eventType) {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
			return
		}
	}
}

func responsesStreamStartFailure(providerName string, err error) streamFailure {
	failure := streamFailure{
		errorClass: "provider_transport",
		message:    fmt.Sprintf("OpenAI Responses provider %q request failed", providerName),
	}
	var statusErr *openairesponses.HTTPError
	if errors.As(err, &statusErr) {
		failure.statusCode = statusErr.StatusCode
		failure.errorClass = "provider_http"
	}
	if errors.Is(err, openairesponses.ErrMalformedProviderOutput) {
		failure.statusCode = http.StatusOK
		failure.errorClass = "provider_protocol"
		failure.message = "OpenAI Responses provider did not honor the streaming request"
	}
	return failure
}

func writeOpenAIResponsesStreamFailure(w http.ResponseWriter, result translatedStreamResult) {
	status := result.HTTPStatus
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	message := result.Message
	if message == "" {
		message = "OpenAI Responses provider stream failed"
	}
	writeAnthropicError(w, status, message)
}

func responsesStreamEventType(data []byte) (string, error) {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return "", fmt.Errorf("decode Responses stream event: %w", err)
	}
	event.Type = strings.TrimSpace(event.Type)
	if event.Type == "" {
		return "", errors.New("responses stream event has no type")
	}
	return event.Type, nil
}

func responsesStreamTerminal(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

type responsesStreamAdapter struct {
	alias            string
	messageID        string
	inputTokens      int
	usage            observability.TokenUsage
	textBlocks       map[responsesTextBlockKey]*responsesTextBlock
	functionCalls    map[responsesOutputKey]*responsesFunctionCall
	outputDone       map[responsesOutputKey]bool
	nextBlock        int
	terminal         *openairesponses.Response
	terminalStreamID int
	terminalUsage    *observability.TokenUsage
	terminalType     string
	terminalFailure  bool
}

// responsesOutputKey scopes provider-local output indexes to one upstream
// response stream. Managed CUA can issue several provider requests while the
// client receives one Anthropic message stream.
type responsesOutputKey struct {
	streamID    int
	outputIndex int
}

type responsesTextBlockKey struct {
	streamID     int
	outputIndex  int
	contentIndex int
}

type responsesTextBlock struct {
	index   int
	closed  bool
	written bool
}

type responsesFunctionCall struct {
	item      openairesponses.OutputItem
	arguments strings.Builder
}

type responsesStreamEnvelope struct {
	Type         string                     `json:"type"`
	OutputIndex  int                        `json:"output_index"`
	ContentIndex int                        `json:"content_index"`
	Delta        string                     `json:"delta"`
	Text         string                     `json:"text"`
	Refusal      string                     `json:"refusal"`
	Name         string                     `json:"name"`
	CallID       string                     `json:"call_id"`
	Arguments    string                     `json:"arguments"`
	Item         openairesponses.OutputItem `json:"item"`
	Part         openairesponses.Content    `json:"part"`
	Response     *openairesponses.Response  `json:"response"`
}

func newResponsesStreamAdapter(alias, messageID string) *responsesStreamAdapter {
	return &responsesStreamAdapter{
		alias:         alias,
		messageID:     messageID,
		textBlocks:    make(map[responsesTextBlockKey]*responsesTextBlock),
		functionCalls: make(map[responsesOutputKey]*responsesFunctionCall),
		outputDone:    make(map[responsesOutputKey]bool),
	}
}

func (a *responsesStreamAdapter) Start(writer *anthropicSSEWriter) error {
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

func (a *responsesStreamAdapter) Consume(writer *anthropicSSEWriter, event upstreamStreamEvent) error {
	envelope, err := decodeResponsesStreamEnvelope(event)
	if err != nil {
		return err
	}
	if responsesStreamTerminal(envelope.Type) {
		return a.consumeTerminalEvent(event, envelope)
	}
	return a.consumeNonTerminalEvent(writer, event, envelope)
}

func decodeResponsesStreamEnvelope(event upstreamStreamEvent) (responsesStreamEnvelope, error) {
	var envelope responsesStreamEnvelope
	if err := json.Unmarshal(event.data, &envelope); err != nil {
		return responsesStreamEnvelope{}, fmt.Errorf("decoding Responses stream event: %w", err)
	}
	if strings.TrimSpace(envelope.Type) == "" {
		envelope.Type = event.sseEvent
	}
	return envelope, nil
}

func (a *responsesStreamAdapter) consumeNonTerminalEvent(writer *anthropicSSEWriter, event upstreamStreamEvent, envelope responsesStreamEnvelope) error {
	switch envelope.Type {
	case "response.created", "response.in_progress", "response.queued":
		return nil
	case "response.output_item.added":
		a.rememberOutputItem(event.streamID, envelope.OutputIndex, envelope.Item)
		return nil
	case "response.content_part.added":
		return a.consumeContentPart(writer, event.streamID, envelope)
	case "response.output_text.delta", "response.refusal.delta":
		return a.writeText(writer, event.streamID, envelope.OutputIndex, envelope.ContentIndex, envelope.Delta)
	case "response.output_text.done":
		return a.writeTerminalText(writer, event.streamID, envelope.OutputIndex, envelope.ContentIndex, envelope.Text)
	case "response.refusal.done":
		return a.writeTerminalText(writer, event.streamID, envelope.OutputIndex, envelope.ContentIndex, envelope.Refusal)
	case "response.function_call_arguments.delta":
		a.function(event.streamID, envelope.OutputIndex).arguments.WriteString(envelope.Delta)
		return nil
	case "response.function_call_arguments.done":
		a.updateFunction(event.streamID, envelope.OutputIndex, envelope)
		return nil
	case "response.output_item.done":
		return a.completeOutputItem(writer, event.streamID, envelope.OutputIndex, &envelope.Item)
	default:
		return nil
	}
}

func (a *responsesStreamAdapter) consumeTerminalEvent(event upstreamStreamEvent, envelope responsesStreamEnvelope) error {
	if envelope.Type == "error" {
		a.terminalFailure = true
		return nil
	}
	if envelope.Response == nil {
		return errors.New("responses terminal event has no response")
	}
	a.terminal = envelope.Response
	a.terminalStreamID = event.streamID
	if event.usage != nil {
		usage := *event.usage
		a.terminalUsage = &usage
	}
	a.terminalType = envelope.Type
	return nil
}

func (a *responsesStreamAdapter) completeOutputItem(writer *anthropicSSEWriter, streamID, outputIndex int, item *openairesponses.OutputItem) error {
	if err := a.consumeCompletedOutputItem(writer, streamID, outputIndex, item); err != nil {
		return err
	}
	a.outputDone[responsesOutputKey{streamID: streamID, outputIndex: outputIndex}] = true
	return nil
}

func (a *responsesStreamAdapter) consumeContentPart(writer *anthropicSSEWriter, streamID int, envelope responsesStreamEnvelope) error {
	switch envelope.Part.Type {
	case "output_text", "text":
		return a.writeText(writer, streamID, envelope.OutputIndex, envelope.ContentIndex, envelope.Part.Text)
	case "refusal":
		return a.writeText(writer, streamID, envelope.OutputIndex, envelope.ContentIndex, envelope.Part.Refusal)
	case "":
		return nil
	default:
		return fmt.Errorf("responses provider returned unsupported message content type %q", envelope.Part.Type)
	}
}

func (a *responsesStreamAdapter) rememberOutputItem(streamID, outputIndex int, item openairesponses.OutputItem) {
	if item.Type != "function_call" {
		return
	}
	function := a.function(streamID, outputIndex)
	function.item = mergeResponsesFunctionItem(function.item, item)
	if item.Arguments != "" {
		function.arguments.Reset()
		function.arguments.WriteString(item.Arguments)
	}
}

func (a *responsesStreamAdapter) updateFunction(streamID, outputIndex int, envelope responsesStreamEnvelope) {
	function := a.function(streamID, outputIndex)
	function.item = mergeResponsesFunctionItem(function.item, openairesponses.OutputItem{
		Type:      "function_call",
		CallID:    envelope.CallID,
		Name:      envelope.Name,
		Arguments: envelope.Arguments,
	})
	if envelope.Arguments != "" {
		function.arguments.Reset()
		function.arguments.WriteString(envelope.Arguments)
	}
}

func (a *responsesStreamAdapter) consumeCompletedOutputItem(writer *anthropicSSEWriter, streamID, outputIndex int, item *openairesponses.OutputItem) error {
	switch item.Type {
	case "message":
		return a.completeMessageItem(writer, streamID, outputIndex, item)
	case "function_call":
		a.rememberOutputItem(streamID, outputIndex, *item)
		return nil
	case "reasoning", "function_call_output", "computer_call_output", "tool_search_call", "tool_search_output", "":
		return nil
	case "computer_call":
		return errors.New("responses provider returned computer output outside the managed CUA path")
	default:
		return fmt.Errorf("responses provider returned unsupported output item type %q", item.Type)
	}
}

func (a *responsesStreamAdapter) completeMessageItem(writer *anthropicSSEWriter, streamID, outputIndex int, item *openairesponses.OutputItem) error {
	if item.Role != "" && item.Role != "assistant" {
		return fmt.Errorf("responses provider returned message role %q", item.Role)
	}
	for contentIndex := range item.Content {
		part := &item.Content[contentIndex]
		var value string
		switch part.Type {
		case "output_text", "text":
			value = part.Text
		case "refusal":
			value = part.Refusal
		default:
			return fmt.Errorf("responses provider returned unsupported message content type %q", part.Type)
		}
		if err := a.writeTerminalText(writer, streamID, outputIndex, contentIndex, value); err != nil {
			return err
		}
	}
	return a.closeTextBlocksForOutput(writer, streamID, outputIndex)
}

func (a *responsesStreamAdapter) writeText(writer *anthropicSSEWriter, streamID, outputIndex, contentIndex int, text string) error {
	block, err := a.textBlock(writer, streamID, outputIndex, contentIndex)
	if err != nil {
		return err
	}
	if block.closed {
		return errors.New("responses provider sent text after its content block stopped")
	}
	if text == "" {
		return nil
	}
	if err := writer.Event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": block.index,
		"delta": map[string]string{"type": "text_delta", "text": text},
	}); err != nil {
		return err
	}
	block.written = true
	return nil
}

func (a *responsesStreamAdapter) writeTerminalText(writer *anthropicSSEWriter, streamID, outputIndex, contentIndex int, text string) error {
	block, err := a.textBlock(writer, streamID, outputIndex, contentIndex)
	if err != nil {
		return err
	}
	if !block.written && text != "" {
		if err := a.writeText(writer, streamID, outputIndex, contentIndex, text); err != nil {
			return err
		}
	}
	return nil
}

func (a *responsesStreamAdapter) textBlock(writer *anthropicSSEWriter, streamID, outputIndex, contentIndex int) (*responsesTextBlock, error) {
	key := responsesTextKey(streamID, outputIndex, contentIndex)
	if block := a.textBlocks[key]; block != nil {
		return block, nil
	}
	block := &responsesTextBlock{index: a.nextBlock}
	a.nextBlock++
	if err := writer.Event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         block.index,
		"content_block": map[string]string{"type": "text", "text": ""},
	}); err != nil {
		return nil, err
	}
	a.textBlocks[key] = block
	return block, nil
}

func (a *responsesStreamAdapter) closeTextBlocksForOutput(writer *anthropicSSEWriter, streamID, outputIndex int) error {
	blocks := make([]*responsesTextBlock, 0)
	for key, block := range a.textBlocks {
		if key.streamID == streamID && key.outputIndex == outputIndex {
			blocks = append(blocks, block)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].index < blocks[j].index })
	for _, block := range blocks {
		if err := a.closeTextBlock(writer, block); err != nil {
			return err
		}
	}
	return nil
}

func (a *responsesStreamAdapter) closeAllTextBlocks(writer *anthropicSSEWriter) error {
	blocks := make([]*responsesTextBlock, 0, len(a.textBlocks))
	for _, block := range a.textBlocks {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].index < blocks[j].index })
	for _, block := range blocks {
		if err := a.closeTextBlock(writer, block); err != nil {
			return err
		}
	}
	return nil
}

func (a *responsesStreamAdapter) closeTextBlock(writer *anthropicSSEWriter, block *responsesTextBlock) error {
	if block.closed {
		return nil
	}
	if err := writer.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": block.index}); err != nil {
		return err
	}
	block.closed = true
	return nil
}

func (a *responsesStreamAdapter) function(streamID, outputIndex int) *responsesFunctionCall {
	key := responsesOutputKey{streamID: streamID, outputIndex: outputIndex}
	function := a.functionCalls[key]
	if function == nil {
		function = &responsesFunctionCall{}
		a.functionCalls[key] = function
	}
	return function
}

func (a *responsesStreamAdapter) Finish(writer *anthropicSSEWriter) (observability.TokenUsage, error) {
	if err := a.validateTerminal(); err != nil {
		return observability.TokenUsage{}, err
	}
	a.recordTerminalUsage()
	if err := a.completeTerminalOutput(writer); err != nil {
		return observability.TokenUsage{}, err
	}
	return a.finishTerminalResponse(writer)
}

func (a *responsesStreamAdapter) validateTerminal() error {
	if a.terminalFailure {
		return errors.New("OpenAI Responses provider stream returned an error event")
	}
	if a.terminal == nil {
		return errors.New("OpenAI Responses provider stream ended before a terminal response event")
	}
	if a.terminalType == "response.failed" {
		return errors.New("OpenAI Responses provider stream returned a failed response")
	}
	return openairesponses.ValidateStatus(a.terminal)
}

func (a *responsesStreamAdapter) recordTerminalUsage() {
	if a.terminalUsage != nil {
		a.usage = *a.terminalUsage
		return
	}
	a.usage = tokenUsageFromResponses(a.terminal)
}

func (a *responsesStreamAdapter) completeTerminalOutput(writer *anthropicSSEWriter) error {
	for outputIndex := range a.terminal.Output {
		key := responsesOutputKey{streamID: a.terminalStreamID, outputIndex: outputIndex}
		if a.outputDone[key] {
			continue
		}
		if err := a.consumeCompletedOutputItem(writer, a.terminalStreamID, outputIndex, &a.terminal.Output[outputIndex]); err != nil {
			return err
		}
	}
	return a.writeTerminalOutputText(writer)
}

func (a *responsesStreamAdapter) writeTerminalOutputText(writer *anthropicSSEWriter) error {
	if len(a.textBlocks) == 0 && strings.TrimSpace(a.terminal.OutputText) != "" {
		return a.writeText(writer, a.terminalStreamID, -1, 0, a.terminal.OutputText)
	}
	return nil
}

func (a *responsesStreamAdapter) finishTerminalResponse(writer *anthropicSSEWriter) (observability.TokenUsage, error) {
	tools := a.orderedFunctionCalls()
	if err := validateResponsesStreamTools(tools); err != nil {
		return observability.TokenUsage{}, err
	}
	if message := invalidStreamAgentToolInput(tools); message != "" {
		return a.finishInvalidAgentToolInput(writer, message)
	}
	if err := a.ensureVisibleTerminalContent(writer, tools); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.closeAllTextBlocks(writer); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.writeTerminalToolBlocks(writer, tools); err != nil {
		return observability.TokenUsage{}, err
	}
	return a.finishMessage(writer, a.terminalStopReason(tools))
}

func validateResponsesStreamTools(tools []openAIToolCall) error {
	for _, tool := range tools {
		if err := validateResponsesStreamTool(tool); err != nil {
			return err
		}
	}
	return nil
}

func validateResponsesStreamTool(tool openAIToolCall) error {
	if strings.TrimSpace(tool.ID) == "" || strings.TrimSpace(tool.Function.Name) == "" {
		return errors.New("responses provider streamed function_call missing call_id or name")
	}
	if strings.TrimSpace(tool.Function.Arguments) == "" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(tool.Function.Arguments), &decoded); err != nil {
		return fmt.Errorf("responses provider streamed function_call arguments are not valid JSON: %w", err)
	}
	return nil
}

func (a *responsesStreamAdapter) finishInvalidAgentToolInput(writer *anthropicSSEWriter, message string) (observability.TokenUsage, error) {
	if err := a.writeText(writer, a.terminalStreamID, -1, 0, message); err != nil {
		return observability.TokenUsage{}, err
	}
	if err := a.closeAllTextBlocks(writer); err != nil {
		return observability.TokenUsage{}, err
	}
	return a.finishMessage(writer, "end_turn")
}

func (a *responsesStreamAdapter) ensureVisibleTerminalContent(writer *anthropicSSEWriter, tools []openAIToolCall) error {
	if len(a.textBlocks) != 0 || len(tools) != 0 {
		return nil
	}
	return a.writeText(writer, a.terminalStreamID, -1, 0, "")
}

func (a *responsesStreamAdapter) writeTerminalToolBlocks(writer *anthropicSSEWriter, tools []openAIToolCall) error {
	for _, tool := range tools {
		if err := writeOpenAIToolStreamBlock(writer, a.nextBlock, tool); err != nil {
			return err
		}
		a.nextBlock++
	}
	return nil
}

func (a *responsesStreamAdapter) terminalStopReason(tools []openAIToolCall) string {
	if len(tools) > 0 {
		return "tool_use"
	}
	if strings.EqualFold(a.terminal.Status, "incomplete") &&
		a.terminal.IncompleteDetails != nil &&
		strings.EqualFold(a.terminal.IncompleteDetails.Reason, "max_output_tokens") {
		return "max_tokens"
	}
	return "end_turn"
}

func (a *responsesStreamAdapter) orderedFunctionCalls() []openAIToolCall {
	keys := make([]responsesOutputKey, 0, len(a.functionCalls))
	for key := range a.functionCalls {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].streamID == keys[j].streamID {
			return keys[i].outputIndex < keys[j].outputIndex
		}
		return keys[i].streamID < keys[j].streamID
	})
	tools := make([]openAIToolCall, 0, len(keys))
	for _, key := range keys {
		function := a.functionCalls[key]
		item := function.item
		if item.Type != "function_call" {
			continue
		}
		arguments := function.arguments.String()
		if arguments == "" {
			arguments = item.Arguments
		}
		tools = append(tools, openAIToolCall{
			ID:   item.CallID,
			Type: "function",
			Function: openAIFunctionCall{
				Name:      item.Name,
				Arguments: arguments,
			},
		})
	}
	return tools
}

func (a *responsesStreamAdapter) finishMessage(writer *anthropicSSEWriter, stopReason string) (observability.TokenUsage, error) {
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

func responsesTextKey(streamID, outputIndex, contentIndex int) responsesTextBlockKey {
	return responsesTextBlockKey{streamID: streamID, outputIndex: outputIndex, contentIndex: contentIndex}
}

func mergeResponsesFunctionItem(current, update openairesponses.OutputItem) openairesponses.OutputItem {
	if update.Type != "" {
		current.Type = update.Type
	}
	if update.CallID != "" {
		current.CallID = update.CallID
	}
	if update.Name != "" {
		current.Name = update.Name
	}
	if update.Arguments != "" {
		current.Arguments = update.Arguments
	}
	return current
}
