package teamprofile

import (
	"fmt"
	"sort"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type ImportPlan struct {
	Providers         []store.Provider
	Models            []store.Model
	UnboundCredential []string
}

func (m Manifest) PlanImport(bindings map[string]string) (ImportPlan, error) {
	if err := m.Validate(); err != nil {
		return ImportPlan{}, err
	}
	providerNames := make(map[string]struct{}, len(m.Providers))
	for _, provider := range m.Providers {
		providerNames[provider.Name] = struct{}{}
	}
	for providerName, envName := range bindings {
		if _, ok := providerNames[providerName]; !ok {
			return ImportPlan{}, fmt.Errorf("credential binding references unknown provider %q", providerName)
		}
		if err := validateEnvName(envName); err != nil {
			return ImportPlan{}, fmt.Errorf("credential binding for %q: %w", providerName, err)
		}
	}
	plan := ImportPlan{
		Providers: make([]store.Provider, 0, len(m.Providers)),
		Models:    make([]store.Model, 0, len(m.Models)),
	}
	for _, provider := range m.Providers {
		envName := provider.Credential.EnvironmentVariable
		if binding, ok := bindings[provider.Name]; ok {
			envName = binding
		}
		secretRef := ""
		if envName != "" {
			secretRef = secret.EnvRef(envName)
		} else if provider.Credential.Required {
			plan.UnboundCredential = append(plan.UnboundCredential, provider.Name)
		}
		plan.Providers = append(plan.Providers, store.Provider{
			Name:                   provider.Name,
			Type:                   provider.Type,
			BaseURL:                provider.BaseURL,
			SecretRef:              secretRef,
			Protocol:               provider.Protocol,
			SupportsTools:          provider.Capabilities.Tools,
			SupportsStreaming:      provider.Capabilities.Streaming,
			SupportsThinking:       provider.Capabilities.Thinking,
			SupportsModelDiscovery: provider.Capabilities.ModelDiscovery,
			SupportsCountTokens:    provider.Capabilities.CountTokens,
			SupportsResponses:      provider.Capabilities.Responses,
			Mode:                   provider.Mode,
		})
	}
	for _, model := range m.Models {
		storedModel := store.Model{
			Alias:                   model.Alias,
			ProviderName:            model.Provider,
			ProviderModel:           model.ProviderModel,
			Status:                  model.Compatibility,
			CapabilitiesRefreshedAt: model.CapabilitiesRefreshedAt,
		}
		if model.DiscoveredCapabilities != nil {
			storedModel.DiscoveredCapabilities = *model.DiscoveredCapabilities
		}
		if model.CapabilityOverrides != nil {
			storedModel.CapabilityOverrides = *model.CapabilityOverrides
		}
		plan.Models = append(plan.Models, storedModel)
	}
	sort.Strings(plan.UnboundCredential)
	return plan, nil
}
