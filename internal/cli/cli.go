package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/buildinfo"
	"github.com/hishamkaram/claude-code-router/internal/config"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

var errNotImplemented = errors.New("not implemented yet")

type Dependencies struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Secrets secret.Backend
}

type options struct {
	dbPath string
}

type secretPlan struct {
	ref   string
	value string
	store bool
}

type providerAddConfig struct {
	providerType string
	baseURL      string
	apiKeyEnv    string
	apiKeyValue  string
	apiKeyStdin  bool
	noAPIKey     bool
}

func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	cmd := NewRootCommand(ctx, Dependencies{
		In:      in,
		Out:     out,
		Err:     errOut,
		Secrets: secret.DefaultBackend{},
	})
	cmd.SetArgs(args)
	return cmd.Execute()
}

func NewRootCommand(ctx context.Context, deps Dependencies) *cobra.Command {
	if deps.Secrets == nil {
		deps.Secrets = secret.DefaultBackend{}
	}
	opts := &options{}
	cmd := &cobra.Command{
		Use:           "ccr",
		Short:         "Claude Code live model router",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `ccr manages a local Claude Code router.

Claude Code is launched once through a fixed local gateway, then model aliases
can be selected from inside the same Claude Code session with /model <alias>.

ccr stores providers, model aliases, compatibility metadata, sessions, and
usage metadata in a local SQLite database. API keys are never stored raw in
SQLite. Use environment-variable references or the OS keychain-backed prompt
flow when it is available on your machine.

Compatibility policy:
  - use the selected model wherever safely possible
  - degrade unsupported behavior visibly when safe
  - reject unsafe requests clearly
  - never silently fall back to Claude

Significant gateway behavior must be proven with live Claude Code E2E tests.`,
	}
	cmd.SetIn(deps.In)
	cmd.SetOut(deps.Out)
	cmd.SetErr(deps.Err)
	cmd.SetContext(ctx)
	cmd.PersistentFlags().StringVar(&opts.dbPath, "db", "", "SQLite database path (default: user data directory)")

	cmd.AddCommand(
		newVersionCommand(),
		newInitCommand(ctx, opts),
		newProviderCommand(ctx, opts, deps),
		newModelCommand(ctx, opts),
		newLaunchCommand(),
		newStatusCommand(ctx, opts),
		newDoctorCommand(ctx, opts, deps),
		newConformanceCommand(),
		newSessionsCommand(),
		newAgentsCommand(),
	)
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print ccr version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.String())
		},
	}
}

func newInitCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize the local SQLite database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, dbPath, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			fmt.Fprintf(cmd.OutOrStdout(), "Initialized claude-code-router database at %s\n", dbPath)
			return nil
		},
	}
}

func newProviderCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage provider connections",
		Long: `Manage provider connections.

Examples:
  ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
  ccr provider add --interactive litellm
  ccr provider add litellm --base-url http://localhost:4000 --no-api-key
  ccr provider add anthropic --api-key-stdin
  ccr provider discover-models litellm
  ccr provider import-models litellm --all
  ccr provider list`,
	}
	cmd.AddCommand(
		newProviderAddCommand(ctx, opts, deps),
		newProviderListCommand(ctx, opts),
		newProviderDiscoverModelsCommand(ctx, opts, deps),
		newProviderImportModelsCommand(ctx, opts, deps),
		newProviderTestCommand(),
		newNotImplementedCommand("remove <name>", "Remove a provider record", cobra.ExactArgs(1)),
	)
	return cmd
}

func newProviderAddCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var providerType string
	var baseURL string
	var apiKeyEnv string
	var apiKeyStdin bool
	var noAPIKey bool
	var interactive bool

	cmd := &cobra.Command{
		Use:   "add [name]",
		Short: "Add a provider and secret reference",
		Args: func(cmd *cobra.Command, args []string) error {
			if interactive {
				if len(args) > 1 {
					return fmt.Errorf("provider add --interactive accepts at most one provider name")
				}
				if len(args) == 1 {
					return validateName("provider name", args[0])
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider add openrouter --api-key-env OPENROUTER_API_KEY")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if interactive {
				name := ""
				if len(args) == 1 {
					name = args[0]
				}
				return runProviderAddInteractive(ctx, cmd, opts, deps, name, providerAddConfig{
					providerType: providerType,
					baseURL:      baseURL,
					apiKeyEnv:    apiKeyEnv,
					apiKeyStdin:  apiKeyStdin,
					noAPIKey:     noAPIKey,
				})
			}
			return runProviderAdd(ctx, cmd, opts, deps, args[0], providerAddConfig{
				providerType: providerType,
				baseURL:      baseURL,
				apiKeyEnv:    apiKeyEnv,
				apiKeyStdin:  apiKeyStdin,
				noAPIKey:     noAPIKey,
			})
		},
	}
	cmd.Flags().StringVar(&providerType, "type", "", "Provider type: anthropic, openrouter, litellm, or local (defaults to provider name when recognized)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Provider base URL")
	cmd.Flags().StringVar(&apiKeyEnv, "api-key-env", "", "Environment variable containing the API key; stores only env:<name>")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "Read API key from stdin and store it in the OS keychain")
	cmd.Flags().BoolVar(&noAPIKey, "no-api-key", false, "Declare that this provider does not need an API key")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Guide provider setup and optional model import with prompts")
	return cmd
}

