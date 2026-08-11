package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	defaultSubscriptionCooldown       = 15 * time.Minute
	defaultTransientRateLimitCooldown = time.Minute
	maxTransientRateLimitCooldown     = 5 * time.Minute
	credentialFailureCooldown         = 5 * time.Minute
	maxSubscriptionCooldown           = 24 * time.Hour
)

type selectedClaudeAccount struct {
	Account    store.ClaudeAccount
	OAuthToken string
}

func runLaunch(
	ctx context.Context,
	cmd *cobra.Command,
	opts *options,
	deps Dependencies,
	invocation launchInvocation,
) error {
	if invocation.authMode != launchAuthModeSubscriptionPool {
		return runLaunchAttempt(ctx, cmd, opts, deps, invocation, nil, nil)
	}
	if err := validateLaunchInputs(
		invocation.modelAlias,
		invocation.authMode,
		invocation.claudeAccount,
		invocation.permissionMode,
	); err != nil {
		return err
	}
	if err := validateLaunchPassthroughArgs(invocation.claudeArgs); err != nil {
		return err
	}
	if err := preflightLaunch(ctx, opts, deps, invocation); err != nil {
		return err
	}
	return runSubscriptionPoolLaunch(ctx, cmd, opts, deps, invocation)
}

func runSubscriptionPoolLaunch(
	ctx context.Context,
	cmd *cobra.Command,
	opts *options,
	deps Dependencies,
	invocation launchInvocation,
) error {
	selected, err := claimLaunchClaudeAccount(
		ctx, cmd.ErrOrStderr(), opts, deps, invocation.claudeAccount, nil,
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(
		cmd.ErrOrStderr(),
		"Claude model-request account selected: %s (local label; gateway-held OAuth can rotate without restarting Claude Code).\n",
		selected.Account.Name,
	)
	pinned := invocation.claudeAccount != ""
	if pinned {
		fmt.Fprintln(
			cmd.ErrOrStderr(),
			"Automatic account rotation: disabled because --claude-account pins one account; confirmed limits are forwarded without restarting Claude Code.",
		)
	} else {
		fmt.Fprintln(
			cmd.ErrOrStderr(),
			"Automatic account rotation: enabled inside the local gateway for confirmed account-wide quota responses; the Claude Code process is not restarted.",
		)
	}
	pool := newSubscriptionPoolController(deps, selected, pinned)
	return runLaunchAttempt(ctx, cmd, opts, deps, invocation, &selected, pool)
}

func claimLaunchClaudeAccount(
	ctx context.Context,
	out io.Writer,
	opts *options,
	deps Dependencies,
	explicitName string,
	excluded []string,
) (selectedClaudeAccount, error) {
	selected, found, err := tryClaimLaunchClaudeAccount(
		ctx, out, opts, deps, explicitName, excluded,
	)
	if err != nil {
		return selectedClaudeAccount{}, err
	}
	if !found {
		return selectedClaudeAccount{}, noUsableClaudeAccountError(explicitName)
	}
	return selected, nil
}

func tryClaimLaunchClaudeAccount(
	ctx context.Context,
	out io.Writer,
	opts *options,
	deps Dependencies,
	explicitName string,
	excluded []string,
) (selectedClaudeAccount, bool, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return selectedClaudeAccount{}, false, err
	}
	defer closeStore(s)

	for {
		account, found, claimErr := claimEligibleClaudeAccount(ctx, s, explicitName, excluded)
		if claimErr != nil {
			return selectedClaudeAccount{}, false, claimErr
		}
		if !found {
			return selectedClaudeAccount{}, false, nil
		}
		selected, resolveErr := resolveClaimedClaudeAccount(ctx, deps, account)
		if resolveErr == nil {
			return selected, true, nil
		}
		cooldown := time.Now().UTC().Add(credentialFailureCooldown)
		if markErr := s.MarkClaudeAccountFailure(ctx, account.Name, cooldown, "credential_unavailable"); markErr != nil {
			return selectedClaudeAccount{}, false, errors.Join(
				fmt.Errorf("resolving Claude account %q credential: %w", account.Name, resolveErr),
				markErr,
			)
		}
		if reportErr := reportUnavailableClaudeCredential(out, account.Name, explicitName != "", cooldown, resolveErr); reportErr != nil {
			return selectedClaudeAccount{}, false, reportErr
		}
		excluded = append(excluded, account.Name)
	}
}

func resolveClaimedClaudeAccount(
	ctx context.Context,
	deps Dependencies,
	account store.ClaudeAccount,
) (selectedClaudeAccount, error) {
	token, err := deps.Secrets.Resolve(ctx, account.AccessTokenRef)
	if err != nil {
		return selectedClaudeAccount{}, err
	}
	token, err = claudeaccount.ValidateToken(token)
	if err != nil {
		return selectedClaudeAccount{}, err
	}
	return selectedClaudeAccount{Account: account, OAuthToken: token}, nil
}

func reportUnavailableClaudeCredential(
	out io.Writer,
	name string,
	explicit bool,
	cooldown time.Time,
	resolveErr error,
) error {
	if explicit {
		return fmt.Errorf(
			"claude account %q credential is unavailable; run ccr claude-account refresh %s: %w",
			name, name, resolveErr,
		)
	}
	fmt.Fprintf(
		out,
		"Claude account %s credential is unavailable; skipping it until %s.\n",
		name,
		cooldown.Format(time.RFC3339),
	)
	return nil
}

