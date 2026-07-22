package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type routeKind int

const (
	routeOpenAI routeKind = iota
	routeOpenAIResponses
	routeAnthropic
)

type anthropicAuthMode int

const (
	anthropicAuthProviderSecret anthropicAuthMode = iota
	anthropicAuthIncoming
)

type messageRoute struct {
	kind                routeKind
	model               store.Model
	provider            store.Provider
	anthropicProvider   *store.Provider
	anthropicAuth       anthropicAuthMode
	firstPartyAnthropic bool
	capabilities        providers.Capabilities
	modelCapabilities   modelcap.Values
	responseModel       string
}

func validateRouteMessageCapabilities(route messageRoute, req anthropicRequest) *requestValidationError {
	if validationErr := validateModelMessageCapabilities(route.model, route.modelCapabilities, req); validationErr != nil {
		return validationErr
	}
	if req.Stream && !route.capabilities.SupportsStreaming {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("streaming is not supported for model %q with provider protocol %q", req.Model, route.capabilities.Protocol)}
	}
	if requestUsesThinkingFeature(req) && !route.capabilities.SupportsThinking {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("thinking is not supported for model %q with provider protocol %q", req.Model, route.capabilities.Protocol)}
	}
	if cua.UsesComputerTool(req.Tools) {
		if !route.firstPartyAnthropic && (route.modelCapabilities.SupportsComputerUse == nil || !*route.modelCapabilities.SupportsComputerUse) {
			return unsupportedModelCapability(route.model, "computer use")
		}
		if route.kind == routeOpenAI {
			return &requestValidationError{
				status:  http.StatusNotImplemented,
				message: fmt.Sprintf("computer use for model %q requires an OpenAI Responses-capable route; Chat Completions fallback is disabled", req.Model),
			}
		}
	}
	if !anthropicRequestUsesTools(req) {
		return nil
	}
	if route.model.Status == "chat-only" {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("model alias %q is chat-only and cannot be used with tools", route.model.Alias)}
	}
	if route.capabilities.Mode == providers.ModeChatOnly || !route.capabilities.SupportsTools {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("provider protocol %q for model %q does not support tools", route.capabilities.Protocol, req.Model)}
	}
	return nil
}

func (h *handler) validateManagedRouteMessageCapabilities(route messageRoute, req anthropicRequest) *requestValidationError {
	if validationErr := validateRouteMessageCapabilities(route, req); validationErr != nil {
		return validationErr
	}
	if h.cfg.ManagedCUA != nil && cua.UsesComputerTool(req.Tools) &&
		!route.firstPartyAnthropic && route.kind != routeOpenAIResponses {
		return &requestValidationError{
			status:  http.StatusNotImplemented,
			message: fmt.Sprintf("managed computer use for model %q requires an OpenAI Responses-capable route; use client-managed CUA for Anthropic-compatible routes", req.Model),
		}
	}
	return nil
}

func (h *handler) selectRoute(ctx context.Context, requested string) (messageRoute, *requestValidationError) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: "message request model is required"}
	}
	aliasLookup, validationErr := h.configuredAliasForRequest(ctx, requested)
	if validationErr != nil {
		return messageRoute{}, validationErr
	}
	if aliasLookup.exists {
		return h.routeConfiguredAlias(ctx, aliasLookup.alias, requested)
	}
	if aliasLookup.prefixed {
		if aliasLookup.malformed {
			return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: fmt.Sprintf("model discovery alias %q is malformed", requested)}
		}
		return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: fmt.Sprintf("model discovery alias %q maps to unconfigured ccr alias %q", requested, aliasLookup.alias)}
	}
	if isFirstPartyAnthropicModelRequest(requested) {
		return h.routeFirstPartyAnthropicModel(requested), nil
	}
	return messageRoute{}, &requestValidationError{status: http.StatusBadGateway, message: fmt.Sprintf("model %q is not a configured ccr alias, a ccr discovery alias, or a first-party Anthropic model; refusing to route it to the startup alias", requested)}
}

type configuredAliasLookup struct {
	alias     string
	exists    bool
	prefixed  bool
	malformed bool
}

func (h *handler) configuredAliasForRequest(ctx context.Context, requested string) (configuredAliasLookup, *requestValidationError) {
	alias := requested
	aliasExists, err := h.cfg.Store.ModelExists(ctx, alias)
	if err != nil {
		return configuredAliasLookup{}, &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("checking requested model alias %q: %v", alias, err)}
	}
	if aliasExists {
		return configuredAliasLookup{alias: alias, exists: true}, nil
	}
	discovery := parseDiscoveryID(requested)
	if !discovery.prefixed {
		return configuredAliasLookup{alias: alias}, nil
	}
	if !discovery.valid {
		return configuredAliasLookup{prefixed: true, malformed: true}, nil
	}
	aliasExists, err = h.cfg.Store.ModelExists(ctx, discovery.alias)
	if err != nil {
		return configuredAliasLookup{}, &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("checking requested model alias %q: %v", discovery.alias, err)}
	}
	return configuredAliasLookup{alias: discovery.alias, exists: aliasExists, prefixed: true}, nil
}

