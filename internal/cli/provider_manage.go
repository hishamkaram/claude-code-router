package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func newProviderUpdateCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
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
		Use:   "update <name>",
		Short: "Update provider config and secret reference",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider update litellm --base-url http://localhost:4000")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := providerAddConfig{
				providerType: providerType,
				protocol:     protocol,
				mode:         mode,
				baseURL:      baseURL,
				apiKeyEnv:    apiKeyEnv,
				apiKeyFile:   apiKeyFile,
				apiKeyStdin:  apiKeyStdin,
				noAPIKey:     noAPIKey,
			}
			changes := providerUpdateChangesFromCommand(cmd)
			if interactive {
				return runProviderUpdateInteractive(ctx, cmd, opts, deps, args[0], cfg, changes)
			}
			return runProviderUpdate(ctx, cmd, opts, deps, args[0], cfg)
		},
	}
	cmd.Flags().StringVar(&providerType, "type", "", "Provider type/preset: "+providers.SupportedProviderTypes())
	cmd.Flags().StringVar(&protocol, "protocol", "", "Provider protocol: anthropic-compatible or openai-compatible")
	cmd.Flags().StringVar(&mode, "mode", "", "Provider compatibility mode: full, degraded, or chat-only")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Provider base URL")
	cmd.Flags().StringVar(&apiKeyEnv, "api-key-env", "", "Environment variable containing the API key; stores only env:<name>")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "Path to a 0600 file containing the API key; stores only file:<absolute-path>")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "Read API key from stdin and store it in the OS keychain")
	cmd.Flags().BoolVar(&noAPIKey, "no-api-key", false, "Clear the provider API key reference")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Guide provider updates with prompts")
	return cmd
}

func runProviderUpdate(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, name string, cfg providerAddConfig) error {
	changes := providerUpdateChangesFromCommand(cmd)
	if !changes.any() {
		return fmt.Errorf("provider update requires at least one change flag or --interactive")
	}
	if err := validateProviderAuthSourceFlags(cfg); err != nil {
		return err
	}
	if err := validateProviderUpdateStaticFlags(name, cfg, changes); err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	existing, err := s.GetProvider(ctx, name)
	if err != nil {
		return err
	}
	updated, plan, err := buildProviderUpdateFromFlags(deps, name, cfg, existing, changes)
	if err != nil {
		return err
	}
	if plan.store {
		if err := deps.Secrets.Store(ctx, plan.ref, plan.value); err != nil {
			return fmt.Errorf("storing API key for provider %q: %w", name, err)
		}
	}
	if err := s.UpdateProvider(ctx, updated); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q updated (%s, protocol=%s, mode=%s, %s, secret=%s)\n", name, updated.Type, updated.Protocol, updated.Mode, updated.BaseURL, secret.RedactRef(updated.SecretRef))
	return nil
}

type providerUpdateChanges struct {
	providerType bool
	protocol     bool
	mode         bool
	baseURL      bool
	auth         bool
}

func (c providerUpdateChanges) any() bool {
	return c.providerType || c.protocol || c.mode || c.baseURL || c.auth
}

func providerUpdateChangesFromCommand(cmd *cobra.Command) providerUpdateChanges {
	flags := cmd.Flags()
	return providerUpdateChanges{
		providerType: flags.Changed("type"),
		protocol:     flags.Changed("protocol"),
		mode:         flags.Changed("mode"),
		baseURL:      flags.Changed("base-url"),
		auth:         flags.Changed("api-key-env") || flags.Changed("api-key-file") || flags.Changed("api-key-stdin") || flags.Changed("no-api-key"),
	}
}

