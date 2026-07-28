package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

var errAnthropicSubscriptionCredentialUnavailable = errors.New("anthropic subscription credential unavailable")

const maxAnthropicSubscriptionAttempts = 64

// AnthropicSubscriptionCredential is the in-memory credential selected for a
// first-party model request. OAuthToken must never be logged or persisted.
type AnthropicSubscriptionCredential struct {
	AccountName string
	OAuthToken  string
	Generation  uint64
}

// AnthropicSubscriptionPool separates gateway retry timing from account,
// keychain, cooldown, and persistence policy owned by the launching CLI.
type AnthropicSubscriptionPool interface {
	CurrentCredential(context.Context) (AnthropicSubscriptionCredential, error)
	RotateCredential(
		context.Context,
		AnthropicSubscriptionCredential,
		AnthropicSubscriptionExhaustionEvent,
	) (AnthropicSubscriptionCredential, bool, error)
	ActiveAccount() string
}

func (h *handler) executeAnthropicPassThrough(
	r *http.Request,
	body []byte,
	endpoint string,
	provider store.Provider,
	authMode anthropicAuthMode,
	resource string,
	providerSecret string,
) (*http.Response, error) {
	credential, err := h.currentAnthropicSubscriptionCredential(r.Context(), provider, authMode)
	if err != nil {
		return nil, err
	}
	attempted := make(map[string]struct{})
	for {
		if credential.AccountName != "" {
			attempted[credential.AccountName] = struct{}{}
		}
		req, requestErr := h.newAnthropicPassThroughRequest(
			r, body, endpoint, authMode, providerSecret, credential,
		)
		if requestErr != nil {
			return nil, requestErr
		}
		resp, requestErr := h.httpClient().Do(req)
		if requestErr != nil {
			return nil, requestErr
		}
		event, confirmed := h.confirmedAccountSubscriptionExhaustion(resp, provider, authMode, resource)
		if !confirmed || h.cfg.AnthropicSubscriptionPool == nil {
			return resp, nil
		}
		next, retry, rotateErr := h.cfg.AnthropicSubscriptionPool.RotateCredential(
			r.Context(), credential, event,
		)
		if rotateErr != nil {
			closeAnthropicRetryResponse(resp)
			return nil, fmt.Errorf("rotate Anthropic subscription credential: %w", rotateErr)
		}
		if !retry || !validAnthropicSubscriptionCredential(next) {
			return resp, nil
		}
		if len(attempted) >= maxAnthropicSubscriptionAttempts {
			return resp, nil
		}
		if _, duplicate := attempted[next.AccountName]; duplicate {
			return resp, nil
		}
		closeAnthropicRetryResponse(resp)
		credential = next
	}
}

func (h *handler) currentAnthropicSubscriptionCredential(
	ctx context.Context,
	provider store.Provider,
	authMode anthropicAuthMode,
) (AnthropicSubscriptionCredential, error) {
	if h.cfg.AnthropicSubscriptionPool == nil || !h.isFirstPartyAnthropicPassThrough(provider, authMode) {
		return AnthropicSubscriptionCredential{}, nil
	}
	credential, err := h.cfg.AnthropicSubscriptionPool.CurrentCredential(ctx)
	if err != nil || !validAnthropicSubscriptionCredential(credential) {
		return AnthropicSubscriptionCredential{}, errAnthropicSubscriptionCredentialUnavailable
	}
	return credential, nil
}

func validAnthropicSubscriptionCredential(credential AnthropicSubscriptionCredential) bool {
	return strings.TrimSpace(credential.AccountName) != "" &&
		credential.OAuthToken != "" &&
		credential.Generation != 0
}

func closeAnthropicRetryResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}
