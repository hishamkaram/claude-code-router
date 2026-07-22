package store

import "github.com/hishamkaram/claude-code-router/internal/providers"

func providerWithMetadataDefaults(provider Provider) Provider {
	supportsResponses := provider.SupportsResponses
	if providerHasMetadata(provider) {
		return provider
	}
	caps := providers.DefaultCapabilities(provider.Type)
	provider.Protocol = caps.Protocol
	provider.SupportsTools = caps.SupportsTools
	provider.SupportsStreaming = caps.SupportsStreaming
	provider.SupportsThinking = caps.SupportsThinking
	provider.SupportsModelDiscovery = caps.SupportsModelDiscovery
	provider.SupportsCountTokens = caps.SupportsCountTokens
	provider.Mode = caps.Mode
	provider.SupportsResponses = provider.SupportsResponses || supportsResponses
	return provider
}

func providerHasMetadata(provider Provider) bool {
	return provider.Protocol != "" || provider.Mode != "" || provider.SupportsTools ||
		provider.SupportsStreaming || provider.SupportsThinking ||
		provider.SupportsModelDiscovery || provider.SupportsCountTokens
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intToBool(value int) bool {
	return value == 1
}
