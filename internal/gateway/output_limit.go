package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
)

const ccrOutputLimitHeader = "X-CCR-Output-Limit"

type outputLimitClamp struct {
	requested int
	applied   int
	limit     int64
}

func normalizeModelOutputLimit(route messageRoute, req anthropicRequest) (anthropicRequest, *outputLimitClamp, *requestValidationError) {
	limit := route.modelCapabilities.MaxOutputTokens
	if req.MaxTokens <= 0 || limit == nil {
		return req, nil, nil
	}
	if *limit <= 0 {
		return req, nil, &requestValidationError{
			status:  http.StatusInternalServerError,
			message: fmt.Sprintf("model alias %q has an invalid maximum output capability", route.model.Alias),
		}
	}
	if *limit > int64(^uint(0)>>1) {
		return req, nil, &requestValidationError{
			status:  http.StatusInternalServerError,
			message: fmt.Sprintf("model alias %q has an unsupported maximum output capability", route.model.Alias),
		}
	}
	if int64(req.MaxTokens) <= *limit {
		return req, nil, nil
	}
	clamp := &outputLimitClamp{requested: req.MaxTokens, applied: int(*limit), limit: *limit}
	req.MaxTokens = clamp.applied
	return req, clamp, nil
}

func applyOutputLimitHeader(header http.Header, clamp *outputLimitClamp) {
	if clamp == nil {
		return
	}
	header.Set(ccrOutputLimitHeader, "clamped;requested="+strconv.Itoa(clamp.requested)+";applied="+strconv.Itoa(clamp.applied))
}

func (h *handler) recordOutputLimitClamp(ctx context.Context, span *observability.RouteSpan, route messageRoute, clamp *outputLimitClamp) {
	if clamp == nil || h.cfg.Recorder == nil {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	sessionID := int64(0)
	if h.cfg.Tracker != nil {
		sessionID = h.cfg.Tracker.CurrentSessionID()
	}
	externalID := ""
	if span != nil {
		externalID = span.RequestID()
	}
	h.cfg.Recorder.RecordLifecycle(recordCtx, observability.LifecycleEvent{
		Name:       "compatibility_degradation",
		Status:     "degraded",
		ExternalID: externalID,
		SessionID:  sessionID,
		Reason: fmt.Sprintf("max_tokens_clamped; requested=%d; applied=%d; model_limit=%d; alias=%s",
			clamp.requested, clamp.applied, clamp.limit, route.model.Alias),
	})
}
