package providers

import "fmt"

const (
	ProtocolAnthropicCompatible = "anthropic-compatible"
	ProtocolOpenAICompatible    = "openai-compatible"

	ModeFull     = "full"
	ModeDegraded = "degraded"
	ModeChatOnly = "chat-only"
)

type Capabilities struct {
	Protocol               string
	SupportsTools          bool
	SupportsStreaming      bool
	SupportsThinking       bool
	SupportsModelDiscovery bool
	SupportsCountTokens    bool
	Mode                   string
}

type Profile struct {
	Type           string
	Protocol       string
	DefaultBaseURL string
	RequiresAPIKey bool
	Capabilities   Capabilities
}

type Registry struct{}

func (Registry) ResolveType(name, explicitType, explicitProtocol string) (string, error) {
	providerType := explicitType
	if providerType == "" {
		providerType = name
	}
	if explicitType == "" && explicitProtocol != "" {
		if _, ok := (Registry{}).Profile(providerType); !ok {
			providerType = explicitProtocol
		}
	}
	if _, ok := (Registry{}).Profile(providerType); !ok {
		return "", fmt.Errorf("invalid provider type %q; expected %s", providerType, SupportedProviderTypes())
	}
	if explicitProtocol == "" {
		return providerType, nil
	}
	if err := ValidateProtocol(explicitProtocol); err != nil {
		return "", err
	}
	profile, _ := (Registry{}).Profile(providerType)
	if profile.Protocol != explicitProtocol {
		return "", fmt.Errorf("provider type %q uses protocol %q, not %q", providerType, profile.Protocol, explicitProtocol)
	}
	return providerType, nil
}

func (Registry) Profile(providerType string) (Profile, bool) {
	switch providerType {
	case "anthropic":
		return anthropicProfile(providerType, "https://api.anthropic.com", true), true
	case "zai":
		caps := anthropicCapabilities(ModeFull)
		caps.SupportsModelDiscovery = false
		return Profile{
			Type:           providerType,
			Protocol:       ProtocolAnthropicCompatible,
			DefaultBaseURL: "https://api.z.ai/api/anthropic",
			RequiresAPIKey: true,
			Capabilities:   caps,
		}, true
	case "anthropic-compatible":
		caps := anthropicCapabilities(ModeDegraded)
		caps.SupportsModelDiscovery = false
		caps.SupportsCountTokens = false
		return Profile{
			Type:           providerType,
			Protocol:       ProtocolAnthropicCompatible,
			RequiresAPIKey: true,
			Capabilities:   caps,
		}, true
	case "openrouter":
		return openAIProfile(providerType, "https://openrouter.ai/api", true), true
	case "zai-openai":
		return openAIProfile(providerType, "https://api.z.ai/api/coding/paas/v4", true), true
	case "litellm":
		return openAIProfile(providerType, "", false), true
	case "local":
		return openAIProfile(providerType, "", false), true
	case "openai-compatible":
		return openAIProfile(providerType, "", true), true
	default:
		return Profile{}, false
	}
}

func SupportedProviderTypes() string {
	return "anthropic, anthropic-compatible, zai, openrouter, zai-openai, litellm, local, or openai-compatible"
}

func ValidateProtocol(protocol string) error {
	switch protocol {
	case ProtocolAnthropicCompatible, ProtocolOpenAICompatible:
		return nil
	default:
		return fmt.Errorf("invalid provider protocol %q; expected %s or %s", protocol, ProtocolAnthropicCompatible, ProtocolOpenAICompatible)
	}
}

func DefaultCapabilities(providerType string) Capabilities {
	profile, ok := (Registry{}).Profile(providerType)
	if !ok {
		return Capabilities{Mode: ModeDegraded}
	}
	return profile.Capabilities
}

func NormalizeCapabilities(providerType string, caps Capabilities) Capabilities {
	if caps.Protocol == "" && caps.Mode == "" && !caps.SupportsTools && !caps.SupportsStreaming &&
		!caps.SupportsThinking && !caps.SupportsModelDiscovery && !caps.SupportsCountTokens {
		return DefaultCapabilities(providerType)
	}
	defaults := DefaultCapabilities(providerType)
	if caps.Protocol == "" {
		caps.Protocol = defaults.Protocol
	}
	if caps.Mode == "" {
		caps.Mode = defaults.Mode
	}
	return caps
}

func SupportsOpenAICompatibleRouting(providerType string) bool {
	return DefaultCapabilities(providerType).Protocol == ProtocolOpenAICompatible
}

func SupportsOpenAIModelDiscovery(providerType string) bool {
	caps := DefaultCapabilities(providerType)
	return caps.Protocol == ProtocolOpenAICompatible && caps.SupportsModelDiscovery
}

func anthropicProfile(providerType, defaultBaseURL string, requiresAPIKey bool) Profile {
	return Profile{
		Type:           providerType,
		Protocol:       ProtocolAnthropicCompatible,
		DefaultBaseURL: defaultBaseURL,
		RequiresAPIKey: requiresAPIKey,
		Capabilities:   anthropicCapabilities(ModeFull),
	}
}

func openAIProfile(providerType, defaultBaseURL string, requiresAPIKey bool) Profile {
	return Profile{
		Type:           providerType,
		Protocol:       ProtocolOpenAICompatible,
		DefaultBaseURL: defaultBaseURL,
		RequiresAPIKey: requiresAPIKey,
		Capabilities: Capabilities{
			Protocol:               ProtocolOpenAICompatible,
			SupportsTools:          true,
			SupportsStreaming:      true,
			SupportsThinking:       true,
			SupportsModelDiscovery: true,
			SupportsCountTokens:    false,
			Mode:                   ModeDegraded,
		},
	}
}

func anthropicCapabilities(mode string) Capabilities {
	return Capabilities{
		Protocol:               ProtocolAnthropicCompatible,
		SupportsTools:          true,
		SupportsStreaming:      true,
		SupportsThinking:       true,
		SupportsModelDiscovery: true,
		SupportsCountTokens:    true,
		Mode:                   mode,
	}
}
