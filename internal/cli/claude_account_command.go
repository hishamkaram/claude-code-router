package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type claudeAccountCredentialOptions struct {
	name            string
	from            string
	oauthTokenStdin bool
}

func newClaudeAccountCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude-account",
		Short: "Manage local Claude subscription accounts",
		Long: `Manage local Claude subscription accounts used by subscription-pool launches.

CCR stores account metadata and secret references in SQLite. OAuth access and
refresh tokens are stored only in the OS keychain. Account rotation always
starts a new Claude Code process and is never performed inside a running
session.

Examples:
  ccr claude-account import personal --from current
  ccr claude-account import work --oauth-token-stdin
  ccr claude-account list
  ccr claude-account test personal
  ccr claude-account refresh personal --from current
  ccr claude-account disable work
  ccr launch --auth-mode subscription-pool`,
	}
	cmd.AddCommand(
		newClaudeAccountImportCommand(ctx, opts, deps),
		newClaudeAccountListCommand(ctx, opts),
		newClaudeAccountShowCommand(ctx, opts),
		newClaudeAccountTestCommand(ctx, opts, deps),
		newClaudeAccountRefreshCommand(ctx, opts, deps),
		newClaudeAccountEnableCommand(ctx, opts, true),
		newClaudeAccountEnableCommand(ctx, opts, false),
		newClaudeAccountRemoveCommand(ctx, opts, deps),
	)
	return cmd
}

func newClaudeAccountImportCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var credentialOptions claudeAccountCredentialOptions
	var replace bool
	cmd := &cobra.Command{
		Use:   "import [name]",
		Short: "Import a Claude account into the local keychain-backed registry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveClaudeAccountName(args, credentialOptions.name)
			if err != nil {
				return err
			}
			credentialOptions.name = name
			return runClaudeAccountImport(ctx, cmd, opts, deps, credentialOptions, replace, false)
		},
	}
	addClaudeAccountCredentialFlags(cmd, &credentialOptions)
	cmd.Flags().BoolVar(&replace, "replace", false, "Replace an existing account's credentials")
	return cmd
}

func newClaudeAccountRefreshCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var credentialOptions claudeAccountCredentialOptions
	cmd := &cobra.Command{
		Use:   "refresh [name]",
		Short: "Replace a registered account's credentials explicitly",
		Long: `Replace a registered account's credentials explicitly.

CCR does not call an undocumented OAuth refresh endpoint. Re-import the active
Claude login with --from current, or generate a new long-lived token with
claude setup-token and provide it through --oauth-token-stdin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveClaudeAccountName(args, credentialOptions.name)
			if err != nil {
				return err
			}
			credentialOptions.name = name
			return runClaudeAccountImport(ctx, cmd, opts, deps, credentialOptions, true, true)
		},
	}
	addClaudeAccountCredentialFlags(cmd, &credentialOptions)
	return cmd
}

func addClaudeAccountCredentialFlags(cmd *cobra.Command, options *claudeAccountCredentialOptions) {
	cmd.Flags().StringVar(&options.name, "name", "", "Account name (alternative to the positional name)")
	cmd.Flags().StringVar(&options.from, "from", "", "Import source; supported value: current")
	cmd.Flags().BoolVar(&options.oauthTokenStdin, "oauth-token-stdin", false, "Read an OAuth token from stdin into the OS keychain")
}

func resolveClaudeAccountName(args []string, flagName string) (string, error) {
	name := strings.TrimSpace(flagName)
	if len(args) == 1 {
		if name != "" && name != args[0] {
			return "", fmt.Errorf("claude account name was provided twice with different values")
		}
		name = args[0]
	}
	if err := validateName("Claude account name", name); err != nil {
		return "", err
	}
	return name, nil
}

func runClaudeAccountImport(
	ctx context.Context,
	cmd *cobra.Command,
	opts *options,
	deps Dependencies,
	options claudeAccountCredentialOptions,
	replace bool,
	refresh bool,
) error {
	if err := validateClaudeAccountCredentialSource(options); err != nil {
		return err
	}
	s, existing, exists, err := preflightClaudeAccountImport(
		ctx, opts, options.name, replace, refresh,
	)
	if err != nil {
		return err
	}
	if s != nil {
		defer closeStore(s)
	}
	if availabilityErr := deps.Secrets.Available(ctx); availabilityErr != nil {
		return fmt.Errorf(
			"claude account import requires a working OS keychain; configure or unlock it and retry (OAuth credentials cannot use API-key environment or file references): %w",
			availabilityErr,
		)
	}
	credentials, err := readClaudeAccountCredentials(cmd.InOrStdin(), cmd.ErrOrStderr(), options)
	if err != nil {
		return err
	}
	if s == nil {
		s, _, err = openMigratedStore(ctx, opts)
		if err != nil {
			return err
		}
		defer closeStore(s)
		existing, exists, err = inspectClaudeAccountImportState(
			ctx, s, options.name, replace, refresh,
		)
		if err != nil {
			return err
		}
	}
	account := newClaudeAccountImport(options.name, existing, credentials, exists)
	if err := persistClaudeAccountImport(ctx, s, deps.Secrets, existing, account, credentials, exists); err != nil {
		return err
	}
	action := "imported"
	if exists {
		action = "refreshed"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: %s (credentials stored in OS keychain)\n", options.name, action)
	return nil
}

func newClaudeAccountImport(
	name string,
	existing store.ClaudeAccount,
	credentials claudeaccount.Credentials,
	exists bool,
) store.ClaudeAccount {
	account := store.ClaudeAccount{
		Name: name, AccessTokenRef: secret.ClaudeAccountAccessTokenRef(name),
		ExpiresAt: credentials.ExpiresAt, ScopesJSON: credentials.ScopesJSON, Enabled: true,
	}
	if exists {
		account.ID = existing.ID
		account.Enabled = existing.Enabled
		account.LastRefreshAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if credentials.RefreshToken != "" {
		account.RefreshTokenRef = secret.ClaudeAccountRefreshTokenRef(name)
	}
	return account
}

func persistClaudeAccountImport(
	ctx context.Context,
	s *store.Store,
	backend secret.Backend,
	existing, account store.ClaudeAccount,
	credentials claudeaccount.Credentials,
	exists bool,
) error {
	rollbackSecrets, err := storeClaudeAccountSecrets(ctx, backend, existing, account, credentials, exists)
	if err != nil {
		return err
	}
	if exists && existing.RefreshTokenRef != "" && account.RefreshTokenRef == "" {
		if err := deleteSecretRef(ctx, backend, existing.RefreshTokenRef); err != nil {
			return errors.Join(
				fmt.Errorf("removing obsolete Claude account refresh token: %w", err),
				rollbackSecrets(),
			)
		}
	}
	if err := saveClaudeAccount(ctx, s, account, exists); err != nil {
		return errors.Join(err, rollbackSecrets())
	}
	return nil
}

func readClaudeAccountCredentials(input io.Reader, errOut io.Writer, options claudeAccountCredentialOptions) (claudeaccount.Credentials, error) {
	if err := validateClaudeAccountCredentialSource(options); err != nil {
		return claudeaccount.Credentials{}, err
	}
	from := strings.TrimSpace(options.from)
	if from != "" {
		return claudeaccount.ReadCurrentCredentials()
	}
	if isTerminal(input) {
		return readTerminalClaudeOAuthToken(input, errOut)
	}
	return claudeaccount.CredentialsFromToken(input)
}

func validateClaudeAccountCredentialSource(options claudeAccountCredentialOptions) error {
	from := strings.TrimSpace(options.from)
	sourceCount := 0
	if from != "" {
		sourceCount++
	}
	if options.oauthTokenStdin {
		sourceCount++
	}
	if sourceCount != 1 {
		return fmt.Errorf("choose exactly one credential source: --from current or --oauth-token-stdin")
	}
	if from != "" && from != "current" {
		return fmt.Errorf("unsupported Claude account import source %q; expected current", from)
	}
	return nil
}

func readTerminalClaudeOAuthToken(input io.Reader, errOut io.Writer) (claudeaccount.Credentials, error) {
	file, ok := input.(interface{ Fd() uintptr })
	if !ok {
		return claudeaccount.Credentials{}, fmt.Errorf("reading Claude OAuth token: terminal input is unavailable")
	}
	fmt.Fprint(errOut, "Claude OAuth token: ")
	raw, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(errOut)
	if err != nil {
		return claudeaccount.Credentials{}, fmt.Errorf("reading Claude OAuth token without echo: %w", err)
	}
	return claudeaccount.CredentialsFromToken(bytes.NewReader(raw))
}

func findClaudeAccount(ctx context.Context, s *store.Store, name string) (store.ClaudeAccount, bool, error) {
	account, err := s.GetClaudeAccount(ctx, name)
	if err == nil {
		return account, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ClaudeAccount{}, false, nil
	}
	return store.ClaudeAccount{}, false, err
}

func storeClaudeAccountSecrets(
	ctx context.Context,
	backend secret.Backend,
	existing store.ClaudeAccount,
	account store.ClaudeAccount,
	credentials claudeaccount.Credentials,
	replace bool,
) (func() error, error) {
	previous, err := snapshotClaudeAccountSecrets(ctx, backend, existing, replace)
	if err != nil {
		return func() error { return nil }, err
	}
	rollback := func() error {
		return rollbackClaudeAccountSecrets(context.WithoutCancel(ctx), backend, account, previous)
	}
	if err := backend.Store(ctx, account.AccessTokenRef, credentials.AccessToken); err != nil {
		return func() error { return nil }, fmt.Errorf("storing Claude account access token: %w", err)
	}
	if account.RefreshTokenRef == "" {
		return rollback, nil
	}
	if err := backend.Store(ctx, account.RefreshTokenRef, credentials.RefreshToken); err != nil {
		return func() error { return nil }, errors.Join(
			fmt.Errorf("storing Claude account refresh token: %w", err),
			rollback(),
		)
	}
	return rollback, nil
}

func snapshotClaudeAccountSecrets(
	ctx context.Context,
	backend secret.Backend,
	existing store.ClaudeAccount,
	replace bool,
) (map[string]string, error) {
	previous := make(map[string]string, 2)
	if !replace {
		return previous, nil
	}
	for _, ref := range []string{existing.AccessTokenRef, existing.RefreshTokenRef} {
		if ref == "" {
			continue
		}
		value, err := backend.Resolve(ctx, ref)
		if err != nil {
			if errors.Is(err, secret.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("reading existing Claude account credential for safe replacement: %w", err)
		}
		previous[ref] = value
	}
	return previous, nil
}

func rollbackClaudeAccountSecrets(
	ctx context.Context,
	backend secret.Backend,
	account store.ClaudeAccount,
	previous map[string]string,
) error {
	var rollbackErr error
	for ref, value := range previous {
		if err := backend.Store(ctx, ref, value); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restoring %s: %w", secret.RedactRef(ref), err))
		}
	}
	for _, ref := range []string{account.AccessTokenRef, account.RefreshTokenRef} {
		if ref == "" {
			continue
		}
		if _, existed := previous[ref]; existed {
			continue
		}
		rollbackErr = errors.Join(rollbackErr, deleteSecretRef(ctx, backend, ref))
	}
	return rollbackErr
}

func saveClaudeAccount(ctx context.Context, s *store.Store, account store.ClaudeAccount, exists bool) error {
	if exists {
		if err := s.UpdateClaudeAccount(ctx, account); err != nil {
			return fmt.Errorf("updating Claude account %q: %w", account.Name, err)
		}
		return nil
	}
	if _, err := s.AddClaudeAccount(ctx, account); err != nil {
		return fmt.Errorf("adding Claude account %q: %w", account.Name, err)
	}
	return nil
}

func newClaudeAccountTestCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Verify that a registered account credential resolves locally",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateName("Claude account name", args[0]); err != nil {
				return err
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			account, err := s.GetClaudeAccount(ctx, args[0])
			if err != nil {
				return err
			}
			token, err := deps.Secrets.Resolve(ctx, account.AccessTokenRef)
			if err != nil {
				return fmt.Errorf("resolving Claude account %q credential: %w", account.Name, err)
			}
			if _, err := claudeaccount.ValidateToken(token); err != nil {
				return fmt.Errorf("claude account %q credential is invalid", account.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: passed (credential resolved; no network request)\n", account.Name)
			return nil
		},
	}
}

func newClaudeAccountEnableCommand(ctx context.Context, opts *options, enabled bool) *cobra.Command {
	action := "enable"
	if !enabled {
		action = "disable"
	}
	return &cobra.Command{
		Use:   action + " <name>",
		Short: strings.ToUpper(action[:1]) + action[1:] + " a registered Claude account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateName("Claude account name", args[0]); err != nil {
				return err
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			if enabled {
				err = s.EnableClaudeAccount(ctx, args[0])
			} else {
				err = s.DisableClaudeAccount(ctx, args[0])
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: %sd\n", args[0], action)
			return nil
		},
	}
}

func newClaudeAccountRemoveCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a Claude account and its keychain credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateName("Claude account name", args[0]); err != nil {
				return err
			}
			if !yes {
				return fmt.Errorf("refusing to remove Claude account %q without --yes", args[0])
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			account, err := s.GetClaudeAccount(ctx, args[0])
			if err != nil {
				return err
			}
			if err := cleanupClaudeAccountSecrets(ctx, deps.Secrets, account); err != nil {
				return fmt.Errorf("claude account keychain cleanup failed; metadata was retained for retry: %w", err)
			}
			if err := s.RemoveClaudeAccount(ctx, account.Name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: removed\n", account.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm account and keychain credential removal")
	return cmd
}

func cleanupClaudeAccountSecrets(ctx context.Context, backend secret.Backend, account store.ClaudeAccount) error {
	return errors.Join(
		deleteSecretRef(ctx, backend, account.AccessTokenRef),
		deleteSecretRef(ctx, backend, account.RefreshTokenRef),
	)
}

func deleteSecretRef(ctx context.Context, backend secret.Backend, ref string) error {
	if ref == "" {
		return nil
	}
	if deleter, ok := backend.(secret.Deleter); ok {
		return deleter.Delete(ctx, ref)
	}
	return fmt.Errorf("secret backend does not support deleting %s", secret.RedactRef(ref))
}

func claudeAccountStatus(account store.ClaudeAccount, now time.Time) string {
	if !account.Enabled {
		return "disabled"
	}
	if timestampElapsed(account.ExpiresAt, now) {
		return "expired"
	}
	if timestampPending(account.CooldownUntil, now) {
		return "cooldown"
	}
	return "ready"
}

func timestampElapsed(value string, now time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, value)
	return err == nil && !parsed.After(now)
}

func timestampPending(value string, now time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, value)
	return err == nil && parsed.After(now)
}

func displayTimestamp(value string) string {
	if value == "" {
		return "never"
	}
	return value
}

func displayValue(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
