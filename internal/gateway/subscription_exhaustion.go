package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	anthropicUnifiedRateLimitStatusHeader = "Anthropic-Ratelimit-Unified-Status"
	anthropicUnifiedRateLimitResetHeader  = "Anthropic-Ratelimit-Unified-Reset"
	anthropicUnifiedRateLimitRejected     = "rejected"
)

// AnthropicSubscriptionExhaustionEvent reports safe metadata from a first-party
// Anthropic response that explicitly identifies subscription quota exhaustion.
type AnthropicSubscriptionExhaustionEvent struct {
	StatusCode         int
	RetryAfterDuration time.Duration
	RetryAfterTime     time.Time
}

func (h *handler) notifyAnthropicSubscriptionExhaustion(
	resp *http.Response,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
) {
	if h.cfg.AnthropicSubscriptionExhaustion == nil ||
		resource != "messages" ||
		resp.StatusCode != http.StatusTooManyRequests ||
		!h.isFirstPartyAnthropicPassThrough(provider, authMode) ||
		!strings.EqualFold(
			strings.TrimSpace(resp.Header.Get(anthropicUnifiedRateLimitStatusHeader)),
			anthropicUnifiedRateLimitRejected,
		) {
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
	if resetAt, ok := parseAnthropicUnifiedRateLimitReset(resp.Header); ok {
		event.RetryAfterTime = resetAt
	}
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
	if retryAfterTime, err := http.ParseTime(retryAfter); err == nil && event.RetryAfterTime.IsZero() {
		event.RetryAfterTime = retryAfterTime
	}
	return event
}

func parseAnthropicUnifiedRateLimitReset(header http.Header) (time.Time, bool) {
	raw := strings.TrimSpace(header.Get(anthropicUnifiedRateLimitResetHeader))
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func normalizedBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
