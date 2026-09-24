package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const maxAnthropicStreamBytes int64 = 32 << 20

func (h *handler) handleAnthropicPassThroughStream(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	endpoint string,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
	providerSecret string,
	responseModel string,
	spawnAlias string,
	firstParty bool,
) (observability.TokenUsage, translatedStreamResult) {
	adapter := &nativeAnthropicStreamAdapter{
		responseModel:            responseModel,
		preserveNativeChildInput: firstParty,
		agentChildMarker:         h.activeModel.agentChildMarker(claudeCodeSessionID(r), spawnAlias),
		agentTools:               make(map[int]*nativeAnthropicAgentTool),
	}
	result := runTranslatedProviderStream(
		r.Context(),
		w,
		func(ctx context.Context, events chan<- upstreamStreamEvent) {
			h.produceAnthropicStream(
				ctx,
				r,
				body,
				endpoint,
				provider,
				authMode,
				resource,
				providerSecret,
				firstParty,
				events,
			)
		},
		adapter,
	)
	if !result.Committed {
		h.writeAnthropicPassThroughStreamFailure(w, r, result, firstParty)
	}
	return result.Usage, result
}

func (h *handler) writeAnthropicPassThroughStreamFailure(w http.ResponseWriter, r *http.Request, result translatedStreamResult, firstParty bool) {
	if result.ErrorClass == "subscription_credential" {
		if firstParty {
			h.recordClaudeAuthState(r.Context(), claudeAuthBroken, "local_subscription_credential_unavailable")
			writeAnthropicAuthenticationError(w, http.StatusUnauthorized, claudeSubscriptionAuthFailureMessage())
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "Claude subscription credential is unavailable")
		return
	}
	if firstParty && (result.HTTPStatus == http.StatusUnauthorized || result.HTTPStatus == http.StatusForbidden) {
		h.recordClaudeAuthState(r.Context(), claudeAuthNeedsRelogin, "upstream_authentication_rejected")
		writeAnthropicAuthenticationError(w, result.HTTPStatus, claudeSubscriptionAuthFailureMessage())
		return
	}
	status := result.HTTPStatus
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	message := result.Message
	if message == "" {
		message = "Anthropic provider stream failed"
	}
	writeAnthropicError(w, status, message)
}

func (h *handler) produceAnthropicStream(
	ctx context.Context,
	r *http.Request,
	body []byte,
	endpoint string,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
	providerSecret string,
	firstParty bool,
	events chan<- upstreamStreamEvent,
) {
	resp, failure := h.openAnthropicStream(ctx, r, body, endpoint, provider, authMode, resource, providerSecret)
	if failure != nil {
		sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: failure})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if failure := h.validateAnthropicStreamResponse(ctx, resp, provider, authMode, resource, firstParty); failure != nil {
		sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
			kind:    upstreamStreamFailed,
			failure: failure,
		})
		return
	}
	if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
		kind:    upstreamStreamReady,
		status:  resp.StatusCode,
		headers: resp.Header.Clone(),
	}) {
		return
	}

	h.forwardAnthropicStreamFrames(ctx, resp.Body, events)
}

func (h *handler) openAnthropicStream(
	ctx context.Context,
	r *http.Request,
	body []byte,
	endpoint string,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
	providerSecret string,
) (*http.Response, *streamFailure) {
	resp, err := h.executeAnthropicPassThrough(
		r.WithContext(ctx),
		body,
		endpoint,
		provider,
		authMode,
		resource,
		providerSecret,
	)
	if err == nil {
		return resp, nil
	}
	failure := &streamFailure{
		errorClass: "provider_transport",
		message:    "requesting Anthropic provider failed",
	}
	if errors.Is(err, errAnthropicSubscriptionCredentialUnavailable) {
		failure.errorClass = "subscription_credential"
		failure.message = "Claude subscription credential is unavailable"
	}
	return nil, failure
}

