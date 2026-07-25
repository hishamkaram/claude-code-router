package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
	claudeProcessStopTimeout          = 5 * time.Second
)

type selectedClaudeAccount struct {
	Account    store.ClaudeAccount
	OAuthToken string
}

type subscriptionExhaustedError struct {
	Event      gateway.AnthropicSubscriptionExhaustionEvent
	CleanupErr error
}

type subscriptionPoolRelaunchPlan struct {
	enabled bool
	args    []string
	reason  string
}

type subscriptionPoolContinuityState struct {
	resume         bool
	continuitySeen bool
	worktreeSeen   bool
}

func (e *subscriptionExhaustedError) Error() string {
	if e.CleanupErr != nil {
		return fmt.Sprintf("Claude subscription account is rate limited and launch cleanup failed: %v", e.CleanupErr)
	}
	return "Claude subscription account is rate limited"
}

func (e *subscriptionExhaustedError) Unwrap() error {
	return e.CleanupErr
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
	relaunch := planSubscriptionPoolRelaunch(invocation)
	excluded := []string{}
	attempt := invocation
	for {
		selected, err := claimLaunchClaudeAccount(ctx, cmd, opts, deps, invocation.claudeAccount, excluded)
		if err != nil {
			return err
		}
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"Claude model-request account selected: %s (local label; OAuth token is fixed for this process).\n",
			selected.Account.Name,
		)
		writeSubscriptionRotationNotice(cmd.ErrOrStderr(), relaunch)
		exhaustion := make(chan gateway.AnthropicSubscriptionExhaustionEvent, 1)
		err = runLaunchAttempt(ctx, cmd, opts, deps, attempt, &selected, exhaustion)
		var rateLimited *subscriptionExhaustedError
		if !errors.As(err, &rateLimited) {
			return err
		}
		excluded = append(excluded, selected.Account.Name)
		cooldownUntil := subscriptionCooldownUntil(time.Now().UTC(), rateLimited.Event)
		failureClass := subscriptionRateLimitFailureClass(rateLimited.Event)
		if markErr := markClaudeAccountRateLimited(
			ctx, opts, selected.Account.Name, cooldownUntil, failureClass,
		); markErr != nil {
			return errors.Join(err, markErr)
		}
		if rateLimited.CleanupErr != nil {
			return err
		}
		if !relaunch.enabled {
			return fmt.Errorf(
				"claude account %q %s; unavailable until %s; select another account or clear a false cooldown with ccr claude-account clear-cooldown %s: %w",
				selected.Account.Name,
				subscriptionRateLimitDescription(rateLimited.Event),
				cooldownUntil.Format(time.RFC3339),
				selected.Account.Name,
				err,
			)
		}
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"Claude account %s %s; unavailable until %s; relaunching with the next available account.\n",
			selected.Account.Name,
			subscriptionRateLimitDescription(rateLimited.Event),
			cooldownUntil.Format(time.RFC3339),
		)
		attempt.claudeArgs = append([]string(nil), relaunch.args...)
	}
}

func subscriptionPoolCanRelaunch(invocation launchInvocation) bool {
	return planSubscriptionPoolRelaunch(invocation).enabled
}

func planSubscriptionPoolRelaunch(invocation launchInvocation) subscriptionPoolRelaunchPlan {
	switch {
	case invocation.printMode:
		return subscriptionPoolRelaunchPlan{reason: "--print is a one-shot Claude Code process"}
	case invocation.claudeAccount != "":
		return subscriptionPoolRelaunchPlan{reason: "--claude-account pins one account"}
	case invocation.cuaOptionsConfigured():
		return subscriptionPoolRelaunchPlan{reason: "managed CUA launch state cannot be resumed safely"}
	}
	args, resumable := subscriptionPoolResumeArgs(invocation.claudeArgs)
	if !resumable {
		return subscriptionPoolRelaunchPlan{
			reason: "the Claude Code arguments are not a replay-safe --resume/--continue worktree launch",
		}
	}
	return subscriptionPoolRelaunchPlan{enabled: true, args: args}
}

func subscriptionPoolResumeArgs(args []string) ([]string, bool) {
	if len(args) == 0 {
		return []string{"--continue"}, true
	}
	state := subscriptionPoolContinuityState{}
	for index := 0; index < len(args); index++ {
		option, inlineValue, inline := strings.Cut(args[index], "=")
		if !state.accept(option, inline) {
			return nil, false
		}
		if option == "--continue" || option == "-c" {
			continue
		}
		if inline && strings.TrimSpace(inlineValue) == "" {
			return nil, false
		}
		if !inline {
			index++
			if index >= len(args) ||
				strings.TrimSpace(args[index]) == "" ||
				strings.HasPrefix(args[index], "-") {
				return nil, false
			}
		}
	}
	relaunchArgs := append([]string(nil), args...)
	if !state.resume {
		relaunchArgs = append(relaunchArgs, "--continue")
	}
	return relaunchArgs, true
}

