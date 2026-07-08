package cli

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

func resolveProviderSecretPlan(deps Dependencies, name, providerType, apiKeyEnv, apiKeyValue string, apiKeyStdin, noAPIKey bool) (secretPlan, error) {
	if err := validateProviderAuthSources(apiKeyEnv != "", strings.TrimSpace(apiKeyValue) != "", apiKeyStdin, noAPIKey); err != nil {
		return secretPlan{}, err
	}
	if apiKeyEnv != "" {
		if err := validateEnvName(apiKeyEnv); err != nil {
			return secretPlan{}, err
		}
		return secretPlan{ref: secret.EnvRef(apiKeyEnv)}, nil
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
	if noAPIKey || providerType == "local" || providerType == "litellm" {
		return secretPlan{}, nil
	}
	return secretPlan{}, fmt.Errorf("API key required for provider type %q; use --api-key-env <ENV>, --api-key-stdin, or --no-api-key if this endpoint is intentionally unauthenticated", providerType)
}

func validateProviderAuthSourceFlags(cfg providerAddConfig) error {
	return validateProviderAuthSources(cfg.apiKeyEnv != "", strings.TrimSpace(cfg.apiKeyValue) != "", cfg.apiKeyStdin, cfg.noAPIKey)
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
	providerType := explicit
	if providerType == "" {
		providerType = name
	}
	switch providerType {
	case "anthropic", "openrouter", "litellm", "local":
		return providerType, nil
	default:
		return "", fmt.Errorf("invalid provider type %q; expected anthropic, openrouter, litellm, or local", providerType)
	}
}

func resolveBaseURL(providerType, explicit string) (string, error) {
	baseURL := explicit
	if baseURL == "" {
		switch providerType {
		case "anthropic":
			baseURL = "https://api.anthropic.com"
		case "openrouter":
			baseURL = "https://openrouter.ai/api"
		case "litellm", "local":
			return "", fmt.Errorf("--base-url is required for provider type %q", providerType)
		default:
			return "", fmt.Errorf("unsupported provider type %q", providerType)
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