func (h *handler) validateAnthropicStreamResponse(
	ctx context.Context,
	resp *http.Response,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
	firstParty bool,
) *streamFailure {
	h.observeClaudeAuthResponse(ctx, firstParty, resp.StatusCode)
	h.notifyAnthropicSubscriptionExhaustion(resp, provider, authMode, resource)
	switch {
	case firstParty && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		return &streamFailure{
			statusCode: resp.StatusCode,
			errorClass: "provider_auth",
			message:    claudeSubscriptionAuthFailureMessage(),
		}
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return &streamFailure{
			statusCode: resp.StatusCode,
			errorClass: "provider_http",
			message:    fmt.Sprintf("Anthropic provider %q returned HTTP %d", provider.Name, resp.StatusCode),
		}
	case !isOpenAIEventStream(resp.Header.Get("Content-Type")):
		return &streamFailure{
			statusCode: resp.StatusCode,
			errorClass: "provider_protocol",
			message:    "Anthropic provider did not honor the streaming request",
		}
	default:
		return nil
	}
}

func (h *handler) forwardAnthropicStreamFrames(
	ctx context.Context,
	body io.Reader,
	events chan<- upstreamStreamEvent,
) {
	scanner := newAnthropicSSEScanner(body)
	for {
		frame, ok, scanErr := scanner.next()
		if scanErr != nil {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
				kind: upstreamStreamFailed,
				failure: &streamFailure{
					errorClass: "provider_stream",
					message:    fmt.Sprintf("reading Anthropic provider stream: %v", scanErr),
				},
			})
			return
		}
		if !ok {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
			return
		}
		if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
			kind:     upstreamStreamData,
			sseEvent: frame.event,
			data:     frame.data,
		}) {
			return
		}
	}
}

type nativeAnthropicStreamAdapter struct {
	responseModel                string
	preserveNativeChildInput     bool
	agentChildMarker             func([]agentChildDescriptor) (func(), error)
	agentChildRollback           func()
	agentTools                   map[int]*nativeAnthropicAgentTool
	bufferingToolUse             bool
	bufferedEvents               []bufferedAnthropicEvent
	childToolsExposed            bool
	usage                        observability.TokenUsage
	upstreamReady                bool
	sawStop                      bool
	sawError                     bool
	nextVisibleContentBlockIndex int
}

func (a *nativeAnthropicStreamAdapter) ExposedChildTools() bool {
	return a.childToolsExposed
}

type bufferedAnthropicEvent struct {
	name string
	data []byte
}

type nativeAnthropicAgentTool struct {
	name     string
	input    strings.Builder
	hasDelta bool
}

func (a *nativeAnthropicStreamAdapter) Ready(writer *anthropicSSEWriter, headers http.Header) error {
	copyResponseHeaders(writer.w.Header(), headers)
	a.upstreamReady = true
	return nil
}

func (a *nativeAnthropicStreamAdapter) Start(writer *anthropicSSEWriter) error {
	if a.upstreamReady {
		return nil
	}
	return writer.Comment("ccr-stream-started")
}

func (a *nativeAnthropicStreamAdapter) Consume(writer *anthropicSSEWriter, event upstreamStreamEvent) error {
	name, err := anthropicStreamEventName(event)
	if err != nil {
		return err
	}
	if name == "error" {
		a.sawError = true
		return writer.ErrorWithType(anthropicStreamErrorType(event.data), "Anthropic provider stream failed")
	}
	data := event.data
	mergeTokenUsage(&a.usage, anthropicUsageFromJSON(data))
	a.observeAnthropicStreamTool(name, data)
	if rewritten, ok := rewriteAnthropicResponse(data, a.responseModel); ok {
		data = rewritten
	}
	return a.writeAnthropicStreamEvent(writer, name, data)
}

func anthropicStreamEventName(event upstreamStreamEvent) (string, error) {
	name := strings.TrimSpace(event.sseEvent)
	if name == "" {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event.data, &payload); err != nil {
			return "", fmt.Errorf("decoding Anthropic SSE event: %w", err)
		}
		name = strings.TrimSpace(payload.Type)
	}
	if name == "" {
		return "", errors.New("anthropic SSE event has no event name")
	}
	return name, nil
}

