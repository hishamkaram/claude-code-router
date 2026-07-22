package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/hishamkaram/claude-code-router/internal/providers"
)

func promptProviderSetup(ctx context.Context, deps Dependencies, initial providerSetupPrompt) (providerSetupPrompt, error) {
	setup := initial
	modeDefaulted := strings.TrimSpace(setup.mode) == ""
	if setup.providerType == "" {
		setup.providerType = "litellm"
	}
	initialModeDefault := ""
	if setup.mode == "" {
		initialModeDefault = defaultProviderMode(setup.providerType)
		setup.mode = initialModeDefault
	}
	if setup.authMode == "" {
		setup.authMode = authModeKeychain
	}
	if !isTerminal(readerOrDefault(deps.In, os.Stdin)) {
		prompted, err := promptProviderSetupNonTerminal(ctx, deps, setup)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		return applyProviderPromptModeDefault(prompted, modeDefaulted, initialModeDefault), nil
	}

	if err := runProviderSetupForm(ctx, deps, &setup); err != nil {
		return providerSetupPrompt{}, err
	}
	completed, err := completeTerminalProviderAuth(ctx, deps, setup)
	if err != nil {
		return providerSetupPrompt{}, err
	}
	return applyProviderPromptModeDefault(completed, modeDefaulted, initialModeDefault), nil
}

func runProviderSetupForm(ctx context.Context, deps Dependencies, setup *providerSetupPrompt) error {
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Provider profile").
			Description(providerProfilePromptDescription()).
			Options(providerProfileOptions()...).
			Filtering(true).
			Height(8).
			Value(&setup.providerType),
		huh.NewInput().
			Title("Connection name").
			Value(&setup.name).
			Validate(func(value string) error {
				return validateName("provider name", value)
			}),
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
				huh.NewOption("Store API key in OS keychain", authModeKeychain),
				huh.NewOption("Use environment variable", authModeEnv),
				huh.NewOption("Use API key file", authModeFile),
				huh.NewOption("No API key", authModeNone),
			).
			Value(&setup.authMode),
	))
	return runHuhForm(ctx, deps, form)
}

func completeTerminalProviderAuth(ctx context.Context, deps Dependencies, setup providerSetupPrompt) (providerSetupPrompt, error) {
	switch setup.authMode {
	case authModeKeychain:
		value, err := promptAPIKey(ctx, deps)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		return providerSetupWithKeychainAuth(setup, value), nil
	case authModeEnv:
		envName, err := promptAPIKeyEnv(ctx, deps, setup.apiKeyEnv)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		return providerSetupWithEnvAuth(setup, envName), nil
	case authModeFile:
		filePath, err := promptAPIKeyFile(ctx, deps, setup.apiKeyFile)
		if err != nil {
			return providerSetupPrompt{}, err
		}
		return providerSetupWithFileAuth(setup, filePath), nil
	case authModeNone:
		return providerSetupWithNoAuth(setup), nil
	default:
		return providerSetupPrompt{}, fmt.Errorf("invalid authentication mode %q", setup.authMode)
	}
}

func applyProviderPromptModeDefault(setup providerSetupPrompt, modeDefaulted bool, initialDefault string) providerSetupPrompt {
	if modeDefaulted && setup.mode == initialDefault {
		setup.mode = defaultProviderMode(setup.providerType)
	}
	return setup
}

func promptProviderSetupNonTerminal(ctx context.Context, deps Dependencies, setup providerSetupPrompt) (providerSetupPrompt, error) {
	in := readerOrDefault(deps.In, os.Stdin)
	providerType, err := readNonTerminalChoice(ctx, in, setup.providerType, providerTypeChoices())
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("provider profile: %w", err)
	}
	name, err := readNonTerminalPromptValue(ctx, in, setup.name, true)
	if err != nil {
		return providerSetupPrompt{}, fmt.Errorf("provider name: %w", err)
	}
	if validateErr := validateName("provider name", name); validateErr != nil {
		return providerSetupPrompt{}, validateErr
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
		return providerSetupWithKeychainAuth(setup, value), nil
	case authModeEnv:
		envName, err := readNonTerminalPromptValue(ctx, in, setup.apiKeyEnv, true)
		if err != nil {
			return providerSetupPrompt{}, fmt.Errorf("API key environment variable: %w", err)
		}
		if err := validateEnvName(envName); err != nil {
			return providerSetupPrompt{}, err
		}
		return providerSetupWithEnvAuth(setup, envName), nil
	case authModeFile:
		filePath, err := readNonTerminalPromptValue(ctx, in, setup.apiKeyFile, true)
		if err != nil {
			return providerSetupPrompt{}, fmt.Errorf("API key file: %w", err)
		}
		return providerSetupWithFileAuth(setup, filePath), nil
	case authModeNone:
		return providerSetupWithNoAuth(setup), nil
	default:
		return providerSetupPrompt{}, fmt.Errorf("invalid authentication mode %q", setup.authMode)
	}
}

func providerSetupWithKeychainAuth(setup providerSetupPrompt, value string) providerSetupPrompt {
	setup.apiKeyEnv = ""
	setup.apiKeyFile = ""
	setup.apiKeyValue = value
	setup.noAPIKey = false
	return setup
}

func providerSetupWithEnvAuth(setup providerSetupPrompt, envName string) providerSetupPrompt {
	setup.apiKeyEnv = envName
	setup.apiKeyFile = ""
	setup.apiKeyValue = ""
	setup.noAPIKey = false
	return setup
}

func providerSetupWithFileAuth(setup providerSetupPrompt, filePath string) providerSetupPrompt {
	setup.apiKeyEnv = ""
	setup.apiKeyFile = filePath
	setup.apiKeyValue = ""
	setup.noAPIKey = false
	return setup
}

func providerSetupWithNoAuth(setup providerSetupPrompt) providerSetupPrompt {
	setup.apiKeyEnv = ""
	setup.apiKeyFile = ""
	setup.apiKeyValue = ""
	setup.noAPIKey = true
	return setup
}

func defaultProviderMode(providerType string) string {
	mode := providers.DefaultCapabilities(providerType).Mode
	if mode == "" {
		return providers.ModeDegraded
	}
	return mode
}

func providerModeForTypeChange(existingType, existingMode, resolvedType, requestedMode string, modeChanged bool) string {
	if modeChanged {
		return requestedMode
	}
	if existingMode == "" || existingMode == defaultProviderMode(existingType) {
		return defaultProviderMode(resolvedType)
	}
	return existingMode
}

func addAuthModeChoices() map[string]string {
	return map[string]string{
		"1": authModeKeychain,
		"2": authModeEnv,
		"3": authModeNone,
		"4": authModeFile,
	}
}

func updateAuthModeChoices() map[string]string {
	return map[string]string{
		"1": authModeKeep,
		"2": authModeKeychain,
		"3": authModeEnv,
		"4": authModeNone,
		"5": authModeFile,
	}
}
