package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type Config struct {
	Store             *store.Store
	Secrets           secret.Backend
	HTTPClient        *http.Client
	Token             string
	DefaultModelAlias string
	AnthropicBaseURL  string
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	url        string
}

func NewToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("gateway.NewToken: reading random token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("gateway.Start: store is required")
	}
	if cfg.Secrets == nil {
		cfg.Secrets = secret.DefaultBackend{}
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("gateway.Start: token is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gateway.Start: listening on loopback: %w", err)
	}

	handler := &handler{cfg: cfg}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	gateway := &Server{
		httpServer: server,
		listener:   listener,
		url:        "http://" + listener.Addr().String(),
	}
	errCh := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !strings.Contains(serveErr.Error(), "Server closed") {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = gateway.Shutdown(shutdownCtx)
		return nil, fmt.Errorf("gateway.Start: context canceled while starting: %w", ctx.Err())
	case err := <-errCh:
		if err != nil {
			return nil, fmt.Errorf("gateway.Start: serving: %w", err)
		}
		return nil, fmt.Errorf("gateway.Start: server stopped during startup")
	default:
	}
	return gateway, nil
}

func (s *Server) URL() string {
	if s == nil {
		return ""
	}
	return s.url
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("gateway.Shutdown: shutting down server: %w", err)
	}
	return nil
}

type handler struct {
	cfg Config
}

const (
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	discoveryAliasPrefix    = "claude-ccr-"
	ccrSessionTokenHeader   = "X-CCR-Session-Token"
	ccrSessionTokenLower    = "x-ccr-session-token"
	ccrIgnoredFieldsHeader  = "X-CCR-Ignored-Anthropic-Fields"
)

// DiscoveryAliasPrefix returns the Claude-compatible prefix used for CCR model aliases.
func DiscoveryAliasPrefix() string {
	return discoveryAliasPrefix
}

// DiscoveryIDForAlias returns the Claude-compatible model ID for a CCR alias.
func DiscoveryIDForAlias(alias string) string {
	return discoveryAliasPrefix + alias
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead && r.URL.Path == "/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !h.authorized(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "invalid local gateway token")
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models") {
		h.handleModels(w, r)
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/messages") {
		h.handleMessages(w, r)
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages/count_tokens" || r.URL.Path == "/messages/count_tokens") {
		h.handleCountTokens(w, r)
		return
	}
	writeAnthropicError(w, http.StatusNotFound, "unsupported gateway route")
}

func (h *handler) authorized(r *http.Request) bool {
	token := strings.TrimSpace(h.cfg.Token)
	if token == "" {
		return false
	}
	if got := strings.TrimSpace(r.Header.Get(ccrSessionTokenHeader)); got == token {
		return true
	}
	if got := strings.TrimSpace(r.Header.Get("x-api-key")); got == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.EqualFold(auth, "Bearer "+token)
}

func (h *handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	req, body, ok := decodeAnthropicRequest(w, r)
	if !ok {
		return
	}
	route, validationErr := h.selectRoute(r.Context(), req.Model)
	if validationErr != nil {
		writeAnthropicError(w, validationErr.status, validationErr.message)
		return
	}
	if capabilityErr := validateRouteMessageCapabilities(route, req); capabilityErr != nil {
		writeAnthropicError(w, capabilityErr.status, capabilityErr.message)
		return
	}
	if route.kind == routeAnthropic {
		passBody := body
		if route.model.Alias != "" && route.model.ProviderModel != "" {
			rewritten, err := rewriteAnthropicRequestModel(body, route.model.ProviderModel)
			if err != nil {
				writeAnthropicError(w, http.StatusBadRequest, err.Error())
				return
			}
			passBody = rewritten
		}
		h.handleAnthropicPassThrough(w, r, passBody, route.anthropicProvider, route.anthropicAuth, route.responseModel)
		return
	}
	if err := h.validateOpenAIMessageRequest(&req); err != nil {
		writeAnthropicError(w, err.status, err.message)
		return
	}
	addIgnoredAnthropicFieldsHeader(w.Header(), ignoredOpenAIAnthropicFields(req.Fields))

	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, route.provider.SecretRef)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(route.provider.SecretRef)))
		return
	}
	openAIReq, err := toOpenAIChatRequest(req, route.model.ProviderModel)
	if err != nil {
		writeAnthropicError(w, http.StatusNotImplemented, err.Error())
		return
	}
	resp, err := h.callOpenAICompatible(r.Context(), route.provider, apiKey, openAIReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	finishReason, err := anthropicStopReasonFromOpenAI(resp)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	if req.Stream {
		writeAnthropicStream(w, route.responseModel, resp, finishReason)
		return
	}
	writeJSON(w, http.StatusOK, toAnthropicResponse(route.responseModel, resp, finishReason))
}

