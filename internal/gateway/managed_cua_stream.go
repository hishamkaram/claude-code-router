package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
)

// produceManagedResponsesStream keeps the client-visible Anthropic stream open
// while managed CUA completes any internal computer turns. Every provider turn
// remains an SSE request; the coordinator emits protocol-valid pings while the
// managed executor acts between those requests.
func (h *handler) produceManagedResponsesStream(
	ctx context.Context,
	client *openairesponses.Client,
	providerName string,
	initial *openairesponses.Request,
	events chan<- upstreamStreamEvent,
) {
	current := initial
	var totalUsage openairesponses.Usage
	usageObserved := false

	for streamID := 0; ; streamID++ {
		turn, failure := readManagedResponsesStreamTurn(ctx, client, providerName, current, streamID, events)
		if failure != nil {
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: failure})
			return
		}
		if statusErr := openairesponses.ValidateStatus(turn.response); statusErr != nil {
			failure := streamFailure{
				statusCode: http.StatusBadGateway,
				errorClass: "provider_protocol",
				message:    "OpenAI Responses provider stream returned an unsuccessful response",
			}
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
			return
		}

		totalUsage.InputTokens += turn.response.Usage.InputTokens
		totalUsage.OutputTokens += turn.response.Usage.OutputTokens
		usageObserved = usageObserved || turn.response.UsageObserved || turn.response.Usage.InputTokens != 0 || turn.response.Usage.OutputTokens != 0

		calls, hasFunction := computerCalls(turn.response.Output)
		if hasFunction && len(calls) > 0 {
			failure := managedCUAStreamFailure(newManagedCUAError(http.StatusNotImplemented, "managed computer-use response mixed computer and host function calls; no action was executed"))
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
			return
		}
		if len(calls) == 0 {
			usage := observability.TokenUsage{
				Observed:     usageObserved,
				InputTokens:  int64(totalUsage.InputTokens),
				OutputTokens: int64(totalUsage.OutputTokens),
			}
			turn.terminal.usage = &usage
			if !sendUpstreamStreamEvent(ctx, events, turn.terminal) {
				return
			}
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamDone})
			return
		}
		if strings.TrimSpace(turn.response.ID) == "" {
			failure := managedCUAStreamFailure(newManagedCUAError(http.StatusBadGateway, "managed computer-use provider response did not include a response id"))
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
			return
		}
		if beginErr := h.cfg.ManagedCUA.BeginTurn(ctx); beginErr != nil {
			failure := managedCUAStreamFailure(managedCUATurnError(beginErr))
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
			return
		}
		outputs, err := h.managedComputerOutputs(ctx, calls)
		if err != nil {
			failure := managedCUAStreamFailure(err)
			sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{kind: upstreamStreamFailed, failure: &failure})
			return
		}
		current = managedComputerFollowUp(initial, turn.response.ID, outputs)
	}
}

type managedResponsesStreamTurn struct {
	response *openairesponses.Response
	terminal upstreamStreamEvent
}

func readManagedResponsesStreamTurn(
	ctx context.Context,
	client *openairesponses.Client,
	providerName string,
	request *openairesponses.Request,
	streamID int,
	events chan<- upstreamStreamEvent,
) (*managedResponsesStreamTurn, *streamFailure) {
	stream, err := client.StartStream(ctx, request)
	if err != nil {
		failure := responsesStreamStartFailure(providerName, err)
		return nil, &failure
	}
	defer func() { _ = stream.Close() }()
	if !sendUpstreamStreamEvent(ctx, events, upstreamStreamEvent{
		kind:     upstreamStreamReady,
		status:   stream.StatusCode(),
		headers:  stream.Header(),
		streamID: streamID,
	}) {
		return nil, &streamFailure{errorClass: "canceled", message: "client request canceled"}
	}
	return readManagedResponsesStreamEvents(ctx, stream, streamID, events)
}

func readManagedResponsesStreamEvents(
	ctx context.Context,
	stream *openairesponses.Stream,
	streamID int,
	events chan<- upstreamStreamEvent,
) (*managedResponsesStreamTurn, *streamFailure) {
	for {
		data, ok, streamErr := stream.Next()
		if streamErr != nil {
			return nil, &streamFailure{errorClass: "provider_stream", message: "reading OpenAI Responses provider stream failed"}
		}
		if !ok {
			return nil, &streamFailure{errorClass: "provider_stream", message: "OpenAI Responses provider stream ended before a terminal response event"}
		}
		turn, event, failure := classifyManagedResponsesStreamEvent(data, streamID)
		if failure != nil {
			return nil, failure
		}
		if turn != nil {
			return turn, nil
		}
		if event != nil && !sendUpstreamStreamEvent(ctx, events, *event) {
			return nil, &streamFailure{errorClass: "canceled", message: "client request canceled"}
		}
	}
}

func classifyManagedResponsesStreamEvent(data []byte, streamID int) (*managedResponsesStreamTurn, *upstreamStreamEvent, *streamFailure) {
	eventType, err := responsesStreamEventType(data)
	if err != nil {
		return nil, nil, &streamFailure{errorClass: "provider_protocol", message: "OpenAI Responses provider stream contained an invalid event"}
	}
	if responsesStreamTerminal(eventType) {
		return managedResponsesTerminalEvent(data, streamID, eventType)
	}
	forward, err := forwardManagedResponsesStreamEvent(eventType, data)
	if err != nil {
		return nil, nil, &streamFailure{errorClass: "provider_protocol", message: "OpenAI Responses provider stream contained an invalid output event"}
	}
	if !forward {
		return nil, nil, nil
	}
	event := upstreamStreamEvent{
		kind:     upstreamStreamData,
		streamID: streamID,
		sseEvent: eventType,
		data:     data,
	}
	return nil, &event, nil
}

func managedResponsesTerminalEvent(data []byte, streamID int, eventType string) (*managedResponsesStreamTurn, *upstreamStreamEvent, *streamFailure) {
	if eventType == "error" {
		return nil, nil, &streamFailure{errorClass: "provider_stream", message: "OpenAI Responses provider stream returned an error event"}
	}
	var terminal responsesStreamEnvelope
	if err := json.Unmarshal(data, &terminal); err != nil || terminal.Response == nil {
		return nil, nil, &streamFailure{errorClass: "provider_protocol", message: "OpenAI Responses provider stream contained an invalid terminal response"}
	}
	return &managedResponsesStreamTurn{
		response: terminal.Response,
		terminal: upstreamStreamEvent{
			kind:     upstreamStreamData,
			streamID: streamID,
			sseEvent: eventType,
			data:     data,
		},
	}, nil, nil
}

// Computer output is consumed by the managed executor and must not be exposed
// as an Anthropic host tool call. Other events retain their stream identity so
// text from separate provider turns cannot collide on output indexes.
func forwardManagedResponsesStreamEvent(eventType string, data []byte) (bool, error) {
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		var envelope struct {
			Item openairesponses.OutputItem `json:"item"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return false, err
		}
		return envelope.Item.Type != "computer_call", nil
	default:
		return true, nil
	}
}

func managedCUAStreamFailure(err error) streamFailure {
	var managedErr *managedCUAError
	if errors.As(err, &managedErr) {
		return streamFailure{
			statusCode: managedErr.status,
			errorClass: "managed_cua",
			message:    managedErr.message,
		}
	}
	return streamFailure{
		statusCode: http.StatusBadGateway,
		errorClass: "managed_cua",
		message:    "managed computer-use execution failed; no fallback was attempted",
	}
}
