package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type claudeAccountTestOptions struct {
	all  bool
	live bool
}

type claudeAccountProbeResult struct {
	name        string
	diagnostics claudeaccount.AccountDiagnostics
}

func newClaudeAccountTestCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var testOptions claudeAccountTestOptions
	cmd := &cobra.Command{
		Use:   "test [name]",
		Short: "Verify registered account credentials and optional live identity and quota",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveClaudeAccountTestTarget(args, testOptions.all)
			if err != nil {
				return err
			}
			return runClaudeAccountTests(ctx, cmd, opts, deps, name, testOptions)
		},
	}
	cmd.Flags().BoolVar(&testOptions.all, "all", false, "Test every registered Claude account")
	cmd.Flags().BoolVar(&testOptions.live, "live", false, "Query advisory account identity and quota diagnostics")
	return cmd
}

func resolveClaudeAccountTestTarget(args []string, all bool) (string, error) {
	switch {
	case all && len(args) != 0:
		return "", fmt.Errorf("--all cannot be combined with an account name")
	case all:
		return "", nil
	case len(args) == 0:
		return "", fmt.Errorf("provide an account name or --all")
	default:
		if err := validateName("Claude account name", args[0]); err != nil {
			return "", err
		}
		return args[0], nil
	}
}

func runClaudeAccountTests(
	ctx context.Context,
	cmd *cobra.Command,
	opts *options,
	deps Dependencies,
	name string,
	testOptions claudeAccountTestOptions,
) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	accounts, err := loadClaudeAccountTestTargets(ctx, s, name)
	if err != nil {
		return err
	}
	results := make([]claudeAccountProbeResult, 0, len(accounts))
	var failures []error
	for index := range accounts {
		result, testErr := testClaudeAccount(ctx, cmd, deps, accounts[index], testOptions.live)
		if testErr != nil {
			failures = append(failures, testErr)
			continue
		}
		if testOptions.live {
			results = append(results, result)
		}
	}
	if duplicateErr := duplicateClaudeAccountIdentityError(results); duplicateErr != nil {
		failures = append(failures, duplicateErr)
	}
	return errors.Join(failures...)
}

func loadClaudeAccountTestTargets(
	ctx context.Context,
	s *store.Store,
	name string,
) ([]store.ClaudeAccount, error) {
	if name != "" {
		account, err := s.GetClaudeAccount(ctx, name)
		if err != nil {
			return nil, err
		}
		return []store.ClaudeAccount{account}, nil
	}
	accounts, err := s.ListClaudeAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no Claude accounts registered")
	}
	return accounts, nil
}

func testClaudeAccount(
	ctx context.Context,
	cmd *cobra.Command,
	deps Dependencies,
	account store.ClaudeAccount,
	live bool,
) (claudeAccountProbeResult, error) {
	fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: checking local credential\n", account.Name)
	token, err := deps.Secrets.Resolve(ctx, account.AccessTokenRef)
	if err != nil {
		return claudeAccountProbeResult{}, fmt.Errorf(
			"resolving Claude account %q credential: %w", account.Name, err,
		)
	}
	token, err = claudeaccount.ValidateToken(token)
	if err != nil {
		return claudeAccountProbeResult{}, fmt.Errorf("claude account %q credential is invalid", account.Name)
	}
	if !live {
		fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: passed (credential resolved; no network request)\n", account.Name)
		return claudeAccountProbeResult{name: account.Name}, nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: querying live identity and quota\n", account.Name)
	diagnostics, err := deps.ProbeClaudeAccount(ctx, token)
	if err != nil {
		return claudeAccountProbeResult{}, fmt.Errorf("probing Claude account %q: %w", account.Name, err)
	}
	writeClaudeAccountDiagnostics(cmd.OutOrStdout(), account.Name, diagnostics)
	return claudeAccountProbeResult{name: account.Name, diagnostics: diagnostics}, nil
}

func writeClaudeAccountDiagnostics(
	out io.Writer,
	name string,
	diagnostics claudeaccount.AccountDiagnostics,
) {
	fmt.Fprintf(
		out, "Claude account %s: passed identity=%s plan=%s",
		name, diagnostics.IdentityFingerprint, diagnostics.Plan,
	)
	for _, window := range diagnostics.Windows {
		reset := "unknown"
		if !window.ResetsAt.IsZero() {
			reset = window.ResetsAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(
			out, " %s=%.2f%%(reset=%s)",
			window.Kind, window.Utilization, reset,
		)
	}
	fmt.Fprintln(out)
}

func duplicateClaudeAccountIdentityError(results []claudeAccountProbeResult) error {
	namesByIdentity := make(map[string][]string, len(results))
	for _, result := range results {
		identity := strings.TrimSpace(result.diagnostics.IdentityFingerprint)
		if identity != "" {
			namesByIdentity[identity] = append(namesByIdentity[identity], result.name)
		}
	}
	var duplicates []string
	for identity, names := range namesByIdentity {
		if len(names) > 1 {
			sort.Strings(names)
			duplicates = append(duplicates, fmt.Sprintf("identity %s is used by labels %s", identity, strings.Join(names, ",")))
		}
	}
	if len(duplicates) == 0 {
		return nil
	}
	sort.Strings(duplicates)
	return fmt.Errorf("duplicate Claude subscription credentials detected: %s", strings.Join(duplicates, "; "))
}
