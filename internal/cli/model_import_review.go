package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

var errModelImportAborted = errors.New("model import aborted")

type discoveryFailureAction string

const (
	discoveryFailureEdit         discoveryFailureAction = "edit"
	discoveryFailureSaveProvider discoveryFailureAction = "save-provider"
	discoveryFailureCancel       discoveryFailureAction = "cancel"
)

type manualModelAction string

const (
	manualModelAdd          manualModelAction = "add"
	manualModelFinish       manualModelAction = "finish"
	manualModelProviderOnly manualModelAction = "provider-only"
	manualModelCancel       manualModelAction = "cancel"
)

type modelImportReviewAction string

const (
	modelImportReviewSave   modelImportReviewAction = "save"
	modelImportReviewRename modelImportReviewAction = "rename"
	modelImportReviewCompat modelImportReviewAction = "compat"
	modelImportReviewRemove modelImportReviewAction = "remove"
	modelImportReviewCancel modelImportReviewAction = "cancel"
)

func promptDiscoveryFailureAction(ctx context.Context, deps Dependencies) (discoveryFailureAction, error) {
	return promptProviderSetupFailureAction(ctx, deps, "Model discovery failed")
}

func promptProviderValidationFailureAction(ctx context.Context, deps Dependencies) (discoveryFailureAction, error) {
	return promptProviderSetupFailureAction(ctx, deps, "Provider validation failed")
}

func promptProviderSetupFailureAction(ctx context.Context, deps Dependencies, title string) (discoveryFailureAction, error) {
	action := discoveryFailureEdit
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[discoveryFailureAction]().
			Title(title).
			Options(
				huh.NewOption("Edit provider setup", discoveryFailureEdit),
				huh.NewOption("Save provider only", discoveryFailureSaveProvider),
				huh.NewOption("Cancel without saving", discoveryFailureCancel),
			).
			Value(&action),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return action, nil
}

func manualModelImportPlan(ctx context.Context, cmd *cobra.Command, deps Dependencies, s *store.Store, provider store.Provider, plan secretPlan) ([]plannedModelImport, modelImportSummary, error) {
	if _, err := validateProviderConfigAndSecretWithPlan(ctx, deps, provider, plan); err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Provider validation failed for provider %q: %v\n", provider.Name, err)
		action, promptErr := promptProviderValidationFailureAction(ctx, deps)
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
			return nil, modelImportSummary{}, fmt.Errorf("invalid provider validation failure action %q", action)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), providerDiscoveryUnavailableSummary(provider))
	fmt.Fprintln(cmd.OutOrStdout(), "Config and credential validation passed; live routing is still unverified until a saved model alias is tested.")
	planned, err := promptManualModelImports(ctx, deps, s)
	if errors.Is(err, errModelImportAborted) {
		fmt.Fprintf(cmd.OutOrStdout(), "Provider %q was not saved.\n", provider.Name)
	}
	return planned, modelImportSummary{}, err
}

func promptManualModelImports(ctx context.Context, deps Dependencies, s *store.Store) ([]plannedModelImport, error) {
	existing, err := existingModelAliases(ctx, s)
	if err != nil {
		return nil, err
	}
	planned := []plannedModelImport{}
	for {
		action, err := promptManualModelAction(ctx, deps, len(planned) > 0)
		if err != nil {
			return nil, err
		}
		switch action {
		case manualModelAdd:
			item, err := promptManualModelImport(ctx, deps, planned, existing)
			if err != nil {
				return nil, err
			}
			planned = append(planned, item)
		case manualModelFinish:
			return planned, nil
		case manualModelProviderOnly:
			return nil, nil
		case manualModelCancel:
			return nil, errModelImportAborted
		default:
			return nil, fmt.Errorf("invalid manual model action %q", action)
		}
	}
}

func promptManualModelAction(ctx context.Context, deps Dependencies, hasPlanned bool) (manualModelAction, error) {
	action := manualModelAdd
	options := []huh.Option[manualModelAction]{
		huh.NewOption("Add a model alias", manualModelAdd),
	}
	if hasPlanned {
		action = manualModelFinish
		options = append(options, huh.NewOption("Finish and save provider with aliases", manualModelFinish))
	}
	options = append(options,
		huh.NewOption("Save provider only", manualModelProviderOnly),
		huh.NewOption("Cancel without saving", manualModelCancel),
	)
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[manualModelAction]().
			Title("Manual model setup").
			Options(options...).
			Value(&action),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return action, nil
}