func (a *nativeAnthropicStreamAdapter) observeAnthropicStreamTool(name string, data []byte) {
	if a.preserveNativeChildInput {
		return
	}
	if name == "content_block_start" {
		if a.observeAgentToolStart(data) || anthropicToolUseStart(data) {
			// Hold tool blocks until the complete turn is validated. If a malformed
			// Agent/Task/Workflow block follows one, exposing the earlier tool would
			// leave Claude with a partial unsafe turn.
			a.bufferingToolUse = true
		}
	}
	if name == "content_block_delta" {
		a.observeAgentToolDelta(data)
	}
}

func (a *nativeAnthropicStreamAdapter) writeAnthropicStreamEvent(writer *anthropicSSEWriter, name string, data []byte) error {
	if a.bufferingToolUse {
		a.bufferedEvents = append(a.bufferedEvents, bufferedAnthropicEvent{name: name, data: data})
		if name == "message_stop" {
			return a.finishBufferedToolResponse(writer)
		}
		return nil
	}
	if name == "content_block_start" {
		a.observeVisibleContentBlock(data)
	}
	if err := writer.RawEvent(name, data); err != nil {
		return err
	}
	if name == "message_stop" {
		a.sawStop = true
		writer.terminal = true
	}
	return nil
}

func anthropicToolUseStart(data []byte) bool {
	var payload struct {
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(payload.ContentBlock.Type), "tool_use")
}

func (a *nativeAnthropicStreamAdapter) observeAgentToolStart(data []byte) bool {
	var payload struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &payload); err != nil ||
		!strings.EqualFold(strings.TrimSpace(payload.ContentBlock.Type), "tool_use") ||
		!isChildSpawnToolName(payload.ContentBlock.Name) {
		return false
	}
	if a.agentTools == nil {
		a.agentTools = make(map[int]*nativeAnthropicAgentTool)
	}
	tool := &nativeAnthropicAgentTool{name: payload.ContentBlock.Name}
	if len(payload.ContentBlock.Input) > 0 {
		tool.input.Write(payload.ContentBlock.Input)
	}
	a.agentTools[payload.Index] = tool
	return true
}

func (a *nativeAnthropicStreamAdapter) observeAgentToolDelta(data []byte) {
	var payload struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Delta.Type != "input_json_delta" {
		return
	}
	if tool := a.agentTools[payload.Index]; tool != nil {
		if !tool.hasDelta {
			tool.input.Reset()
			tool.hasDelta = true
		}
		tool.input.WriteString(payload.Delta.PartialJSON)
	}
}

func (a *nativeAnthropicStreamAdapter) agentChildDescriptors() []agentChildDescriptor {
	descriptors := make([]agentChildDescriptor, 0, len(a.agentTools))
	for _, tool := range a.agentTools {
		var input map[string]any
		if err := json.Unmarshal([]byte(tool.input.String()), &input); err != nil {
			continue
		}
		descriptors = append(descriptors, agentChildDescriptorsFromToolInput(tool.name, input)...)
	}
	return descriptors
}

func (a *nativeAnthropicStreamAdapter) exposedAgentChildDescriptors() []agentChildDescriptor {
	if !a.childToolsExposed {
		return nil
	}
	return a.agentChildDescriptors()
}

func (a *nativeAnthropicStreamAdapter) invalidChildToolMessage() string {
	for _, tool := range a.agentTools {
		raw := strings.TrimSpace(tool.input.String())
		if raw == "" {
			raw = "{}"
		}
		var input any
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return fmt.Sprintf("CCR provider compatibility error: external provider returned invalid %s tool input. The subagent was not started.", childToolDisplayName(tool.name))
		}
		if message := invalidChildToolInputMessage(tool.name, input); message != "" {
			return message
		}
	}
	return ""
}

