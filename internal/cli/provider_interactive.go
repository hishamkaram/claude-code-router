package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

var (
	errProviderAddAborted = errors.New("provider add aborted")
	errProviderSetupEdit  = errors.New("provider setup edit requested")
)

const (
	authModeKeep     = "keep"
	authModeKeychain = "keychain"
	authModeEnv      = "env"
	authModeFile     = "file"
	authModeNone     = "none"
)

type providerSetupPrompt struct {
	name         string
	providerType string
	protocol     string
	mode         string
	baseURL      string
	authMode     string
	apiKeyEnv    string
	apiKeyFile   string
	apiKeyValue  string
	noAPIKey     bool
}

func runProviderAddInteractive(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, initialName string, cfg providerAddConfig) error {
	if err := validateProviderAuthSourceFlags(cfg); err != nil {
		return err
	}
	if cfg.apiKeyStdin {
		return fmt.Errorf("--interactive uses a hidden key prompt; use --api-key-stdin without --interactive")
	}
	if err := validateProviderAddInteractiveStaticFlags(initialName, cfg); err != nil {
		return err
	}
	for {
		err := runProviderAddInteractiveAttempt(ctx, cmd, opts, deps, initialName, cfg)
		switch {
		case errors.Is(err, errProviderSetupEdit):
			initialName = ""
			continue
		case errors.Is(err, errProviderAddAborted), errors.Is(err, errModelImportAborted):
			return nil
		default:
			return err
		}
	}
}

func runProviderAddInteractiveAttempt(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, initialName string, cfg providerAddConfig) error {
	provider, plan, err := promptInteractiveProviderConfig(ctx, deps, initialName, cfg)
	if err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	if addErr := ensureProviderCanBeAdded(ctx, s, provider.Name); addErr != nil {
		return addErr
	}

	planned, summary, err := interactiveModelImportPlan(ctx, cmd, deps, s, provider, plan)
	if err != nil {
		return err
	}
	return saveInteractiveProviderAdd(ctx, cmd, deps, s, provider, plan, planned, summary)
}

func validateProviderAddInteractiveStaticFlags(initialName string, cfg providerAddConfig) error {
	if cfg.providerType != "" {
		if _, err := resolveProviderTypeWithProtocol(initialName, strings.TrimSpace(cfg.providerType), strings.TrimSpace(cfg.protocol)); err != nil {
			return err
		}
	}
	if cfg.protocol != "" {
		if _, err := resolveProviderTypeWithProtocol(initialName, strings.TrimSpace(cfg.providerType), strings.TrimSpace(cfg.protocol)); err != nil {
			return err
		}
	}
	if err := validateProviderMode(cfg.mode); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.baseURL) != "" {
		if err := validateBaseURLSyntax(cfg.baseURL); err != nil {
			return err
		}
	}
	if cfg.apiKeyEnv != "" {
		if err := validateEnvName(cfg.apiKeyEnv); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.apiKeyFile) != "" {
		if _, err := secret.FileRefFromPath(cfg.apiKeyFile); err != nil {
			return fmt.Errorf("--api-key-file: %w", err)
		}
	}
	return nil
}

func promptInteractiveProviderConfig(ctx context.Context, deps Dependencies, initialName string, cfg providerAddConfig) (store.Provider, secretPlan, error) {
	setup, err := promptProviderSetup(ctx, deps, providerSetupPrompt{
		name:         initialName,
		providerType: interactiveProviderTypeDefault(initialName, cfg.providerType, cfg.protocol),
		protocol:     cfg.protocol,
		mode:         cfg.mode,
		baseURL:      cfg.baseURL,
		authMode:     interactiveAuthModeDefault(cfg),
		apiKeyEnv:    cfg.apiKeyEnv,
		apiKeyFile:   cfg.apiKeyFile,
		noAPIKey:     cfg.noAPIKey,
	})
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}

	resolvedType, err := resolveProviderTypeWithProtocol(setup.name, setup.providerType, setup.protocol)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	resolvedURL, err := resolveBaseURL(resolvedType, strings.TrimSpace(setup.baseURL))
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	plan, err := resolveProviderSecretPlan(deps, setup.name, resolvedType, setup.apiKeyEnv, setup.apiKeyFile, setup.apiKeyValue, false, setup.noAPIKey)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	provider := providerWithCapabilities(setup.name, resolvedType, resolvedURL, plan.ref, setup.mode)
	provider.SupportsResponses = cfg.supportsResponses
	if err := validateProviderResponsesCapability(provider); err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	return provider, plan, nil
}