func claimEligibleClaudeAccount(
	ctx context.Context,
	s *store.Store,
	explicitName string,
	excluded []string,
) (store.ClaudeAccount, bool, error) {
	now := time.Now().UTC()
	if explicitName != "" {
		return s.ClaimClaudeAccountByName(ctx, explicitName, now)
	}
	return s.ClaimClaudeAccount(ctx, now, excluded)
}

func noUsableClaudeAccountError(explicitName string) error {
	if explicitName != "" {
		return fmt.Errorf(
			"claude account %q is disabled, expired, or cooling down; inspect it with ccr claude-account show %s; if disabled run ccr claude-account enable %s, if cooling down run ccr claude-account clear-cooldown %s, or refresh credentials with ccr claude-account refresh %s --from current",
			explicitName, explicitName, explicitName, explicitName, explicitName,
		)
	}
	return fmt.Errorf(
		"claude subscription pool has no usable accounts; run ccr claude-account list and refresh, enable, clear-cooldown, or add an account",
	)
}

func subscriptionCooldownUntil(
	now time.Time,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) time.Time {
	if !confirmedAccountSubscriptionLimit(event) {
		return now.Add(transientRateLimitCooldown(now, event))
	}
	cooldown := defaultSubscriptionCooldown
	if event.RetryAfterTime.After(now) {
		cooldown = event.RetryAfterTime.Sub(now)
	} else if event.RetryAfterDuration > 0 {
		cooldown = event.RetryAfterDuration
	}
	if cooldown > maxSubscriptionCooldown {
		cooldown = maxSubscriptionCooldown
	}
	if cooldown <= 0 {
		cooldown = defaultSubscriptionCooldown
	}
	return now.Add(cooldown)
}

func transientRateLimitCooldown(
	now time.Time,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) time.Duration {
	cooldown := defaultTransientRateLimitCooldown
	if event.RetryAfterDuration > 0 {
		cooldown = event.RetryAfterDuration
	} else if event.RetryAfterTime.After(now) {
		cooldown = event.RetryAfterTime.Sub(now)
	}
	if cooldown > maxTransientRateLimitCooldown {
		return maxTransientRateLimitCooldown
	}
	if cooldown <= 0 {
		return defaultTransientRateLimitCooldown
	}
	return cooldown
}

func confirmedAccountSubscriptionLimit(event gateway.AnthropicSubscriptionExhaustionEvent) bool {
	return event.RepresentativeClaim != gateway.AnthropicRateLimitClaimUnknown && !event.FallbackAvailable
}

func subscriptionRateLimitFailureClass(event gateway.AnthropicSubscriptionExhaustionEvent) string {
	claim := event.RepresentativeClaim.String()
	switch {
	case event.FallbackAvailable && event.RepresentativeClaim != gateway.AnthropicRateLimitClaimUnknown:
		return "model_limit_" + claim
	case event.FallbackAvailable:
		return "model_rate_limit"
	case event.RepresentativeClaim != gateway.AnthropicRateLimitClaimUnknown:
		return "subscription_limit_" + claim
	default:
		return "transient_rate_limit"
	}
}

func subscriptionRateLimitDescription(event gateway.AnthropicSubscriptionExhaustionEvent) string {
	claim := event.RepresentativeClaim.String()
	switch {
	case event.FallbackAvailable && event.RepresentativeClaim != gateway.AnthropicRateLimitClaimUnknown:
		return fmt.Sprintf("hit model limit %s (Anthropic reports fallback available)", claim)
	case event.FallbackAvailable:
		return "hit a model limit (Anthropic reports fallback available)"
	case event.RepresentativeClaim != gateway.AnthropicRateLimitClaimUnknown:
		return fmt.Sprintf("reached subscription limit %s", claim)
	default:
		return "received an unclassified rate limit"
	}
}

func selectedClaudeAccountName(account *selectedClaudeAccount) string {
	if account == nil {
		return ""
	}
	return account.Account.Name
}

func writeSubscriptionStatuslineNotice(
	out io.Writer,
	account *selectedClaudeAccount,
	disabled bool,
	statuslineState string,
) {
	if account == nil {
		return
	}
	name := account.Account.Name
	if disabled {
		fmt.Fprintf(
			out,
			"Warning: CCR account-aware status line is disabled; subscription limits shown by Claude or an existing status line may belong to another local profile. Active account: %s.\n",
			name,
		)
		return
	}
	if statuslineState == "disabled" {
		fmt.Fprintf(
			out,
			"Warning: Claude statusLine is explicitly disabled by effective settings; CCR preserved that choice. Active account: %s; subscription limits remain unknown.\n",
			name,
		)
		return
	}
	fmt.Fprintf(
		out,
		"Subscription limits for account %s: unknown in the status line (use ccr claude-account test %s --live for advisory quota; routing does not reuse shared-profile data).\n",
		name,
		name,
	)
	writeSubscriptionStatuslineIsolationNotice(out, name, statuslineState)
}

func writeSubscriptionStatuslineIsolationNotice(out io.Writer, accountName, statuslineState string) {
	switch statuslineState {
	case "isolated":
		fmt.Fprintf(
			out,
			"Existing Claude statusLine preserved through a launch-only credential-isolation wrapper; CCR_CLAUDE_ACCOUNT=%s remains available while OAuth and gateway tokens are removed from that command's environment.\n",
			accountName,
		)
	case "replaced":
		fmt.Fprintln(
			out,
			"Existing Claude statusLine bypassed for this launch because credential isolation is unavailable on Windows; using CCR's account-aware status line instead.",
		)
	}
}