func promptManualModelImport(ctx context.Context, deps Dependencies, planned []plannedModelImport, existing map[string]struct{}) (plannedModelImport, error) {
	item := plannedModelImport{status: "degraded"}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Model alias").
			Description("Lowercase CCR alias shown in /model as CCR <alias>.").
			Value(&item.alias).
			Validate(func(value string) error {
				return validatePlannedModelAlias(value, planned, -1, existing)
			}),
		huh.NewInput().
			Title("Provider model ID").
			Value(&item.providerModel).
			Validate(func(value string) error {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("provider model ID is required")
				}
				return nil
			}),
		huh.NewSelect[string]().
			Title("Compatibility").
			Options(modelCompatibilityOptions()...).
			Value(&item.status),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return plannedModelImport{}, err
	}
	item.alias = strings.TrimSpace(item.alias)
	item.providerModel = strings.TrimSpace(item.providerModel)
	if err := validateCompatibilityStatus(item.status); err != nil {
		return plannedModelImport{}, err
	}
	return item, nil
}

func reviewPlannedModelImports(ctx context.Context, cmd *cobra.Command, deps Dependencies, s *store.Store, planned []plannedModelImport, summary modelImportSummary) ([]plannedModelImport, modelImportSummary, error) {
	if len(planned) == 0 {
		return planned, summary, nil
	}
	printPlannedModelReview(cmd.OutOrStdout(), planned)
	existing, err := existingModelAliases(ctx, s)
	if err != nil {
		return nil, modelImportSummary{}, err
	}
	reviewed, removed, err := promptModelImportReview(ctx, deps, planned, existing)
	if err != nil {
		return nil, modelImportSummary{}, err
	}
	summary.removed += removed
	return reviewed, summary, nil
}

func promptModelImportReview(ctx context.Context, deps Dependencies, planned []plannedModelImport, existing map[string]struct{}) ([]plannedModelImport, int, error) {
	removed := 0
	for len(planned) > 0 {
		action, err := promptModelImportReviewAction(ctx, deps)
		if err != nil {
			return nil, 0, err
		}
		switch action {
		case modelImportReviewSave:
			return planned, removed, nil
		case modelImportReviewRename:
			index, err := promptPlannedModelIndex(ctx, deps, "Choose alias to rename", planned)
			if err != nil {
				return nil, 0, err
			}
			if err := promptRenamePlannedModel(ctx, deps, planned, index, existing); err != nil {
				return nil, 0, err
			}
		case modelImportReviewCompat:
			index, err := promptPlannedModelIndex(ctx, deps, "Choose alias compatibility", planned)
			if err != nil {
				return nil, 0, err
			}
			status, err := promptModelCompatibility(ctx, deps, planned[index].status)
			if err != nil {
				return nil, 0, err
			}
			planned[index].status = status
		case modelImportReviewRemove:
			index, err := promptPlannedModelIndex(ctx, deps, "Choose alias to remove", planned)
			if err != nil {
				return nil, 0, err
			}
			planned = append(planned[:index], planned[index+1:]...)
			removed++
		case modelImportReviewCancel:
			return nil, removed, errModelImportAborted
		default:
			return nil, 0, fmt.Errorf("invalid model import review action %q", action)
		}
	}
	return planned, removed, nil
}

func promptModelImportReviewAction(ctx context.Context, deps Dependencies) (modelImportReviewAction, error) {
	action := modelImportReviewSave
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[modelImportReviewAction]().
			Title("Review model aliases before saving").
			Options(
				huh.NewOption("Save aliases", modelImportReviewSave),
				huh.NewOption("Rename an alias", modelImportReviewRename),
				huh.NewOption("Change compatibility", modelImportReviewCompat),
				huh.NewOption("Remove a model", modelImportReviewRemove),
				huh.NewOption("Cancel import", modelImportReviewCancel),
			).
			Value(&action),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	return action, nil
}

