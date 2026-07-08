package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultDiscoveryTimeout = 10 * time.Second

type DiscoveryConfig struct {
	Type    string
	BaseURL string
	APIKey  string
}

type Discoverer struct {
	HTTPClient *http.Client
	Timeout    time.Duration
}

func (d Discoverer) DiscoverOpenAICompatibleModels(ctx context.Context, cfg DiscoveryConfig) ([]string, error) {
	if !SupportsOpenAIModelDiscovery(cfg.Type) {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: provider type %q does not support OpenAI-compatible model discovery", cfg.Type)
	}
	endpoint, err := ModelsEndpoint(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	timeout := d.Timeout
	if timeout == 0 {
		timeout = DefaultDiscoveryTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(cfg.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	client := d.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: requesting %s: %w", endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, discoveryHTTPError(resp.StatusCode)
	}

	models, err := parseOpenAIModels(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: parsing response: %w", err)
	}
	return models, nil
}

func ModelsEndpoint(baseURL string) (string, error) {
	return endpointWithPath(baseURL, "models")
}

func ChatCompletionsEndpoint(baseURL string) (string, error) {
	return endpointWithPath(baseURL, "chat/completions")
}

func endpointWithPath(baseURL, resource string) (string, error) {
	cleanBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.ParseRequestURI(cleanBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("providers.endpointWithPath: invalid base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("providers.endpointWithPath: invalid base URL %q: scheme must be http or https", baseURL)
	}

	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = path + "/" + resource
	} else {
		parsed.Path = path + "/v1/" + resource
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func SupportsOpenAICompatibleRouting(providerType string) bool {
	switch providerType {
	case "litellm", "local", "openrouter":
		return true
	default:
		return false
	}
}

func SupportsOpenAIModelDiscovery(providerType string) bool {
	return SupportsOpenAICompatibleRouting(providerType)
}

func discoveryHTTPError(statusCode int) error {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "unknown status"
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return fmt.Errorf("providers.DiscoverOpenAICompatibleModels: authentication failed (HTTP %d %s)", statusCode, statusText)
	}
	return fmt.Errorf("providers.DiscoverOpenAICompatibleModels: model discovery failed (HTTP %d %s)", statusCode, statusText)
}

func parseOpenAIModels(body io.Reader) ([]string, error) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	return models, nil
}
