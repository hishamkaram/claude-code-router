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
	authModeKeep     = "keep"
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
	if err := validateProviderAddInteractiveStaticFlags(initialName, cfg); err != nil {
		return err
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

func validateProviderAddInteractiveStaticFlags(initialName string, cfg providerAddConfig) error {
	if cfg.providerType != "" {
		if _, err := resolveProviderType(initialName, strings.TrimSpace(cfg.providerType)); err != nil {
			return err
		}
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
	return nil
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
	setup := providerSetupPrompt{
		name:         existing.Name,
		providerType: firstNonEmptyString(cfg.providerType, existing.Type),
		baseURL:      firstNonEmptyString(cfg.baseURL, existing.BaseURL),
		authMode:     authModeKeep,
		apiKeyEnv:    cfg.apiKeyEnv,
		noAPIKey:     cfg.noAPIKey,
	}
	if cfg.apiKeyEnv != "" || cfg.noAPIKey {
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
				huh.NewOption("Keep current secret reference", authModeKeep),
				huh.NewOption("Store new API key in OS keychain", authModeKeychain),
				huh.NewOption("Use environment variable", authModeEnv),
				huh.NewOption("No API key", authModeNone),
			).
			Value(&setup.authMode),
	))
	return runHuhForm(ctx, deps, form)
}

func resolveProviderUpdateBase(existing store.Provider, setup providerSetupPrompt) (store.Provider, error) {
	updated := existing
	resolvedType, err := resolveProviderType(existing.Name, setup.providerType)
	if err != nil {
		return store.Provider{}, err
	}
	resolvedURL, err := resolveBaseURL(resolvedType, strings.TrimSpace(setup.baseURL))
	if err != nil {
		return store.Provider{}, err
	}
	updated.Type = resolvedType
	updated.BaseURL = resolvedURL
	return updated, nil
}

func applyProviderUpdateAuth(ctx context.Context, deps Dependencies, updated store.Provider, setup providerSetupPrompt) (store.Provider, secretPlan, error) {
	switch setup.authMode {
	case authModeKeep:
		if providerTypeRequiresAPIKey(updated.Type) && updated.SecretRef == "" {
			return store.Provider{}, secretPlan{}, fmt.Errorf("provider type %q requires an API key; choose an API key source or No API key to confirm unauthenticated use", updated.Type)
		}
		return updated, secretPlan{}, nil
	case authModeKeychain:
		value, err := promptAPIKey(ctx, deps)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, "", value, false, false)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		updated.SecretRef = plan.ref
		return updated, plan, nil
	case authModeEnv:
		var envName string
		var err error
		in := readerOrDefault(deps.In, os.Stdin)
		if !isTerminal(in) {
			envName, err = readNonTerminalPromptValue(ctx, in, setup.apiKeyEnv, true)
			if err == nil {
				err = validateEnvName(envName)
			}
		} else {
			envName, err = promptAPIKeyEnv(ctx, deps, setup.apiKeyEnv)
		}
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, envName, "", false, false)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		updated.SecretRef = plan.ref
		return updated, plan, nil
	case authModeNone:
		plan, err := resolveProviderSecretPlan(deps, updated.Name, updated.Type, "", "", false, true)
		if err != nil {
			return store.Provider{}, secretPlan{}, err
		}
		updated.SecretRef = plan.ref
		return updated, plan, nil
	default:
		return store.Provider{}, secretPlan{}, fmt.Errorf("invalid authentication mode %q", setup.authMode)
	}
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
	if !isTerminal(readerOrDefault(deps.In, os.Stdin)) {
		return promptProviderSetupNonTerminal(ctx, deps, setup)
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

func promptProviderSetupNonTerminal(ctx context.Context, deps Dependencies, setup providerSetupPrompt) (providerSetupPrompt, error) {
	in := readerOrDefault(deps.In, os.Stdin)
	name, err := readNonTerminalPromptValue(ctx, in, setup.name, true)
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("provider name: %w", err)
	}
	if validateErr := validateName("provider name", name); validateErr != nil {
		return providerSetupPrompt{}, validateErr
	}
	providerType, err := readNonTerminalChoice(ctx, in, setup.providerType, providerTypeChoices())
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("provider type: %w", err)
	}
	baseURL, err := readNonTerminalPromptValue(ctx, in, setup.baseURL, false)
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("base URL: %w", err)
	}
	authMode, err := readNonTerminalChoice(ctx, in, setup.authMode, addAuthModeChoices())
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("authentication: %w", err)
	}
	setup.name = name
	setup.providerType = providerType
	setup.baseURL = baseURL
	setup.authMode = authMode
	return completeNonTerminalProviderAuth(ctx, in, setup)
}

