package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const subscriptionPoolNoticeBuffer = 64

type subscriptionPoolController struct {
	deps       Dependencies
	pinned     bool
	rotation   chan struct{}
	notices    chan string
	stateMu    sync.RWMutex
	active     selectedClaudeAccount
	generation uint64
	store      *store.Store
	launchID   int64
}

func newSubscriptionPoolController(
	deps Dependencies,
	selected selectedClaudeAccount,
	pinned bool,
) *subscriptionPoolController {
	rotation := make(chan struct{}, 1)
	rotation <- struct{}{}
	return &subscriptionPoolController{
		deps: deps, pinned: pinned, rotation: rotation,
		notices: make(chan string, subscriptionPoolNoticeBuffer),
		active:  selected, generation: 1,
	}
}

func (p *subscriptionPoolController) bindLaunch(s *store.Store, launchID int64) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.store = s
	p.launchID = launchID
}

func (p *subscriptionPoolController) CurrentCredential(
	ctx context.Context,
) (gateway.AnthropicSubscriptionCredential, error) {
	if err := ctx.Err(); err != nil {
		return gateway.AnthropicSubscriptionCredential{}, err
	}
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.active.Account.Name == "" || p.active.OAuthToken == "" || p.generation == 0 {
		return gateway.AnthropicSubscriptionCredential{}, fmt.Errorf("subscription pool has no active credential")
	}
	return p.gatewayCredentialLocked(), nil
}

func (p *subscriptionPoolController) ActiveAccount() string {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.active.Account.Name
}

func (p *subscriptionPoolController) Notices() <-chan string {
	return p.notices
}

func (p *subscriptionPoolController) RotateCredential(
	ctx context.Context,
	failed gateway.AnthropicSubscriptionCredential,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) (gateway.AnthropicSubscriptionCredential, bool, error) {
	select {
	case <-ctx.Done():
		return gateway.AnthropicSubscriptionCredential{}, false, ctx.Err()
	case <-p.rotation:
	}
	defer func() { p.rotation <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return gateway.AnthropicSubscriptionCredential{}, false, err
	}

	current, sameGeneration := p.currentForFailure(failed)
	if !sameGeneration {
		return current, true, nil
	}
	return p.rotateCurrentCredential(ctx, failed, event)
}

func (p *subscriptionPoolController) rotateCurrentCredential(
	ctx context.Context,
	failed gateway.AnthropicSubscriptionCredential,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) (gateway.AnthropicSubscriptionCredential, bool, error) {
	cooldownUntil := subscriptionCooldownUntil(time.Now().UTC(), event)
	p.recordFailure(ctx, failed.AccountName, cooldownUntil, event)
	if p.pinned {
		p.notice(fmt.Sprintf(
			"Claude account %s reached %s; this launch pins that account, so CCR forwarded Anthropic's limit response and kept Claude Code running.",
			failed.AccountName, subscriptionRateLimitDescription(event),
		))
		return gateway.AnthropicSubscriptionCredential{}, false, nil
	}
	next, found := p.nextCredential(ctx, failed.AccountName)
	if !found {
		p.notice(fmt.Sprintf(
			"Claude account %s reached %s; no replacement account is usable. CCR forwarded Anthropic's limit response and kept Claude Code running.",
			failed.AccountName, subscriptionRateLimitDescription(event),
		))
		return gateway.AnthropicSubscriptionCredential{}, false, nil
	}
	p.activate(ctx, failed.AccountName, next)
	credential, err := p.CurrentCredential(ctx)
	return credential, err == nil, err
}

func (p *subscriptionPoolController) currentForFailure(
	failed gateway.AnthropicSubscriptionCredential,
) (gateway.AnthropicSubscriptionCredential, bool) {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	current := p.gatewayCredentialLocked()
	return current, failed.Generation == p.generation && failed.AccountName == p.active.Account.Name
}

func (p *subscriptionPoolController) recordFailure(
	ctx context.Context,
	account string,
	cooldownUntil time.Time,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) {
	p.stateMu.RLock()
	s := p.store
	p.stateMu.RUnlock()
	if s == nil {
		p.notice("Warning: CCR could not persist the exhausted Claude account because launch state is unavailable.")
		return
	}
	if err := s.MarkClaudeAccountFailure(
		ctx, account, cooldownUntil, subscriptionRateLimitFailureClass(event),
	); err != nil {
		p.notice(fmt.Sprintf(
			"Warning: CCR could not persist the cooldown for Claude account %s; it remains excluded from this running process.",
			account,
		))
	}
}

func (p *subscriptionPoolController) nextCredential(
	ctx context.Context,
	failedAccount string,
) (selectedClaudeAccount, bool) {
	p.stateMu.RLock()
	s := p.store
	p.stateMu.RUnlock()
	if s == nil {
		return selectedClaudeAccount{}, false
	}
	excluded := map[string]struct{}{failedAccount: {}}
	for {
		account, found, err := s.ClaimClaudeAccount(
			ctx, time.Now().UTC(), subscriptionPoolExcludedNames(excluded),
		)
		if err != nil {
			p.notice("Warning: CCR could not query the Claude account pool; the current limit response was preserved.")
			return selectedClaudeAccount{}, false
		}
		if !found {
			return selectedClaudeAccount{}, false
		}
		selected, err := resolveClaimedClaudeAccount(ctx, p.deps, account)
		if err == nil {
			return selected, true
		}
		excluded[account.Name] = struct{}{}
		cooldown := time.Now().UTC().Add(credentialFailureCooldown)
		_ = s.MarkClaudeAccountFailure(ctx, account.Name, cooldown, "credential_unavailable")
		p.notice(fmt.Sprintf(
			"Claude account %s credential is unavailable; CCR skipped it without exposing credential details.",
			account.Name,
		))
	}
}

func subscriptionPoolExcludedNames(excluded map[string]struct{}) []string {
	names := make([]string, 0, len(excluded))
	for name := range excluded {
		names = append(names, name)
	}
	return names
}

func (p *subscriptionPoolController) activate(
	ctx context.Context,
	previous string,
	next selectedClaudeAccount,
) {
	p.stateMu.Lock()
	p.active = next
	p.generation++
	s, launchID := p.store, p.launchID
	p.stateMu.Unlock()
	if s != nil {
		if err := s.SetLaunchClaudeAccount(ctx, launchID, next.Account.Name); err != nil {
			p.notice(fmt.Sprintf(
				"Warning: Claude model requests switched to account %s, but CCR could not update launch metadata.",
				next.Account.Name,
			))
		}
	}
	p.notice(fmt.Sprintf(
		"Claude model requests switched from account %s to %s inside the existing gateway; Claude Code was not restarted.",
		previous, next.Account.Name,
	))
}

func (p *subscriptionPoolController) gatewayCredentialLocked() gateway.AnthropicSubscriptionCredential {
	return gateway.AnthropicSubscriptionCredential{
		AccountName: p.active.Account.Name,
		OAuthToken:  p.active.OAuthToken,
		Generation:  p.generation,
	}
}

func (p *subscriptionPoolController) notice(message string) {
	select {
	case p.notices <- message:
		return
	default:
	}
	select {
	case <-p.notices:
	default:
	}
	p.notices <- "Warning: CCR omitted an earlier subscription-pool notice because output delivery was saturated. " + message
}