func (a *nativeAnthropicStreamAdapter) finishBufferedToolResponse(writer *anthropicSSEWriter) error {
	if message := a.invalidChildToolMessage(); message != "" {
		a.childToolsExposed = false
		return a.finishInvalidChildToolResponse(writer, message)
	}
	descriptors := a.agentChildDescriptors()
	if len(descriptors) > 0 && a.agentChildMarker != nil {
		rollback, err := a.agentChildMarker(descriptors)
		if err != nil {
			return err
		}
		a.agentChildRollback = rollback
	}
	for _, event := range a.normalizedBufferedChildEvents() {
		if err := writer.RawEvent(event.name, event.data); err != nil {
			a.rollbackAgentChildren()
			return err
		}
	}
	a.bufferedEvents = nil
	a.bufferingToolUse = false
	a.childToolsExposed = len(descriptors) > 0
	a.sawStop = true
	writer.terminal = true
	return nil
}

func (a *nativeAnthropicStreamAdapter) finishInvalidChildToolResponse(writer *anthropicSSEWriter, message string) error {
	a.rollbackAgentChildren()
	a.bufferedEvents = nil
	a.bufferingToolUse = false
	index := a.nextVisibleContentBlockIndex
	if err := writer.Event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]string{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	if err := writer.Event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]string{"type": "text_delta", "text": message},
	}); err != nil {
		return err
	}
	if err := writer.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
		return err
	}
	if err := writer.Event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": int(a.usage.OutputTokens)},
	}); err != nil {
		return err
	}
	if err := writer.Event("message_stop", map[string]string{"type": "message_stop"}); err != nil {
		return err
	}
	a.sawStop = true
	writer.terminal = true
	return nil
}

func (a *nativeAnthropicStreamAdapter) observeVisibleContentBlock(data []byte) {
	index, ok := anthropicStreamEventIndex(data)
	if !ok || index < a.nextVisibleContentBlockIndex {
		return
	}
	a.nextVisibleContentBlockIndex = index + 1
}

func (a *nativeAnthropicStreamAdapter) normalizedBufferedChildEvents() []bufferedAnthropicEvent {
	canonicalInputs := canonicalAnthropicAgentToolInputs(a.agentTools)
	if len(canonicalInputs) == 0 {
		return a.bufferedEvents
	}

	emittedInput := make(map[int]bool, len(canonicalInputs))
	rewritten := make([]bufferedAnthropicEvent, 0, len(a.bufferedEvents)+len(canonicalInputs))
	for _, event := range a.bufferedEvents {
		rewritten = append(rewritten, normalizeBufferedAnthropicEvent(event, canonicalInputs, emittedInput)...)
	}
	return rewritten
}

func canonicalAnthropicAgentToolInputs(tools map[int]*nativeAnthropicAgentTool) map[int][]byte {
	inputs := make(map[int][]byte)
	for index, tool := range tools {
		if !isAgentChildToolName(tool.name) {
			continue
		}
		if input, ok := normalizeAnthropicAgentToolInput([]byte(tool.input.String())); ok {
			inputs[index] = input
		}
	}
	return inputs
}

func normalizeBufferedAnthropicEvent(event bufferedAnthropicEvent, canonicalInputs map[int][]byte, emittedInput map[int]bool) []bufferedAnthropicEvent {
	if event.name == "message_stop" {
		return appendMissingAnthropicChildInputDeltas(nil, canonicalInputs, emittedInput, event)
	}

	index, hasIndex := anthropicStreamEventIndex(event.data)
	canonical, isChild := canonicalInputs[index]
	if !hasIndex || !isChild {
		return []bufferedAnthropicEvent{event}
	}

	switch event.name {
	case "content_block_start":
		if data, ok := emptyAnthropicChildToolStart(event.data); ok {
			event.data = data
		}
	case "content_block_delta":
		if !isAnthropicInputJSONDelta(event.data) {
			return []bufferedAnthropicEvent{event}
		}
		if emittedInput[index] {
			return nil
		}
		if data, ok := rewriteAnthropicInputJSONDelta(event.data, canonical); ok {
			event.data = data
			emittedInput[index] = true
		}
	case "content_block_stop":
		if !emittedInput[index] {
			emittedInput[index] = true
			return []bufferedAnthropicEvent{
				{name: "content_block_delta", data: anthropicInputJSONDelta(index, canonical)},
				event,
			}
		}
	}
	return []bufferedAnthropicEvent{event}
}