func promptProviderUpdateConfig(ctx context.Context, deps Dependencies, existing store.Provider, cfg providerAddConfig) (store.Provider, secretPlan, error) {
	setup := initialProviderUpdatePrompt(existing, cfg)
	if err := runProviderUpdatePrompt(ctx, deps, &setup); err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated, err := resolveProviderUpdateBase(existing, setup)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	return applyProviderUpdateAuth(ctx, deps, updated, setup)
}

func initialProviderUpdatePrompt(existing store.Provider, cfg providerAddConfig) providerSetupPrompt {
	providerType := interactiveProviderUpdateTypeDefault(existing, cfg)
	mode := cfg.mode
	if mode == "" {
		mode = existing.Mode
	}
	if mode == "" {
		mode = defaultProviderMode(providerType)
	}
	setup := providerSetupPrompt{
		name:         existing.Name,
		providerType: providerType,
		protocol:     cfg.protocol,
		mode:         mode,
		baseURL:      firstNonEmptyString(cfg.baseURL, existing.BaseURL),
		authMode:     authModeKeep,
		apiKeyEnv:    cfg.apiKeyEnv,
		apiKeyFile:   cfg.apiKeyFile,
		noAPIKey:     cfg.noAPIKey,
	}
	if cfg.apiKeyEnv != "" || cfg.apiKeyFile != "" || cfg.noAPIKey {
		setup.authMode = interactiveAuthModeDefault(cfg)
	}
	return setup
}

func runProviderUpdatePrompt(ctx context.Context, deps Dependencies, setup *providerSetupPrompt) error {
	if !isTerminal(readerOrDefault(deps.In, os.Stdin)) {
		return runProviderUpdatePromptNonTerminal(ctx, deps, setup)
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Provider profile").
			Description(providerProfilePromptDescription()).
			Options(providerProfileOptions()...).
			Filtering(true).
			Height(8).
			Value(&setup.providerType),
		huh.NewSelect[string]().
			Title("Provider mode").
			Options(
				huh.NewOption("Full", providers.ModeFull),
				huh.NewOption("Degraded", providers.ModeDegraded),
				huh.NewOption("Chat only", providers.ModeChatOnly),
			).
			Value(&setup.mode),
		huh.NewInput().
			Title("Base URL").
			Description("Leave empty for default provider URLs when available.").
			Value(&setup.baseURL).
			Validate(func(value string) error {
				_, err := resolveBaseURL(setup.providerType, strings.TrimSpace(value))
				return err
			}),
		huh.NewSelect[string]().
			Title("Authentication").
			Options(
				huh.NewOption("Keep current secret reference", authModeKeep),
				huh.NewOption("Store new API key in OS keychain", authModeKeychain),
				huh.NewOption("Use environment variable", authModeEnv),
				huh.NewOption("Use API key file", authModeFile),
				huh.NewOption("No API key", authModeNone),
			).
			Value(&setup.authMode),
	))
	return runHuhForm(ctx, deps, form)
}

func resolveProviderUpdateBase(existing store.Provider, setup providerSetupPrompt) (store.Provider, error) {
	updated := existing
	resolvedType, err := resolveProviderTypeWithProtocol(existing.Name, setup.providerType, setup.protocol)
	if err != nil {
		return store.Provider{}, err
	}
	resolvedURL, err := resolveBaseURL(resolvedType, strings.TrimSpace(setup.baseURL))
	if err != nil {
		return store.Provider{}, err
	}
	updated.Type = resolvedType
	updated.BaseURL = resolvedURL
	mode := providerModeForTypeChange(existing.Type, existing.Mode, resolvedType, setup.mode, setup.mode != "" && setup.mode != existing.Mode)
	caps := providerWithCapabilities(updated.Name, updated.Type, updated.BaseURL, updated.SecretRef, mode)
	updated.Protocol = caps.Protocol
	updated.SupportsTools = caps.SupportsTools
	updated.SupportsStreaming = caps.SupportsStreaming
	updated.SupportsThinking = caps.SupportsThinking
	updated.SupportsModelDiscovery = caps.SupportsModelDiscovery
	updated.SupportsCountTokens = caps.SupportsCountTokens
	if existing.Type == resolvedType && existing.Protocol == caps.Protocol {
		updated.SupportsResponses = existing.SupportsResponses
	} else {
		updated.SupportsResponses = caps.SupportsResponses
	}
	updated.Mode = caps.Mode
	return updated, nil
}

