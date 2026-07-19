package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/buildinfo"
	"github.com/hishamkaram/claude-code-router/internal/config"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type Dependencies struct {
	In           io.Reader
	Out          io.Writer
	Err          io.Writer
	Secrets      secret.Backend
	Launcher     ClaudeLauncher
	StartGateway func(context.Context, gateway.Config) (*gateway.Server, error)
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
	protocol     string
	mode         string
	baseURL      string
	apiKeyEnv    string
	apiKeyFile   string
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
	if deps.Launcher == nil {
		deps.Launcher = ExecClaudeLauncher{}
	}
	if deps.StartGateway == nil {
		deps.StartGateway = gateway.Start
	}
	opts := &options{}
	cmd := &cobra.Command{
		Use:           "ccr",
		Short:         "Claude Code live model router",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `ccr manages a local Claude Code router.

	Claude Code is launched once through a fixed local gateway. First-party Claude
	model names route to Anthropic, and configured CCR aliases are exposed in
	Claude Code's /model picker. ccr launch keeps Claude Code's normal startup
	model unless you explicitly pass ccr launch --model <alias>.

ccr stores providers, model aliases, compatibility metadata, sessions, and
usage metadata in a local SQLite database. API keys are never stored raw in
SQLite. Use environment-variable references, 0600 API-key file references, or
the OS keychain-backed prompt flow when it is available on your machine.

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
		newModelCommand(ctx, opts, deps),
		newProfileCommand(ctx, opts),
		newLaunchCommand(ctx, opts, deps),
		newStatusCommand(ctx, opts),
		newTraceCommand(ctx, opts),
		newDoctorCommand(ctx, opts, deps),
		newConformanceCommand(ctx, opts, deps),
		newSessionsCommand(ctx, opts),
		newAgentsCommand(ctx, opts),
		newStatuslineCommand(),
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
	  ccr provider add --interactive
	  ccr provider add --interactive litellm
	  ccr provider add litellm --base-url http://localhost:4000 --api-key-file ~/.config/ccr/litellm.key
	  ccr provider add litellm --base-url http://localhost:4000 --no-api-key
	  ccr provider add anthropic --api-key-stdin
	  ccr provider discover-models litellm
	  ccr provider import-models litellm
	  ccr provider import-models litellm --all
	  ccr provider test litellm
	  ccr provider update litellm --base-url http://localhost:5000
	  ccr provider remove litellm --yes
	  ccr provider list`,
	}
	cmd.AddCommand(
		newProviderAddCommand(ctx, opts, deps),
		newProviderListCommand(ctx, opts),
		newProviderDiscoverModelsCommand(ctx, opts, deps),
		newProviderImportModelsCommand(ctx, opts, deps),
		newProviderTestCommand(ctx, opts, deps),
		newProviderUpdateCommand(ctx, opts, deps),
		newProviderRemoveCommand(ctx, opts, deps),
	)
	return cmd
}

func newProviderAddCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var providerType string
	var protocol string
	var mode string
	var baseURL string
	var apiKeyEnv string
	var apiKeyFile string
	var apiKeyStdin bool
	var noAPIKey bool
	var interactive bool

	cmd := &cobra.Command{
		Use:   "add [name]",
		Short: "Add a provider and secret reference",
		Long: `Add a provider and secret reference.

	Use --interactive for the guided provider profile picker, credential setup, and
	model import flow. The optional name becomes an editable connection-name default.

	Examples:
	  ccr provider add --interactive
	  ccr provider add --interactive openrouter
	  ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
	  ccr provider add litellm --base-url http://localhost:4000 --api-key-file ~/.config/ccr/litellm.key
	  ccr provider add local --base-url http://localhost:4000 --no-api-key`,
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
					protocol:     protocol,
					mode:         mode,
					baseURL:      baseURL,
					apiKeyEnv:    apiKeyEnv,
					apiKeyFile:   apiKeyFile,
					apiKeyStdin:  apiKeyStdin,
					noAPIKey:     noAPIKey,
				})
			}
			return runProviderAdd(ctx, cmd, opts, deps, args[0], providerAddConfig{
				providerType: providerType,
				protocol:     protocol,
				mode:         mode,
				baseURL:      baseURL,
				apiKeyEnv:    apiKeyEnv,
				apiKeyFile:   apiKeyFile,
				apiKeyStdin:  apiKeyStdin,
				noAPIKey:     noAPIKey,
			})
		},
	}
	cmd.Flags().StringVar(&providerType, "type", "", "Provider type/preset: "+providers.SupportedProviderTypes()+" (defaults to provider name when recognized)")
	cmd.Flags().StringVar(&protocol, "protocol", "", "Provider protocol: anthropic-compatible or openai-compatible")
	cmd.Flags().StringVar(&mode, "mode", "", "Provider compatibility mode: full, degraded, or chat-only (default comes from provider type)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Provider base URL")
	cmd.Flags().StringVar(&apiKeyEnv, "api-key-env", "", "Environment variable containing the API key; stores only env:<name>")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "Path to a 0600 file containing the API key; stores only file:<absolute-path>")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "Read API key from stdin and store it in the OS keychain")
	cmd.Flags().BoolVar(&noAPIKey, "no-api-key", false, "Declare that this provider does not need an API key")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Guide provider setup and optional model import with prompts")
	return cmd
}

