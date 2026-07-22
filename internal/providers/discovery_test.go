package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

func TestDiscoverOpenAICompatibleModelsParsesModelIDs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"gpt-5"},` +
			`{"id":"glm-5.2[1m]","display_name":"GLM 5.2 1M","max_input_tokens":1000000,"max_tokens":65536,"capabilities":{"image_input":{"supported":true},"pdf_input":{"supported":false},"structured_outputs":{"supported":true},"thinking":{"supported":true,"types":{"enabled":{"supported":true}}}}},` +
			`{"id":"gpt-5"},{"id":""}]}`))
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "openrouter",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	want := []string{"glm-5.2[1m]", "gpt-5"}
	if got := discovery.RoutableIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
	model := discovery.Models[0]
	if model.ID != "glm-5.2[1m]" || model.DisplayName != "GLM 5.2 1M" ||
		model.Capabilities.Values.ContextWindowTokens == nil || *model.Capabilities.Values.ContextWindowTokens != 1_000_000 ||
		model.Capabilities.Values.MaxOutputTokens == nil || *model.Capabilities.Values.MaxOutputTokens != 65_536 ||
		model.Capabilities.Values.SupportsVision == nil || !*model.Capabilities.Values.SupportsVision ||
		model.Capabilities.Values.SupportsPDFInput == nil || *model.Capabilities.Values.SupportsPDFInput ||
		model.Capabilities.Values.SupportsResponseSchema == nil || !*model.Capabilities.Values.SupportsResponseSchema ||
		model.Capabilities.Values.SupportsThinking == nil || !*model.Capabilities.Values.SupportsThinking {
		t.Fatalf("standard model metadata = %#v", model)
	}
}

func TestDiscoverLiteLLMModelsFiltersControlAndNonChatModels(t *testing.T) {
	t.Parallel()
	const secret = "secret-that-must-not-survive"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"all-proxy-models"},{"id":"embed"},{"id":"glm-5.2[1m]"},{"id":"unknown-chat"}]}`))
		case "/model/info":
			_, _ = w.Write([]byte(`{"data":[` +
				`{"model_name":"embed","litellm_params":{"api_key":"` + secret + `"},"model_info":{"mode":"embedding"}},` +
				`{"model_name":"glm-5.2[1m]","litellm_params":{"api_key":"` + secret + `"},"model_info":{"mode":"chat","max_tokens":200000,"max_input_tokens":1000000,"max_output_tokens":65536,"supports_function_calling":true,"supports_reasoning":true}},` +
				`{"model_name":"unknown-chat","model_info":{"mode":"chat","max_tokens":200000,"max_input_tokens":1000000,"supported_modalities":["text","image","audio","pdf"],"supported_output_modalities":["text","audio"],"supported_openai_params":["tools","stream","response_format"],"supports_function_calling":false,"supports_native_streaming":false,"supports_vision":false,"supports_pdf_input":false,"supports_audio_input":false,"supports_audio_output":false,"supports_system_messages":true,"supports_native_structured_output":true}},` +
				`{"model_name":"metadata-only","model_info":{"mode":"chat","max_tokens":9999999}}` +
				`]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type: "litellm", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	want := []string{"glm-5.2[1m]", "unknown-chat"}
	if got := discovery.RoutableIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RoutableIDs() = %#v, want %#v", got, want)
	}
	if discovery.SkippedCount() != 2 || !discovery.MetadataComplete {
		t.Fatalf("discovery = %#v", discovery)
	}
	var glm DiscoveredModel
	for _, model := range discovery.Models {
		if model.ID == "glm-5.2[1m]" {
			glm = model
		}
	}
	if glm.Capabilities.Values.ContextWindowTokens == nil || *glm.Capabilities.Values.ContextWindowTokens != 1_000_000 ||
		glm.Capabilities.Values.SupportsTools == nil || !*glm.Capabilities.Values.SupportsTools ||
		glm.Capabilities.Sources["supports_tools"] != "litellm:/model/info" || !glm.CapabilityMetadataComplete {
		t.Fatalf("glm capabilities = %#v", glm.Capabilities)
	}
	var unknown DiscoveredModel
	for _, model := range discovery.Models {
		if model.ID == "metadata-only" {
			t.Fatalf("metadata-only model became discoverable: %#v", model)
		}
		if model.ID == "unknown-chat" {
			unknown = model
		}
	}
	values := unknown.Capabilities.Values
	if values.ContextWindowTokens == nil || *values.ContextWindowTokens != 1_000_000 ||
		values.SupportsTools == nil || *values.SupportsTools ||
		values.SupportsStreaming == nil || !*values.SupportsStreaming ||
		values.SupportsVision == nil || *values.SupportsVision ||
		values.SupportsPDFInput == nil || *values.SupportsPDFInput ||
		values.SupportsAudioInput == nil || *values.SupportsAudioInput ||
		values.SupportsAudioOutput == nil || *values.SupportsAudioOutput ||
		values.SupportsSystemMessages == nil || !*values.SupportsSystemMessages ||
		values.SupportsResponseSchema == nil || !*values.SupportsResponseSchema {
		t.Fatalf("unknown-chat capabilities = %#v", unknown.Capabilities)
	}
	if unknown.Capabilities.Sources["supports_streaming"] != modelcap.SourceOpenAIAdapter {
		t.Fatalf("streaming source = %q", unknown.Capabilities.Sources["supports_streaming"])
	}
	encoded, err := json.Marshal(discovery)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("discovery retained secret: %s", encoded)
	}
}

func TestDiscoverLiteLLMModelsReportsOptionalMetadataFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-model"}]}`))
			return
		}
		http.Error(w, "private response", http.StatusForbidden)
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type: "litellm", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	if discovery.MetadataComplete || len(discovery.Warnings) != 1 || !strings.Contains(discovery.Warnings[0], "HTTP 403 Forbidden") {
		t.Fatalf("discovery = %#v", discovery)
	}
	if got := discovery.RoutableIDs(); len(got) != 1 || got[0] != "chat-model" {
		t.Fatalf("RoutableIDs() = %#v", got)
	}
	if discovery.Models[0].CapabilityMetadataComplete {
		t.Fatalf("model metadata unexpectedly complete: %#v", discovery.Models[0])
	}
}