type requestValidationError struct {
	status  int
	message string
}

func decodeAnthropicRequest(w http.ResponseWriter, r *http.Request) (anthropicRequest, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return anthropicRequest{}, nil, false
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return anthropicRequest{}, nil, false
	}
	if err := json.Unmarshal(body, &req.Fields); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return anthropicRequest{}, nil, false
	}
	return req, body, true
}

func (h *handler) validateOpenAIMessageRequest(req *anthropicRequest) *requestValidationError {
	for field := range req.Fields {
		if !openAIPathSupportsAnthropicField(field) {
			return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("Anthropic request field %q is not supported by the OpenAI-compatible gateway path", field)}
		}
	}
	if err := validateThinking(req.Thinking); err != nil {
		return &requestValidationError{status: http.StatusNotImplemented, message: err.Error()}
	}
	return nil
}

func openAIPathSupportsAnthropicField(field string) bool {
	switch field {
	case "model", "system", "messages", "max_tokens", "temperature", "stop_sequences", "stream", "tools", "tool_choice", "thinking", "metadata", "output_config", "context_management":
		return true
	default:
		return false
	}
}

func ignoredOpenAIAnthropicFields(fields map[string]json.RawMessage) []string {
	if _, ok := fields["context_management"]; ok {
		return []string{"context_management"}
	}
	return nil
}

func addIgnoredAnthropicFieldsHeader(header http.Header, fields []string) {
	if len(fields) == 0 {
		return
	}
	header.Set(ccrIgnoredFieldsHeader, strings.Join(fields, ", "))
}

type routeKind int

const (
	routeOpenAI routeKind = iota
	routeAnthropic
)

type anthropicAuthMode int

const (
	anthropicAuthProviderSecret anthropicAuthMode = iota
	anthropicAuthIncoming
)

var errNoDefaultModelAlias = errors.New("no default model alias configured")

type messageRoute struct {
	kind              routeKind
	model             store.Model
	provider          store.Provider
	anthropicProvider *store.Provider
	anthropicAuth     anthropicAuthMode
	capabilities      providers.Capabilities
	responseModel     string
}

func validateRouteMessageCapabilities(route messageRoute, req anthropicRequest) *requestValidationError {
	if req.Stream && !route.capabilities.SupportsStreaming {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("streaming is not supported for model %q with provider protocol %q", req.Model, route.capabilities.Protocol)}
	}
	if rawJSONPresent(req.Thinking) && !route.capabilities.SupportsThinking {
		return &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("thinking is not supported for model %q with provider protocol %q", req.Model, route.capabilities.Protocol)}
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
		return messageRoute{}, &requestValidationError{status: http.StatusBadRequest, message: fmt.Sprintf("model discovery alias %q maps to unconfigured ccr alias %q", requested, aliasLookup.alias)}
	}
	if isFirstPartyAnthropicModelRequest(requested) {
		return h.routeFirstPartyAnthropicModel(requested), nil
	}
	defaultRoute, defaultErr := h.defaultAliasRoute(ctx)
	if defaultErr == nil {
		return defaultRoute, nil
	}
	if !errors.Is(defaultErr, errNoDefaultModelAlias) {
		return messageRoute{}, &requestValidationError{status: http.StatusBadGateway, message: fmt.Sprintf("default model alias could not be routed: %v", defaultErr)}
	}
	return messageRoute{}, &requestValidationError{status: http.StatusBadGateway, message: fmt.Sprintf("model %q is not a configured ccr alias, a ccr discovery alias, or a first-party Anthropic model", requested)}
}

