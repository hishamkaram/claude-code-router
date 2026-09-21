package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

// Claude Code 2.1.278 auto mode asks the Anthropic API to run its tool-use
// classifier server-side with this top-level field. OpenAI-compatible
// providers have no equivalent, so translated routes must drop it visibly
// instead of failing the whole session with 501.
const claudeCodeSafeguardsField = `"safeguards":[{"type":"dangerous_tool_use","classifier_context":"permission mode: auto"}]`

func TestGatewayDropsSafeguardsOnOpenAIChatPath(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantHeader string
	}{
		{
			name:       "safeguards only",
			body:       `{"model":"gpt",` + claudeCodeSafeguardsField + `,"messages":[{"role":"user","content":"hello"}]}`,
			wantHeader: "safeguards",
		},
		{
			name:       "safeguards with context management",
			body:       `{"model":"gpt","context_management":{"edits":[{"type":"clear_tool_uses"}]},` + claudeCodeSafeguardsField + `,"messages":[{"role":"user","content":"hello"}]}`,
			wantHeader: "context_management, safeguards",
		},
		{
			name:       "empty safeguards list",
			body:       `{"model":"gpt","safeguards":[],"messages":[{"role":"user","content":"hello"}]}`,
			wantHeader: "safeguards",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var providerPayload map[string]json.RawMessage
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&providerPayload); err != nil {
					t.Errorf("provider decode error = %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer provider.Close()
			s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			resp := postGatewayMessage(t, ctx, server.URL(), test.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("gateway status = %d, want 200: %s", resp.StatusCode, raw)
			}
			if providerPayload == nil {
				t.Fatal("provider was not called")
			}
			if _, ok := providerPayload["safeguards"]; ok {
				t.Fatal("provider received Anthropic safeguards field")
			}
			if got := resp.Header.Get(ccrIgnoredFieldsHeader); got != test.wantHeader {
				t.Fatalf("%s = %q, want %q", ccrIgnoredFieldsHeader, got, test.wantHeader)
			}
		})
	}
}

func TestGatewayDropsSafeguardsOnOpenAIResponsesPath(t *testing.T) {
	ctx := context.Background()
	var providerPayload map[string]json.RawMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&providerPayload); err != nil {
			t.Errorf("provider decode error = %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp_test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer provider.Close()
	s := newGatewayStore(t,
		store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: provider.URL, SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
		store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
			Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
		}},
	)
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() { _ = server.Shutdown(ctx) }()

	resp := postGatewayMessage(t, ctx, server.URL(), `{"model":"responses","max_tokens":16,`+claudeCodeSafeguardsField+`,"messages":[{"role":"user","content":"hello"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("gateway status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if providerPayload == nil {
		t.Fatal("Responses provider was not called")
	}
	if _, ok := providerPayload["safeguards"]; ok {
		t.Fatal("Responses provider received Anthropic safeguards field")
	}
	if got := resp.Header.Get(ccrIgnoredFieldsHeader); got != "safeguards" {
		t.Fatalf("%s = %q, want safeguards", ccrIgnoredFieldsHeader, got)
	}
}

func TestGatewayRejectsMalformedSafeguardsWithoutProviderCall(t *testing.T) {
	tests := []struct {
		name     string
		provider store.Provider
		model    store.Model
		body     string
	}{
		{
			name:     "chat route object instead of list",
			provider: store.Provider{Name: "litellm", Type: "litellm", BaseURL: "", SecretRef: ""},
			model:    store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
			body:     `{"model":"gpt","safeguards":{"type":"dangerous_tool_use"},"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:     "responses route string instead of list",
			provider: store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: "", SupportsTools: true, SupportsStreaming: true, SupportsResponses: true},
			model: store.Model{Alias: "responses", ProviderName: "openai", ProviderModel: "gpt-responses", Status: "degraded", CapabilityOverrides: modelcap.Values{
				Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true),
			}},
			body: `{"model":"responses","safeguards":"dangerous_tool_use","messages":[{"role":"user","content":"hello"}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.Error(w, "should not be called", http.StatusInternalServerError)
			}))
			defer provider.Close()
			test.provider.BaseURL = provider.URL
			s := newGatewayStore(t, test.provider, test.model)
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() { _ = server.Shutdown(ctx) }()

			resp := postGatewayMessage(t, ctx, server.URL(), test.body)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "safeguards") {
				t.Fatalf("gateway status = %d body = %q, want 400 naming safeguards", resp.StatusCode, raw)
			}
			if called {
				t.Fatal("provider was called for malformed safeguards field")
			}
		})
	}
}

func TestIgnoredOpenAIAnthropicFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fields map[string]json.RawMessage
		want   []string
	}{
		{name: "none", fields: map[string]json.RawMessage{"model": json.RawMessage(`"gpt"`)}, want: nil},
		{name: "context management", fields: map[string]json.RawMessage{"context_management": json.RawMessage(`{}`)}, want: []string{"context_management"}},
		{name: "safeguards", fields: map[string]json.RawMessage{"safeguards": json.RawMessage(`[]`)}, want: []string{"safeguards"}},
		{
			name:   "both in stable order",
			fields: map[string]json.RawMessage{"safeguards": json.RawMessage(`[]`), "context_management": json.RawMessage(`{}`)},
			want:   []string{"context_management", "safeguards"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ignoredOpenAIAnthropicFields(test.fields)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("ignoredOpenAIAnthropicFields() = %v, want %v", got, test.want)
			}
		})
	}
}
