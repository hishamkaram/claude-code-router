package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const autoModeClassifierSystemPrefix = "You are a security monitor for autonomous AI coding agents."

const claudeCodeSessionIDHeader = "x-claude-code-session-id"

type activeModelSelection struct {
	mu           sync.RWMutex
	defaultAlias string
	aliases      map[string]string
}

func newActiveModelSelection(alias string) activeModelSelection {
	return activeModelSelection{
		defaultAlias: strings.TrimSpace(alias),
		aliases:      make(map[string]string),
	}
}

func (s *activeModelSelection) currentAlias(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sessionID != "" {
		alias, observed := s.aliases[sessionID]
		if observed {
			return alias
		}
	}
	return s.defaultAlias
}

func (s *activeModelSelection) observe(sessionID string, route messageRoute) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	alias := ""
	if !route.firstPartyAnthropic {
		alias = route.model.Alias
	}
	s.mu.Lock()
	s.aliases[sessionID] = alias
	s.mu.Unlock()
}

func (h *handler) selectRouteForRequest(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, *requestValidationError) {
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

func (h *handler) selectMessageRouteForRequest(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, *requestValidationError) {
	route, validationErr := h.selectRouteForRequest(ctx, sessionID, req)
	classifier := isAutoModeClassifierRequest(req)
	if validationErr == nil && (!classifier || classifierExplicitlySelectedRoute(req.Model, route)) {
		h.activeModel.observe(sessionID, route)
	}
	return route, validationErr
}

func classifierExplicitlySelectedRoute(requestedModel string, route messageRoute) bool {
	if route.firstPartyAnthropic || route.model.Alias == "" {
		return false
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == route.model.Alias {
		return true
	}
	discovery := parseDiscoveryID(requestedModel)
	return discovery.valid && discovery.alias == route.model.Alias
}

func claudeCodeSessionID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(claudeCodeSessionIDHeader))
}

func isAutoModeClassifierRequest(req anthropicRequest) bool {
	switch system := req.System.(type) {
	case string:
		return hasAutoModeClassifierPrefix(system)
	case []any:
		for _, item := range system {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, ok := block["type"].(string)
			if !ok || blockType != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if ok && hasAutoModeClassifierPrefix(text) {
				return true
			}
		}
	}
	return false
}

func hasAutoModeClassifierPrefix(system string) bool {
	return strings.HasPrefix(strings.TrimSpace(system), autoModeClassifierSystemPrefix)
}
