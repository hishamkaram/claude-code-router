package cli

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/huh"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type providerProfileChoice struct {
	Type  string
	Label string
}

func providerProfileChoices() []providerProfileChoice {
	return []providerProfileChoice{
		{Type: "anthropic", Label: "Anthropic - anthropic-compatible, manual models, full mode, default base URL"},
		{Type: "zai", Label: "Z.AI Anthropic-compatible - anthropic-compatible, manual models, full mode, default base URL"},
		{Type: "zai-openai", Label: "Z.AI OpenAI-compatible - openai-compatible, searchable discovery, degraded mode, default base URL"},
		{Type: "openrouter", Label: "OpenRouter - openai-compatible, searchable discovery, degraded mode, default base URL"},
		{Type: "litellm", Label: "LiteLLM - openai-compatible, searchable discovery, degraded mode, custom base URL required"},
		{Type: "local", Label: "Local OpenAI-compatible - openai-compatible, searchable discovery, degraded mode, custom base URL required"},
		{Type: "anthropic-compatible", Label: "Generic Anthropic-compatible - anthropic-compatible, manual models, degraded mode, custom base URL required"},
		{Type: "openai-compatible", Label: "Generic OpenAI-compatible - openai-compatible, searchable discovery, degraded mode, custom base URL required"},
	}
}

func providerProfileOptions() []huh.Option[string] {
	choices := providerProfileChoices()
	options := make([]huh.Option[string], 0, len(choices))
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice.Label, choice.Type))
	}
	return options
}

func providerTypeChoices() map[string]string {
	choices := providerProfileChoices()
	values := make(map[string]string, len(choices))
	for index, choice := range choices {
		values[strconv.Itoa(index+1)] = choice.Type
	}
	return values
}

func providerProfilePromptDescription() string {
	return "Filter by provider, protocol, discovery support, compatibility mode, or base URL requirement."
}

func supportsInteractiveModelDiscovery(provider store.Provider) bool {
	caps := effectiveProviderCapabilities(provider)
	return caps.Protocol == providers.ProtocolOpenAICompatible && caps.SupportsModelDiscovery
}

func providerDiscoveryUnavailableSummary(provider store.Provider) string {
	caps := effectiveProviderCapabilities(provider)
	return fmt.Sprintf("Provider %q uses protocol=%s mode=%s; searchable OpenAI-compatible discovery is unavailable.", provider.Name, caps.Protocol, caps.Mode)
}
