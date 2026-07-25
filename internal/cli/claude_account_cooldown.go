package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newClaudeAccountClearCooldownCommand(ctx context.Context, opts *options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "clear-cooldown [name]",
		Short: "Clear Claude account cooldown state",
		Long: `Clear cooldown and last-error state after verifying that an account is usable.

This command does not change account enablement, expiry, or keychain credentials.`,
		Args: func(_ *cobra.Command, args []string) error {
			return validateClaudeAccountClearCooldownArgs(args, all)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)

			if all {
				affected, clearErr := s.ClearAllClaudeAccountFailures(ctx)
				if clearErr != nil {
					return clearErr
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Claude account cooldowns cleared: %d\n", affected)
				return nil
			}
			if err := s.ClearClaudeAccountFailure(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Claude account %s: cooldown cleared\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Clear cooldown state for all registered Claude accounts")
	return cmd
}

func validateClaudeAccountClearCooldownArgs(args []string, all bool) error {
	if all {
		if len(args) != 0 {
			return fmt.Errorf("--all cannot be combined with a Claude account name")
		}
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("requires exactly one Claude account name or --all")
	}
	return validateName("Claude account name", args[0])
}
