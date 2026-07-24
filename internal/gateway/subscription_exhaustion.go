package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

// AnthropicSubscriptionExhaustionEvent reports safe metadata from a first-party
// Anthropic HTTP 429 response.
type AnthropicSubscriptionExhaustionEvent struct {
	StatusCode         int
	RetryAfterDuration time.Duration
	RetryAfterTime     time.Time
}

func (h *handler) notifyAnthropicSubscriptionExhaustion(resp *http.Response, provider store.Provider, authMode anthropicAuthMode) {
	if h.cfg.AnthropicSubscriptionExhaustion == nil ||
		resp.StatusCode != http.StatusTooManyRequests ||
		!h.isFirstPartyAnthropicPassThrough(provider, authMode) {
		return
	}
	event := newAnthropicSubscriptionExhaustionEvent(resp)
	select {
	case h.cfg.AnthropicSubscriptionExhaustion <- event:
	default:
	}
}

func (h *handler) isFirstPartyAnthropicPassThrough(provider store.Provider, authMode anthropicAuthMode) bool {
	if authMode != anthropicAuthIncoming {
		return false
	}
	firstParty := h.firstPartyAnthropicProvider()
	return provider.Name == firstParty.Name &&
		provider.Type == firstParty.Type &&
		normalizedBaseURL(provider.BaseURL) == normalizedBaseURL(firstParty.BaseURL)
}

func newAnthropicSubscriptionExhaustionEvent(resp *http.Response) AnthropicSubscriptionExhaustionEvent {
	event := AnthropicSubscriptionExhaustionEvent{StatusCode: resp.StatusCode}
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if retryAfter == "" {
		return event
	}
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil {
		if seconds >= 0 {
			event.RetryAfterDuration = time.Duration(seconds) * time.Second
		}
		return event
	}
	if retryAfterTime, err := http.ParseTime(retryAfter); err == nil {
		event.RetryAfterTime = retryAfterTime
	}
	return event
}

func normalizedBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
