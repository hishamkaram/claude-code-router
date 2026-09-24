package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/agentinput"
	"github.com/hishamkaram/claude-code-router/internal/observability"
)

// copyJSONProviderResponseBody registers child reservations before the response
// is exposed to Claude Code. The reservation and the tool envelope therefore
// describe one provider turn, and a failed downstream write can roll it back.
func copyJSONProviderResponseBody(dst io.Writer, src io.Reader, responseModel string, onAgentTools func([]agentChildDescriptor) (func(), error)) (observability.TokenUsage, error) {
	raw, err := readBoundedAnthropicProviderResponse(src)
	if err != nil {
		return observability.TokenUsage{}, writeBufferedAnthropicJSONFailure(dst, responseModel, err)
	}
	usage := anthropicUsageFromJSON(raw)
	if onAgentTools != nil {
		if normalized, ok := normalizeAnthropicChildTools(raw); ok {
			raw = normalized
		}
		if sanitized, ok := sanitizeAnthropicChildTools(raw); ok {
			raw = sanitized
		}
	}
	if rewritten, ok := rewriteAnthropicResponse(raw, responseModel); ok {
		raw = rewritten
	}
	var rollbackAgentChildren func()
	if onAgentTools != nil {
		var registrationErr error
		rollbackAgentChildren, registrationErr = onAgentTools(agentChildDescriptorsFromJSON(raw))
		if registrationErr != nil {
			raw = anthropicChildCompatibilityResponse(raw, responseModel, registrationErr.Error())
			rollbackAgentChildren = nil
		}
	}
	if _, err := dst.Write(raw); err != nil {
		if rollbackAgentChildren != nil {
			rollbackAgentChildren()
		}
		return usage, err
	}
	return usage, nil
}

// copySSEProviderResponseBody handles an SSE body returned for a non-stream
// request. The request path is non-streaming, so buffering the complete body
// lets CCR discover the complete child tool input and register its routing
// reservation before exposing any part of the response.
func copySSEProviderResponseBody(dst io.Writer, src io.Reader, responseModel string, onAgentTools func([]agentChildDescriptor) (func(), error), preserveNativeChildInput bool) (observability.TokenUsage, error) {
	raw, err := readBoundedAnthropicProviderResponse(src)
	if err != nil {
		return observability.TokenUsage{}, writeBufferedAnthropicSSEFailure(dst, responseModel, err)
	}
	output := &anthropicSSEBuffer{}
	writer := newAnthropicSSEWriter(output)
	// This is an already-buffered compatibility response, so no downstream
	// headers or startup comment belong in the body. Mark the internal writer
	// started and run the same native Anthropic adapter used by live streams.
	writer.started = true
	adapter := &nativeAnthropicStreamAdapter{
		responseModel:            responseModel,
		preserveNativeChildInput: preserveNativeChildInput,
		// Registration belongs to this buffered response's downstream commit
		// point below. The adapter only validates/normalizes the complete
		// response; assigning the marker here would register once at message_stop
		// and again before the buffered bytes are written.
		agentTools: make(map[int]*nativeAnthropicAgentTool),
	}
	scanner := newAnthropicSSEScanner(bytes.NewReader(raw))
	for {
		frame, ok, scanErr := scanner.next()
		if scanErr != nil {
			return adapter.usage, writeBufferedAnthropicSSEFailure(dst, responseModel, scanErr)
		}
		if !ok {
			break
		}
		if consumeErr := adapter.Consume(writer, upstreamStreamEvent{
			kind:     upstreamStreamData,
			sseEvent: frame.event,
			data:     frame.data,
		}); consumeErr != nil {
			return adapter.usage, writeBufferedAnthropicSSEFailure(dst, responseModel, consumeErr)
		}
	}
	usage, err := adapter.Finish(writer)
	if err != nil {
		return usage, writeBufferedAnthropicSSEFailure(dst, responseModel, err)
	}
	outputBytes := output.Bytes()
	var rollbackAgentChildren func()
	if onAgentTools != nil {
		var registrationErr error
		rollbackAgentChildren, registrationErr = onAgentTools(adapter.exposedAgentChildDescriptors())
		if registrationErr != nil {
			outputBytes = anthropicChildCompatibilitySSEResponse(responseModel, registrationErr.Error())
			rollbackAgentChildren = nil
		}
	}
	if _, err := dst.Write(outputBytes); err != nil {
		if rollbackAgentChildren != nil {
			rollbackAgentChildren()
		}
		return usage, err
	}
	return usage, nil
}

