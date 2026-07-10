package cli

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func resolveProviderSecretPlan(deps Dependencies, name, providerType, apiKeyEnv, apiKeyFile, apiKeyValue string, apiKeyStdin, noAPIKey bool) (secretPlan, error) {
	if err := validateProviderAuthSources(apiKeyEnv != "", strings.TrimSpace(apiKeyFile) != "", strings.TrimSpace(apiKeyValue) != "", apiKeyStdin, noAPIKey); err != nil {
		return secretPlan{}, err
	}
	if apiKeyEnv != "" {
		if err := validateEnvName(apiKeyEnv); err != nil {
			return secretPlan{}, err
		}
		return secretPlan{ref: secret.EnvRef(apiKeyEnv)}, nil
	}
	if strings.TrimSpace(apiKeyFile) != "" {
		ref, err := secret.FileRefFromPath(apiKeyFile)
		if err != nil {
			return secretPlan{}, fmt.Errorf("--api-key-file: %w", err)
		}
		return secretPlan{ref: ref}, nil
	}
	if strings.TrimSpace(apiKeyValue) != "" {
		return secretPlan{ref: secret.KeyringRef(name), value: strings.TrimSpace(apiKeyValue), store: true}, nil
	}
	if apiKeyStdin {
		raw, err := io.ReadAll(deps.In)
		if err != nil {
			return secretPlan{}, fmt.Errorf("reading API key from stdin: %w", err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return secretPlan{}, fmt.Errorf("--api-key-stdin received an empty API key")
		}
		return secretPlan{ref: secret.KeyringRef(name), value: value, store: true}, nil
	}
	if noAPIKey || !providerTypeRequiresAPIKey(providerType) {
		return secretPlan{}, nil
	}
	return secretPlan{}, fmt.Errorf("API key required for provider type %q; use --api-key-env <ENV>, --api-key-file <PATH>, --api-key-stdin, or --no-api-key if this endpoint is intentionally unauthenticated", providerType)
}

func providerTypeRequiresAPIKey(providerType string) bool {
	profile, ok := (providers.Registry{}).Profile(providerType)
	return !ok || profile.RequiresAPIKey
}

func validateProviderAuthSourceFlags(cfg providerAddConfig) error {
	return validateProviderAuthSources(cfg.apiKeyEnv != "", strings.TrimSpace(cfg.apiKeyFile) != "", strings.TrimSpace(cfg.apiKeyValue) != "", cfg.apiKeyStdin, cfg.noAPIKey)
}

func validateProviderAuthSources(sources ...bool) error {
	selected := 0
	for _, enabled := range sources {
		if enabled {
			selected++
		}
	}
	if selected > 1 {
		return fmt.Errorf("choose only one API key source")
	}
	return nil
}

func resolveProviderType(name, explicit string) (string, error) {
	return resolveProviderTypeWithProtocol(name, explicit, "")
}

func resolveProviderTypeWithProtocol(name, explicit, protocol string) (string, error) {
	return (providers.Registry{}).ResolveType(name, explicit, protocol)
}

func resolveBaseURL(providerType, explicit string) (string, error) {
	baseURL := explicit
	if baseURL == "" {
		profile, ok := (providers.Registry{}).Profile(providerType)
		if !ok {
			return "", fmt.Errorf("unsupported provider type %q", providerType)
		}
		baseURL = profile.DefaultBaseURL
		if baseURL == "" {
			return "", fmt.Errorf("--base-url is required for provider type %q", providerType)
		}
	}
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid --base-url %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid --base-url %q: scheme must be http or https", baseURL)
	}
	return baseURL, nil
}

func resolveProviderCapabilities(providerType string) providers.Capabilities {
	return providers.DefaultCapabilities(providerType)
}

func validateName(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	matched, err := regexp.MatchString(`^[a-z][a-z0-9_-]{1,63}$`, value)
	if err != nil {
		return fmt.Errorf("validating %s: %w", label, err)
	}
	if !matched {
		return fmt.Errorf("invalid %s %q: use 2-64 chars, lowercase letters, digits, underscore, or hyphen, starting with a letter", label, value)
	}
	return nil
}

func validateEnvName(value string) error {
	matched, err := regexp.MatchString(`^[A-Z_][A-Z0-9_]*$`, value)
	if err != nil {
		return fmt.Errorf("validating environment variable name: %w", err)
	}
	if !matched {
		return fmt.Errorf("invalid environment variable name %q", value)
	}
	return nil
}

func validateCompatibilityStatus(value string) error {
	switch value {
	case "full", "degraded", "chat-only", "blocked":
		return nil
	default:
		return fmt.Errorf("invalid compatibility status %q; expected full, degraded, chat-only, or blocked", value)
	}
}

func validateProviderMode(value string) error {
	switch value {
	case "", providers.ModeFull, providers.ModeDegraded, providers.ModeChatOnly:
		return nil
	default:
		return fmt.Errorf("invalid provider mode %q; expected full, degraded, or chat-only", value)
	}
}

func providerWithCapabilities(name, providerType, baseURL, secretRef, mode string) store.Provider {
	caps := resolveProviderCapabilities(providerType)
	if mode != "" {
		caps.Mode = mode
		if mode == providers.ModeChatOnly {
			caps.SupportsTools = false
		}
	}
	return store.Provider{
		Name:                   name,
		Type:                   providerType,
		BaseURL:                baseURL,
		SecretRef:              secretRef,
		Protocol:               caps.Protocol,
		SupportsTools:          caps.SupportsTools,
		SupportsStreaming:      caps.SupportsStreaming,
		SupportsThinking:       caps.SupportsThinking,
		SupportsModelDiscovery: caps.SupportsModelDiscovery,
		SupportsCountTokens:    caps.SupportsCountTokens,
		Mode:                   caps.Mode,
	}
}

func providerCapabilitySummary(provider store.Provider) string {
	caps := effectiveProviderCapabilities(provider)
	enabled := make([]string, 0, 6)
	if caps.SupportsTools {
		enabled = append(enabled, "tools")
	}
	if caps.SupportsStreaming {
		enabled = append(enabled, "streaming")
	}
	if caps.SupportsThinking {
		enabled = append(enabled, "thinking")
	}
	if caps.SupportsModelDiscovery {
		enabled = append(enabled, "models")
	}
	enabled = append(enabled, "token-count="+providerTokenCountMode(provider))
	return strings.Join(enabled, ",")
}

func providerTokenCountMode(provider store.Provider) string {
	caps := effectiveProviderCapabilities(provider)
	if caps.SupportsCountTokens {
		return "provider"
	}
	return "estimated"
}

func effectiveProviderCapabilities(provider store.Provider) providers.Capabilities {
	return providers.NormalizeCapabilities(provider.Type, providers.Capabilities{
		Protocol:               provider.Protocol,
		SupportsTools:          provider.SupportsTools,
		SupportsStreaming:      provider.SupportsStreaming,
		SupportsThinking:       provider.SupportsThinking,
		SupportsModelDiscovery: provider.SupportsModelDiscovery,
		SupportsCountTokens:    provider.SupportsCountTokens,
		Mode:                   provider.Mode,
	})
}
