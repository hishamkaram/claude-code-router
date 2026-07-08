package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	Store            *store.Store
	Secrets          secret.Backend
	HTTPClient       *http.Client
	Token            string
	ForcedModelAlias string
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

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "invalid local gateway token")
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/messages") {
		h.handleMessages(w, r)
		return
	}
	writeAnthropicError(w, http.StatusNotFound, "unsupported gateway route")
}

func (h *handler) authorized(r *http.Request) bool {
	token := strings.TrimSpace(h.cfg.Token)
	if token == "" {
		return false
	}
	if got := strings.TrimSpace(r.Header.Get("x-api-key")); got == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.EqualFold(auth, "Bearer "+token)
}

func (h *handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeAnthropicRequest(w, r)
	if !ok {
		return
	}
	if err := h.validateMessageRequest(&req); err != nil {
		writeAnthropicError(w, err.status, err.message)
		return
	}
	routeAlias, validationErr := h.selectRouteAlias(r.Context(), req.Model)
	if validationErr != nil {
		writeAnthropicError(w, validationErr.status, validationErr.message)
		return
	}
	model, provider, ok := h.loadRouteTarget(w, r, routeAlias)
	if !ok {
		return
	}

	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, provider.SecretRef)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(provider.SecretRef)))
		return
	}
	openAIReq, err := toOpenAIChatRequest(req, model.ProviderModel)
	if err != nil {
		writeAnthropicError(w, http.StatusNotImplemented, err.Error())
		return
	}
	resp, err := h.callOpenAICompatible(r.Context(), provider, apiKey, openAIReq)
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
		writeAnthropicStream(w, routeAlias, resp, finishReason)
		return
	}
	writeJSON(w, http.StatusOK, toAnthropicResponse(routeAlias, resp, finishReason))
}

type requestValidationError struct {
	status  int
	message string
}

func decodeAnthropicRequest(w http.ResponseWriter, r *http.Request) (anthropicRequest, bool) {
	var req anthropicRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			writeAnthropicError(w, http.StatusNotImplemented, "unsupported Anthropic request field: "+err.Error())
			return anthropicRequest{}, false
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return anthropicRequest{}, false
	}
	return req, true
}

func (h *handler) validateMessageRequest(req *anthropicRequest) *requestValidationError {
	if len(req.Tools) > 0 || len(req.ToolChoice) > 0 {
		return &requestValidationError{status: http.StatusNotImplemented, message: "tool use is not supported by the OpenAI-compatible gateway path yet"}
	}
	if err := validateThinking(req.Thinking); err != nil {
		return &requestValidationError{status: http.StatusNotImplemented, message: err.Error()}
	}
	return nil
}

func (h *handler) selectRouteAlias(ctx context.Context, requested string) (string, *requestValidationError) {
	requested = strings.TrimSpace(requested)
	forced := strings.TrimSpace(h.cfg.ForcedModelAlias)
	if requested == "" {
		if forced != "" {
			return forced, nil
		}
		return "", &requestValidationError{status: http.StatusBadRequest, message: "message request model must be a configured ccr model alias"}
	}
	if forced != "" {
		exists, err := h.cfg.Store.ModelExists(ctx, requested)
		if err != nil {
			return "", &requestValidationError{status: http.StatusInternalServerError, message: fmt.Sprintf("checking requested model alias %q: %v", requested, err)}
		}
		if exists {
			return requested, nil
		}
		return forced, nil
	}
	return requested, nil
}

func (h *handler) loadRouteTarget(w http.ResponseWriter, r *http.Request, routeAlias string) (store.Model, store.Provider, bool) {
	model, err := h.cfg.Store.GetModel(r.Context(), routeAlias)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("model alias %q is not configured", routeAlias))
		return store.Model{}, store.Provider{}, false
	}
	if model.Status == "blocked" {
		writeAnthropicError(w, http.StatusForbidden, fmt.Sprintf("model alias %q is blocked and cannot be routed", routeAlias))
		return store.Model{}, store.Provider{}, false
	}
	provider, err := h.cfg.Store.GetProvider(r.Context(), model.ProviderName)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("provider %q for model alias %q is not configured", model.ProviderName, routeAlias))
		return store.Model{}, store.Provider{}, false
	}
	if !providers.SupportsOpenAICompatibleRouting(provider.Type) {
		writeAnthropicError(w, http.StatusNotImplemented, fmt.Sprintf("provider type %q is not supported by the OpenAI-compatible gateway path", provider.Type))
		return store.Model{}, store.Provider{}, false
	}
	return model, provider, true
}

func validateThinking(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("unsupported thinking field: %w", err)
	}
	switch payload.Type {
	case "", "adaptive", "disabled":
		return nil
	default:
		return fmt.Errorf("thinking mode %q is not supported by the OpenAI-compatible gateway path", payload.Type)
	}
}