func applyProviderUpdateAuth(ctx context.Context, deps Dependencies, updated store.Provider, setup providerSetupPrompt) (store.Provider, secretPlan, error) {
	switch setup.authMode {
	case authModeKeep:
		return applyProviderUpdateKeepAuth(updated)
	case authModeKeychain:
		return applyProviderUpdateKeychainAuth(ctx, deps, updated)
	case authModeEnv:
		return applyProviderUpdateEnvAuth(ctx, deps, updated, setup.apiKeyEnv)
	case authModeFile:
		return applyProviderUpdateFileAuth(ctx, deps, updated, setup.apiKeyFile)
	case authModeNone:
		return applyProviderUpdateNoAuth(deps, updated)
	default:
		return store.Provider{}, secretPlan{}, fmt.Errorf("invalid authentication mode %q", setup.authMode)
	}
}

func applyProviderUpdateKeepAuth(updated store.Provider) (store.Provider, secretPlan, error) {
	if providerTypeRequiresAPIKey(updated.Type) && updated.SecretRef == "" {
		return store.Provider{}, secretPlan{}, fmt.Errorf("provider type %q requires an API key; choose an API key source or No API key to confirm unauthenticated use", updated.Type)
	}
	return updated, secretPlan{}, nil
}

func applyProviderUpdateKeychainAuth(ctx context.Context, deps Dependencies, updated store.Provider) (store.Provider, secretPlan, error) {
	value, err := promptAPIKey(ctx, deps)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, "", "", value, false, false)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated.SecretRef = plan.ref
	return updated, plan, nil
}

func applyProviderUpdateEnvAuth(ctx context.Context, deps Dependencies, updated store.Provider, initial string) (store.Provider, secretPlan, error) {
	envName, err := promptProviderUpdateEnvName(ctx, deps, initial)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, envName, "", "", false, false)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated.SecretRef = plan.ref
	return updated, plan, nil
}

func promptProviderUpdateEnvName(ctx context.Context, deps Dependencies, initial string) (string, error) {
	in := readerOrDefault(deps.In, os.Stdin)
	if isTerminal(in) {
		return promptAPIKeyEnv(ctx, deps, initial)
	}
	envName, err := readNonTerminalPromptValue(ctx, in, initial, true)
	if err != nil {
		return "", err
	}
	if err := validateEnvName(envName); err != nil {
		return "", err
	}
	return envName, nil
}

func applyProviderUpdateFileAuth(ctx context.Context, deps Dependencies, updated store.Provider, initial string) (store.Provider, secretPlan, error) {
	filePath, err := promptProviderUpdateAPIKeyFile(ctx, deps, initial)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, "", filePath, "", false, false)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated.SecretRef = plan.ref
	return updated, plan, nil
}

func promptProviderUpdateAPIKeyFile(ctx context.Context, deps Dependencies, initial string) (string, error) {
	in := readerOrDefault(deps.In, os.Stdin)
	if isTerminal(in) {
		return promptAPIKeyFile(ctx, deps, initial)
	}
	return readNonTerminalPromptValue(ctx, in, initial, true)
}

func applyProviderUpdateNoAuth(deps Dependencies, updated store.Provider) (store.Provider, secretPlan, error) {
	plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, "", "", "", false, true)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	updated.SecretRef = plan.ref
	return updated, plan, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func ensureProviderCanBeAdded(ctx context.Context, s *store.Store, providerName string) error {
	exists, err := s.ProviderExists(ctx, providerName)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("provider %q already exists", providerName)
	}
	return nil
}