func buildProviderUpdateFromFlags(deps Dependencies, name string, cfg providerAddConfig, existing store.Provider, changes providerUpdateChanges) (store.Provider, secretPlan, error) {
	updated := existing
	if changes.providerType || changes.protocol {
		var err error
		updated, err = applyProviderTypeProtocolUpdate(name, cfg, updated, changes)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
	}
	if changes.baseURL || changes.providerType || changes.protocol {
		var err error
		updated, err = applyProviderBaseURLUpdate(cfg, updated, changes)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
	}
	if changes.mode {
		if modeErr := validateProviderMode(cfg.mode); modeErr != nil {
			return store.Provider{}, secretPlan{}, modeErr
		}
		updated = applyProviderModeUpdate(updated, cfg.mode)
	}
	if !changes.auth {
		if err := validateProviderUpdateAuthDecision(existing, updated, changes); err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		return updated, secretPlan{}, nil
	}
	plan, err := resolveProviderSecretPlan(deps, name, updated.Type, cfg.apiKeyEnv, cfg.apiKeyFile, "", cfg.apiKeyStdin, cfg.noAPIKey)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated.SecretRef = plan.ref
	return updated, plan, nil
}

func applyProviderTypeProtocolUpdate(name string, cfg providerAddConfig, existing store.Provider, changes providerUpdateChanges) (store.Provider, error) {
	providerType := strings.TrimSpace(cfg.providerType)
	if !changes.providerType {
		providerType = existing.Type
	}
	resolvedType, err := resolveProviderTypeWithProtocol(name, providerType, strings.TrimSpace(cfg.protocol))
	if err != nil {
		return store.Provider{}, err
	}
	capsMode := providerModeForTypeChange(existing.Type, existing.Mode, resolvedType, cfg.mode, changes.mode)
	return providerWithCapabilities(name, resolvedType, existing.BaseURL, existing.SecretRef, capsMode), nil
}

func applyProviderBaseURLUpdate(cfg providerAddConfig, provider store.Provider, changes providerUpdateChanges) (store.Provider, error) {
	baseURL := provider.BaseURL
	if changes.baseURL {
		baseURL = cfg.baseURL
	}
	resolvedURL, err := resolveBaseURL(provider.Type, strings.TrimSpace(baseURL))
	if err != nil {
		return store.Provider{}, err
	}
	provider.BaseURL = resolvedURL
	return provider, nil
}

func applyProviderModeUpdate(provider store.Provider, mode string) store.Provider {
	provider.Mode = mode
	provider.SupportsTools = mode != providers.ModeChatOnly
	return provider
}

func validateProviderUpdateAuthDecision(existing, updated store.Provider, changes providerUpdateChanges) error {
	if !changes.providerType && !changes.protocol {
		return nil
	}
	oldCaps := effectiveProviderCapabilities(existing)
	newCaps := effectiveProviderCapabilities(updated)
	if existing.Type == updated.Type && oldCaps.Protocol == newCaps.Protocol {
		return nil
	}
	if !providerTypeRequiresAPIKey(updated.Type) || existing.SecretRef != "" {
		return nil
	}
	return fmt.Errorf("provider type %q requires an API key; pass --api-key-env, --api-key-file, --api-key-stdin, or --no-api-key to confirm unauthenticated use", updated.Type)
}

func validateProviderUpdateStaticFlags(name string, cfg providerAddConfig, changes providerUpdateChanges) error {
	if err := validateProviderUpdateTypeProtocol(name, cfg, changes); err != nil {
		return err
	}
	if err := validateProviderUpdateMode(cfg, changes); err != nil {
		return err
	}
	if err := validateProviderUpdateBaseURL(cfg, changes); err != nil {
		return err
	}
	if err := validateProviderUpdateAuthFlags(cfg, changes); err != nil {
		return err
	}
	return nil
}

func validateProviderUpdateTypeProtocol(name string, cfg providerAddConfig, changes providerUpdateChanges) error {
	if !changes.providerType && !changes.protocol {
		return nil
	}
	_, err := resolveProviderTypeWithProtocol(name, strings.TrimSpace(cfg.providerType), strings.TrimSpace(cfg.protocol))
	return err
}

func validateProviderUpdateMode(cfg providerAddConfig, changes providerUpdateChanges) error {
	if !changes.mode {
		return nil
	}
	return validateProviderMode(cfg.mode)
}

