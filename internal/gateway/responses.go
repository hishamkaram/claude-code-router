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

func (h *handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request, req anthropicRequest, route messageRoute, completion *routeCompletionState) observability.TokenUsage {
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
	addIgnoredAnthropicFieldsHeader(w.Header(), ignoredOpenAIRequestFields(req))
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
	messageID, err := newGatewayMessageID()
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "creating response message identifier")
		return usage
	}
	if req.Stream {
		return h.streamOpenAIResponses(w, r, route, completion, client, providerRequest, usesComputer, messageID)
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
	providerResponse.ID = messageID
	usage = tokenUsageFromResponses(providerResponse)
	antResponse, err := openairesponses.AnthropicResponseFromResponses(providerResponse)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return usage
	}
	antResponse.Model = route.responseModel
	writeJSON(w, http.StatusOK, antResponse)
	return usage
}

func (h *handler) streamOpenAIResponses(
	w http.ResponseWriter,
	r *http.Request,
	route messageRoute,
	completion *routeCompletionState,
	client *openairesponses.Client,
	providerRequest *openairesponses.Request,
	usesComputer bool,
	messageID string,
) observability.TokenUsage {
	adapter := newResponsesStreamAdapter(route.responseModel, messageID)
	adapter.inputTokens = estimateTranslatedInputTokens(providerRequest)
	producer := func(ctx context.Context, events chan<- upstreamStreamEvent) {
		produceResponsesStream(ctx, client, route.provider.Name, providerRequest, events)
	}
	if usesComputer {
		producer = func(ctx context.Context, events chan<- upstreamStreamEvent) {
			h.produceManagedResponsesStream(ctx, client, route.provider.Name, providerRequest, events)
		}
	}
	result := runTranslatedProviderStream(r.Context(), w, producer, adapter)
	recordStreamCompletion(completion, result)
	if !result.Committed {
		writeOpenAIResponsesStreamFailure(w, result)
	}
	return result.Usage
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
