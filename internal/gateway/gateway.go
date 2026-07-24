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

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type Config struct {
	Store             *store.Store
	Secrets           secret.Backend
	HTTPClient        *http.Client
	ImageHTTPClient   *http.Client
	Token             string
	ObserverToken     string
	DefaultModelAlias string
	AnthropicBaseURL  string
	Recorder          *observability.Recorder
	Tracker           *session.Tracker
	ManagedCUA        *cua.ManagedRuntime
	ManagedCUAProject string

	// AnthropicSubscriptionExhaustion receives safe metadata when a first-party
	// Anthropic pass-through request receives HTTP 429. The gateway sends with a
	// nonblocking select and drops the event if the caller-owned sink is not
	// ready. Keep the sink open while the gateway is running. Events never carry
	// headers, bodies, URLs, provider identifiers, or auth material.
	AnthropicSubscriptionExhaustion chan<- AnthropicSubscriptionExhaustionEvent
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
	if (cfg.Recorder != nil || cfg.Tracker != nil) && strings.TrimSpace(cfg.ObserverToken) == "" {
		return nil, fmt.Errorf("gateway.Start: observer token is required when runtime observation is enabled")
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
	maxGatewayRequestBytes  = 32 << 20
	ccrSessionTokenHeader   = "X-CCR-Session-Token"
	ccrSessionTokenLower    = "x-ccr-session-token"
	ccrIgnoredFieldsHeader  = "X-CCR-Ignored-Anthropic-Fields"
)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead && r.URL.Path == "/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/v1/") {
		h.handleRuntimeRequest(w, r)
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
	observedWriter := &observedResponseWriter{ResponseWriter: w}
	w = observedWriter
	span := h.beginRoute(w, r, "messages", req)
	var usage observability.TokenUsage
	defer func(ctx context.Context) {
		completeRoute(span, ctx, observedWriter.Status(), usage)
	}(r.Context())
	route, validationErr := h.selectRoute(r.Context(), req.Model)
	if validationErr != nil {
		writeAnthropicError(w, validationErr.status, validationErr.message)
		return
	}
	h.observeRoute(r.Context(), span, route)
	if capabilityErr := h.validateManagedRouteMessageCapabilities(route, req); capabilityErr != nil {
		writeAnthropicError(w, capabilityErr.status, capabilityErr.message)
		return
	}
	switch route.kind {
	case routeAnthropic:
		passBody, err := rewriteAnthropicMessageBody(
			body,
			route.model.ProviderModel,
			len(req.Tools) > 0 &&
				explicitlyFalse(route.modelCapabilities.SupportsParallelTools) &&
				!explicitlyFalse(route.modelCapabilities.SupportsToolChoice),
		)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
		usage = h.handleAnthropicPassThrough(w, r, passBody, route.anthropicProvider, route.anthropicAuth, route.responseModel)
		return
	case routeOpenAIResponses:
		usage = h.handleOpenAIResponses(w, r, req, route)
		return
	case routeOpenAI:
		usage = h.handleOpenAIChat(w, r, req, route)
		return
	default:
		writeAnthropicError(w, http.StatusInternalServerError, "gateway selected an unknown route")
		return
	}
}

func (h *handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request, req anthropicRequest, route messageRoute) observability.TokenUsage {
	var usage observability.TokenUsage
	if err := h.validateOpenAIMessageRequest(&req); err != nil {
		writeAnthropicError(w, err.status, err.message)
		return usage
	}
	addIgnoredAnthropicFieldsHeader(w.Header(), ignoredOpenAIAnthropicFields(req.Fields))

	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, route.provider.SecretRef)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(route.provider.SecretRef)))
		return usage
	}
	openAIReq, err := h.toOpenAIChatRequest(r.Context(), req, openAIModelRoute{
		alias:                         route.model.Alias,
		providerName:                  route.provider.Name,
		providerModel:                 route.model.ProviderModel,
		requestModel:                  route.responseModel,
		suppressIdentitySystemMessage: explicitlyFalse(route.modelCapabilities.SupportsSystemMessages),
		forceDisableParallelTools:     explicitlyFalse(route.modelCapabilities.SupportsParallelTools),
	})
	if err != nil {
		writeAnthropicError(w, http.StatusNotImplemented, err.Error())
		return usage
	}
	resp, err := h.callOpenAICompatible(r.Context(), route.provider, apiKey, openAIReq)
	if err != nil {
		var statusErr *openAIProviderStatusError
		status := http.StatusBadGateway
		if errors.As(err, &statusErr) {
			status = statusErr.SafeStatusCode()
		}
		writeAnthropicError(w, status, err.Error())
		return usage
	}
	usage = tokenUsageFromOpenAI(resp)
	finishReason, err := anthropicStopReasonFromOpenAI(resp)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return usage
	}
	if req.Stream {
		writeAnthropicStream(w, route.responseModel, resp, finishReason)
		return usage
	}
	writeJSON(w, http.StatusOK, toAnthropicResponse(route.responseModel, resp, finishReason))
	return usage
}

type requestValidationError struct {
	status  int
	message string
}

func decodeAnthropicRequest(w http.ResponseWriter, r *http.Request) (anthropicRequest, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayRequestBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return anthropicRequest{}, nil, false
	}
	if len(body) > maxGatewayRequestBytes {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "Anthropic request exceeds the 32 MiB gateway limit")
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
	if err := validateOpenAIContextManagement(req.Fields); err != nil {
		return err
	}
	if err := validateThinking(req.Thinking); err != nil {
		return &requestValidationError{status: http.StatusNotImplemented, message: err.Error()}
	}
	return nil
}

func validateOpenAIContextManagement(fields map[string]json.RawMessage) *requestValidationError {
	raw, ok := fields["context_management"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var contextManagement struct {
		Edits []struct {
			Type string `json:"type"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &contextManagement); err != nil {
		return &requestValidationError{status: http.StatusBadRequest, message: "invalid Anthropic context_management field"}
	}
	for _, edit := range contextManagement.Edits {
		if strings.HasPrefix(edit.Type, "compact_") {
			return &requestValidationError{
				status: http.StatusNotImplemented,
				message: fmt.Sprintf(
					"Anthropic context_management edit %q requires compaction support; OpenAI-compatible routes in ccr cannot apply it safely",
					edit.Type,
				),
			}
		}
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