func runProviderUpdatePromptNonTerminal(ctx context.Context, deps Dependencies, setup *providerSetupPrompt) error {
	in := readerOrDefault(deps.In, os.Stdin)
	providerType, err := readNonTerminalChoice(ctx, in, setup.providerType, providerTypeChoices())
	if err != nil {
		return fmt.Errorf("provider type: %w", err)
	}
	baseURL, err := readNonTerminalPromptValue(ctx, in, setup.baseURL, false)
	if err != nil {
		return fmt.Errorf("base URL: %w", err)
	}
	authMode, err := readNonTerminalChoice(ctx, in, setup.authMode, updateAuthModeChoices())
	if err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	setup.providerType = providerType
	setup.baseURL = baseURL
	setup.authMode = authMode
	return nil
}

func completeNonTerminalProviderAuth(ctx context.Context, in io.Reader, setup providerSetupPrompt) (providerSetupPrompt, error) {
	switch setup.authMode {
	case authModeKeychain:
		value, err := readNonTerminalAPIKey(ctx, in)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		setup.apiKeyValue = value
		setup.apiKeyEnv = ""
		setup.noAPIKey = false
	case authModeEnv:
		envName, err := readNonTerminalPromptValue(ctx, in, setup.apiKeyEnv, true)
		if err != nil {
			return providerSetupPrompt{}, fmt.Errorf("API key environment variable: %w", err)
		}
		if err := validateEnvName(envName); err != nil {
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

func providerTypeChoices() map[string]string {
	return map[string]string{
		"1": "litellm",
		"2": "openrouter",
		"3": "anthropic",
		"4": "local",
	}
}

func addAuthModeChoices() map[string]string {
	return map[string]string{
		"1": authModeKeychain,
		"2": authModeEnv,
		"3": authModeNone,
	}
}

func updateAuthModeChoices() map[string]string {
	return map[string]string{
		"1": authModeKeep,
		"2": authModeKeychain,
		"3": authModeEnv,
		"4": authModeNone,
	}
}

func promptAPIKey(ctx context.Context, deps Dependencies) (string, error) {
	in := readerOrDefault(deps.In, os.Stdin)
	if !isTerminal(in) {
		value, err := readNonTerminalAPIKey(ctx, in)
		if err != nil {
			return "", err
		}
		return value, nil
	}

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
	input = input.EchoMode(huh.EchoModeNone)
	if err := runHuhForm(ctx, deps, huh.NewForm(huh.NewGroup(input))); err != nil {
		return "", err
	}
	return strings.TrimSpace(apiKey), nil
}

func readNonTerminalPromptValue(ctx context.Context, in io.Reader, fallback string, required bool) (string, error) {
	line, err := readLine(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	value := strings.TrimSpace(line)
	if value == "" {
		value = strings.TrimSpace(fallback)
	}
	if required && value == "" {
		return "", fmt.Errorf("value is required")
	}
	return value, nil
}

func readNonTerminalChoice(ctx context.Context, in io.Reader, fallback string, choices map[string]string) (string, error) {
	value, err := readNonTerminalPromptValue(ctx, in, fallback, true)
	if err != nil {
		return "", err
	}
	if selected, ok := choices[value]; ok {
		return selected, nil
	}
	for _, selected := range choices {
		if value == selected {
			return selected, nil
		}
	}
	return "", fmt.Errorf("invalid choice %q", value)
}

func readNonTerminalAPIKey(ctx context.Context, in io.Reader) (string, error) {
	for {
		line, err := readLine(in)
		value := strings.TrimSpace(line)
		if value != "" {
			return value, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("API key is required")
			}
			return "", fmt.Errorf("reading API key: %w", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("reading API key: %w", ctxErr)
		}
	}
}

func readLine(in io.Reader) (string, error) {
	var builder strings.Builder
	var buf [1]byte
	for {
		n, err := in.Read(buf[:])
		if n > 0 {
			switch buf[0] {
			case '\n':
				return builder.String(), nil
			case '\r':
				continue
			default:
				_ = builder.WriteByte(buf[0])
			}
		}
		if err != nil {
			if builder.Len() > 0 && errors.Is(err, io.EOF) {
				return builder.String(), nil
			}
			return builder.String(), err
		}
	}
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

func confirmRemoval(ctx context.Context, deps Dependencies, yes, interactive bool, title string) (bool, error) {
	if yes {
		return true, nil
	}
	if !interactive && !isTerminal(readerOrDefault(deps.In, os.Stdin)) {
		return false, fmt.Errorf("removal requires confirmation; rerun with --yes or --interactive")
	}
	confirmed := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(title).
			Affirmative("Remove").
			Negative("Cancel").
			Value(&confirmed),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return false, err
	}
	return confirmed, nil
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
