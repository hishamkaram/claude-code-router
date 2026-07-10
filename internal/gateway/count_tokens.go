package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	ccrTokenCountModeHeader     = "X-CCR-Token-Count-Mode"
	ccrTokenCountFallbackHeader = "X-CCR-Token-Count-Fallback"

	tokenCountModeProvider  = "provider"
	tokenCountModeEstimated = "estimated"

	tokenCountFallbackSecret          = "secret"
	tokenCountFallbackTransport       = "transport"
	tokenCountFallbackUpstreamStatus  = "upstream-status"
	tokenCountFallbackInvalidResponse = "invalid-response"
)

func (h *handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	req, body, ok := decodeAnthropicRequest(w, r)
	if !ok {
		return
	}
	route, validationErr := h.selectRoute(r.Context(), req.Model)
	if validationErr != nil {
		writeAnthropicError(w, validationErr.status, validationErr.message)
		return
	}
	if route.kind == routeAnthropic {
		h.handleAnthropicCountTokens(w, r, route, body)
		return
	}
	h.handleOpenAICountTokens(w, r, route, body)
}

func (h *handler) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request, route messageRoute, body []byte) {
	if !route.capabilities.SupportsCountTokens {
		if writeTokenCountCanceled(w, r.Context()) {
			return
		}
		writeEstimatedTokenCount(w, body, "")
		return
	}
	passBody, ok := rewriteCountTokenBody(w, body, route.model.ProviderModel)
	if !ok {
		return
	}
	w.Header().Set(ccrTokenCountModeHeader, tokenCountModeProvider)
	h.handleAnthropicPassThrough(w, r, passBody, route.anthropicProvider, route.anthropicAuth, route.responseModel)
}

func (h *handler) handleOpenAICountTokens(w http.ResponseWriter, r *http.Request, route messageRoute, body []byte) {
	if !route.capabilities.SupportsCountTokens {
		if writeTokenCountCanceled(w, r.Context()) {
			return
		}
		writeEstimatedTokenCount(w, body, "")
		return
	}
	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, route.provider.SecretRef)
	if err != nil {
		if writeTokenCountCanceled(w, r.Context()) {
			return
		}
		writeEstimatedTokenCount(w, body, tokenCountFallbackSecret)
		return
	}
	passBody, ok := rewriteCountTokenBody(w, body, route.model.ProviderModel)
	if !ok {
		return
	}
	inputTokens, fallback, ok := h.callOpenAICompatibleCountTokens(r.Context(), route.provider, apiKey, passBody)
	if !ok {
		if writeTokenCountCanceled(w, r.Context()) {
			return
		}
		writeEstimatedTokenCount(w, body, fallback)
		return
	}
	writeProviderTokenCount(w, inputTokens)
}

func rewriteCountTokenBody(w http.ResponseWriter, body []byte, providerModel string) ([]byte, bool) {
	if providerModel == "" {
		return body, true
	}
	rewritten, err := rewriteAnthropicRequestModel(body, providerModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return rewritten, true
}

func (h *handler) callOpenAICompatibleCountTokens(ctx context.Context, provider store.Provider, apiKey string, body []byte) (inputTokens int, fallback string, ok bool) {
	endpoint, err := providers.MessagesCountTokensEndpoint(provider.BaseURL)
	if err != nil {
		return 0, tokenCountFallbackTransport, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, tokenCountFallbackTransport, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", false
		}
		return 0, tokenCountFallbackTransport, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, tokenCountFallbackUpstreamStatus, false
	}
	tokens, err := decodeCountTokensResponse(resp.Body)
	if err != nil {
		return 0, tokenCountFallbackInvalidResponse, false
	}
	return tokens, "", true
}

func decodeCountTokensResponse(body io.Reader) (int, error) {
	var decoded struct {
		InputTokens *int `json:"input_tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&decoded); err != nil {
		return 0, fmt.Errorf("decoding count_tokens response: %w", err)
	}
	if decoded.InputTokens == nil {
		return 0, fmt.Errorf("count_tokens response missing input_tokens")
	}
	if *decoded.InputTokens < 0 {
		return 0, fmt.Errorf("count_tokens response input_tokens is negative")
	}
	return *decoded.InputTokens, nil
}

func writeProviderTokenCount(w http.ResponseWriter, inputTokens int) {
	w.Header().Set(ccrTokenCountModeHeader, tokenCountModeProvider)
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": inputTokens})
}

func writeEstimatedTokenCount(w http.ResponseWriter, body []byte, fallback string) {
	w.Header().Set(ccrTokenCountModeHeader, tokenCountModeEstimated)
	if fallback != "" {
		w.Header().Set(ccrTokenCountFallbackHeader, fallback)
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimatedTokenCount(body)})
}

func writeTokenCountCanceled(w http.ResponseWriter, ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	writeAnthropicError(w, http.StatusRequestTimeout, "token counting canceled")
	return true
}

func estimatedTokenCount(body []byte) int {
	if len(body) == 0 {
		return 1
	}
	return len(body)
}