func TestResponsesKindInfersResponsesSupportFromAdapter(t *testing.T) {
	t.Parallel()

	for name, parse := range map[string]func() ([]DiscoveredModel, error){
		"openai": func() ([]DiscoveredModel, error) {
			return parseOpenAIModels(strings.NewReader(`{"data":[{"id":"responses-model","mode":"responses"}]}`))
		},
		"litellm": func() ([]DiscoveredModel, error) {
			return parseLiteLLMModelInfo(strings.NewReader(`{"data":[{"model_name":"responses-model","model_info":{"mode":"responses"}}]}`))
		},
	} {
		name, parse := name, parse
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			models, err := parse()
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			if len(models) != 1 {
				t.Fatalf("models = %#v", models)
			}
			caps := models[0].Capabilities
			if caps.Values.SupportsResponses == nil || !*caps.Values.SupportsResponses {
				t.Fatalf("supports_responses = %#v", caps.Values.SupportsResponses)
			}
			if caps.Sources["supports_responses"] != modelcap.SourceOpenAIAdapter {
				t.Fatalf("supports_responses source = %q", caps.Sources["supports_responses"])
			}
		})
	}
}

func TestProviderDiscoveryToleratesAndNormalizesModalitySynonyms(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"document-model"}]}`))
		case "/model/info":
			_, _ = w.Write([]byte(`{"data":[{"model_name":"document-model","model_info":{"mode":"chat","supported_modalities":["text","document","file","images","vendor-binary"],"supported_output_modalities":["text","files"]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type: "litellm", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	if len(discovery.Models) != 1 {
		t.Fatalf("discovery = %#v", discovery)
	}
	values := discovery.Models[0].Capabilities.Values
	if got := strings.Join(values.InputModalities, ","); got != "image,pdf,text" {
		t.Fatalf("input modalities = %q", got)
	}
	if got := strings.Join(values.OutputModalities, ","); got != "text" {
		t.Fatalf("output modalities = %q", got)
	}
	if values.SupportsVision == nil || !*values.SupportsVision ||
		values.SupportsPDFInput == nil || !*values.SupportsPDFInput {
		t.Fatalf("derived modality capabilities = %#v", values)
	}
}

func TestProviderDiscoveryTreatsInvalidTokenLimitsAsUnknown(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"unknown-limits","context_length":0,"max_input_tokens":-1,"max_output_tokens":"unknown"},` +
			`{"id":"known-limit","context_length":"1000000"}` +
			`]}`))
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type: "openrouter", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	if len(discovery.Models) != 2 {
		t.Fatalf("discovery = %#v", discovery)
	}
	unknown := discovery.Models[1]
	if unknown.ID != "unknown-limits" {
		t.Fatalf("models = %#v", discovery.Models)
	}
	if unknown.Capabilities.Values.ContextWindowTokens != nil ||
		unknown.Capabilities.Values.MaxInputTokens != nil ||
		unknown.Capabilities.Values.MaxOutputTokens != nil {
		t.Fatalf("invalid limits were retained: %#v", unknown.Capabilities.Values)
	}
	known := discovery.Models[0]
	if known.ID != "known-limit" || known.Capabilities.Values.ContextWindowTokens == nil ||
		*known.Capabilities.Values.ContextWindowTokens != 1_000_000 {
		t.Fatalf("known limit = %#v", known)
	}
}

func TestDiscoverLiteLLMModelsTracksMetadataCompletenessPerModel(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"present"},{"id":"omitted"}]}`))
		case "/model/info":
			_, _ = w.Write([]byte(`{"data":[{"model_name":"present","model_info":{"mode":"chat","max_tokens":200000}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovery, err := (Discoverer{}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type: "litellm", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
	}
	if !discovery.MetadataComplete || len(discovery.Models) != 2 {
		t.Fatalf("discovery = %#v", discovery)
	}
	if discovery.Models[0].ID != "omitted" || discovery.Models[0].CapabilityMetadataComplete ||
		discovery.Models[1].ID != "present" || !discovery.Models[1].CapabilityMetadataComplete {
		t.Fatalf("per-model completeness = %#v", discovery.Models)
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

func TestLiteLLMModelInfoEndpointNormalizesBaseURL(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"http://localhost:4000":        "http://localhost:4000/model/info",
		"http://localhost:4000/v1/":    "http://localhost:4000/model/info",
		"https://proxy.example/api/v1": "https://proxy.example/api/model/info",
	} {
		got, err := LiteLLMModelInfoEndpoint(input)
		if err != nil {
			t.Fatalf("LiteLLMModelInfoEndpoint(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("LiteLLMModelInfoEndpoint(%q) = %q, want %q", input, got, want)
		}
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

func TestResponsesEndpointNormalizesBaseURL(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"https://api.openai.com":        "https://api.openai.com/v1/responses",
		"https://openrouter.ai/api/v1/": "https://openrouter.ai/api/v1/responses",
	} {
		got, err := ResponsesEndpoint(input)
		if err != nil {
			t.Fatalf("ResponsesEndpoint(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("ResponsesEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMessagesCountTokensEndpointNormalizesBaseURL(t *testing.T) {
	t.Parallel()

	got, err := MessagesCountTokensEndpoint("http://localhost:4000/v1/")
	if err != nil {
		t.Fatalf("MessagesCountTokensEndpoint() error = %v", err)
	}
	if got != "http://localhost:4000/v1/messages/count_tokens" {
		t.Fatalf("MessagesCountTokensEndpoint() = %q", got)
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
	litellm, ok := registry.Profile("litellm")
	if !ok {
		t.Fatalf("litellm profile missing")
	}
	if litellm.Protocol != ProtocolOpenAICompatible || !litellm.Capabilities.SupportsCountTokens {
		t.Fatalf("litellm profile = %#v", litellm)
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
	var httpErr *DiscoveryHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || !httpErr.Authentication {
		t.Fatalf("error = %#v, want typed authentication HTTP error", err)
	}
}

func TestDiscoverOpenAICompatibleModelsDefaultTimeout(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatalf("discovery request context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining < 25*time.Second || remaining > DefaultDiscoveryTimeout {
			t.Fatalf("discovery request deadline remaining = %s, want close to %s", remaining, DefaultDiscoveryTimeout)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}

	_, err := (Discoverer{HTTPClient: client}).DiscoverOpenAICompatibleModels(context.Background(), DiscoveryConfig{
		Type:    "openrouter",
		BaseURL: "https://provider.example",
	})
	if err != nil {
		t.Fatalf("DiscoverOpenAICompatibleModels() error = %v", err)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
