package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"golang.org/x/term"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

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

func promptAPIKeyFile(ctx context.Context, deps Dependencies, initial string) (string, error) {
	apiKeyFile := initial
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("API key file").
			Description("Must be a regular file with permissions 0600.").
			Value(&apiKeyFile).
			Validate(func(value string) error {
				_, err := secret.FileRefFromPath(value)
				return err
			}),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return strings.TrimSpace(apiKeyFile), nil
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
	return promptModelSelectionWithValidation(ctx, deps, models, nil)
}

func promptRequiredModelSelection(ctx context.Context, deps Dependencies, models []string) ([]string, error) {
	selectionErr := errors.New("select at least one model before continuing")
	accessible := shouldUseAccessiblePrompts(Dependencies{
		In:  readerOrDefault(deps.In, os.Stdin),
		Err: writerOrDefault(deps.Err, os.Stderr),
	})
	selected, err := promptModelSelectionWithValidation(ctx, deps, models, func(selected []string) error {
		if len(selected) == 0 && !accessible {
			return selectionErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, selectionErr
	}
	return selected, nil
}

func promptModelSelectionWithValidation(ctx context.Context, deps Dependencies, models []string, validate func([]string) error) ([]string, error) {
	selected := make([]string, 0, len(models))
	options := make([]huh.Option[string], 0, len(models))
	for _, model := range models {
		options = append(options, huh.NewOption(model, model))
	}
	selection := huh.NewMultiSelect[string]().
		Title("Select models to import").
		Options(options...).
		Filterable(true).
		Height(12).
		Value(&selected)
	if validate != nil {
		selection = selection.Validate(validate)
	}
	form := huh.NewForm(huh.NewGroup(selection))
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

func interactiveProviderTypeDefault(providerName, explicit, protocol string) string {
	if explicit != "" {
		return explicit
	}
	resolved, err := resolveProviderTypeWithProtocol(providerName, "", protocol)
	if err != nil {
		return "litellm"
	}
	return resolved
}

func interactiveProviderUpdateTypeDefault(existing store.Provider, cfg providerAddConfig) string {
	if cfg.providerType == "" && cfg.protocol == "" {
		return existing.Type
	}
	return interactiveProviderTypeDefault(existing.Name, cfg.providerType, cfg.protocol)
}

func interactiveAuthModeDefault(cfg providerAddConfig) string {
	switch {
	case cfg.apiKeyEnv != "":
		return authModeEnv
	case cfg.apiKeyFile != "":
		return authModeFile
	case cfg.noAPIKey:
		return authModeNone
	default:
		return authModeKeychain
	}
}
