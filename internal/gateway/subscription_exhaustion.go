package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	anthropicUnifiedRateLimitStatusHeader              = "Anthropic-Ratelimit-Unified-Status"
	anthropicUnifiedRateLimitResetHeader               = "Anthropic-Ratelimit-Unified-Reset"
	anthropicUnifiedRateLimitRepresentativeClaimHeader = "Anthropic-Ratelimit-Unified-Representative-Claim"
	anthropicUnifiedRateLimitFallbackHeader            = "Anthropic-Ratelimit-Unified-Fallback"
	anthropicUnifiedRateLimitRejected                  = "rejected"
	anthropicUnifiedRateLimitFallbackAvailable         = "available"
)

// AnthropicRateLimitClaim is a closed set of quota buckets observed from
// Claude Code's first-party response metadata.
type AnthropicRateLimitClaim uint8

const (
	AnthropicRateLimitClaimUnknown AnthropicRateLimitClaim = iota
	AnthropicRateLimitClaimFiveHour
	AnthropicRateLimitClaimSevenDay
	AnthropicRateLimitClaimSevenDayOpus
	AnthropicRateLimitClaimSevenDaySonnet
	AnthropicRateLimitClaimSevenDayOAuthApps
	AnthropicRateLimitClaimOverage
)

// AnthropicSubscriptionExhaustionEvent reports safe metadata from a first-party
// Anthropic response that explicitly identifies subscription quota exhaustion.
type AnthropicSubscriptionExhaustionEvent struct {
	StatusCode          int
	RetryAfterDuration  time.Duration
	RetryAfterTime      time.Time
	RepresentativeClaim AnthropicRateLimitClaim
	FallbackAvailable   bool
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
	event := AnthropicSubscriptionExhaustionEvent{
		StatusCode: resp.StatusCode,
		RepresentativeClaim: normalizeAnthropicRateLimitClaim(
			resp.Header.Get(anthropicUnifiedRateLimitRepresentativeClaimHeader),
		),
		FallbackAvailable: strings.EqualFold(
			strings.TrimSpace(resp.Header.Get(anthropicUnifiedRateLimitFallbackHeader)),
			anthropicUnifiedRateLimitFallbackAvailable,
		),
	}
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

func normalizeAnthropicRateLimitClaim(value string) AnthropicRateLimitClaim {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "five_hour":
		return AnthropicRateLimitClaimFiveHour
	case "seven_day":
		return AnthropicRateLimitClaimSevenDay
	case "seven_day_opus":
		return AnthropicRateLimitClaimSevenDayOpus
	case "seven_day_sonnet":
		return AnthropicRateLimitClaimSevenDaySonnet
	case "seven_day_oauth_apps":
		return AnthropicRateLimitClaimSevenDayOAuthApps
	case "overage":
		return AnthropicRateLimitClaimOverage
	default:
		return AnthropicRateLimitClaimUnknown
	}
}

func (claim AnthropicRateLimitClaim) String() string {
	switch claim {
	case AnthropicRateLimitClaimFiveHour:
		return "five_hour"
	case AnthropicRateLimitClaimSevenDay:
		return "seven_day"
	case AnthropicRateLimitClaimSevenDayOpus:
		return "seven_day_opus"
	case AnthropicRateLimitClaimSevenDaySonnet:
		return "seven_day_sonnet"
	case AnthropicRateLimitClaimSevenDayOAuthApps:
		return "seven_day_oauth_apps"
	case AnthropicRateLimitClaimOverage:
		return "overage"
	default:
		return "unknown"
	}
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