func validateProviderUpdateBaseURL(cfg providerAddConfig, changes providerUpdateChanges) error {
	if !changes.baseURL || strings.TrimSpace(cfg.baseURL) == "" {
		return nil
	}
	return validateBaseURLSyntax(cfg.baseURL)
}

func validateProviderUpdateAuthFlags(cfg providerAddConfig, changes providerUpdateChanges) error {
	if !changes.auth {
		return nil
	}
	if cfg.apiKeyEnv != "" {
		if err := validateEnvName(cfg.apiKeyEnv); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.apiKeyFile) == "" {
		return nil
	}
	if _, err := secret.FileRefFromPath(cfg.apiKeyFile); err != nil {
		return fmt.Errorf("--api-key-file: %w", err)
	}
	return nil
}

func validateBaseURLSyntax(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid --base-url %q", value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid --base-url %q: scheme must be http or https", value)
	}
	return nil
}

func runProviderUpdateInteractive(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, name string, cfg providerAddConfig, changes providerUpdateChanges) error {
	if err := validateProviderAuthSourceFlags(cfg); err != nil {
		return err
	}
	if err := validateProviderUpdateStaticFlags(name, cfg, changes); err != nil {
		return err
	}
	if cfg.apiKeyStdin {
		return fmt.Errorf("--interactive uses a hidden key prompt; use --api-key-stdin without --interactive")
	}
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	existing, err := s.GetProvider(ctx, name)
	if err != nil {
		return err
	}
	updated, plan, err := promptProviderUpdateConfig(ctx, deps, existing, cfg)
	if err != nil {
		return err
	}
	if plan.store {
		if err := deps.Secrets.Store(ctx, plan.ref, plan.value); err != nil {
			return fmt.Errorf("storing API key for provider %q: %w", name, err)
		}
	}
	if err := s.UpdateProvider(ctx, updated); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q updated (%s, protocol=%s, mode=%s, %s, secret=%s)\n", name, updated.Type, updated.Protocol, updated.Mode, updated.BaseURL, secret.RedactRef(updated.SecretRef))
	return nil
}

func runProviderTest(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, name string) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	provider, err := s.GetProvider(ctx, name)
	if err != nil {
		return err
	}
	apiKey, err := validateProviderConfigAndSecret(ctx, deps, provider)
	if err != nil {
		return err
	}
	caps := effectiveProviderCapabilities(provider)
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q: type=%s protocol=%s mode=%s caps=%s base=%s secret=%s\n", provider.Name, provider.Type, caps.Protocol, caps.Mode, providerCapabilitySummary(provider), provider.BaseURL, secret.RedactRef(provider.SecretRef))
	if caps.Protocol != providers.ProtocolOpenAICompatible || !caps.SupportsModelDiscovery {
		fmt.Fprintln(cmd.OutOrStdout(), "Config and secret validation passed. Anthropic live routing is outside this pass.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Checking %s/v1/models-compatible endpoint.\n", strings.TrimRight(provider.BaseURL, "/"))
	models, err := discoverProviderModelsWithPlan(ctx, deps, provider, secretPlan{ref: provider.SecretRef, value: apiKey})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q test passed; discovered %d models.\n", provider.Name, len(models))
	return nil
}

func validateProviderConfigAndSecret(ctx context.Context, deps Dependencies, provider store.Provider) (string, error) {
	if _, err := resolveProviderType(provider.Name, provider.Type); err != nil {
		return "", err
	}
	if _, err := resolveBaseURL(provider.Type, provider.BaseURL); err != nil {
		return "", err
	}
	apiKey, err := resolveDiscoveryAPIKey(ctx, deps, secretPlan{ref: provider.SecretRef})
	if err != nil {
		return "", fmt.Errorf("resolving API key for provider %q (secret=%s): %w", provider.Name, secret.RedactRef(provider.SecretRef), err)
	}
	if strings.TrimSpace(provider.SecretRef) != "" && strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("resolving API key for provider %q (secret=%s): resolved secret is empty", provider.Name, secret.RedactRef(provider.SecretRef))
	}
	return apiKey, nil
}
