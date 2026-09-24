package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	firstParty bool,
) (observability.TokenUsage, translatedStreamResult) {
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
		&nativeAnthropicStreamAdapter{responseModel: responseModel},
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
	scanner := newAnthropicSSEScanner(body, maxAnthropicStreamBytes)
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
	responseModel string
	usage         observability.TokenUsage
	upstreamReady bool
	sawStop       bool
	sawError      bool
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
	name := strings.TrimSpace(event.sseEvent)
	if name == "" {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event.data, &payload); err != nil {
			return fmt.Errorf("decoding Anthropic SSE event: %w", err)
		}
		name = strings.TrimSpace(payload.Type)
	}
	if name == "" {
		return errors.New("anthropic SSE event has no event name")
	}
	if name == "error" {
		a.sawError = true
		return writer.ErrorWithType(anthropicStreamErrorType(event.data), "Anthropic provider stream failed")
	}
	data := event.data
	mergeTokenUsage(&a.usage, anthropicUsageFromJSON(data))
	if rewritten, ok := rewriteAnthropicResponseModel(data, a.responseModel); ok {
		data = rewritten
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

func (a *nativeAnthropicStreamAdapter) Finish(_ *anthropicSSEWriter) (observability.TokenUsage, error) {
	if a.sawError {
		return a.usage, errors.New("anthropic provider stream returned an error event")
	}
	if !a.sawStop {
		return a.usage, errors.New("anthropic provider stream ended before message_stop")
	}
	return a.usage, nil
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

type anthropicSSEFrame struct {
	event string
	data  []byte
}

type anthropicSSEScanner struct {
	scanner   *bufio.Scanner
	limited   *io.LimitedReader
	eventName string
	eventData bytes.Buffer
	maxBytes  int64
	finished  bool
}

func newAnthropicSSEScanner(body io.Reader, maxBytes int64) *anthropicSSEScanner {
	limited := &io.LimitedReader{R: body, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), int(maxBytes+1))
	return &anthropicSSEScanner{scanner: scanner, limited: limited, maxBytes: maxBytes}
}

func (s *anthropicSSEScanner) next() (anthropicSSEFrame, bool, error) {
	if s.finished {
		return anthropicSSEFrame{}, false, nil
	}
	for s.scanner.Scan() {
		line := trimSSELineTerminator(s.scanner.Bytes())
		if len(line) == 0 {
			if frame, ok := s.take(); ok {
				return frame, true, nil
			}
			continue
		}
		s.appendLine(line)
	}
	s.finished = true
	if s.limited.N <= 0 {
		return anthropicSSEFrame{}, false, fmt.Errorf("provider stream exceeds the %d byte limit", s.maxBytes)
	}
	if err := s.scanner.Err(); err != nil {
		return anthropicSSEFrame{}, false, fmt.Errorf("reading provider stream: %w", err)
	}
	if frame, ok := s.take(); ok {
		return frame, true, nil
	}
	return anthropicSSEFrame{}, false, nil
}

func (s *anthropicSSEScanner) appendLine(line []byte) {
	if bytes.HasPrefix(line, []byte("event:")) {
		s.eventName = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	if len(data) > 0 && data[0] == ' ' {
		data = data[1:]
	}
	if s.eventData.Len() > 0 {
		s.eventData.WriteByte('\n')
	}
	_, _ = s.eventData.Write(data)
}

func (s *anthropicSSEScanner) take() (anthropicSSEFrame, bool) {
	data := bytes.Clone(bytes.TrimSpace(s.eventData.Bytes()))
	event := s.eventName
	s.eventData.Reset()
	s.eventName = ""
	if len(data) == 0 {
		return anthropicSSEFrame{}, false
	}
	return anthropicSSEFrame{event: event, data: data}, true
}
