package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

func (h *handler) selectRouteForRequest(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, *requestValidationError) {
	return h.selectRouteForRequestWithAgentInheritance(ctx, sessionID, req, true)
}

func (h *handler) selectRouteForTokenCountRequest(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, *requestValidationError) {
	if !isAutoModeClassifierRequest(req) {
		if route, handled, validationErr := h.selectAgentChildTokenCountRoute(ctx, sessionID, req); handled {
			return route, validationErr
		}
	}
	return h.selectRouteForRequestWithAgentInheritance(ctx, sessionID, req, false)
}

func (h *handler) selectAgentChildTokenCountRoute(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, bool, *requestValidationError) {
	reservation := h.activeModel.peekAgentChild(sessionID, req)
	if !reservation.pending && !reservation.identified {
		return messageRoute{}, false, nil
	}
	if reservation.ambiguous {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the first-party child token-count request matched multiple active CCR routing reservations; routing was refused rather than guessing",
		}
	}
	if reservation.expired {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the Agent child token-count request arrived after its CCR routing reservation expired; first-party Anthropic fallback was refused",
		}
	}
	if reservation.identified && !reservation.pending {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the first-party child token-count request could not be correlated with its CCR routing reservation; routing was refused rather than falling back to Anthropic",
		}
	}
	if reservation.alias == "" {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the active ccr model is unavailable for this Agent child token-count request; first-party Anthropic fallback was refused",
		}
	}
	route, validationErr := h.routeConfiguredAlias(ctx, reservation.alias, req.Model)
	if validationErr != nil {
		return messageRoute{}, true, validationErr
	}
	// Token counting is a child-side protocol probe. Mark it as child-routed so
	// the route cannot overwrite the parent session's explicitly selected alias
	// when the request is observed by the normal route lifecycle.
	route.agentChildRouted = true
	return route, true, nil
}

func (h *handler) selectRouteForRequestWithAgentInheritance(ctx context.Context, sessionID string, req anthropicRequest, inheritAgentChild bool) (messageRoute, *requestValidationError) {
	if inheritAgentChild && !isAutoModeClassifierRequest(req) {
		if route, handled, validationErr := h.selectAgentChildRoute(ctx, sessionID, req); handled {
			return route, validationErr
		}
	}
	if !isAutoModeClassifierRequest(req) {
		return h.selectRoute(ctx, req.Model)
	}
	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: "message request model is required"}
	}
	if route, explicit, validationErr := h.explicitClassifierRoute(ctx, requestedModel); explicit {
		return route, validationErr
	}
	alias := h.activeModel.currentAlias(sessionID)
	if alias == "" {
		return h.selectRoute(ctx, requestedModel)
	}
	route, validationErr := h.routeConfiguredAlias(ctx, alias, requestedModel)
	if validationErr != nil {
		return messageRoute{}, &requestValidationError{
			status: validationErr.status,
			message: fmt.Sprintf(
				"auto-mode classifier could not use active ccr model alias %q: %s; first-party Anthropic fallback was refused",
				alias,
				validationErr.message,
			),
		}
	}
	return route, nil
}

func (h *handler) selectAgentChildRoute(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, bool, *requestValidationError) {
	reservation := h.activeModel.reserveAgentChild(sessionID, req)
	if !reservation.pending && !reservation.identified {
		return messageRoute{}, false, nil
	}
	if reservation.ambiguous {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the first-party child request matched multiple active CCR routing reservations; routing was refused rather than guessing",
		}
	}
	if reservation.expired {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the Agent child request arrived after its CCR routing reservation expired; first-party Anthropic fallback was refused",
		}
	}
	if reservation.identified && !reservation.pending {
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the first-party child request could not be correlated with its CCR routing reservation; routing was refused rather than falling back to Anthropic",
		}
	}
	if reservation.alias == "" {
		if reservation.rollback != nil {
			reservation.rollback()
		}
		return messageRoute{}, true, &requestValidationError{
			status:  http.StatusServiceUnavailable,
			message: "the active ccr model is unavailable for this Agent child request; first-party Anthropic fallback was refused",
		}
	}
	route, validationErr := h.routeConfiguredAlias(ctx, reservation.alias, req.Model)
	if validationErr != nil {
		if reservation.rollback != nil {
			reservation.rollback()
		}
		return messageRoute{}, true, validationErr
	}
	route.agentChildRollback = reservation.rollback
	route.agentChildRouted = true
	return route, true, nil
}

func (h *handler) explicitClassifierRoute(ctx context.Context, requestedModel string) (messageRoute, bool, *requestValidationError) {
	lookup, validationErr := h.configuredAliasForRequest(ctx, requestedModel)
	if validationErr != nil {
		return messageRoute{}, true, validationErr
	}
	if !lookup.exists && !lookup.prefixed {
		return messageRoute{}, false, nil
	}
	route, validationErr := h.selectRoute(ctx, requestedModel)
	if validationErr != nil {
		return messageRoute{}, true, &requestValidationError{
			status: validationErr.status,
			message: fmt.Sprintf(
				"auto-mode classifier could not use requested ccr model %q: %s; first-party Anthropic fallback was refused",
				requestedModel,
				validationErr.message,
			),
		}
	}
	return route, true, nil
}
