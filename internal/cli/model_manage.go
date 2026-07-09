package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func newModelUpdateCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var providerName string
	var providerModel string
	var status string
	var interactive bool
	cmd := &cobra.Command{
		Use:   "update <alias>",
		Short: "Update a model alias",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr model update qwen --model qwen/qwen3-coder")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			changes := modelUpdateChangesFromCommand(cmd)
			if interactive {
				defaultProviderModel, err := validateModelUpdateStaticFlags(providerName, providerModel, status, changes)
				if err != nil {
					return err
				}
				return runModelUpdateInteractive(ctx, cmd, opts, deps, args[0], providerName, defaultProviderModel, status, changes)
			}
			return runModelUpdate(ctx, cmd, opts, args[0], providerName, providerModel, status)
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "Configured provider name")
	cmd.Flags().StringVar(&providerModel, "model", "", "Provider-specific model name")
	cmd.Flags().StringVar(&status, "compat", "", "Compatibility status: full, degraded, chat-only, or blocked")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Guide model alias updates with prompts")
	return cmd
}

func runModelUpdate(ctx context.Context, cmd *cobra.Command, opts *options, alias, providerName, providerModel, status string) error {
	changes := modelUpdateChangesFromCommand(cmd)
	if !changes.any() {
		return fmt.Errorf("model update requires at least one change flag or --interactive")
	}
	providerModel, err := validateModelUpdateStaticFlags(providerName, providerModel, status, changes)
	if err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return err
	}
	model, err = applyModelUpdateChanges(ctx, s, model, providerName, providerModel, status, changes)
	if err != nil {
		return err
	}
	if err := s.UpdateModel(ctx, model); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q updated (provider=%s, model=%s, compat=%s)\n", alias, model.ProviderName, model.ProviderModel, model.Status)
	return nil
}

type modelUpdateChanges struct {
	provider bool
	model    bool
	status   bool
}

func (c modelUpdateChanges) any() bool {
	return c.provider || c.model || c.status
}

func modelUpdateChangesFromCommand(cmd *cobra.Command) modelUpdateChanges {
	flags := cmd.Flags()
	return modelUpdateChanges{
		provider: flags.Changed("provider"),
		model:    flags.Changed("model"),
		status:   flags.Changed("compat"),
	}
}

func validateModelUpdateStaticFlags(providerName, providerModel, status string, changes modelUpdateChanges) (string, error) {
	if changes.provider {
		if err := validateName("provider", providerName); err != nil {
			return "", err
		}
	}
	if changes.model {
		providerModel = strings.TrimSpace(providerModel)
		if providerModel == "" {
			return "", fmt.Errorf("--model is required")
		}
	}
	if changes.status {
		if err := validateCompatibilityStatus(status); err != nil {
			return "", err
		}
	}
	return providerModel, nil
}

func applyModelUpdateChanges(ctx context.Context, s *store.Store, model store.Model, providerName, providerModel, status string, changes modelUpdateChanges) (store.Model, error) {
	if changes.provider {
		if _, err := s.GetProvider(ctx, providerName); err != nil {
			return store.Model{}, err
		}
		model.ProviderName = providerName
	}
	if changes.model {
		model.ProviderModel = providerModel
	}
	if changes.status {
		model.Status = status
	}
	return model, nil
}

func runModelUpdateInteractive(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, alias, providerName, providerModel, status string, changes modelUpdateChanges) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return err
	}
	model, err = applyInteractiveModelUpdateDefaults(model, providerName, providerModel, status, changes)
	if err != nil {
		return err
	}
	updated, err := promptModelUpdate(ctx, deps, model)
	if err != nil {
		return err
	}
	if _, err := s.GetProvider(ctx, updated.ProviderName); err != nil {
		return err
	}
	if err := s.UpdateModel(ctx, updated); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q updated (provider=%s, model=%s, compat=%s)\n", alias, updated.ProviderName, updated.ProviderModel, updated.Status)
	return nil
}

func applyInteractiveModelUpdateDefaults(model store.Model, providerName, providerModel, status string, changes modelUpdateChanges) (store.Model, error) {
	if changes.provider {
		model.ProviderName = providerName
	}
	if changes.model {
		model.ProviderModel = providerModel
	}
	if changes.status {
		if err := validateCompatibilityStatus(status); err != nil {
			return store.Model{}, err
		}
		model.Status = status
	}
	return model, nil
}

