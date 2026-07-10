package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayTranslatesAutoModeClassifierRequest(t *testing.T) {
	ctx := context.Background()
	var got openAIChatRequest
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-classifier","choices":[{"message":{"content":"<block>no</block>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL}, store.Model{Alias: "glm-5-2", ProviderName: "litellm", ProviderModel: "glm-5.2[1m]", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"claude-ccr-glm-5-2",
		"max_tokens":64,
		"temperature":1,
		"stop_sequences":["</block>"],
		"system":[{"type":"text","text":"Classify the action.","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"<tool_use>Agent</tool_use>"}]}]
	}`
	resp := postClassifierRequest(t, ctx, server.URL(), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if got.Model != "glm-5.2[1m]" || got.MaxTokens != 64 {
		t.Fatalf("provider received model=%q max_tokens=%d", got.Model, got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 1 {
		t.Fatalf("provider temperature = %v, want 1", got.Temperature)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "</block>" {
		t.Fatalf("provider stop = %#v, want [\"</block>\"]", got.Stop)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Fatalf("provider messages = %#v, want system then user", got.Messages)
	}
}

func TestGatewayPreservesAutoModeClassifierRequestForAnthropicCompatible(t *testing.T) {
	ctx := context.Background()
	var got struct {
		Model         string   `json:"model"`
		Temperature   *float64 `json:"temperature"`
		StopSequences []string `json:"stop_sequences"`
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"msg-classifier","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"<block>no</block>"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "zai", Type: "zai", BaseURL: provider.URL}, store.Model{Alias: "glm", ProviderName: "zai", ProviderModel: "glm-4.7", Status: "full"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{"model":"claude-ccr-glm","max_tokens":64,"temperature":1,"stop_sequences":["</block>"],"messages":[{"role":"user","content":"classify Agent"}]}`
	resp := postClassifierRequest(t, ctx, server.URL(), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if got.Model != "glm-4.7" {
		t.Fatalf("provider model = %q, want glm-4.7", got.Model)
	}
	if got.Temperature == nil || *got.Temperature != 1 {
		t.Fatalf("provider temperature = %v, want 1", got.Temperature)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "</block>" {
		t.Fatalf("provider stop_sequences = %#v, want [\"</block>\"]", got.StopSequences)
	}
}

func TestGatewayRejectsInvalidAutoModeClassifierOptions(t *testing.T) {
	ctx := context.Background()
	tests := []string{
		`{"model":"gpt","temperature":"hot","messages":[]}`,
		`{"model":"gpt","stop_sequences":"stop","messages":[]}`,
	}
	for _, body := range tests {
		body := body
		t.Run(body, func(t *testing.T) {
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			defer provider.Close()
			s := newGatewayStoreWithContext(t, ctx, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL}, store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"})
			server := startGateway(t, ctx, s, fakeGatewaySecrets{})
			defer func() {
				if err := server.Shutdown(ctx); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
			}()

			resp := postClassifierRequest(t, ctx, server.URL(), body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("gateway status = %d, want 400", resp.StatusCode)
			}
			if called {
				t.Fatal("provider was called for an invalid classifier request")
			}
		})
	}
}

func postClassifierRequest(t *testing.T, ctx context.Context, gatewayURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	return resp
}
