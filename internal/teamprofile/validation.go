package teamprofile

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
)

const (
	namePattern    = `^[a-z][a-z0-9_-]{1,63}$`
	envNamePattern = `^[A-Z_][A-Z0-9_]*$`
)

func (m Manifest) Validate() error {
	if err := validateManifestHeader(m); err != nil {
		return err
	}
	providerTypes, err := validateManifestProviders(m.Providers)
	if err != nil {
		return err
	}
	return validateManifestModels(m.SchemaVersion, m.Models, providerTypes)
}

func validateManifestHeader(m Manifest) error {
	if m.SchemaVersion < MinSchemaVersion || m.SchemaVersion > SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d; supported versions are %d through %d", m.SchemaVersion, MinSchemaVersion, SchemaVersion)
	}
	if m.Kind != Kind {
		return fmt.Errorf("invalid kind %q; expected %q", m.Kind, Kind)
	}
	if len(m.Providers) > MaxProviders {
		return fmt.Errorf("provider count %d exceeds limit %d", len(m.Providers), MaxProviders)
	}
	if len(m.Models) > MaxModels {
		return fmt.Errorf("model count %d exceeds limit %d", len(m.Models), MaxModels)
	}
	return nil
}

func validateManifestProviders(configuredProviders []Provider) (map[string]string, error) {
	providerTypes := make(map[string]string, len(configuredProviders))
	for index := range configuredProviders {
		provider := &configuredProviders[index]
		if err := validateProvider(*provider); err != nil {
			return nil, fmt.Errorf("providers[%d]: %w", index, err)
		}
		if _, duplicate := providerTypes[provider.Name]; duplicate {
			return nil, fmt.Errorf("providers[%d]: duplicate provider name %q", index, provider.Name)
		}
		providerTypes[provider.Name] = provider.Type
	}
	return providerTypes, nil
}

func validateManifestModels(schemaVersion int, models []Model, providerTypes map[string]string) error {
	modelAliases := make(map[string]struct{}, len(models))
	for index := range models {
		model := &models[index]
		if schemaVersion == 1 && modelUsesCapabilityMetadata(*model) {
			return fmt.Errorf("models[%d]: capability metadata requires schema_version 2", index)
		}
		if err := validateModel(*model, providerTypes); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		if _, duplicate := modelAliases[model.Alias]; duplicate {
			return fmt.Errorf("models[%d]: duplicate model alias %q", index, model.Alias)
		}
		modelAliases[model.Alias] = struct{}{}
	}
	return nil
}

func modelUsesCapabilityMetadata(model Model) bool {
	return model.DiscoveredCapabilities != nil || model.CapabilityOverrides != nil ||
		model.CapabilitiesRefreshedAt != ""
}

func validateProvider(provider Provider) error {
	if err := validateName("provider name", provider.Name); err != nil {
		return err
	}
	profile, ok := (providers.Registry{}).Profile(provider.Type)
	if !ok {
		return fmt.Errorf("unsupported provider type %q", provider.Type)
	}
	if profile.RequiresAPIKey && !provider.Credential.Required {
		return fmt.Errorf("provider type %q requires credential.required=true", provider.Type)
	}
	if err := providers.ValidateProtocol(provider.Protocol); err != nil {
		return err
	}
	if provider.Protocol != profile.Protocol {
		return fmt.Errorf("provider type %q requires protocol %q", provider.Type, profile.Protocol)
	}
	if err := validateBaseURL(provider.BaseURL); err != nil {
		return err
	}
	if err := validateMode(provider.Mode); err != nil {
		return err
	}
	if provider.Mode == providers.ModeChatOnly && provider.Capabilities.Tools {
		return fmt.Errorf("chat-only provider mode cannot declare tools capability")
	}
	if provider.Credential.EnvironmentVariable != "" {
		if !provider.Credential.Required {
			return fmt.Errorf("credential environment_variable requires required=true")
		}
		if err := validateEnvName(provider.Credential.EnvironmentVariable); err != nil {
			return err
		}
	}
	return nil
}

