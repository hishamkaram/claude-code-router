package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

var errProviderAddAborted = errors.New("provider add aborted")

const (
	authModeKeychain = "keychain"
	authModeEnv      = "env"
	authModeNone     = "none"
)

type providerSetupPrompt struct {
	name         string
	providerType string
	baseURL      string
	authMode     string
	apiKeyEnv    string
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
		if errors.Is(err, errProviderAddAborted) {
			return nil
		}
		return err
	}
	return saveInteractiveProviderAdd(ctx, cmd, deps, s, provider, plan, planned, summary)
}

func promptInteractiveProviderConfig(ctx context.Context, deps Dependencies, initialName string, cfg providerAddConfig) (store.Provider, secretPlan, error) {
	setup, err := promptProviderSetup(ctx, deps, providerSetupPrompt{
		name:         initialName,
		providerType: interactiveProviderTypeDefault(initialName, cfg.providerType),
		baseURL:      cfg.baseURL,
		authMode:     interactiveAuthModeDefault(cfg),
		apiKeyEnv:    cfg.apiKeyEnv,
		noAPIKey:     cfg.noAPIKey,
	})
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}

	resolvedType, err := resolveProviderType(setup.name, setup.providerType)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	resolvedURL, err := resolveBaseURL(resolvedType, strings.TrimSpace(setup.baseURL))
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	plan, err := resolveProviderSecretPlan(deps, setup.name, resolvedType, setup.apiKeyEnv, setup.apiKeyValue, false, setup.noAPIKey)
	if err != nil {
		return store.Provider{}, secretPlan{}, err
	}
	provider := store.Provider{Name: setup.name, Type: resolvedType, BaseURL: resolvedURL, SecretRef: plan.ref}
	return provider, plan, nil
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
	if plan.store {
		if err := deps.Secrets.Store(ctx, plan.ref, plan.value); err != nil {
			return fmt.Errorf("storing API key for provider %q: %w", provider.Name, err)
		}
	}
	if err := s.AddProvider(ctx, provider); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Provider %q added (%s, %s, secret=%s)\n", provider.Name, provider.Type, provider.BaseURL, secret.RedactRef(provider.SecretRef))
	if len(planned) == 0 && summary.skipped == 0 {
		if providers.SupportsOpenAIModelDiscovery(provider.Type) {
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
	return nil
}

func interactiveModelImportPlan(ctx context.Context, cmd *cobra.Command, deps Dependencies, s *store.Store, provider store.Provider, plan secretPlan) ([]plannedModelImport, modelImportSummary, error) {
	fmt.Fprintf(cmd.OutOrStdout(), "Discovering models for provider %q (%s)\n", provider.Name, provider.BaseURL)
	models, err := discoverProviderModelsWithPlan(ctx, deps, provider, plan)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Model discovery failed for provider %q: %v\n", provider.Name, err)
		saveOnly, promptErr := promptSaveProviderOnly(ctx, deps)
		if promptErr != nil {
			return nil, modelImportSummary{}, promptErr
		}
		if !saveOnly {
			fmt.Fprintf(cmd.OutOrStdout(), "Provider %q was not saved.\n", provider.Name)
			return nil, modelImportSummary{}, errProviderAddAborted
		}
		return nil, modelImportSummary{}, nil
	}
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
		selected, err = promptModelSelection(ctx, deps, models)
		if err != nil {
			return nil, modelImportSummary{}, err
		}
		if len(selected) == 0 {
			return nil, modelImportSummary{}, nil
		}
	}
	return planModelImports(ctx, deps, s, provider.Name, selected, choice)
}

func promptProviderSetup(ctx context.Context, deps Dependencies, initial providerSetupPrompt) (providerSetupPrompt, error) {
	setup := initial
	if setup.providerType == "" {
		setup.providerType = "litellm"
	}
	if setup.authMode == "" {
		setup.authMode = authModeKeychain
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Provider name").
			Value(&setup.name).
			Validate(func(value string) error {
				return validateName("provider name", value)
			}),
		huh.NewSelect[string]().
			Title("Provider type").
			Options(
				huh.NewOption("LiteLLM/OpenAI-compatible", "litellm"),
				huh.NewOption("OpenRouter", "openrouter"),
				huh.NewOption("Anthropic", "anthropic"),
				huh.NewOption("Local OpenAI-compatible", "local"),
			).
			Value(&setup.providerType),
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
				huh.NewOption("Store API key in OS keychain", authModeKeychain),
				huh.NewOption("Use environment variable", authModeEnv),
				huh.NewOption("No API key", authModeNone),
			).
			Value(&setup.authMode),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return providerSetupPrompt{}, err
	}

	switch setup.authMode {
	case authModeKeychain:
		value, err := promptAPIKey(ctx, deps)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		setup.apiKeyValue = value
		setup.apiKeyEnv = ""
		setup.noAPIKey = false
	case authModeEnv:
		envName, err := promptAPIKeyEnv(ctx, deps, setup.apiKeyEnv)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		setup.apiKeyEnv = envName
		setup.apiKeyValue = ""
		setup.noAPIKey = false
	case authModeNone:
		setup.apiKeyEnv = ""
		setup.apiKeyValue = ""
		setup.noAPIKey = true
	default:
		return providerSetupPrompt{}, fmt.Errorf("invalid authentication mode %q", setup.authMode)
	}
	return setup, nil
}