type configuredAliasLookup struct {
	alias    string
	exists   bool
	prefixed bool
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
	discoveryAlias, ok := aliasFromDiscoveryID(requested)
	if !ok {
		return configuredAliasLookup{alias: alias}, nil
	}
	aliasExists, err = h.cfg.Store.ModelExists(ctx, discoveryAlias)
	if err != nil {
		return configuredAliasLookup{}, &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("checking requested model alias %q: %v", discoveryAlias, err)}
	}
	return configuredAliasLookup{alias: discoveryAlias, exists: aliasExists, prefixed: true}, nil
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
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol == providers.ProtocolOpenAICompatible {
		return messageRoute{kind: routeOpenAI, model: model, provider: provider, capabilities: caps, responseModel: responseModel}, nil
	}
	if caps.Protocol == providers.ProtocolAnthropicCompatible {
		rewrittenProvider := provider
		return messageRoute{kind: routeAnthropic, model: model, anthropicProvider: &rewrittenProvider, anthropicAuth: anthropicAuthProviderSecret, capabilities: caps, responseModel: responseModel}, nil
	}
	return messageRoute{}, &requestValidationError{status: http.StatusNotImplemented, message: fmt.Sprintf("provider type %q with protocol %q is not supported by the gateway path", provider.Type, caps.Protocol)}
}

func (h *handler) defaultAliasRoute(ctx context.Context) (messageRoute, error) {
	alias := strings.TrimSpace(h.cfg.DefaultModelAlias)
	if alias == "" {
		return messageRoute{}, errNoDefaultModelAlias
	}
	model, err := h.cfg.Store.GetModel(ctx, alias)
	if err != nil {
		return messageRoute{}, err
	}
	if model.Status == "blocked" {
		return messageRoute{}, fmt.Errorf("default model alias %q is blocked", alias)
	}
	provider, err := h.cfg.Store.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return messageRoute{}, err
	}
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol == providers.ProtocolOpenAICompatible {
		return messageRoute{kind: routeOpenAI, model: model, provider: provider, capabilities: caps, responseModel: alias}, nil
	}
	if caps.Protocol == providers.ProtocolAnthropicCompatible {
		rewrittenProvider := provider
		return messageRoute{kind: routeAnthropic, model: model, anthropicProvider: &rewrittenProvider, anthropicAuth: anthropicAuthProviderSecret, capabilities: caps, responseModel: alias}, nil
	}
	return messageRoute{}, fmt.Errorf("default model alias %q uses provider type %q with protocol %q", alias, provider.Type, caps.Protocol)
}

func aliasFromDiscoveryID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, discoveryAliasPrefix) {
		return "", false
	}
	alias := strings.TrimPrefix(id, discoveryAliasPrefix)
	if alias == "" {
		return "", false
	}
	return alias, true
}

func (h *handler) routeFirstPartyAnthropicModel(requested string) messageRoute {
	anthropicProvider := h.firstPartyAnthropicProvider()
	return messageRoute{
		kind:              routeAnthropic,
		model:             store.Model{Alias: requested},
		anthropicProvider: &anthropicProvider,
		anthropicAuth:     anthropicAuthIncoming,
		capabilities:      effectiveProviderCapabilities(anthropicProvider),
		responseModel:     requested,
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
	case "default", "best", "fable", "sonnet", "opus", "haiku", "sonnet[1m]", "opus[1m]", "opusplan":
		return true
	default:
		return looksLikeAnthropicModelID(id) && !strings.HasPrefix(strings.TrimSpace(id), discoveryAliasPrefix)
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

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