func appendMissingAnthropicChildInputDeltas(result []bufferedAnthropicEvent, canonicalInputs map[int][]byte, emittedInput map[int]bool, terminal bufferedAnthropicEvent) []bufferedAnthropicEvent {
	for _, index := range sortedAnthropicChildIndices(canonicalInputs) {
		if emittedInput[index] {
			continue
		}
		result = append(result, bufferedAnthropicEvent{
			name: "content_block_delta",
			data: anthropicInputJSONDelta(index, canonicalInputs[index]),
		})
		emittedInput[index] = true
	}
	return append(result, terminal)
}

func sortedAnthropicChildIndices(inputs map[int][]byte) []int {
	indices := make([]int, 0, len(inputs))
	for index := range inputs {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func anthropicStreamEventIndex(data []byte) (int, bool) {
	var payload struct {
		Index *int `json:"index"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Index == nil {
		return 0, false
	}
	return *payload.Index, true
}

func emptyAnthropicChildToolStart(data []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, false
	}
	contentBlock, ok := payload["content_block"].(map[string]any)
	if !ok {
		return data, false
	}
	contentBlock["input"] = map[string]any{}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return rewritten, true
}

func isAnthropicInputJSONDelta(data []byte) bool {
	var payload struct {
		Delta struct {
			Type string `json:"type"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false
	}
	return payload.Delta.Type == "input_json_delta"
}

func rewriteAnthropicInputJSONDelta(data, partialJSON []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return data, false
	}
	delta, ok := payload["delta"].(map[string]any)
	if !ok {
		return data, false
	}
	delta["partial_json"] = string(partialJSON)
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return rewritten, true
}

func anthropicInputJSONDelta(index int, partialJSON []byte) []byte {
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(partialJSON),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
	}
	return data
}

func (a *nativeAnthropicStreamAdapter) Finish(_ *anthropicSSEWriter) (observability.TokenUsage, error) {
	if a.sawError {
		// A provider may append a transport error after message_stop. Once the
		// terminal child tool envelope was exposed, Claude can already start the
		// child; rolling the reservation back here would make that child lose its
		// spawn-time route (and could send it to native Anthropic after /model).
		if !a.childToolsExposed {
			a.rollbackAgentChildren()
		} else {
			a.agentChildRollback = nil
		}
		return a.usage, errors.New("anthropic provider stream returned an error event")
	}
	if !a.sawStop {
		a.rollbackAgentChildren()
		return a.usage, errors.New("anthropic provider stream ended before message_stop")
	}
	a.agentChildRollback = nil
	return a.usage, nil
}

func (a *nativeAnthropicStreamAdapter) rollbackAgentChildren() {
	if a.agentChildRollback == nil {
		return
	}
	rollback := a.agentChildRollback
	a.agentChildRollback = nil
	rollback()
}

func (a *nativeAnthropicStreamAdapter) Cleanup() {
	// The coordinator calls Cleanup when an upstream failure arrives before it
	// can call Finish. A provider may emit message_stop, exposing a valid child
	// tool, and then fail; that child can already start and must retain its
	// spawn-time route. Failed or incomplete turns that never exposed a child
	// still roll their reservation back normally.
	if a.childToolsExposed {
		a.agentChildRollback = nil
	} else {
		a.rollbackAgentChildren()
	}
	a.bufferedEvents = nil
	a.agentTools = nil
	a.bufferingToolUse = false
}

// anthropicStreamErrorType retains the documented, non-secret error category
// that Claude Code can act on while refusing to echo arbitrary provider data.
func anthropicStreamErrorType(data []byte) string {
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "api_error"
	}
	switch strings.TrimSpace(payload.Error.Type) {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "request_too_large", "rate_limit_error", "api_error", "overloaded_error":
		return payload.Error.Type
	default:
		return "api_error"
	}
}
