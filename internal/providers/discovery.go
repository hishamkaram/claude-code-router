package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

const DefaultDiscoveryTimeout = 10 * time.Second

type DiscoveryConfig struct {
	Type    string
	BaseURL string
	APIKey  string
}

type DiscoveredModel struct {
	ID                         string
	DisplayName                string
	Routable                   bool
	SkipReason                 string
	Capabilities               modelcap.Snapshot
	CapabilityMetadataComplete bool
}

type DiscoveryResult struct {
	Models           []DiscoveredModel
	Warnings         []string
	MetadataComplete bool
}

// DiscoveryHTTPError reports a failed model-list request without retaining the
// provider response body.
type DiscoveryHTTPError struct {
	StatusCode     int
	Authentication bool
}

func (err *DiscoveryHTTPError) Error() string {
	statusText := http.StatusText(err.StatusCode)
	if statusText == "" {
		statusText = "unknown status"
	}
	if err.Authentication {
		return fmt.Sprintf("providers.DiscoverOpenAICompatibleModels: authentication failed (HTTP %d %s)", err.StatusCode, statusText)
	}
	return fmt.Sprintf("providers.DiscoverOpenAICompatibleModels: model discovery failed (HTTP %d %s)", err.StatusCode, statusText)
}

func (result DiscoveryResult) RoutableModels() []DiscoveredModel {
	models := make([]DiscoveredModel, 0, len(result.Models))
	for index := range result.Models {
		model := result.Models[index]
		if model.Routable {
			models = append(models, model)
		}
	}
	return models
}

func (result DiscoveryResult) RoutableIDs() []string {
	models := result.RoutableModels()
	ids := make([]string, 0, len(models))
	for index := range models {
		ids = append(ids, models[index].ID)
	}
	return ids
}

func (result DiscoveryResult) HasRoutableID(id string) bool {
	for index := range result.Models {
		model := &result.Models[index]
		if model.Routable && model.ID == id {
			return true
		}
	}
	return false
}

func (result DiscoveryResult) SkippedCount() int {
	return len(result.Models) - len(result.RoutableModels())
}

type Discoverer struct {
	HTTPClient *http.Client
	Timeout    time.Duration
}

func (d Discoverer) DiscoverOpenAICompatibleModels(ctx context.Context, cfg DiscoveryConfig) (DiscoveryResult, error) {
	if !SupportsOpenAIModelDiscovery(cfg.Type) {
		return DiscoveryResult{}, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: provider type %q does not support OpenAI-compatible model discovery", cfg.Type)
	}
	endpoint, err := ModelsEndpoint(cfg.BaseURL)
	if err != nil {
		return DiscoveryResult{}, err
	}

	timeout := d.Timeout
	if timeout == 0 {
		timeout = DefaultDiscoveryTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	models, err := d.fetchOpenAIModels(requestCtx, endpoint, cfg.APIKey)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{Models: models, MetadataComplete: true}
	for index := range result.Models {
		result.Models[index].CapabilityMetadataComplete = cfg.Type != "litellm"
	}
	if cfg.Type == "litellm" {
		metadataEndpoint, endpointErr := LiteLLMModelInfoEndpoint(cfg.BaseURL)
		if endpointErr != nil {
			return DiscoveryResult{}, endpointErr
		}
		metadata, metadataErr := d.fetchLiteLLMModelInfo(requestCtx, metadataEndpoint, cfg.APIKey)
		if metadataErr != nil {
			result.MetadataComplete = false
			result.Warnings = append(result.Warnings, "LiteLLM capability metadata unavailable: "+metadataErr.Error())
		} else {
			result.Models, err = mergeDiscoveredModels(result.Models, metadata)
			if err != nil {
				return DiscoveryResult{}, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: merging capability metadata: %w", err)
			}
		}
	}
	classifyDiscoveredModels(result.Models)
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].ID < result.Models[j].ID })
	return result, nil
}

func (d Discoverer) fetchOpenAIModels(ctx context.Context, endpoint, apiKey string) ([]DiscoveredModel, error) {
	resp, err := d.discoveryRequest(ctx, endpoint, apiKey)
	if err != nil {
		return nil, err
	}
	defer closeDiscoveryResponse(resp)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, discoveryHTTPError(resp.StatusCode)
	}
	models, err := parseOpenAIModels(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: parsing response: %w", err)
	}
	return models, nil
}

func (d Discoverer) fetchLiteLLMModelInfo(ctx context.Context, endpoint, apiKey string) ([]DiscoveredModel, error) {
	resp, err := d.discoveryRequest(ctx, endpoint, apiKey)
	if err != nil {
		return nil, err
	}
	defer closeDiscoveryResponse(resp)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, metadataHTTPError(resp.StatusCode)
	}
	models, err := parseLiteLLMModelInfo(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing /model/info response: %w", err)
	}
	return models, nil
}

func (d Discoverer) discoveryRequest(ctx context.Context, endpoint, apiKey string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("providers.DiscoverOpenAICompatibleModels: creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := d.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting model discovery endpoint: %w", err)
	}
	return resp, nil
}

func closeDiscoveryResponse(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func ModelsEndpoint(baseURL string) (string, error) {
	return endpointWithPath(baseURL, "models")
}

func LiteLLMModelInfoEndpoint(baseURL string) (string, error) {
	cleanBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.ParseRequestURI(cleanBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("providers.LiteLLMModelInfoEndpoint: invalid base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("providers.LiteLLMModelInfoEndpoint: invalid base URL %q: scheme must be http or https", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if pathEndsWithVersion(path) {
		path = path[:strings.LastIndex(path, "/")]
	}
	parsed.Path = strings.TrimRight(path, "/") + "/model/info"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func ChatCompletionsEndpoint(baseURL string) (string, error) {
	return endpointWithPath(baseURL, "chat/completions")
}

func MessagesCountTokensEndpoint(baseURL string) (string, error) {
	return endpointWithPath(baseURL, "messages/count_tokens")
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
	if pathEndsWithVersion(path) {
		parsed.Path = path + "/" + resource
	} else {
		parsed.Path = path + "/v1/" + resource
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func pathEndsWithVersion(path string) bool {
	lastSlash := strings.LastIndex(path, "/")
	last := path[lastSlash+1:]
	if len(last) < 2 || last[0] != 'v' {
		return false
	}
	for _, char := range last[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func discoveryHTTPError(statusCode int) error {
	return &DiscoveryHTTPError{
		StatusCode:     statusCode,
		Authentication: statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden,
	}
}

func metadataHTTPError(statusCode int) error {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "unknown status"
	}
	return fmt.Errorf("HTTP %d %s", statusCode, statusText)
}