func resolveProviderSecret(ctx context.Context, backend secret.Backend, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	value, err := backend.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (h *handler) callOpenAICompatible(ctx context.Context, provider store.Provider, apiKey string, payload openAIChatRequest) (openAIChatResponse, error) {
	endpoint, err := providers.ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return openAIChatResponse{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("encoding OpenAI-compatible request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("creating OpenAI-compatible request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := h.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("requesting OpenAI-compatible provider %q: %w", provider.Name, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return openAIChatResponse{}, fmt.Errorf("OpenAI-compatible provider %q returned HTTP %d %s", provider.Name, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var decoded openAIChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return openAIChatResponse{}, fmt.Errorf("decoding OpenAI-compatible provider response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return openAIChatResponse{}, fmt.Errorf("OpenAI-compatible provider %q returned no choices", provider.Name)
	}
	return decoded, nil
}

type anthropicRequest struct {
	Model        string             `json:"model"`
	System       any                `json:"system,omitempty"`
	Messages     []anthropicMessage `json:"messages"`
	MaxTokens    int                `json:"max_tokens,omitempty"`
	Stream       bool               `json:"stream,omitempty"`
	Tools        []json.RawMessage  `json:"tools,omitempty"`
	ToolChoice   json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking     json.RawMessage    `json:"thinking,omitempty"`
	Metadata     json.RawMessage    `json:"metadata,omitempty"`
	OutputConfig json.RawMessage    `json:"output_config,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAIChatRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Stream          bool            `json:"stream"`
	User            string          `json:"user,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func toOpenAIChatRequest(req anthropicRequest, providerModel string) (openAIChatRequest, error) {
	options, err := openAIOptionsFromAnthropic(req)
	if err != nil {
		return openAIChatRequest{}, err
	}
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != nil {
		text, err := anthropicContentText(req.System)
		if err != nil {
			return openAIChatRequest{}, fmt.Errorf("unsupported system content: %w", err)
		}
		if text != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: text})
		}
	}
	for _, message := range req.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			return openAIChatRequest{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
		text, err := anthropicContentText(message.Content)
		if err != nil {
			return openAIChatRequest{}, fmt.Errorf("unsupported %s message content: %w", message.Role, err)
		}
		messages = append(messages, openAIMessage{Role: message.Role, Content: text})
	}
	return openAIChatRequest{
		Model:           providerModel,
		Messages:        messages,
		MaxTokens:       req.MaxTokens,
		Stream:          false,
		User:            options.user,
		ReasoningEffort: options.reasoningEffort,
	}, nil
}

type openAIRequestOptions struct {
	user            string
	reasoningEffort string
}

func openAIOptionsFromAnthropic(req anthropicRequest) (openAIRequestOptions, error) {
	user, err := openAIUserFromMetadata(req.Metadata)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	reasoningEffort, err := openAIReasoningEffortFromOutputConfig(req.OutputConfig)
	if err != nil {
		return openAIRequestOptions{}, err
	}
	return openAIRequestOptions{user: user, reasoningEffort: reasoningEffort}, nil
}

func openAIUserFromMetadata(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported metadata: %w", err)
	}
	var user string
	for key, value := range payload {
		switch key {
		case "user_id":
			if err := json.Unmarshal(value, &user); err != nil {
				return "", fmt.Errorf("metadata.user_id must be a string")
			}
		default:
			return "", fmt.Errorf("metadata field %q is not supported by the OpenAI-compatible gateway path", key)
		}
	}
	return strings.TrimSpace(user), nil
}

func openAIReasoningEffortFromOutputConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("unsupported output_config: %w", err)
	}
	var effort string
	for key, value := range payload {
		switch key {
		case "effort":
			if err := json.Unmarshal(value, &effort); err != nil {
				return "", fmt.Errorf("output_config.effort must be a string")
			}
		default:
			return "", fmt.Errorf("output_config field %q is not supported by the OpenAI-compatible gateway path", key)
		}
	}
	return openAIReasoningEffortFromClaudeEffort(effort)
}

func openAIReasoningEffortFromClaudeEffort(effort string) (string, error) {
	trimmed := strings.TrimSpace(effort)
	switch trimmed {
	case "":
		return "", nil
	case "low", "medium", "high":
		return trimmed, nil
	case "xhigh", "max":
		return "high", nil
	default:
		return "", fmt.Errorf("output_config.effort %q is not supported by the OpenAI-compatible gateway path", effort)
	}
}

func anthropicContentText(value any) (string, error) {
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		var parts []string
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content block is not an object")
			}
			text, err := anthropicTextBlockText(block)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("content type %T is not supported", value)
	}
}

func anthropicTextBlockText(block map[string]any) (string, error) {
	for key := range block {
		if key != "type" && key != "text" && key != "cache_control" {
			return "", fmt.Errorf("content block field %q is not supported", key)
		}
	}
	blockType, _ := block["type"].(string)
	if blockType != "text" {
		return "", fmt.Errorf("content block type %q is not supported", blockType)
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", fmt.Errorf("content block text must be a string")
	}
	return text, nil
}

func writeAnthropicStream(w http.ResponseWriter, alias string, resp openAIChatResponse, finishReason string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	text := ""
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
	}
	id := firstNonEmpty(resp.ID, "msg_ccr")
	writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         alias,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  resp.Usage.PromptTokens,
				"output_tokens": 0,
			},
		},
	})
	writeSSEEvent(w, flusher, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	if text != "" {
		writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": text},
		})
	}
	writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   finishReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": resp.Usage.CompletionTokens},
	})
	writeSSEEvent(w, flusher, "message_stop", map[string]string{"type": "message_stop"})
}

func writeSSEEvent(w io.Writer, flusher http.Flusher, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

func toAnthropicResponse(alias string, resp openAIChatResponse, finishReason string) map[string]any {
	text := ""
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
	}
	return map[string]any{
		"id":            firstNonEmpty(resp.ID, "msg_ccr"),
		"type":          "message",
		"role":          "assistant",
		"model":         alias,
		"content":       []map[string]string{{"type": "text", "text": text}},
		"stop_reason":   finishReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
}

func anthropicStopReasonFromOpenAI(resp openAIChatResponse) (string, error) {
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("OpenAI-compatible provider returned no choices")
	}
	switch finishReason := resp.Choices[0].FinishReason; finishReason {
	case "", "stop":
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	default:
		return "", fmt.Errorf("OpenAI-compatible provider returned unsupported finish_reason %q", finishReason)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
