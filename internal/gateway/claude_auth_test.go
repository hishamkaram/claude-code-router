package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayFirstPartyAuthFailureKeepsProviderAliasesRoutable(t *testing.T) {
	ctx := context.Background()
	var providerCalls atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"upstream-private-secret"}}`)
	}))
	defer anthropic.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if got := r.Header.Get("x-api-key"); got != "provider-secret" {
			t.Errorf("configured provider x-api-key = %q, want provider-secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"provider-message","type":"message","role":"assistant","model":"provider-claude","content":[{"type":"text","text":"provider-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	if err := s.AddProvider(ctx, store.Provider{
		Name: "registered-provider", Type: "anthropic", BaseURL: provider.URL, SecretRef: "env:PROVIDER_KEY",
	}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{
		Alias: "registered", ProviderName: "registered-provider", ProviderModel: "provider-claude", Status: "full",
	}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	launchID, err := s.CreateLaunch(ctx, "coder", "running", "running")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{"env:PROVIDER_KEY": "provider-secret"}, Token: "local-token",
		AnthropicBaseURL: anthropic.URL, Recorder: recorder,
	})
	defer shutdownGateway(t, ctx, server)

	firstParty := postGatewayModel(t, ctx, server, "sonnet")
	firstPartyBody, readErr := io.ReadAll(firstParty.Body)
	firstParty.Body.Close()
	if readErr != nil {
		t.Fatalf("reading first-party error: %v", readErr)
	}
	if firstParty.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first-party status = %d, want 401", firstParty.StatusCode)
	}
	if !strings.Contains(string(firstPartyBody), `"authentication_error"`) ||
		!strings.Contains(string(firstPartyBody), "Registered CCR provider aliases") ||
		!strings.Contains(string(firstPartyBody), "/model") ||
		strings.Contains(string(firstPartyBody), "upstream-private-secret") {
		t.Fatalf("first-party error body = %s", firstPartyBody)
	}

	modelsResp, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL()+"/v1/models", http.NoBody)
	if err != nil {
		t.Fatalf("models request = %v", err)
	}
	modelsResp.Header.Set("X-CCR-Session-Token", "local-token")
	models, err := http.DefaultClient.Do(modelsResp)
	if err != nil {
		t.Fatalf("models request error = %v", err)
	}
	defer models.Body.Close()
	var modelList struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if decodeErr := json.NewDecoder(models.Body).Decode(&modelList); decodeErr != nil {
		t.Fatalf("models response error = %v", decodeErr)
	}
	var foundFirstParty, foundProvider bool
	for _, model := range modelList.Data {
		if model.ID == "sonnet" {
			foundFirstParty = true
			if !strings.Contains(model.DisplayName, "re-login required") {
				t.Errorf("first-party display name = %q, want degraded marker", model.DisplayName)
			}
		}
		if model.ID == "anthropic.ccr.registered" {
			foundProvider = true
		}
	}
	if !foundFirstParty || !foundProvider {
		t.Fatalf("model discovery = %#v, want first-party and registered provider aliases", modelList.Data)
	}

	registered := postGatewayModel(t, ctx, server, "registered")
	registeredBody, readErr := io.ReadAll(registered.Body)
	registered.Body.Close()
	if readErr != nil {
		t.Fatalf("reading registered response: %v", readErr)
	}
	if registered.StatusCode != http.StatusOK || !strings.Contains(string(registeredBody), "provider-ok") {
		t.Fatalf("registered response = %d %s", registered.StatusCode, registeredBody)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("registered provider calls = %d, want 1", providerCalls.Load())
	}

	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Kind: "lifecycle", Name: claudeAuthLifecycleName, Limit: 1})
	if err != nil {
		t.Fatalf("auth lifecycle events error = %v", err)
	}
	if len(events) != 1 || events[0].Lifecycle.Status != string(claudeAuthNeedsRelogin) ||
		events[0].Lifecycle.Reason != "upstream_authentication_rejected" {
		t.Fatalf("auth lifecycle events = %#v", events)
	}
}

func TestGatewayFirstPartyForbiddenReturnsSanitizedAuthenticationError(t *testing.T) {
	ctx := context.Background()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"message":"private-upstream-body"}}`)
	}))
	defer anthropic.Close()
	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	server := startGatewayWithConfig(t, ctx, Config{Store: s, Token: "local-token", AnthropicBaseURL: anthropic.URL})
	defer shutdownGateway(t, ctx, server)

	resp := postGatewayModel(t, ctx, server, "sonnet")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading forbidden response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `"authentication_error"`) ||
		strings.Contains(string(body), "private-upstream-body") {
		t.Fatalf("forbidden response = %d %s", resp.StatusCode, body)
	}
}

func TestClaudeAuthStatePersistsAfterRequestCancellation(t *testing.T) {
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "litellm", Type: "litellm", BaseURL: "http://127.0.0.1:1", SecretRef: ""},
		store.Model{Alias: "gpt", ProviderName: "litellm", ProviderModel: "gpt-5", Status: "degraded"},
	)
	launchID, err := s.CreateLaunch(ctx, "coder", "running", "running")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	h := &handler{cfg: Config{Recorder: recorder}}
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	h.recordClaudeAuthState(canceled, claudeAuthNeedsRelogin, "upstream_authentication_rejected")
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{
		LaunchID: launchID, Kind: "lifecycle", Name: claudeAuthLifecycleName, Limit: 1,
	})
	if err != nil {
		t.Fatalf("auth lifecycle events error = %v", err)
	}
	if len(events) != 1 || events[0].Lifecycle.Status != string(claudeAuthNeedsRelogin) {
		t.Fatalf("auth lifecycle events = %#v, want persisted canceled-request transition", events)
	}
}

func postGatewayModel(t *testing.T, ctx context.Context, server *Server, model string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model,
	)))
	if err != nil {
		t.Fatalf("message request = %v", err)
	}
	req.Header.Set("X-CCR-Session-Token", "local-token")
	resp, err := http.DefaultClient.Do(req)
	return mustHTTPResponse(t, resp, err)
}

func mustHTTPResponse(t *testing.T, resp *http.Response, err error) *http.Response {
	t.Helper()
	if err != nil {
		t.Fatalf("HTTP request error = %v", err)
	}
	return resp
}