func runProviderAdd(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, name string, cfg providerAddConfig) error {
	resolvedType, err := resolveProviderType(name, cfg.providerType)
	if err != nil {
		return err
	}
	resolvedURL, err := resolveBaseURL(resolvedType, cfg.baseURL)
	if err != nil {
		return err
	}
	plan, err := resolveProviderSecretPlan(deps, name, resolvedType, cfg.apiKeyEnv, cfg.apiKeyValue, cfg.apiKeyStdin, cfg.noAPIKey)
	if err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	exists, err := s.ProviderExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("provider %q already exists", name)
	}

	if plan.store {
		if err := deps.Secrets.Store(ctx, plan.ref, plan.value); err != nil {
			return fmt.Errorf("storing API key for provider %q: %w", name, err)
		}
	}
	if err := s.AddProvider(ctx, store.Provider{Name: name, Type: resolvedType, BaseURL: resolvedURL, SecretRef: plan.ref}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q added (%s, %s, secret=%s)\n", name, resolvedType, resolvedURL, secret.RedactRef(plan.ref))
	fmt.Fprintf(cmd.OutOrStdout(), "Next: ccr model add <alias> --provider %s --model <provider-model>\n", name)
	return nil
}

func newProviderListCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			providers, err := s.ListProviders(ctx)
			if err != nil {
				return err
			}
			if len(providers) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No providers configured.")
				return nil
			}
			for _, provider := range providers {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tsecret=%s\n", provider.Name, provider.Type, provider.BaseURL, secret.RedactRef(provider.SecretRef))
			}
			return nil
		},
	}
}

func newProviderTestCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Validate provider config and connectivity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("provider test for %q: %w", args[0], errNotImplemented)
		},
	}
}

func newModelCommand(ctx context.Context, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Manage Claude Code model aliases",
		Long: `Manage model aliases used by Claude Code /model.

Examples:
  ccr model add qwen --provider openrouter --model qwen/qwen3-coder
  ccr model add gpt --provider litellm --model gpt-5
  ccr model list`,
	}
	cmd.AddCommand(
		newModelAddCommand(ctx, opts),
		newModelListCommand(ctx, opts),
		newNotImplementedCommand("test <alias>", "Validate a model alias against its provider", cobra.ExactArgs(1)),
		newNotImplementedCommand("remove <alias>", "Remove a model alias", cobra.ExactArgs(1)),
	)
	return cmd
}

func newModelAddCommand(ctx context.Context, opts *options) *cobra.Command {
	var providerName string
	var providerModel string
	var status string
	cmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a model alias",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr model add qwen --provider openrouter --model qwen/qwen3-coder")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]
			if err := validateName("provider", providerName); err != nil {
				return err
			}
			if strings.TrimSpace(providerModel) == "" {
				return fmt.Errorf("--model is required")
			}
			if err := validateCompatibilityStatus(status); err != nil {
				return err
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			if _, err := s.GetProvider(ctx, providerName); err != nil {
				return err
			}
			if err := s.AddModel(ctx, store.Model{Alias: alias, ProviderName: providerName, ProviderModel: providerModel, Status: status}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q added for provider %q model %q (%s)\n", alias, providerName, providerModel, status)
			fmt.Fprintf(cmd.OutOrStdout(), "Next: ccr conformance run %s\n", alias)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "Configured provider name")
	cmd.Flags().StringVar(&providerModel, "model", "", "Provider-specific model name")
	cmd.Flags().StringVar(&status, "compat", "degraded", "Compatibility status: full, degraded, chat-only, or blocked")
	return cmd
}

func newModelListCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured model aliases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			models, err := s.ListModels(ctx)
			if err != nil {
				return err
			}
			if len(models) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No model aliases configured.")
				return nil
			}
			for _, model := range models {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tprovider=%s\tmodel=%s\tcompat=%s\n", model.Alias, model.ProviderName, model.ProviderModel, model.Status)
			}
			return nil
		},
	}
}

func newLaunchCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "launch",
		Short: "Launch Claude Code through the local router",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("launching Claude Code through the gateway: %w", errNotImplemented)
		},
	}
}

func newStatusCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local router status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, dbPath, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			providers, err := s.ListProviders(ctx)
			if err != nil {
				return err
			}
			models, err := s.ListModels(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Database: %s\nProviders: %d\nModels: %d\n", dbPath, len(providers), len(models))
			return nil
		},
	}
}

func newDoctorCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local database, secret backend, and Claude Code availability",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, dbPath, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			version, err := s.SchemaVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "SQLite: ok (%s, schema=%d)\n", dbPath, version)
			if err := deps.Secrets.Available(ctx); err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Secrets: unavailable (%v)\n", err)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Secrets: ok")
			}
			if path, err := exec.LookPath("claude"); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Claude Code: found (%s)\n", path)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Claude Code: not found in PATH")
			}
			return nil
		},
	}
}

func newConformanceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run model compatibility checks",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "run <alias>",
		Short: "Run conformance checks for a model alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("conformance run for %q: %w", args[0], errNotImplemented)
		},
	})
	return cmd
}

func newSessionsCommand() *cobra.Command {
	return newNotImplementedCommand("sessions", "List tracked Claude Code sessions", cobra.NoArgs)
}

func newAgentsCommand() *cobra.Command {
	return newNotImplementedCommand("agents", "List tracked Claude Code agents and workers", cobra.NoArgs)
}

func newNotImplementedCommand(use, short string, args cobra.PositionalArgs) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%s: %w", cmd.CommandPath(), errNotImplemented)
		},
	}
}

func openMigratedStore(ctx context.Context, opts *options) (*store.Store, string, error) {
	dbPath := opts.dbPath
	if dbPath == "" {
		var err error
		dbPath, err = config.DefaultDBPath()
		if err != nil {
			return nil, "", err
		}
	}
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, "", err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, "", err
	}
	return s, dbPath, nil
}

func closeStore(s *store.Store) {
	_ = s.Close()
}