// writeBufferedAnthropicSSEFailure keeps a successful upstream HTTP status from
// becoming an empty success when a buffered compatibility stream is malformed
// or reports an upstream error. The caller still receives the original error so
// routing state is not committed, while Claude receives a complete visible
// Anthropic response instead of an empty body.
func writeBufferedAnthropicSSEFailure(dst io.Writer, responseModel string, cause error) error {
	const message = "CCR provider compatibility error: external provider returned an incomplete or failed Anthropic stream. No successful turn was exposed."
	_, writeErr := dst.Write(anthropicChildCompatibilitySSEResponse(responseModel, message))
	if writeErr != nil {
		return errors.Join(cause, fmt.Errorf("writing visible Anthropic stream failure: %w", writeErr))
	}
	return cause
}

// writeBufferedAnthropicJSONFailure is the JSON equivalent of the buffered SSE
// failure path. The upstream status has already been committed by the
// pass-through handler, so a read failure must still produce a complete visible
// Anthropic response rather than an empty 2xx body.
func writeBufferedAnthropicJSONFailure(dst io.Writer, responseModel string, cause error) error {
	const message = "CCR provider compatibility error: external provider returned an incomplete or failed Anthropic response. No successful turn was exposed."
	_, writeErr := dst.Write(anthropicCompatibilityMessage(responseModel, message))
	if writeErr != nil {
		return errors.Join(cause, fmt.Errorf("writing visible Anthropic response failure: %w", writeErr))
	}
	return cause
}

func readBoundedAnthropicProviderResponse(src io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(src, maxAnthropicStreamBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxAnthropicStreamBytes {
		return nil, fmt.Errorf("provider response exceeds the %d byte limit", maxAnthropicStreamBytes)
	}
	return raw, nil
}

type anthropicSSEBuffer struct {
	bytes.Buffer
	header http.Header
}

func (*anthropicSSEBuffer) Flush() {}

func (b *anthropicSSEBuffer) Header() http.Header {
	if b.header == nil {
		b.header = make(http.Header)
	}
	return b.header
}

func (*anthropicSSEBuffer) WriteHeader(int) {}

func anthropicChildCompatibilitySSEResponse(responseModel, message string) []byte {
	model := responseModel
	if strings.TrimSpace(model) == "" {
		model = "claude"
	}
	messageStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_ccr_compatibility_error", "type": "message", "role": "assistant",
			"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	}
	frames := []struct {
		event   string
		payload any
	}{
		{"message_start", messageStart},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": message}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}}},
		{"message_stop", map[string]string{"type": "message_stop"}},
	}
	var output bytes.Buffer
	for _, frame := range frames {
		data, err := json.Marshal(frame.payload)
		if err != nil {
			continue
		}
		_, _ = output.WriteString("event: ")
		_, _ = output.WriteString(frame.event)
		_, _ = output.WriteString("\ndata: ")
		_, _ = output.Write(data)
		_, _ = output.WriteString("\n\n")
	}
	return output.Bytes()
}

func anthropicChildCompatibilityResponse(raw []byte, responseModel, message string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return anthropicCompatibilityMessage(responseModel, message)
	}
	if strings.TrimSpace(responseModel) != "" {
		payload["model"] = responseModel
	}
	payload["content"] = []any{map[string]any{"type": "text", "text": message}}
	payload["stop_reason"] = "end_turn"
	payload["stop_sequence"] = nil
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return rewritten
}

