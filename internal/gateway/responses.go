package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
	"github.com/hishamkaram/claude-code-router/internal/secret"
)

func (h *handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request, req anthropicRequest, route messageRoute) observability.TokenUsage {
	var usage observability.TokenUsage
	if validationErr := validateResponsesMessageRequest(&req); validationErr != nil {
		writeAnthropicError(w, validationErr.status, validationErr.message)
		return usage
	}
	usesComputer := cua.UsesComputerTool(req.Tools)
	if managedErr := h.responsesComputerUseAvailabilityError(usesComputer); managedErr != nil {
		writeAnthropicError(w, managedErr.status, managedErr.message)
		return usage
	}
	addIgnoredAnthropicFieldsHeader(w.Header(), ignoredOpenAIAnthropicFields(req.Fields))
	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, route.provider.SecretRef)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(route.provider.SecretRef)))
		return usage
	}
	providerRequest, err := h.toResponsesRequest(r.Context(), req, route)
	if err != nil {
		writeAnthropicError(w, http.StatusNotImplemented, err.Error())
		return usage
	}
	client, err := openairesponses.NewClient(openairesponses.ClientOptions{
		BaseURL:    route.provider.BaseURL,
		APIKey:     apiKey,
		HTTPClient: h.httpClient(),
	})
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("creating OpenAI Responses client for provider %q: %v", route.provider.Name, err))
		return usage
	}
	providerResponse, err := h.createResponses(r.Context(), client, providerRequest, usesComputer)
	if err != nil {
		var managedErr *managedCUAError
		if errors.As(err, &managedErr) {
			writeAnthropicError(w, managedErr.status, managedErr.message)
			return usage
		}
		status := http.StatusBadGateway
		var statusErr *openairesponses.HTTPError
		if errors.As(err, &statusErr) && statusErr.StatusCode >= http.StatusBadRequest && statusErr.StatusCode <= 599 {
			status = statusErr.StatusCode
		}
		writeAnthropicError(w, status, fmt.Sprintf("OpenAI Responses provider %q: %v", route.provider.Name, err))
		return usage
	}
	usage = tokenUsageFromResponses(providerResponse)
	antResponse, err := openairesponses.AnthropicResponseFromResponses(providerResponse)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return usage
	}
	antResponse.Model = route.responseModel
	if req.Stream {
		writeAnthropicResponsesStream(w, antResponse)
		return usage
	}
	writeJSON(w, http.StatusOK, antResponse)
	return usage
}

func tokenUsageFromResponses(response *openairesponses.Response) observability.TokenUsage {
	if response == nil {
		return observability.TokenUsage{}
	}
	return observability.TokenUsage{
		Observed:     response.UsageObserved || response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 0,
		InputTokens:  int64(response.Usage.InputTokens),
		OutputTokens: int64(response.Usage.OutputTokens),
	}
}

func validateResponsesMessageRequest(req *anthropicRequest) *requestValidationError {
	for field := range req.Fields {
		switch field {
		case "model", "system", "messages", "max_tokens", "temperature", "stop_sequences", "stream", "tools", "tool_choice", "metadata", "thinking", "output_config", "context_management":
		default:
			return &requestValidationError{
				status:  http.StatusNotImplemented,
				message: fmt.Sprintf("Anthropic request field %q is not supported by the OpenAI Responses gateway path", field),
			}
		}
	}
	if err := validateOpenAIContextManagement(req.Fields); err != nil {
		return err
	}
	if err := validateThinking(req.Thinking); err != nil {
		return &requestValidationError{status: http.StatusNotImplemented, message: err.Error()}
	}
	return nil
}

func (h *handler) toResponsesRequest(ctx context.Context, req anthropicRequest, route messageRoute) (*openairesponses.Request, error) {
	normalized, err := h.normalizeResponsesImages(ctx, req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encoding normalized Responses request: %w", err)
	}
	converted, err := openairesponses.RequestFromAnthropicMessagesJSON(raw)
	if err != nil {
		return nil, err
	}
	converted.Model = route.model.ProviderModel
	applyResponsesRouteIdentity(req, route, converted)
	if explicitlyFalse(route.modelCapabilities.SupportsParallelTools) && len(converted.Tools) > 0 {
		parallelDisabled := false
		converted.ParallelToolCalls = &parallelDisabled
	}
	return converted, nil
}

func applyResponsesRouteIdentity(req anthropicRequest, route messageRoute, converted *openairesponses.Request) {
	if converted == nil || !latestUserAsksModelIdentity(req.Messages) {
		return
	}
	identity, ok := openAIModelRoute{
		alias:                         route.model.Alias,
		providerName:                  route.provider.Name,
		providerModel:                 route.model.ProviderModel,
		requestModel:                  route.responseModel,
		suppressIdentitySystemMessage: explicitlyFalse(route.modelCapabilities.SupportsSystemMessages),
	}.identityContent()
	if !ok {
		return
	}
	converted.Instructions = appendResponsesInstructions(converted.Instructions, identity)
}

func appendResponsesInstructions(existing, addition string) string {
	existing = strings.TrimSpace(existing)
	addition = strings.TrimSpace(addition)
	switch {
	case existing == "":
		return addition
	case addition == "":
		return existing
	default:
		return existing + "\n\n" + addition
	}
}

func writeAnthropicResponsesStream(w http.ResponseWriter, response *openairesponses.AnthropicResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	blocks := anthropicBlocksFromResponses(response.Content)
	writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            firstNonEmpty(response.ID, "msg_ccr_responses"),
			"type":          "message",
			"role":          "assistant",
			"model":         response.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  response.Usage.InputTokens,
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
		if delta, ok := streamBlockDelta(block); ok {
			writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": delta,
			})
		}
		writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})
	}
	writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": response.StopReason, "stop_sequence": response.StopSequence,
		},
		"usage": map[string]int{"output_tokens": response.Usage.OutputTokens},
	})
	writeSSEEvent(w, flusher, "message_stop", map[string]string{"type": "message_stop"})
}

func anthropicBlocksFromResponses(content []openairesponses.AnthropicContentBlock) []map[string]any {
	blocks := make([]map[string]any, 0, len(content))
	for _, block := range content {
		converted := map[string]any{"type": block.Type}
		switch block.Type {
		case "text":
			converted["text"] = block.Text
		case "tool_use":
			converted["id"] = block.ID
			converted["name"] = block.Name
			converted["input"] = block.Input
		}
		blocks = append(blocks, converted)
	}
	return blocks
}