func (s *subscriptionPoolContinuityState) accept(option string, inline bool) bool {
	switch option {
	case "--continue", "-c":
		if inline || s.continuitySeen {
			return false
		}
		s.continuitySeen = true
		s.resume = true
	case "--resume", "-r":
		if s.continuitySeen {
			return false
		}
		s.continuitySeen = true
		s.resume = true
	case "--worktree", "-w":
		if s.worktreeSeen {
			return false
		}
		s.worktreeSeen = true
	default:
		return false
	}
	return true
}

func writeSubscriptionRotationNotice(out io.Writer, plan subscriptionPoolRelaunchPlan) {
	if plan.enabled {
		fmt.Fprintln(out, "Automatic account rotation: enabled for rejected first-party quota responses.")
		return
	}
	fmt.Fprintf(out, "Automatic account rotation: disabled because %s.\n", plan.reason)
}

func claimLaunchClaudeAccount(
	ctx context.Context,
	cmd *cobra.Command,
	opts *options,
	deps Dependencies,
	explicitName string,
	excluded []string,
) (selectedClaudeAccount, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return selectedClaudeAccount{}, err
	}
	defer closeStore(s)

	for {
		account, found, claimErr := claimEligibleClaudeAccount(ctx, s, explicitName, excluded)
		if claimErr != nil {
			return selectedClaudeAccount{}, claimErr
		}
		if !found {
			return selectedClaudeAccount{}, noUsableClaudeAccountError(explicitName)
		}
		selected, resolveErr := resolveClaimedClaudeAccount(ctx, deps, account)
		if resolveErr == nil {
			return selected, nil
		}
		cooldown := time.Now().UTC().Add(credentialFailureCooldown)
		if markErr := s.MarkClaudeAccountFailure(ctx, account.Name, cooldown, "credential_unavailable"); markErr != nil {
			return selectedClaudeAccount{}, errors.Join(
				fmt.Errorf("resolving Claude account %q credential: %w", account.Name, resolveErr),
				markErr,
			)
		}
		if reportErr := reportUnavailableClaudeCredential(cmd, account.Name, explicitName != "", cooldown, resolveErr); reportErr != nil {
			return selectedClaudeAccount{}, reportErr
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
	cmd *cobra.Command,
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
		cmd.ErrOrStderr(),
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
			"claude account %q is disabled, expired, or cooling down; inspect it with ccr claude-account show %s",
			explicitName, explicitName,
		)
	}
	return fmt.Errorf(
		"claude subscription pool has no usable accounts; run ccr claude-account list and refresh, enable, clear-cooldown, or add an account",
	)
}

func markClaudeAccountRateLimited(
	ctx context.Context,
	opts *options,
	name string,
	cooldownUntil time.Time,
	failureClass string,
) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	if err := s.MarkClaudeAccountFailure(ctx, name, cooldownUntil, failureClass); err != nil {
		return fmt.Errorf("marking Claude account %q rate limited: %w", name, err)
	}
	return nil
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

func selectedClaudeAccountToken(account *selectedClaudeAccount) string {
	if account == nil {
		return ""
	}
	return account.OAuthToken
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
	replacedExisting bool,
) {
	if account == nil {
		return
	}
	if disabled {
		fmt.Fprintf(
			out,
			"Warning: CCR account-aware status line is disabled; subscription limits shown by Claude or an existing status line may belong to another local profile. Active account: %s.\n",
			account.Account.Name,
		)
		return
	}
	fmt.Fprintf(
		out,
		"Subscription limits for account %s: unknown in the status line (use ccr claude-account test %s --live for advisory quota; routing does not reuse shared-profile data).\n",
		account.Account.Name,
		account.Account.Name,
	)
	if replacedExisting {
		fmt.Fprintln(
			out,
			"Existing Claude statusLine bypassed for this launch so another profile's subscription limits are not presented as current; use --no-statusline to opt out.",
		)
	}
}

func waitForClaudeProcess(
	ctx context.Context,
	process ClaudeProcess,
	exhaustion <-chan gateway.AnthropicSubscriptionExhaustionEvent,
) (waitErr error, exhausted *gateway.AnthropicSubscriptionExhaustionEvent, stopErr error) {
	done := process.Done()
	if exhaustion == nil {
		return <-done, nil, nil
	}
	select {
	case event := <-exhaustion:
		stopErr = stopClaudeProcessAndWait(process, done, claudeProcessStopTimeout)
		return nil, &event, stopErr
	case waitErr = <-done:
		select {
		case event := <-exhaustion:
			return nil, &event, nil
		default:
			return waitErr, nil, nil
		}
	case <-ctx.Done():
		stopErr = stopClaudeProcessAndWait(process, done, claudeProcessStopTimeout)
		return ctx.Err(), nil, stopErr
	}
}

func stopClaudeProcessAndWait(process ClaudeProcess, done <-chan error, timeout time.Duration) error {
	stopErr := process.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return stopErr
	case <-timer.C:
		return errors.Join(stopErr, fmt.Errorf("timed out after %s waiting for Claude Code to stop", timeout))
	}
}
