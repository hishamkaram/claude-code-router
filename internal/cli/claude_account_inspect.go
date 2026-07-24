package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type claudeAccountView struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	AccessTokenRef  string `json:"access_token_ref"`
	RefreshTokenRef string `json:"refresh_token_ref,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	Enabled         bool   `json:"enabled"`
	CooldownUntil   string `json:"cooldown_until,omitempty"`
	LastUsedAt      string `json:"last_used_at,omitempty"`
	LastRefreshAt   string `json:"last_refresh_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func newClaudeAccountListCommand(ctx context.Context, opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered Claude accounts without credential values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			accounts, err := s.ListClaudeAccounts(ctx)
			if err != nil {
				return err
			}
			return writeClaudeAccountList(cmd.OutOrStdout(), accounts, jsonOutput, time.Now().UTC())
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned redacted JSON")
	return cmd
}

func writeClaudeAccountList(
	out io.Writer,
	accounts []store.ClaudeAccount,
	jsonOutput bool,
	now time.Time,
) error {
	if jsonOutput {
		views := make([]claudeAccountView, 0, len(accounts))
		for index := range accounts {
			views = append(views, newClaudeAccountView(accounts[index], now))
		}
		return writeVersionedJSON(out, struct {
			SchemaVersion int                 `json:"schema_version"`
			Accounts      []claudeAccountView `json:"accounts"`
		}{SchemaVersion: 1, Accounts: views})
	}
	if len(accounts) == 0 {
		fmt.Fprintln(out, "No Claude accounts registered.")
		return nil
	}
	for index := range accounts {
		account := &accounts[index]
		fmt.Fprintf(
			out, "%s\tstatus=%s\tlast_used=%s\n",
			account.Name, claudeAccountStatus(*account, now), displayTimestamp(account.LastUsedAt),
		)
	}
	return nil
}

func newClaudeAccountShowCommand(ctx context.Context, opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show redacted Claude account metadata",
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
			return writeClaudeAccount(cmd.OutOrStdout(), account, jsonOutput, time.Now().UTC())
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned redacted JSON")
	return cmd
}

func writeClaudeAccount(out io.Writer, account store.ClaudeAccount, jsonOutput bool, now time.Time) error {
	if jsonOutput {
		return writeVersionedJSON(out, struct {
			SchemaVersion int               `json:"schema_version"`
			Account       claudeAccountView `json:"account"`
		}{SchemaVersion: 1, Account: newClaudeAccountView(account, now)})
	}
	fmt.Fprintf(out, "Name: %s\n", account.Name)
	fmt.Fprintf(out, "Status: %s\n", claudeAccountStatus(account, now))
	fmt.Fprintf(out, "Access token: %s\n", secret.RedactRef(account.AccessTokenRef))
	fmt.Fprintf(out, "Refresh token: %s\n", displayValue(secret.RedactRef(account.RefreshTokenRef)))
	fmt.Fprintf(out, "Expires: %s\n", displayTimestamp(account.ExpiresAt))
	fmt.Fprintf(out, "Cooldown: %s\n", displayTimestamp(account.CooldownUntil))
	fmt.Fprintf(out, "Last used: %s\n", displayTimestamp(account.LastUsedAt))
	fmt.Fprintf(out, "Last error: %s\n", displayValue(account.LastError))
	return nil
}

func newClaudeAccountView(account store.ClaudeAccount, now time.Time) claudeAccountView {
	return claudeAccountView{
		Name: account.Name, Status: claudeAccountStatus(account, now),
		AccessTokenRef:  secret.RedactRef(account.AccessTokenRef),
		RefreshTokenRef: secret.RedactRef(account.RefreshTokenRef),
		ExpiresAt:       account.ExpiresAt, Enabled: account.Enabled,
		CooldownUntil: account.CooldownUntil, LastUsedAt: account.LastUsedAt,
		LastRefreshAt: account.LastRefreshAt, LastError: account.LastError,
		CreatedAt: account.CreatedAt, UpdatedAt: account.UpdatedAt,
	}
}
