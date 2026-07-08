package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
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
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.String())
		},
	}
}

func newInitCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize the local SQLite database",
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
  ccr provider add litellm --base-url http://localhost:4000 --no-api-key
  ccr provider add anthropic --api-key-stdin
  ccr provider list`,
	}
	cmd.AddCommand(
		newProviderAddCommand(ctx, opts, deps),
		newProviderListCommand(ctx, opts),
		newProviderTestCommand(),
		newNotImplementedCommand("remove", "Remove a provider record"),
	)
	return cmd
}

func newProviderAddCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var providerType string
	var baseURL string
	var apiKeyEnv string
	var apiKeyStdin bool
	var noAPIKey bool

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a provider and secret reference",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider add openrouter --api-key-env OPENROUTER_API_KEY")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			resolvedType, err := resolveProviderType(name, providerType)
			if err != nil {
				return err
			}
			resolvedURL, err := resolveBaseURL(resolvedType, baseURL)
			if err != nil {
				return err
			}
			secretRef, err := resolveProviderSecret(ctx, deps, name, resolvedType, apiKeyEnv, apiKeyStdin, noAPIKey)
			if err != nil {
				return err
			}

			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)

			if err := s.AddProvider(ctx, store.Provider{Name: name, Type: resolvedType, BaseURL: resolvedURL, SecretRef: secretRef}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Provider %q added (%s, %s, secret=%s)\n", name, resolvedType, resolvedURL, secret.RedactRef(secretRef))
			fmt.Fprintf(cmd.OutOrStdout(), "Next: ccr model add <alias> --provider %s --model <provider-model>\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&providerType, "type", "", "Provider type: anthropic, openrouter, litellm, or local (defaults to provider name when recognized)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Provider base URL")
	cmd.Flags().StringVar(&apiKeyEnv, "api-key-env", "", "Environment variable containing the API key; stores only env:<name>")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "Read API key from stdin and store it in the OS keychain")
	cmd.Flags().BoolVar(&noAPIKey, "no-api-key", false, "Declare that this provider does not need an API key")
	return cmd
}

func newProviderListCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured providers",
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
		newNotImplementedCommand("test", "Validate a model alias against its provider"),
		newNotImplementedCommand("remove", "Remove a model alias"),
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("launching Claude Code through the gateway: %w", errNotImplemented)
		},
	}
}

func newStatusCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local router status",
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
	return newNotImplementedCommand("sessions", "List tracked Claude Code sessions")
}

func newAgentsCommand() *cobra.Command {
	return newNotImplementedCommand("agents", "List tracked Claude Code agents and workers")
}

func newNotImplementedCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%s: %w", use, errNotImplemented)
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

func resolveProviderSecret(ctx context.Context, deps Dependencies, name, providerType, apiKeyEnv string, apiKeyStdin, noAPIKey bool) (string, error) {
	selected := 0
	for _, enabled := range []bool{apiKeyEnv != "", apiKeyStdin, noAPIKey} {
		if enabled {
			selected++
		}
	}
	if selected > 1 {
		return "", fmt.Errorf("choose only one of --api-key-env, --api-key-stdin, or --no-api-key")
	}
	if apiKeyEnv != "" {
		if err := validateEnvName(apiKeyEnv); err != nil {
			return "", err
		}
		return secret.EnvRef(apiKeyEnv), nil
	}
	if apiKeyStdin {
		raw, err := io.ReadAll(deps.In)
		if err != nil {
			return "", fmt.Errorf("reading API key from stdin: %w", err)
		}
		value := strings.TrimSpace(string(raw))
		ref := secret.KeyringRef(name)
		if err := deps.Secrets.Store(ctx, ref, value); err != nil {
			return "", err
		}
		return ref, nil
	}
	if noAPIKey || providerType == "local" || providerType == "litellm" {
		return "", nil
	}
	return "", fmt.Errorf("API key required for provider type %q; use --api-key-env <ENV>, --api-key-stdin, or --no-api-key if this endpoint is intentionally unauthenticated", providerType)
}

func resolveProviderType(name, explicit string) (string, error) {
	providerType := explicit
	if providerType == "" {
		providerType = name
	}
	switch providerType {
	case "anthropic", "openrouter", "litellm", "local":
		return providerType, nil
	default:
		return "", fmt.Errorf("invalid provider type %q; expected anthropic, openrouter, litellm, or local", providerType)
	}
}

func resolveBaseURL(providerType, explicit string) (string, error) {
	baseURL := explicit
	if baseURL == "" {
		switch providerType {
		case "anthropic":
			baseURL = "https://api.anthropic.com"
		case "openrouter":
			baseURL = "https://openrouter.ai/api"
		case "litellm", "local":
			return "", fmt.Errorf("--base-url is required for provider type %q", providerType)
		default:
			return "", fmt.Errorf("unsupported provider type %q", providerType)
		}
	}
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid --base-url %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid --base-url %q: scheme must be http or https", baseURL)
	}
	return baseURL, nil
}

func validateName(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	matched, err := regexp.MatchString(`^[a-z][a-z0-9_-]{1,63}$`, value)
	if err != nil {
		return fmt.Errorf("validating %s: %w", label, err)
	}
	if !matched {
		return fmt.Errorf("invalid %s %q: use 2-64 chars, lowercase letters, digits, underscore, or hyphen, starting with a letter", label, value)
	}
	return nil
}

func validateEnvName(value string) error {
	matched, err := regexp.MatchString(`^[A-Z_][A-Z0-9_]*$`, value)
	if err != nil {
		return fmt.Errorf("validating environment variable name: %w", err)
	}
	if !matched {
		return fmt.Errorf("invalid environment variable name %q", value)
	}
	return nil
}

func validateCompatibilityStatus(value string) error {
	switch value {
	case "full", "degraded", "chat-only", "blocked":
		return nil
	default:
		return fmt.Errorf("invalid compatibility status %q; expected full, degraded, chat-only, or blocked", value)
	}
}