func promptModelUpdate(ctx context.Context, deps Dependencies, model store.Model) (store.Model, error) {
	updated := model
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Provider name").
			Value(&updated.ProviderName).
			Validate(func(value string) error {
				return validateName("provider", value)
			}),
		huh.NewInput().
			Title("Provider model").
			Value(&updated.ProviderModel).
			Validate(func(value string) error {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf("provider model is required")
				}
				return nil
			}),
		huh.NewSelect[string]().
			Title("Compatibility").
			Options(
				huh.NewOption("Full", "full"),
				huh.NewOption("Degraded", "degraded"),
				huh.NewOption("Chat only", "chat-only"),
				huh.NewOption("Blocked", "blocked"),
			).
			Value(&updated.Status),
	))
	if err := runHuhForm(ctx, deps, form); err != nil {
		return store.Model{}, err
	}
	updated.ProviderModel = strings.TrimSpace(updated.ProviderModel)
	if err := validateCompatibilityStatus(updated.Status); err != nil {
		return store.Model{}, err
	}
	return updated, nil
}

func newModelTestCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "test <alias>",
		Short: "Validate a model alias against its provider",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr model test qwen")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			model, provider, discovered, err := validateModelAliasTarget(ctx, opts, deps, args[0], true)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q: provider=%s model=%s compat=%s\n", model.Alias, provider.Name, model.ProviderModel, model.Status)
			caps := effectiveProviderCapabilities(provider)
			if caps.Protocol == providers.ProtocolOpenAICompatible && caps.SupportsModelDiscovery {
				fmt.Fprintf(cmd.OutOrStdout(), "Exact provider model verified via /v1/models (%d models discovered).\n", discovered)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Config and secret validation passed for %s provider. Live routing is outside this pass.\n", caps.Protocol)
			return nil
		},
	}
}

func newModelRemoveCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var yes bool
	var interactive bool
	cmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a model alias",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr model remove qwen")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			if _, getErr := s.GetModel(ctx, alias); getErr != nil {
				return getErr
			}
			confirmed, err := confirmRemoval(ctx, deps, yes, interactive, fmt.Sprintf("Remove model alias %q?", alias))
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q was not removed.\n", alias)
				return nil
			}
			if err := s.RemoveModel(ctx, alias); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Model alias %q removed.\n", alias)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Remove without prompting for confirmation")
	cmd.Flags().BoolVar(&interactive, "interactive", false, "Prompt for removal confirmation")
	return cmd
}

func validateModelAliasTarget(ctx context.Context, opts *options, deps Dependencies, alias string, requireExactProviderModel bool) (store.Model, store.Provider, int, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	defer closeStore(s)
	return validateModelAliasTargetWithStore(ctx, deps, s, alias, requireExactProviderModel)
}

func validateModelAliasTargetWithStore(ctx context.Context, deps Dependencies, s *store.Store, alias string, requireExactProviderModel bool) (store.Model, store.Provider, int, error) {
	model, provider, err := loadModelAliasTargetWithStore(ctx, s, alias)
	if err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	if err := rejectBlockedModelAlias(model); err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	return validateLoadedModelAliasTarget(ctx, deps, model, provider, requireExactProviderModel)
}

func validateRoutableModelAliasTargetWithStore(ctx context.Context, deps Dependencies, s *store.Store, alias string, requireExactProviderModel bool) (store.Model, store.Provider, int, error) {
	model, provider, err := loadModelAliasTargetWithStore(ctx, s, alias)
	if err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	if err := rejectBlockedModelAlias(model); err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol != providers.ProtocolOpenAICompatible && caps.Protocol != providers.ProtocolAnthropicCompatible {
		return store.Model{}, store.Provider{}, 0, fmt.Errorf("model alias %q uses provider type %q with protocol %q, which is not supported by the gateway path", alias, provider.Type, caps.Protocol)
	}
	return validateLoadedModelAliasTarget(ctx, deps, model, provider, requireExactProviderModel)
}

func loadModelAliasTargetWithStore(ctx context.Context, s *store.Store, alias string) (store.Model, store.Provider, error) {
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return store.Model{}, store.Provider{}, err
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return store.Model{}, store.Provider{}, err
	}
	return model, provider, nil
}

func rejectBlockedModelAlias(model store.Model) error {
	if model.Status == "blocked" {
		return fmt.Errorf("model alias %q is blocked and cannot be routed", model.Alias)
	}
	return nil
}

func validateLoadedModelAliasTarget(ctx context.Context, deps Dependencies, model store.Model, provider store.Provider, requireExactProviderModel bool) (store.Model, store.Provider, int, error) {
	apiKey, err := validateProviderConfigAndSecret(ctx, deps, provider)
	if err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol != providers.ProtocolOpenAICompatible || !caps.SupportsModelDiscovery {
		return model, provider, 0, nil
	}
	models, err := discoverProviderModelsWithPlan(ctx, deps, provider, secretPlan{ref: provider.SecretRef, value: apiKey})
	if err != nil {
		return store.Model{}, store.Provider{}, 0, err
	}
	if requireExactProviderModel && !slices.Contains(models, model.ProviderModel) {
		return store.Model{}, store.Provider{}, 0, fmt.Errorf("model alias %q targets provider model %q, but provider %q did not return that exact model from /v1/models", model.Alias, model.ProviderModel, provider.Name)
	}
	return model, provider, len(models), nil
}