func saveInteractiveProviderAdd(ctx context.Context, cmd *cobra.Command, deps Dependencies, s *store.Store, provider store.Provider, plan secretPlan, planned []plannedModelImport, summary modelImportSummary) error {
	if err := validatePlannedProviderModels(provider, planned); err != nil {
		return err
	}
	if plan.store {
		if err := deps.Secrets.Store(ctx, plan.ref, plan.value); err != nil {
			return fmt.Errorf("storing API key for provider %q: %w", provider.Name, err)
		}
	}
	if err := s.AddProvider(ctx, provider); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q added (%s, protocol=%s, mode=%s, token-count=%s, %s, secret=%s)\n", provider.Name, provider.Type, provider.Protocol, provider.Mode, providerTokenCountMode(provider), provider.BaseURL, secret.RedactRef(provider.SecretRef))
	if len(planned) == 0 && summary.skipped == 0 {
		caps := effectiveProviderCapabilities(provider)
		if caps.Protocol == providers.ProtocolOpenAICompatible && caps.SupportsModelDiscovery {
			fmt.Fprintf(cmd.OutOrStdout(), "Next: ccr provider import-models %s\n", provider.Name)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Next: ccr model add <alias> --provider %s --model <provider-model>\n", provider.Name)
		return nil
	}
	if err := addPlannedModelImports(ctx, s, provider.Name, planned, &summary); err != nil {
		return err
	}
	printModelImportSummary(cmd.OutOrStdout(), provider.Name, summary)
	printModelImportDetails(cmd.OutOrStdout(), planned)
	printModelLaunchGuidance(cmd.OutOrStdout(), planned, providerDisablesClaudeTools(provider))
	return nil
}

func interactiveModelImportPlan(ctx context.Context, cmd *cobra.Command, deps Dependencies, s *store.Store, provider store.Provider, plan secretPlan) ([]plannedModelImport, modelImportSummary, error) {
	if !supportsInteractiveModelDiscovery(provider) {
		return manualModelImportPlan(ctx, cmd, deps, s, provider, plan)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Discovering models for provider %q (%s)\n", provider.Name, provider.BaseURL)
	discovery, err := discoverProviderModelsWithPlan(ctx, deps, provider, plan)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Model discovery failed for provider %q: %v\n", provider.Name, err)
		action, promptErr := promptDiscoveryFailureAction(ctx, deps)
		if promptErr != nil {
			return nil, modelImportSummary{}, promptErr
		}
		switch action {
		case discoveryFailureEdit:
			return nil, modelImportSummary{}, errProviderSetupEdit
		case discoveryFailureSaveProvider:
			return nil, modelImportSummary{}, nil
		case discoveryFailureCancel:
			fmt.Fprintf(cmd.OutOrStdout(), "Provider %q was not saved.\n", provider.Name)
			return nil, modelImportSummary{}, errProviderAddAborted
		default:
			return nil, modelImportSummary{}, fmt.Errorf("invalid discovery failure action %q", action)
		}
	}
	writeDiscoveryDiagnostics(cmd.OutOrStdout(), discovery)
	models := discovery.RoutableModels()
	if len(models) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No models discovered for provider %q.\n", provider.Name)
		return nil, modelImportSummary{}, nil
	}

	choice, err := promptImportChoice(ctx, deps, len(models))
	if err != nil {
		return nil, modelImportSummary{}, err
	}
	if choice == modelImportSkip {
		return nil, modelImportSummary{}, nil
	}

	selected := models
	if choice == modelImportSelect {
		selectedIDs, selectionErr := promptRequiredModelSelection(ctx, deps, discoveredModelIDs(models))
		err = selectionErr
		if err != nil {
			return nil, modelImportSummary{}, err
		}
		selected = selectDiscoveredModels(models, selectedIDs)
	}
	planned, summary, err := planModelImports(ctx, deps, s, provider.Name, selected, choice)
	if err != nil {
		return nil, modelImportSummary{}, err
	}
	return reviewPlannedModelImports(ctx, cmd, deps, s, planned, summary)
}