func (h *handler) routeConfiguredAlias(ctx context.Context, alias, responseModel string) (messageRoute, *requestValidationError) {
	model, modelErr := h.cfg.Store.GetModel(ctx, alias)
	if modelErr != nil {
		return messageRoute{}, &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("reading requested model alias %q: %v", alias, modelErr)}
	}
	if model.Status == "blocked" {
		return messageRoute{}, &requestValidationError{status: http.StatusForbidden, message: fmt.Sprintf("model alias %q is blocked and cannot be routed", alias)}
	}
	provider, providerErr := h.cfg.Store.GetProvider(ctx, model.ProviderName)
	if providerErr != nil {
		return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: fmt.Sprintf("provider %q for model alias %q is not configured", model.ProviderName, alias)}
	}
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return messageRoute{}, &requestValidationError{
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("model alias %q targets LiteLLM control model %q and cannot be routed", alias, model.ProviderModel),
		}
	}
	effectiveModel, capabilityErr := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if capabilityErr != nil {
		return messageRoute{}, &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("computing capabilities for model alias %q: %v", alias, capabilityErr)}
	}
	if !modelcap.IsRoutableKind(effectiveModel.Values.Kind) {
		return messageRoute{}, unsupportedModelCapability(model, "kind "+effectiveModel.Values.Kind)
	}
	caps := effectiveProviderCapabilities(provider)
	if modelUsesResponsesAPI(effectiveModel.Values) {
		if !supportsResponsesRoute(caps, effectiveModel.Values) {
			return messageRoute{}, &requestValidationError{
				status:  http.StatusNotImplemented,
				message: fmt.Sprintf("model alias %q requires the OpenAI Responses API, but provider %q is not configured as Responses-capable", alias, provider.Name),
			}
		}
		return messageRoute{kind: routeOpenAIResponses, model: model, provider: provider, capabilities: caps, modelCapabilities: modelCapabilitiesForRoute(caps, effectiveModel.Values), responseModel: responseModel}, nil
	}
	if caps.Protocol == providers.ProtocolOpenAICompatible {
		return messageRoute{kind: routeOpenAI, model: model, provider: provider, capabilities: caps, modelCapabilities: modelCapabilitiesForRoute(caps, effectiveModel.Values), responseModel: responseModel}, nil
	}
	if caps.Protocol == providers.ProtocolAnthropicCompatible {
		rewrittenProvider := provider
		return messageRoute{kind: routeAnthropic, model: model, anthropicProvider: &rewrittenProvider, anthropicAuth: anthropicAuthProviderSecret, capabilities: caps, modelCapabilities: effectiveModel.Values, responseModel: responseModel}, nil
	}
	return messageRoute{}, &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("provider type %q with protocol %q is not supported by the gateway path", provider.Type, caps.Protocol)}
}

func modelUsesResponsesAPI(capabilities modelcap.Values) bool {
	return capabilities.Kind == modelcap.KindResponses || (capabilities.SupportsResponses != nil && *capabilities.SupportsResponses)
}

func supportsResponsesRoute(provider providers.Capabilities, model modelcap.Values) bool {
	return provider.Protocol == providers.ProtocolOpenAICompatible &&
		provider.SupportsResponses &&
		!explicitlyFalse(model.SupportsResponses)
}

func (h *handler) routeFirstPartyAnthropicModel(requested string) messageRoute {
	anthropicProvider := h.firstPartyAnthropicProvider()
	return messageRoute{
		kind:                routeAnthropic,
		model:               store.Model{Alias: requested},
		anthropicProvider:   &anthropicProvider,
		anthropicAuth:       anthropicAuthIncoming,
		firstPartyAnthropic: true,
		capabilities:        effectiveProviderCapabilities(anthropicProvider),
		responseModel:       requested,
	}
}

func (h *handler) firstPartyAnthropicProvider() store.Provider {
	baseURL := strings.TrimSpace(h.cfg.AnthropicBaseURL)
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return store.Provider{
		Name:                   "anthropic",
		Type:                   "anthropic",
		BaseURL:                baseURL,
		Protocol:               providers.ProtocolAnthropicCompatible,
		SupportsTools:          true,
		SupportsStreaming:      true,
		SupportsThinking:       true,
		SupportsModelDiscovery: true,
		SupportsCountTokens:    true,
		Mode:                   providers.ModeFull,
	}
}

func looksLikeAnthropicModelID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), "claude-")
}

func isFirstPartyAnthropicModelRequest(id string) bool {
	switch strings.TrimSpace(id) {
	case "default", "best", "fable", "sonnet", "opus", "haiku", "sonnet[1m]", "opus[1m]", "opusplan", "opusplan[1m]":
		return true
	default:
		return looksLikeAnthropicModelID(id) && !strings.HasPrefix(strings.TrimSpace(id), legacyDiscoveryAliasPrefix)
	}
}

func anthropicRequestUsesTools(req anthropicRequest) bool {
	if len(req.Tools) > 0 || rawJSONPresent(req.ToolChoice) {
		return true
	}
	for _, message := range req.Messages {
		if anthropicContentUsesTools(message.Content) {
			return true
		}
	}
	return false
}

func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func anthropicContentUsesTools(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		if blockType == "tool_use" || blockType == "tool_result" {
			return true
		}
	}
	return false
}
