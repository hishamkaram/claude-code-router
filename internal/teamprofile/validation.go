package teamprofile

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
)

const (
	namePattern    = `^[a-z][a-z0-9_-]{1,63}$`
	envNamePattern = `^[A-Z_][A-Z0-9_]*$`
)

func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d; expected %d", m.SchemaVersion, SchemaVersion)
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
	providerNames := make(map[string]struct{}, len(m.Providers))
	for index, provider := range m.Providers {
		if err := validateProvider(provider); err != nil {
			return fmt.Errorf("providers[%d]: %w", index, err)
		}
		if _, duplicate := providerNames[provider.Name]; duplicate {
			return fmt.Errorf("providers[%d]: duplicate provider name %q", index, provider.Name)
		}
		providerNames[provider.Name] = struct{}{}
	}
	modelAliases := make(map[string]struct{}, len(m.Models))
	for index, model := range m.Models {
		if err := validateModel(model, providerNames); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		if _, duplicate := modelAliases[model.Alias]; duplicate {
			return fmt.Errorf("models[%d]: duplicate model alias %q", index, model.Alias)
		}
		modelAliases[model.Alias] = struct{}{}
	}
	return nil
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

func validateModel(model Model, providerNames map[string]struct{}) error {
	if err := validateName("model alias", model.Alias); err != nil {
		return err
	}
	if _, ok := providerNames[model.Provider]; !ok {
		return fmt.Errorf("model %q references unknown provider %q", model.Alias, model.Provider)
	}
	if strings.TrimSpace(model.ProviderModel) == "" || strings.TrimSpace(model.ProviderModel) != model.ProviderModel {
		return fmt.Errorf("provider_model for %q must be non-empty without surrounding whitespace", model.Alias)
	}
	if len(model.ProviderModel) > 512 {
		return fmt.Errorf("provider_model for %q exceeds 512 characters", model.Alias)
	}
	switch model.Compatibility {
	case providers.ModeFull, providers.ModeDegraded, providers.ModeChatOnly, "blocked":
		return nil
	default:
		return fmt.Errorf("invalid compatibility %q", model.Compatibility)
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
