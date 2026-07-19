package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type modelImportChoice string

const (
	modelImportSelect modelImportChoice = "select"
	modelImportAll    modelImportChoice = "all"
	modelImportSkip   modelImportChoice = "skip"

	maxGeneratedModelAliasLength = 64
)

type plannedModelImport struct {
	alias                   string
	providerModel           string
	status                  string
	discoveredCapabilities  modelcap.Snapshot
	capabilitiesRefreshedAt string
}

type modelImportSummary struct {
	imported       int
	skipped        int
	removed        int
	skippedAliases []string
}

func newProviderDiscoverModelsCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "discover-models <name>",
		Short: "List models discovered from an OpenAI-compatible provider",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider discover-models litellm")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)

			provider, err := s.GetProvider(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Discovering models for provider %q (%s)\n", provider.Name, provider.BaseURL)
			discovery, err := discoverProviderModels(ctx, deps, provider)
			if err != nil {
				return err
			}
			writeDiscoveryDiagnostics(cmd.OutOrStdout(), discovery)
			models := discovery.RoutableModels()
			if len(models) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No models discovered for provider %q.\n", provider.Name)
				return nil
			}
			for index := range models {
				fmt.Fprintln(cmd.OutOrStdout(), models[index].ID)
			}
			return nil
		},
	}
}

func newProviderImportModelsCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var importAll bool
	cmd := &cobra.Command{
		Use:   "import-models <name>",
		Short: "Select, review, and import discovered provider models as aliases",
		Long: `Select, review, and import discovered provider models as aliases.

	By default this runs a guided searchable multi-select and review flow. Use
	--all for deterministic automation that imports every discovered model and skips
	aliases that already exist.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("provider name is required; example: ccr provider import-models litellm")
			}
			return validateName("provider name", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviderImportModels(ctx, cmd, opts, deps, args[0], importAll)
		},
	}
	cmd.Flags().BoolVar(&importAll, "all", false, "Import every discovered model and skip aliases that already exist")
	return cmd
}

func runProviderImportModels(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, providerName string, importAll bool) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	provider, err := s.GetProvider(ctx, providerName)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Discovering models for provider %q (%s)\n", provider.Name, provider.BaseURL)
	discovery, err := discoverProviderModels(ctx, deps, provider)
	if err != nil {
		return err
	}
	writeDiscoveryDiagnostics(cmd.OutOrStdout(), discovery)
	models := discovery.RoutableModels()
	if len(models) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No models discovered for provider %q.\n", provider.Name)
		return nil
	}

	choice := modelImportSelect
	selected := models
	if !importAll {
		selectedIDs, selectionErr := promptModelSelection(ctx, deps, discoveredModelIDs(models))
		err = selectionErr
		if err != nil {
			return err
		}
		selected = selectDiscoveredModels(models, selectedIDs)
		if len(selected) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No models selected.")
			return nil
		}
	} else {
		choice = modelImportAll
	}

	planned, summary, err := planModelImports(ctx, deps, s, provider.Name, selected, choice)
	if err != nil {
		return err
	}
	if !importAll {
		planned, summary, err = reviewPlannedModelImports(ctx, cmd, deps, s, planned, summary)
		if errors.Is(err, errModelImportAborted) {
			fmt.Fprintln(cmd.OutOrStdout(), "No model aliases saved.")
			return nil
		}
		if err != nil {
			return err
		}
	}
	if err := addPlannedModelImports(ctx, s, provider.Name, planned, &summary); err != nil {
		return err
	}
	printModelImportSummary(cmd.OutOrStdout(), provider.Name, summary)
	printModelImportDetails(cmd.OutOrStdout(), planned)
	printModelLaunchGuidance(cmd.OutOrStdout(), planned, providerDisablesClaudeTools(provider))
	return nil
}

func discoverProviderModels(ctx context.Context, deps Dependencies, provider store.Provider) (providers.DiscoveryResult, error) {
	return discoverProviderModelsWithPlan(ctx, deps, provider, secretPlan{ref: provider.SecretRef})
}

func discoverProviderModelsWithPlan(ctx context.Context, deps Dependencies, provider store.Provider, plan secretPlan) (providers.DiscoveryResult, error) {
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol != providers.ProtocolOpenAICompatible || !caps.SupportsModelDiscovery {
		return providers.DiscoveryResult{}, fmt.Errorf("provider %q uses protocol %q and does not support OpenAI-compatible model discovery", provider.Name, caps.Protocol)
	}
	apiKey, err := resolveDiscoveryAPIKey(ctx, deps, plan)
	if err != nil {
		return providers.DiscoveryResult{}, fmt.Errorf("resolving API key for provider %q (secret=%s): %w", provider.Name, secret.RedactRef(plan.ref), err)
	}
	models, err := (providers.Discoverer{}).DiscoverOpenAICompatibleModels(ctx, providers.DiscoveryConfig{
		Type:    provider.Type,
		BaseURL: provider.BaseURL,
		APIKey:  apiKey,
	})
	if err != nil {
		return providers.DiscoveryResult{}, fmt.Errorf("discovering models for provider %q (%s, secret=%s): %w", provider.Name, provider.BaseURL, secret.RedactRef(plan.ref), err)
	}
	return models, nil
}

func resolveDiscoveryAPIKey(ctx context.Context, deps Dependencies, plan secretPlan) (string, error) {
	if strings.TrimSpace(plan.value) != "" {
		return strings.TrimSpace(plan.value), nil
	}
	if plan.ref == "" {
		return "", nil
	}
	apiKey, err := deps.Secrets.Resolve(ctx, plan.ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(apiKey), nil
}

func planModelImports(ctx context.Context, deps Dependencies, s *store.Store, providerName string, models []providers.DiscoveredModel, choice modelImportChoice) ([]plannedModelImport, modelImportSummary, error) {
	existing, err := existingModelAliases(ctx, s)
	if err != nil {
		return nil, modelImportSummary{}, err
	}

	var summary modelImportSummary
	planned := make([]plannedModelImport, 0, len(models))
	refreshedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for index := range models {
		discovered := &models[index]
		modelID := discovered.ID
		alias := generateProviderModelAlias(providerName, modelID)
		if err := validateName("model alias", alias); err != nil {
			return nil, modelImportSummary{}, err
		}
		if _, ok := existing[alias]; ok {
			if choice == modelImportAll {
				summary.skipped++
				summary.skippedAliases = append(summary.skippedAliases, alias)
				continue
			}
			renamedAlias, skip, err := promptAliasConflict(ctx, deps, alias, modelID, existing)
			if err != nil {
				return nil, modelImportSummary{}, err
			}
			if skip {
				summary.skipped++
				summary.skippedAliases = append(summary.skippedAliases, alias)
				continue
			}
			alias = renamedAlias
		}
		existing[alias] = struct{}{}
		planned = append(planned, plannedModelImport{
			alias: alias, providerModel: modelID, status: "degraded",
			discoveredCapabilities: discovered.Capabilities, capabilitiesRefreshedAt: refreshedAt,
		})
	}
	return planned, summary, nil
}

func addPlannedModelImports(ctx context.Context, s *store.Store, providerName string, planned []plannedModelImport, summary *modelImportSummary) error {
	provider, err := s.GetProvider(ctx, providerName)
	if err != nil {
		return err
	}
	if err := validatePlannedProviderModels(provider, planned); err != nil {
		return err
	}
	for index := range planned {
		item := &planned[index]
		err := s.AddModel(ctx, store.Model{
			Alias:                   item.alias,
			ProviderName:            providerName,
			ProviderModel:           item.providerModel,
			Status:                  plannedModelStatus(*item),
			DiscoveredCapabilities:  item.discoveredCapabilities,
			CapabilitiesRefreshedAt: item.capabilitiesRefreshedAt,
		})
		if err != nil {
			return err
		}
		summary.imported++
	}
	return nil
}

func validatePlannedProviderModels(provider store.Provider, planned []plannedModelImport) error {
	for index := range planned {
		if providers.IsProviderControlModel(provider.Type, planned[index].providerModel) {
			return fmt.Errorf("provider model %q is a LiteLLM control model and cannot be routed", planned[index].providerModel)
		}
	}
	return nil
}

func discoveredModelIDs(models []providers.DiscoveredModel) []string {
	ids := make([]string, 0, len(models))
	for index := range models {
		ids = append(ids, models[index].ID)
	}
	return ids
}

func selectDiscoveredModels(models []providers.DiscoveredModel, selectedIDs []string) []providers.DiscoveredModel {
	byID := make(map[string]providers.DiscoveredModel, len(models))
	for index := range models {
		model := &models[index]
		byID[model.ID] = *model
	}
	selected := make([]providers.DiscoveredModel, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		if model, ok := byID[id]; ok {
			selected = append(selected, model)
		}
	}
	return selected
}

func writeDiscoveryDiagnostics(out io.Writer, discovery providers.DiscoveryResult) {
	for _, warning := range discovery.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	if skipped := discovery.SkippedCount(); skipped > 0 {
		fmt.Fprintf(out, "Skipped %d non-chat or provider-control models.\n", skipped)
	}
}

func existingModelAliases(ctx context.Context, s *store.Store) (map[string]struct{}, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]struct{}, len(models))
	for index := range models {
		aliases[models[index].Alias] = struct{}{}
	}
	return aliases, nil
}

func printModelImportSummary(out io.Writer, providerName string, summary modelImportSummary) {
	fmt.Fprintf(out, "Imported %d model aliases for provider %q.", summary.imported, providerName)
	if summary.skipped > 0 {
		fmt.Fprintf(out, " Skipped %d existing aliases.", summary.skipped)
	}
	if summary.removed > 0 {
		fmt.Fprintf(out, " Removed %d aliases during review.", summary.removed)
	}
	fmt.Fprintln(out)
}

func generateProviderModelAlias(providerName, providerModel string) string {
	providerPart := sanitizeAliasPart(providerName)
	modelPart := sanitizeAliasPart(providerModel)
	if modelPart == "" {
		modelPart = "model"
	}
	alias := compactGeneratedAlias(providerPart + "-" + modelPart)
	if err := validateName("model alias", alias); err == nil {
		return alias
	}
	fallback := compactGeneratedAlias(providerPart + "-model")
	if err := validateName("model alias", fallback); err == nil {
		return fallback
	}
	return "provider-model"
}

func compactGeneratedAlias(alias string) string {
	alias = strings.Trim(alias, "-")
	if len(alias) <= maxGeneratedModelAliasLength {
		return alias
	}
	alias = strings.TrimRight(alias[:maxGeneratedModelAliasLength], "-")
	if len(alias) < 2 {
		return "model"
	}
	return alias
}

func sanitizeAliasPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if builder.Len() > 0 && !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	sanitized := strings.Trim(builder.String(), "-")
	if sanitized == "" {
		return "model"
	}
	return sanitized
}
