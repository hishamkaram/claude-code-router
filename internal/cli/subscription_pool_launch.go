package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	defaultSubscriptionCooldown = 15 * time.Minute
	credentialFailureCooldown   = 5 * time.Minute
	maxSubscriptionCooldown     = 24 * time.Hour
	claudeProcessStopTimeout    = 5 * time.Second
)

type selectedClaudeAccount struct {
	Account    store.ClaudeAccount
	OAuthToken string
}

type subscriptionExhaustedError struct {
	Event      gateway.AnthropicSubscriptionExhaustionEvent
	CleanupErr error
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
	automaticRelaunch := subscriptionPoolCanRelaunch(invocation)
	excluded := []string{}
	attempt := invocation
	for {
		selected, err := claimLaunchClaudeAccount(ctx, cmd, opts, deps, invocation.claudeAccount, excluded)
		if err != nil {
			return err
		}
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"Claude account selected: %s (identity is fixed for this Claude Code process).\n",
			selected.Account.Name,
		)
		exhaustion := make(chan gateway.AnthropicSubscriptionExhaustionEvent, 1)
		err = runLaunchAttempt(ctx, cmd, opts, deps, attempt, &selected, exhaustion)
		var rateLimited *subscriptionExhaustedError
		if !errors.As(err, &rateLimited) {
			return err
		}
		excluded = append(excluded, selected.Account.Name)
		cooldownUntil := subscriptionCooldownUntil(time.Now().UTC(), rateLimited.Event)
		if markErr := markClaudeAccountExhausted(ctx, opts, selected.Account.Name, cooldownUntil); markErr != nil {
			return errors.Join(err, markErr)
		}
		if rateLimited.CleanupErr != nil {
			return err
		}
		if !automaticRelaunch {
			return fmt.Errorf(
				"claude account %q is rate limited until %s; rerun the command to select another account: %w",
				selected.Account.Name, cooldownUntil.Format(time.RFC3339), err,
			)
		}
		fmt.Fprintf(
			cmd.ErrOrStderr(),
			"Claude account %s is rate limited until %s; relaunching with the next available account.\n",
			selected.Account.Name,
			cooldownUntil.Format(time.RFC3339),
		)
		attempt.claudeArgs = []string{"--continue"}
	}
}

func subscriptionPoolCanRelaunch(invocation launchInvocation) bool {
	return !invocation.printMode &&
		invocation.claudeAccount == "" &&
		len(invocation.claudeArgs) == 0 &&
		!invocation.cuaOptionsConfigured()
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
		"claude subscription pool has no usable accounts; run ccr claude-account list and refresh, enable, or add an account",
	)
}

func markClaudeAccountExhausted(
	ctx context.Context,
	opts *options,
	name string,
	cooldownUntil time.Time,
) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	if err := s.MarkClaudeAccountFailure(ctx, name, cooldownUntil, "rate_limited"); err != nil {
		return fmt.Errorf("marking Claude account %q rate limited: %w", name, err)
	}
	return nil
}

func subscriptionCooldownUntil(
	now time.Time,
	event gateway.AnthropicSubscriptionExhaustionEvent,
) time.Time {
	cooldown := defaultSubscriptionCooldown
	if event.RetryAfterDuration > 0 {
		cooldown = event.RetryAfterDuration
	} else if event.RetryAfterTime.After(now) {
		cooldown = event.RetryAfterTime.Sub(now)
	}
	if cooldown > maxSubscriptionCooldown {
		cooldown = maxSubscriptionCooldown
	}
	if cooldown <= 0 {
		cooldown = defaultSubscriptionCooldown
	}
	return now.Add(cooldown)
}

func selectedClaudeAccountToken(account *selectedClaudeAccount) string {
	if account == nil {
		return ""
	}
	return account.OAuthToken
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
