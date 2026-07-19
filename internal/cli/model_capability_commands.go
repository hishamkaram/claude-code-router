package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const maxConcurrentProviderRefreshes = 4

type modelShowDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Alias         string            `json:"alias"`
	Provider      string            `json:"provider"`
	ProviderModel string            `json:"provider_model"`
	ClaudeModelID string            `json:"claude_model_id"`
	Compatibility string            `json:"compatibility"`
	RefreshedAt   string            `json:"capabilities_refreshed_at,omitempty"`
	Discovered    modelcap.Snapshot `json:"discovered_capabilities"`
	Overrides     modelcap.Values   `json:"capability_overrides"`
	Effective     modelcap.Snapshot `json:"effective_capabilities"`
}

type modelRefreshDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Refreshed     int                  `json:"refreshed"`
	Skipped       int                  `json:"skipped"`
	Failed        int                  `json:"failed"`
	Results       []modelRefreshResult `json:"results"`
}

type modelRefreshResult struct {
	Alias         string   `json:"alias"`
	Provider      string   `json:"provider"`
	ProviderModel string   `json:"provider_model"`
	Status        string   `json:"status"`
	RefreshedAt   string   `json:"refreshed_at,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

type providerRefreshGroup struct {
	provider store.Provider
	models   []store.Model
}

type providerDiscoveryResult struct {
	group     providerRefreshGroup
	discovery providers.DiscoveryResult
	err       error
}

func newModelShowCommand(ctx context.Context, opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show a model alias and its effective capabilities",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr model show glm-5-2")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModelShow(ctx, cmd, opts, args[0], jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func runModelShow(ctx context.Context, cmd *cobra.Command, opts *options, alias string, jsonOutput bool) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return err
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return fmt.Errorf("computing effective capabilities for model %q: %w", alias, err)
	}
	claudeModelID, err := gateway.DiscoveryIDForModel(model)
	if err != nil {
		return err
	}
	document := modelShowDocument{
		SchemaVersion: 1,
		Alias:         model.Alias, Provider: model.ProviderName, ProviderModel: model.ProviderModel,
		ClaudeModelID: claudeModelID, Compatibility: model.Status, RefreshedAt: model.CapabilitiesRefreshedAt,
		Discovered: model.DiscoveredCapabilities, Overrides: model.CapabilityOverrides, Effective: effective,
	}
	if jsonOutput {
		return writeVersionedJSON(cmd.OutOrStdout(), document)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Alias: %s\nProvider: %s\nProvider model: %s\nClaude model ID: %s\nCompatibility: %s\n",
		document.Alias, document.Provider, document.ProviderModel, document.ClaudeModelID, document.Compatibility)
	if document.RefreshedAt == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Capabilities refreshed: never")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Capabilities refreshed: %s\n", document.RefreshedAt)
	}
	if err := writeCapabilitySection(cmd.OutOrStdout(), "Discovered", document.Discovered); err != nil {
		return err
	}
	if err := writeCapabilitySection(cmd.OutOrStdout(), "Overrides", document.Overrides); err != nil {
		return err
	}
	return writeCapabilitySection(cmd.OutOrStdout(), "Effective", document.Effective)
}

func writeCapabilitySection(out io.Writer, label string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s capabilities: %w", label, err)
	}
	fmt.Fprintf(out, "%s capabilities:\n%s\n", label, encoded)
	return nil
}

func newModelRefreshCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var all bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "refresh [alias]",
		Short: "Refresh provider-discovered model capabilities",
		Args: func(cmd *cobra.Command, args []string) error {
			if all {
				if len(args) != 0 {
					return fmt.Errorf("model refresh accepts either one alias or --all, not both")
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; use ccr model refresh <alias> or ccr model refresh --all")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := ""
			if len(args) == 1 {
				alias = args[0]
			}
			return runModelRefresh(ctx, cmd, opts, deps, alias, all, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Refresh all aliases, grouping discovery by provider")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func runModelRefresh(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, alias string, all, jsonOutput bool) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	groups, initialResults, err := buildProviderRefreshGroups(ctx, s, alias, all)
	if err != nil {
		return err
	}
	discoveryResults := discoverProviderGroups(ctx, deps, groups)
	results := initialResults
	results = append(results, applyProviderDiscoveries(ctx, s, discoveryResults)...)
	sort.Slice(results, func(i, j int) bool { return results[i].Alias < results[j].Alias })
	document := summarizeModelRefresh(results)
	if jsonOutput {
		if err := writeVersionedJSON(cmd.OutOrStdout(), document); err != nil {
			return err
		}
	} else {
		writeModelRefreshSummary(cmd.OutOrStdout(), document)
	}
	if document.Failed > 0 {
		return fmt.Errorf("model capability refresh failed for %d aliases", document.Failed)
	}
	if !all && document.Refreshed == 0 {
		return fmt.Errorf("model capability refresh did not update alias %q", alias)
	}
	return nil
}

func buildProviderRefreshGroups(ctx context.Context, s *store.Store, alias string, all bool) ([]providerRefreshGroup, []modelRefreshResult, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !all {
		model, getErr := s.GetModel(ctx, alias)
		if getErr != nil {
			return nil, nil, getErr
		}
		models = []store.Model{model}
	}
	providersByName := make(map[string]store.Provider)
	groupsByName := make(map[string][]store.Model)
	var skipped []modelRefreshResult
	for index := range models {
		model := &models[index]
		provider, ok := providersByName[model.ProviderName]
		if !ok {
			provider, err = s.GetProvider(ctx, model.ProviderName)
			if err != nil {
				return nil, nil, err
			}
			providersByName[model.ProviderName] = provider
		}
		caps := effectiveProviderCapabilities(provider)
		if caps.Protocol != providers.ProtocolOpenAICompatible || !caps.SupportsModelDiscovery {
			skipped = append(skipped, modelRefreshResult{
				Alias: model.Alias, Provider: model.ProviderName, ProviderModel: model.ProviderModel,
				Status: "skipped", Error: "provider does not support OpenAI-compatible capability discovery",
			})
			continue
		}
		groupsByName[model.ProviderName] = append(groupsByName[model.ProviderName], *model)
	}
	names := make([]string, 0, len(groupsByName))
	for name := range groupsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	groups := make([]providerRefreshGroup, 0, len(names))
	for _, name := range names {
		groups = append(groups, providerRefreshGroup{provider: providersByName[name], models: groupsByName[name]})
	}
	return groups, skipped, nil
}

func discoverProviderGroups(ctx context.Context, deps Dependencies, groups []providerRefreshGroup) []providerDiscoveryResult {
	if len(groups) == 0 {
		return nil
	}
	jobs := make(chan providerRefreshGroup, len(groups))
	results := make(chan providerDiscoveryResult, len(groups))
	for index := range groups {
		jobs <- groups[index]
	}
	close(jobs)
	workers := min(maxConcurrentProviderRefreshes, len(groups))
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for group := range jobs {
				discovery, err := discoverProviderModels(ctx, deps, group.provider)
				results <- providerDiscoveryResult{group: group, discovery: discovery, err: err}
			}
		}()
	}
	for range workers {
		<-done
	}
	close(results)
	discovered := make([]providerDiscoveryResult, 0, len(groups))
	for result := range results {
		discovered = append(discovered, result)
	}
	sort.Slice(discovered, func(i, j int) bool {
		return discovered[i].group.provider.Name < discovered[j].group.provider.Name
	})
	return discovered
}

func applyProviderDiscoveries(ctx context.Context, s *store.Store, discoveries []providerDiscoveryResult) []modelRefreshResult {
	var results []modelRefreshResult
	for providerIndex := range discoveries {
		providerResult := &discoveries[providerIndex]
		for modelIndex := range providerResult.group.models {
			model := &providerResult.group.models[modelIndex]
			result := modelRefreshResult{
				Alias: model.Alias, Provider: model.ProviderName, ProviderModel: model.ProviderModel,
				Warnings: append([]string(nil), providerResult.discovery.Warnings...),
			}
			if providerResult.err != nil {
				result.Status = "failed"
				result.Error = providerResult.err.Error()
				results = append(results, result)
				continue
			}
			discovered, ok := findDiscoveredModel(providerResult.discovery.Models, model.ProviderModel)
			if !ok {
				result.Status = "failed"
				result.Error = "exact provider model is absent from discovery"
				results = append(results, result)
				continue
			}
			if !discovered.Routable {
				result.Warnings = append(result.Warnings,
					"provider classified the model as non-routable ("+discovered.SkipReason+"); CCR will exclude this alias from routing")
			}
			snapshot := discovered.Capabilities
			merged, err := modelcap.MergeSnapshots(model.DiscoveredCapabilities, snapshot)
			if err != nil {
				result.Status = "failed"
				result.Error = "merging capability metadata: " + err.Error()
				results = append(results, result)
				continue
			}
			model.DiscoveredCapabilities = merged
			model.CapabilitiesRefreshedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := s.UpdateModel(ctx, *model); err != nil {
				result.Status = "failed"
				result.Error = err.Error()
				results = append(results, result)
				continue
			}
			result.Status = "refreshed"
			result.RefreshedAt = model.CapabilitiesRefreshedAt
			results = append(results, result)
		}
	}
	return results
}

func findDiscoveredModel(models []providers.DiscoveredModel, id string) (providers.DiscoveredModel, bool) {
	for index := range models {
		model := &models[index]
		if model.ID == id {
			return *model, true
		}
	}
	return providers.DiscoveredModel{}, false
}

func summarizeModelRefresh(results []modelRefreshResult) modelRefreshDocument {
	document := modelRefreshDocument{SchemaVersion: 1, Results: results}
	for index := range results {
		result := &results[index]
		switch result.Status {
		case "refreshed":
			document.Refreshed++
		case "skipped":
			document.Skipped++
		case "failed":
			document.Failed++
		}
	}
	return document
}

func writeModelRefreshSummary(out io.Writer, document modelRefreshDocument) {
	for index := range document.Results {
		result := &document.Results[index]
		fmt.Fprintf(out, "%s: %s provider=%s model=%s", result.Alias, result.Status, result.Provider, result.ProviderModel)
		if result.Error != "" {
			fmt.Fprintf(out, " reason=%s", result.Error)
		}
		fmt.Fprintln(out)
		for _, warning := range result.Warnings {
			fmt.Fprintf(out, "  warning: %s\n", warning)
		}
	}
	fmt.Fprintf(out, "Model capability refresh: refreshed=%d skipped=%d failed=%d\n",
		document.Refreshed, document.Skipped, document.Failed)
}