func promptAPIKey(ctx context.Context, deps Dependencies) (string, error) {
	apiKey := ""
	input := huh.NewInput().
		Title("API key").
		Value(&apiKey).
		Validate(func(value string) error {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("API key is required")
			}
			return nil
		})
	if isTerminal(readerOrDefault(deps.In, os.Stdin)) {
		input = input.EchoMode(huh.EchoModeNone)
	}
	if err := runHuhForm(ctx, deps, huh.NewForm(huh.NewGroup(input))); err != nil {
		return "", err
	}
	return strings.TrimSpace(apiKey), nil
}

func promptAPIKeyEnv(ctx context.Context, deps Dependencies, initial string) (string, error) {
	apiKeyEnv := initial
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("API key environment variable").
			Value(&apiKeyEnv).
			Validate(validateEnvName),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return apiKeyEnv, nil
}

func promptImportChoice(ctx context.Context, deps Dependencies, discovered int) (modelImportChoice, error) {
	choice := modelImportSelect
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[modelImportChoice]().
			Title(fmt.Sprintf("Import %d discovered models?", discovered)).
			Options(
				huh.NewOption("Select models", modelImportSelect),
				huh.NewOption("Import all", modelImportAll),
				huh.NewOption("Skip model import", modelImportSkip),
			).
			Value(&choice),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return choice, nil
}

func promptModelSelection(ctx context.Context, deps Dependencies, models []string) ([]string, error) {
	selected := make([]string, 0, len(models))
	options := make([]huh.Option[string], 0, len(models))
	for _, model := range models {
		options = append(options, huh.NewOption(model, model))
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Select models to import").
			Options(options...).
			Filterable(true).
			Height(12).
			Value(&selected),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return nil, err
	}
	return selected, nil
}

func promptAliasConflict(ctx context.Context, deps Dependencies, alias, modelID string, existing map[string]struct{}) (renamedAlias string, skip bool, err error) {
	choice := "skip"
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(fmt.Sprintf("Alias %q already exists for model %q", alias, modelID)).
			Options(
				huh.NewOption("Skip this model", "skip"),
				huh.NewOption("Rename alias", "rename"),
			).
			Value(&choice),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", false, err
	}
	if choice == "skip" {
		return "", true, nil
	}

	renamed := alias
	renameForm := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("New alias").
			Value(&renamed).
			Validate(func(value string) error {
				if err := validateName("model alias", value); err != nil {
					return err
				}
				if _, ok := existing[value]; ok {
					return fmt.Errorf("model alias %q already exists", value)
				}
				return nil
			}),
	))
	if err := runHuhForm(ctx, deps, renameForm); err != nil {
		return "", false, err
	}
	return renamed, false, nil
}

func promptSaveProviderOnly(ctx context.Context, deps Dependencies) (bool, error) {
	saveOnly := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Save provider without importing models?").
			Affirmative("Save provider").
			Negative("Do not save").
			Value(&saveOnly),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return false, err
	}
	return saveOnly, nil
}

func runHuhForm(ctx context.Context, deps Dependencies, form *huh.Form) error {
	in := readerOrDefault(deps.In, os.Stdin)
	out := writerOrDefault(deps.Err, os.Stderr)
	form = form.WithInput(in).WithOutput(out)
	if shouldUseAccessiblePrompts(Dependencies{In: in, Err: out}) {
		form = form.WithAccessible(true)
	}
	if err := form.RunWithContext(ctx); err != nil {
		return fmt.Errorf("running interactive prompt: %w", err)
	}
	return nil
}

func shouldUseAccessiblePrompts(deps Dependencies) bool {
	return !isTerminal(deps.In) || !isTerminal(deps.Err)
}

func isTerminal(value any) bool {
	fd, ok := value.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return term.IsTerminal(int(fd.Fd()))
}

func readerOrDefault(value, fallback io.Reader) io.Reader {
	if value == nil {
		return fallback
	}
	return value
}

func writerOrDefault(value, fallback io.Writer) io.Writer {
	if value == nil {
		return fallback
	}
	return value
}

func interactiveProviderTypeDefault(providerName, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if providerName == "" {
		return "litellm"
	}
	resolved, err := resolveProviderType(providerName, "")
	if err != nil {
		return "litellm"
	}
	return resolved
}

func interactiveAuthModeDefault(cfg providerAddConfig) string {
	switch {
	case cfg.apiKeyEnv != "":
		return authModeEnv
	case cfg.noAPIKey:
		return authModeNone
	default:
		return authModeKeychain
	}
}