func runProviderAdd(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, name string, cfg providerAddConfig) error {
	resolvedType, err := resolveProviderTypeWithProtocol(name, cfg.providerType, cfg.protocol)
	if err != nil {
		return err
	}
	if modeErr := validateProviderMode(cfg.mode); modeErr != nil {
		return modeErr
	}
	resolvedURL, err := resolveBaseURL(resolvedType, cfg.baseURL)
	if err != nil {
		return err
	}
	plan, err := resolveProviderSecretPlan(deps, name, resolvedType, cfg.apiKeyEnv, cfg.apiKeyFile, cfg.apiKeyValue, cfg.apiKeyStdin, cfg.noAPIKey)
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
	provider := providerWithCapabilities(name, resolvedType, resolvedURL, plan.ref, cfg.mode)
	if err := s.AddProvider(ctx, provider); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q added (%s, protocol=%s, mode=%s, token-count=%s, %s, secret=%s)\n", name, resolvedType, provider.Protocol, provider.Mode, providerTokenCountMode(provider), resolvedURL, secret.RedactRef(plan.ref))
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
			providerList, err := s.ListProviders(ctx)
			if err != nil {
				return err
			}
			if len(providerList) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No providers configured.")
				return nil
			}
			for i := range providerList {
				provider := providerList[i]
				caps := effectiveProviderCapabilities(provider)
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tprotocol=%s\tmode=%s\tcaps=%s\t%s\tsecret=%s\n", provider.Name, provider.Type, caps.Protocol, caps.Mode, providerCapabilitySummary(provider), provider.BaseURL, secret.RedactRef(provider.SecretRef))
			}
			return nil
		},
	}
}

func newProviderRemoveCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var yes bool
	var interactive bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a provider and its model aliases",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider remove litellm")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)

			exists, err := s.ProviderExists(ctx, name)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("provider %q does not exist", name)
			}
			confirmed, err := confirmRemoval(ctx, deps, yes, interactive, fmt.Sprintf("Remove provider %q and all associated model aliases?", name))
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Fprintf(cmd.OutOrStdout(), "Provider %q was not removed.\n", name)
				return nil
			}
			modelsRemoved, err := s.RemoveProvider(ctx, name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Provider %q removed. Removed %d model aliases.\n", name, modelsRemoved)
			fmt.Fprintln(cmd.OutOrStdout(), "Secret reference removed from SQLite; OS keychain entries are not deleted.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Remove without prompting for confirmation")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Prompt for removal confirmation")
	return cmd
}

func newProviderTestCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Validate provider config and connectivity",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider test litellm")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderTest(ctx, cmd, opts, deps, args[0])
		},
	}
}

func newModelCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Manage Claude Code model aliases",
		Long: `Manage model aliases used by ccr launch and gateway requests.

Examples:
  ccr model add qwen --provider openrouter --model qwen/qwen3-coder
  ccr model add gpt --provider litellm --model gpt-5
  ccr model update gpt --compat full
  ccr model test gpt
  ccr model remove gpt --yes
  ccr model list`,
	}
	cmd.AddCommand(
		newModelAddCommand(ctx, opts),
		newModelListCommand(ctx, opts),
		newModelShowCommand(ctx, opts),
		newModelRefreshCommand(ctx, opts, deps),
		newModelUpdateCommand(ctx, opts, deps),
		newModelTestCommand(ctx, opts, deps),
		newModelRemoveCommand(ctx, opts, deps),
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
			providerModel = strings.TrimSpace(providerModel)
			if providerModel == "" {
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
			provider, err := s.GetProvider(ctx, providerName)
			if err != nil {
				return err
			}
			if providers.IsProviderControlModel(provider.Type, providerModel) {
				return fmt.Errorf("provider model %q is a LiteLLM control model and cannot be routed", providerModel)
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
			for index := range models {
				model := &models[index]
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tprovider=%s\tmodel=%s\tcompat=%s\n", model.Alias, model.ProviderName, model.ProviderModel, model.Status)
			}
			return nil
		},
	}
}

func openMigratedStore(ctx context.Context, opts *options) (*store.Store, string, error) {
	dbPath, err := resolveDBPath(opts)
	if err != nil {
		return nil, "", err
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

func resolveDBPath(opts *options) (string, error) {
	if opts.dbPath != "" {
		return opts.dbPath, nil
	}
	return config.DefaultDBPath()
}

func closeStore(s *store.Store) {
	_ = s.Close()
}