func validateModel(model Model, providerTypes map[string]string) error {
	providerType, err := validateModelIdentity(model, providerTypes)
	if err != nil {
		return err
	}
	if providers.IsProviderControlModel(providerType, model.ProviderModel) {
		return fmt.Errorf("provider_model for %q is a LiteLLM control model and cannot be routed", model.Alias)
	}
	if err := validateDiscoveredCapabilities(model.Alias, model.DiscoveredCapabilities); err != nil {
		return err
	}
	if err := validateCapabilityOverrides(model.Alias, model.CapabilityOverrides); err != nil {
		return err
	}
	if err := validateCapabilitiesRefreshedAt(model.Alias, model.CapabilitiesRefreshedAt); err != nil {
		return err
	}
	return validateCompatibility(model.Compatibility)
}

func validateModelIdentity(model Model, providerTypes map[string]string) (string, error) {
	if err := validateName("model alias", model.Alias); err != nil {
		return "", err
	}
	providerType, ok := providerTypes[model.Provider]
	if !ok {
		return "", fmt.Errorf("model %q references unknown provider %q", model.Alias, model.Provider)
	}
	if strings.TrimSpace(model.ProviderModel) == "" || strings.TrimSpace(model.ProviderModel) != model.ProviderModel {
		return "", fmt.Errorf("provider_model for %q must be non-empty without surrounding whitespace", model.Alias)
	}
	if len(model.ProviderModel) > 512 {
		return "", fmt.Errorf("provider_model for %q exceeds 512 characters", model.Alias)
	}
	return providerType, nil
}

func validateDiscoveredCapabilities(alias string, capabilities *modelcap.Snapshot) error {
	if capabilities == nil {
		return nil
	}
	normalized, err := modelcap.NormalizeSnapshot(*capabilities)
	if err != nil {
		return fmt.Errorf("invalid discovered_capabilities for %q: %w", alias, err)
	}
	for field, source := range normalized.Sources {
		if source != modelcap.SourceOpenAIModels && source != modelcap.SourceLiteLLMInfo &&
			source != modelcap.SourceOpenAIAdapter {
			return fmt.Errorf("invalid discovered_capabilities source %q for field %q", source, field)
		}
	}
	return nil
}

func validateCapabilityOverrides(alias string, overrides *modelcap.Values) error {
	if overrides == nil {
		return nil
	}
	if _, err := modelcap.NormalizeValues(*overrides); err != nil {
		return fmt.Errorf("invalid capability_overrides for %q: %w", alias, err)
	}
	return nil
}

func validateCapabilitiesRefreshedAt(alias, refreshedAt string) error {
	if refreshedAt == "" {
		return nil
	}
	if strings.TrimSpace(refreshedAt) != refreshedAt {
		return fmt.Errorf("capabilities_refreshed_at for %q has surrounding whitespace", alias)
	}
	if _, err := time.Parse(time.RFC3339Nano, refreshedAt); err != nil {
		return fmt.Errorf("invalid capabilities_refreshed_at for %q: %w", alias, err)
	}
	return nil
}

func validateCompatibility(compatibility string) error {
	switch compatibility {
	case providers.ModeFull, providers.ModeDegraded, providers.ModeChatOnly, "blocked":
		return nil
	default:
		return fmt.Errorf("invalid compatibility %q", compatibility)
	}
}

func validateName(label, value string) error {
	matched, err := regexp.MatchString(namePattern, value)
	if err != nil {
		return fmt.Errorf("validating %s: %w", label, err)
	}
	if !matched {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	return nil
}

func validateEnvName(value string) error {
	matched, err := regexp.MatchString(envNamePattern, value)
	if err != nil {
		return fmt.Errorf("validating environment variable name: %w", err)
	}
	if !matched {
		return fmt.Errorf("invalid environment variable name %q", value)
	}
	return nil
}

func validateBaseURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid base_url %q", value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid base_url %q: scheme must be http or https", value)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("base_url must not contain credentials, query parameters, or fragments")
	}
	return nil
}

func validateMode(value string) error {
	switch value {
	case providers.ModeFull, providers.ModeDegraded, providers.ModeChatOnly:
		return nil
	default:
		return fmt.Errorf("invalid provider mode %q", value)
	}
}
