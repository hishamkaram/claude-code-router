package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/session"
)

const ccrRequestIDHeader = "X-CCR-Request-ID"

func (h *handler) beginRoute(w http.ResponseWriter, r *http.Request, operation string, req anthropicRequest) *observability.RouteSpan {
	if h.cfg.Recorder == nil {
		return nil
	}
	sessionID := int64(0)
	if h.cfg.Tracker != nil {
		sessionID = h.cfg.Tracker.CurrentSessionID()
	}
	span := h.cfg.Recorder.BeginRoute(r.Context(), observability.RouteStart{
		SessionID: sessionID, Operation: operation, RequestedModel: req.Model,
		Streaming: req.Stream, Tools: anthropicRequestUsesTools(req),
		Thinking: rawJSONPresent(req.Thinking),
	})
	if requestID := span.RequestID(); requestID != "" {
		w.Header().Set(ccrRequestIDHeader, requestID)
	}
	return span
}

func (h *handler) observeRoute(ctx context.Context, span *observability.RouteSpan, route messageRoute) {
	observed := sessionRoute(route)
	if h.cfg.Tracker != nil {
		h.cfg.Tracker.ObserveRoute(ctx, observed)
	}
	if span == nil {
		return
	}
	sessionID := int64(0)
	if h.cfg.Tracker != nil {
		sessionID = h.cfg.Tracker.CurrentSessionID()
	}
	span.Resolve(ctx, observability.RouteResolution{
		SessionID: sessionID, RouteKind: observed.Kind, ModelAlias: observed.ModelAlias,
		ProviderName: observed.ProviderName, ProviderModel: observed.ProviderModel,
		Protocol: observed.Protocol,
	})
}

func sessionRoute(route messageRoute) session.Route {
	kind := "registered"
	modelAlias := route.model.Alias
	providerModel := route.model.ProviderModel
	providerName := route.provider.Name
	if route.kind == routeAnthropic {
		if route.anthropicProvider != nil {
			providerName = route.anthropicProvider.Name
		}
		if route.model.ProviderName == "" {
			kind = "first-party-anthropic"
			modelAlias = route.responseModel
			providerModel = route.responseModel
		}
	}
	return session.Route{
		Kind: kind, ModelAlias: modelAlias, ProviderName: providerName,
		ProviderModel: providerModel, Protocol: route.capabilities.Protocol,
	}
}

func completeRoute(span *observability.RouteSpan, requestContext context.Context, status int, usage observability.TokenUsage) {
	if span == nil {
		return
	}
	result := observability.RouteResult{HTTPStatus: status, Usage: usage}
	switch {
	case requestContext.Err() != nil:
		result.Status, result.ErrorClass = "canceled", "canceled"
	case status >= http.StatusOK && status < http.StatusBadRequest:
		result.Status = "succeeded"
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		result.Status, result.ErrorClass = "rejected", "client_request"
	default:
		result.Status, result.ErrorClass = "failed", "gateway_or_provider"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), 2*time.Second)
	defer cancel()
	span.Complete(ctx, result)
}

func tokenUsageFromOpenAI(response openAIChatResponse) observability.TokenUsage {
	return observability.TokenUsage{
		Observed: response.usageObserved || response.Usage.PromptTokens != 0 ||
			response.Usage.CompletionTokens != 0,
		InputTokens:  int64(response.Usage.PromptTokens),
		OutputTokens: int64(response.Usage.CompletionTokens),
	}
}

type observedResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *observedResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *observedResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