func anthropicCompatibilityMessage(responseModel, message string) []byte {
	payload := map[string]any{
		"id":            "msg_ccr_compatibility_error",
		"type":          "message",
		"role":          "assistant",
		"content":       []any{map[string]any{"type": "text", "text": message}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
	}
	if strings.TrimSpace(responseModel) != "" {
		payload["model"] = responseModel
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"CCR provider compatibility error: child work was refused"}}`)
	}
	return rewritten
}

// normalizeAnthropicChildTools converts provider-specific Agent/Task input
// variants into the canonical Claude Code tool envelope before the response is
// exposed. Agent children are a protocol boundary: the descriptor used for
// routing and the tool input received by Claude Code must describe the same
// request.
func normalizeAnthropicChildTools(raw []byte) ([]byte, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw, false
	}
	normalized, changed := normalizeAnthropicChildResponseValue(value)
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(normalized)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func normalizeAnthropicAgentToolInput(raw []byte) ([]byte, bool) {
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil || input == nil {
		return raw, false
	}
	normalized, err := json.Marshal(agentinput.Normalize(input))
	if err != nil {
		return raw, false
	}
	return normalized, true
}

func normalizeAnthropicChildResponseValue(value any) (any, bool) {
	content, ok := anthropicResponseContentBlocks(value)
	if !ok {
		return value, false
	}
	normalized, changed := normalizeAnthropicChildContentBlocks(content)
	if !changed {
		return value, false
	}
	if response, ok := value.(map[string]any); ok {
		if _, hasContent := response["content"]; hasContent {
			response["content"] = normalized
			return response, true
		}
	}
	return normalized, true
}

func normalizeAnthropicChildContentBlocks(value any) (any, bool) {
	switch current := value.(type) {
	case []any:
		changed := false
		for index, item := range current {
			normalized, itemChanged := normalizeAnthropicChildContentBlock(item)
			if !itemChanged {
				continue
			}
			current[index] = normalized
			changed = true
		}
		return current, changed
	case map[string]any:
		return normalizeAnthropicChildContentBlock(current)
	default:
		return value, false
	}
}

func normalizeAnthropicChildContentBlock(value any) (any, bool) {
	block, ok := value.(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") ||
		!isAgentChildToolName(stringValue(block["name"])) {
		return value, false
	}
	input, ok := block["input"].(map[string]any)
	if !ok {
		return value, false
	}
	block["input"] = agentinput.Normalize(input)
	return block, true
}

// sanitizeAnthropicChildTools converts malformed child-spawn tool calls into a
// visible compatibility message before the response reaches Claude Code. A
// malformed Agent/Task/Workflow call must never be exposed as a tool call: the
// next native-model request would otherwise have no request-correlated child
// reservation and could silently fall through to Claude subscription routing.
func sanitizeAnthropicChildTools(raw []byte) ([]byte, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw, false
	}
	sanitized, _, changed := sanitizeAnthropicChildResponseValue(value)
	if !changed {
		return raw, false
	}
	rewritten, err := json.Marshal(sanitized)
	if err != nil {
		return raw, false
	}
	return rewritten, true
}

func sanitizeAnthropicChildResponseValue(value any) (sanitized any, message string, changed bool) {
	content, ok := anthropicResponseContentBlocks(value)
	if !ok {
		return value, "", false
	}
	sanitizedContent, message, changed := sanitizeAnthropicChildContentBlocks(content)
	if !changed {
		return value, "", false
	}
	if response, ok := value.(map[string]any); ok {
		if _, hasContent := response["content"]; hasContent {
			// A malformed child tool invalidates the provider turn as a whole. Keep
			// any unrelated provider tool from being exposed with stop_reason=tool_use
			// while the child compatibility error is surfaced as ordinary text.
			if message != "" {
				response["content"] = []any{map[string]any{"type": "text", "text": message}}
				response["stop_reason"] = "end_turn"
			} else {
				response["content"] = sanitizedContent
			}
			return response, message, true
		}
	}
	return sanitizedContent, message, true
}

func sanitizeAnthropicChildContentBlocks(value any) (sanitized any, message string, changed bool) {
	switch current := value.(type) {
	case []any:
		for index, item := range current {
			sanitizedBlock, blockMessage, blockChanged := sanitizeAnthropicChildContentBlock(item)
			if !blockChanged {
				continue
			}
			current[index] = sanitizedBlock
			changed = true
			if message == "" {
				message = blockMessage
			}
		}
		return current, message, changed
	case map[string]any:
		return sanitizeAnthropicChildContentBlock(current)
	default:
		return value, "", false
	}
}

func sanitizeAnthropicChildContentBlock(value any) (sanitized any, message string, changed bool) {
	block, ok := value.(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") ||
		!isChildSpawnToolName(stringValue(block["name"])) {
		return value, "", false
	}
	if compatibilityMessage := invalidChildToolInputMessage(stringValue(block["name"]), block["input"]); compatibilityMessage != "" {
		return map[string]any{"type": "text", "text": compatibilityMessage}, compatibilityMessage, true
	}
	return value, "", false
}

func agentChildDescriptorsFromJSON(raw []byte) []agentChildDescriptor {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return agentChildDescriptorsFromValue(payload)
}

func countAnthropicAgentToolsValue(value any) int {
	normalized, ok := normalizedAgentChildResponseValue(value)
	if !ok {
		return 0
	}
	content, ok := anthropicResponseContentBlocks(normalized)
	if !ok {
		return 0
	}
	var blocks []any
	switch current := content.(type) {
	case []any:
		blocks = current
	case map[string]any:
		blocks = []any{current}
	default:
		return 0
	}
	count := 0
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if ok && strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") &&
			isAgentChildToolName(stringValue(block["name"])) {
			count++
		}
	}
	return count
}
