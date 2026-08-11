package gateway

import (
	"context"
	"net/http"
	"sync"

	"github.com/hishamkaram/claude-code-router/internal/observability"
)

type claudeAuthState string

const (
	claudeAuthUnknown      claudeAuthState = "unknown"
	claudeAuthOK           claudeAuthState = "ok"
	claudeAuthNeedsRelogin claudeAuthState = "needs_relogin"
	claudeAuthBroken       claudeAuthState = "broken"

	claudeAuthLifecycleName = "claude_auth"
)

type claudeAuthTracker struct {
	mu    sync.RWMutex
	state claudeAuthState
}

func (t *claudeAuthTracker) current() claudeAuthState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.state == "" {
		return claudeAuthUnknown
	}
	return t.state
}

func (h *handler) recordClaudeAuthState(ctx context.Context, state claudeAuthState, reason string) {
	h.claudeAuth.mu.Lock()
	defer h.claudeAuth.mu.Unlock()
	if h.claudeAuth.state == state {
		return
	}
	h.claudeAuth.state = state
	if h.cfg.Recorder != nil {
		// Auth recovery state must survive request cancellation so status can explain the repair.
		h.cfg.Recorder.RecordLifecycle(context.WithoutCancel(ctx), observability.LifecycleEvent{
			Name:   claudeAuthLifecycleName,
			Status: string(state),
			Reason: reason,
		})
	}
}

func (h *handler) observeClaudeAuthResponse(ctx context.Context, firstParty bool, status int) {
	if !firstParty {
		return
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		h.recordClaudeAuthState(ctx, claudeAuthNeedsRelogin, "upstream_authentication_rejected")
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		h.recordClaudeAuthState(ctx, claudeAuthOK, "upstream_authentication_accepted")
	}
}

func (h *handler) claudeAuthState() claudeAuthState {
	return h.claudeAuth.current()
}

func claudeSubscriptionAuthFailureMessage() string {
	return "Claude subscription authentication needs re-login; run claude /login or refresh the selected account with ccr claude-account refresh <name> --from current, then relaunch. Registered CCR provider aliases remain available through /model <alias>."
}