func promptPlannedModelIndex(ctx context.Context, deps Dependencies, title string, planned []plannedModelImport) (int, error) {
	selected := "0"
	options := make([]huh.Option[string], 0, len(planned))
	for index, item := range planned {
		options = append(options, huh.NewOption(modelImportReviewLabel(item), strconv.Itoa(index)))
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(title).
			Options(options...).
			Filtering(true).
			Height(10).
			Value(&selected),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return 0, err
	}
	index, err := strconv.Atoi(selected)
	if err != nil || index < 0 || index >= len(planned) {
		return 0, fmt.Errorf("invalid model selection %q", selected)
	}
	return index, nil
}

func promptRenamePlannedModel(ctx context.Context, deps Dependencies, planned []plannedModelImport, index int, existing map[string]struct{}) error {
	alias := planned[index].alias
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("New alias").
			Value(&alias).
			Validate(func(value string) error {
				return validatePlannedModelAlias(value, planned, index, existing)
			}),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return err
	}
	planned[index].alias = strings.TrimSpace(alias)
	return nil
}

func promptModelCompatibility(ctx context.Context, deps Dependencies, initial string) (string, error) {
	status := initial
	if status == "" {
		status = "degraded"
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Compatibility").
			Options(modelCompatibilityOptions()...).
			Value(&status),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return "", err
	}
	if err := validateCompatibilityStatus(status); err != nil {
		return "", err
	}
	return status, nil
}

func validatePlannedModelAlias(value string, planned []plannedModelImport, current int, existing map[string]struct{}) error {
	value = strings.TrimSpace(value)
	if err := validateName("model alias", value); err != nil {
		return err
	}
	if _, ok := existing[value]; ok {
		return fmt.Errorf("model alias %q already exists", value)
	}
	for index, item := range planned {
		if index != current && item.alias == value {
			return fmt.Errorf("model alias %q is already planned", value)
		}
	}
	return nil
}

func modelCompatibilityOptions() []huh.Option[string] {
	return []huh.Option[string]{
		huh.NewOption("Degraded", "degraded"),
		huh.NewOption("Full", "full"),
		huh.NewOption("Chat only", "chat-only"),
		huh.NewOption("Blocked", "blocked"),
	}
}

func printPlannedModelReview(out io.Writer, planned []plannedModelImport) {
	fmt.Fprintln(out, "Review model aliases before saving:")
	for _, item := range planned {
		fmt.Fprintf(out, "  %s -> %s (compat=%s)\n", item.alias, item.providerModel, plannedModelStatus(item))
	}
}

func printModelImportDetails(out io.Writer, planned []plannedModelImport) {
	for _, item := range planned {
		fmt.Fprintf(out, "Alias %s -> %s (compat=%s)\n", item.alias, item.providerModel, plannedModelStatus(item))
	}
}

func printModelLaunchGuidance(out io.Writer, planned []plannedModelImport, providerToolDisabled bool) {
	if len(planned) == 0 {
		return
	}
	aliases := make([]string, 0, len(planned))
	directLaunchAliases := make([]string, 0)
	for _, item := range planned {
		status := plannedModelStatus(item)
		if status == "blocked" {
			continue
		}
		if status == "chat-only" || providerToolDisabled {
			directLaunchAliases = append(directLaunchAliases, item.alias)
			continue
		}
		aliases = append(aliases, "CCR "+item.alias)
	}
	if len(aliases) > 0 {
		fmt.Fprintln(out, "Launch command: ccr launch")
		fmt.Fprintf(out, "/model: choose %s.\n", strings.Join(aliases, ", "))
	}
	for _, alias := range directLaunchAliases {
		fmt.Fprintf(out, "Launch command: ccr launch --model %s\n", alias)
	}
}

func modelImportReviewLabel(item plannedModelImport) string {
	return fmt.Sprintf("%s -> %s (compat=%s)", item.alias, item.providerModel, plannedModelStatus(item))
}

func plannedModelStatus(item plannedModelImport) string {
	if item.status == "" {
		return "degraded"
	}
	return item.status
}
