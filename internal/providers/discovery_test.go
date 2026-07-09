package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscoverOpenAICompatibleModelsParsesModelIDs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"glm-5.2[1m]"},{"id":"gpt-5"},{"id":""}]}`))
	}))
	defer server.Close()

	models, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "litellm",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	want := []string{"gpt-5", "glm-5.2[1m]"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestModelsEndpointNormalizesBaseURL(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"http://localhost:4000":         "http://localhost:4000/v1/models",
		"http://localhost:4000/":        "http://localhost:4000/v1/models",
		"http://localhost:4000/v1":      "http://localhost:4000/v1/models",
		"http://localhost:4000/v1/":     "http://localhost:4000/v1/models",
		"https://openrouter.ai/api":     "https://openrouter.ai/api/v1/models",
		"https://openrouter.ai/api////": "https://openrouter.ai/api/v1/models",
	}
	for input, want := range tests {
		input := input
		want := want
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got, err := ModelsEndpoint(input)
			if err != nil {
				t.Fatalf("ModelsEndpoint(%q) error = %v", input, err)
			}
			if got != want {
				t.Fatalf("ModelsEndpoint(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestChatCompletionsEndpointNormalizesBaseURL(t *testing.T) {
	t.Parallel()

	got, err := ChatCompletionsEndpoint("https://openrouter.ai/api/v1/")
	if err != nil {
		t.Fatalf("ChatCompletionsEndpoint() error = %v", err)
	}
	if got != "https://openrouter.ai/api/v1/chat/completions" {
		t.Fatalf("ChatCompletionsEndpoint() = %q", got)
	}
}

func TestChatCompletionsEndpointKeepsVersionedBaseURL(t *testing.T) {
	t.Parallel()

	got, err := ChatCompletionsEndpoint("https://api.z.ai/api/coding/paas/v4")
	if err != nil {
		t.Fatalf("ChatCompletionsEndpoint() error = %v", err)
	}
	if got != "https://api.z.ai/api/coding/paas/v4/chat/completions" {
		t.Fatalf("ChatCompletionsEndpoint() = %q", got)
	}
}

func TestRegistryProfilesIncludeZAIProtocols(t *testing.T) {
	t.Parallel()

	registry := Registry{}
	zai, ok := registry.Profile("zai")
	if !ok {
		t.Fatalf("zai profile missing")
	}
	if zai.Protocol != ProtocolAnthropicCompatible || zai.DefaultBaseURL != "https://api.z.ai/api/anthropic" || !zai.Capabilities.SupportsTools || zai.Capabilities.SupportsModelDiscovery {
		t.Fatalf("zai profile = %#v", zai)
	}
	zaiOpenAI, ok := registry.Profile("zai-openai")
	if !ok {
		t.Fatalf("zai-openai profile missing")
	}
	if zaiOpenAI.Protocol != ProtocolOpenAICompatible || zaiOpenAI.DefaultBaseURL != "https://api.z.ai/api/coding/paas/v4" || !zaiOpenAI.Capabilities.SupportsModelDiscovery {
		t.Fatalf("zai-openai profile = %#v", zaiOpenAI)
	}
}

func TestDiscoverOpenAICompatibleModelsIncludesBearerAuth(t *testing.T) {
	t.Parallel()

	const apiKey = "sk-test-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Fatalf("authorization header = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()

	_, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "openrouter",
		BaseURL: server.URL,
		APIKey:  apiKey,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
}

func TestDiscoverOpenAICompatibleModelsDoesNotLeakKeyOnAuthError(t *testing.T) {
	t.Parallel()

	const apiKey = "sk-test-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret "+apiKey, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "litellm",
		BaseURL: server.URL,
		APIKey:  apiKey,
	})
	if err == nil {
		t.Fatalf("expected auth error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("auth error leaked API key: %v", err)
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("error = %v, want authentication failed", err)
	}
}

func TestDiscoverOpenAICompatibleModelsRejectsUnsupportedProvider(t *testing.T) {
	t.Parallel()

	_, err := (Discoverer{Timeout: time.Millisecond}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "anthropic",
		BaseURL: "https://api.anthropic.com",
	})
	if err == nil {
		t.Fatalf("expected unsupported provider error")
	}
}
