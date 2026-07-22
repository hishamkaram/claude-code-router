package cli

import (
	"fmt"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func liveProbeSkipReason(model store.Model, provider store.Provider) (string, error) {
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return "provider control model is excluded from routing and live probes", nil
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return "", fmt.Errorf("computing capabilities for model alias %q: %w", model.Alias, err)
	}
	if !modelcap.IsRoutableKind(effective.Values.Kind) {
		return "non-chat model kind " + effective.Values.Kind + " is excluded from routing and live probes", nil
	}
	providerCapabilities := effectiveProviderCapabilities(provider)
	if cliModelUsesResponsesAPI(effective.Values) && !cliSupportsResponsesRoute(providerCapabilities, effective.Values) {
		return "OpenAI Responses API route is excluded because provider or model capabilities do not make it routable", nil
	}
	return "", nil
}

func cliModelUsesResponsesAPI(capabilities modelcap.Values) bool {
	return capabilities.Kind == modelcap.KindResponses || (capabilities.SupportsResponses != nil && *capabilities.SupportsResponses)
}

func cliSupportsResponsesRoute(provider providers.Capabilities, model modelcap.Values) bool {
	return provider.Protocol == providers.ProtocolOpenAICompatible &&
		provider.SupportsResponses &&
		!cliCapabilityExplicitlyFalse(model.SupportsResponses)
}

func cliCapabilityExplicitlyFalse(value *bool) bool {
	return value != nil && !*value
}
