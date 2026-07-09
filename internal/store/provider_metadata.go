package store

func providerWithMetadataDefaults(provider Provider) Provider {
	if provider.Protocol != "" || provider.Mode != "" || provider.SupportsTools ||
		provider.SupportsStreaming || provider.SupportsThinking ||
		provider.SupportsModelDiscovery || provider.SupportsCountTokens {
		return provider
	}
	switch provider.Type {
	case "anthropic":
		provider.Protocol = "anthropic-compatible"
		provider.SupportsTools = true
		provider.SupportsStreaming = true
		provider.SupportsThinking = true
		provider.SupportsModelDiscovery = true
		provider.SupportsCountTokens = true
		provider.Mode = "full"
	case "zai":
		provider.Protocol = "anthropic-compatible"
		provider.SupportsTools = true
		provider.SupportsStreaming = true
		provider.SupportsThinking = true
		provider.SupportsCountTokens = true
		provider.Mode = "full"
	case "anthropic-compatible":
		provider.Protocol = "anthropic-compatible"
		provider.SupportsTools = true
		provider.SupportsStreaming = true
		provider.SupportsThinking = true
		provider.Mode = "degraded"
	case "litellm", "local", "openrouter", "zai-openai", "openai-compatible":
		provider.Protocol = "openai-compatible"
		provider.SupportsTools = true
		provider.SupportsStreaming = true
		provider.SupportsThinking = true
		provider.SupportsModelDiscovery = true
		provider.Mode = "degraded"
	default:
		provider.Mode = "degraded"
	}
	return provider
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
