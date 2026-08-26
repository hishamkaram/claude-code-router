package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayRoutesAutoModeClassifierThroughActiveAliasProtocols(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic-compatible"} {
		protocol := protocol
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			providerServer, provider, model, capture := newAutoModeClassifierProvider(t, protocol)
			defer providerServer.Close()
			firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
			defer firstPartyServer.Close()
			s := newGatewayStore(t, provider, model)
			server := startGatewayWithConfig(t, ctx, Config{
				Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token",
				DefaultModelAlias: model.Alias, AnthropicBaseURL: firstPartyServer.URL,
			})
			defer func() { _ = server.Shutdown(ctx) }()

			response := postClassifierRequest(t, ctx, server.URL(), autoModeClassifierBody(t))
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read classifier response: %v", err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("classifier status=%d body=%s, want 200", response.StatusCode, body)
			}
			var decoded struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode classifier response: %v", err)
			}
			if decoded.Model != "claude-sonnet-5" {
				t.Fatalf("classifier response model = %q, want original classifier model", decoded.Model)
			}
			if len(decoded.Content) != 1 || decoded.Content[0].Type != "text" || decoded.Content[0].Text != "<block>no</block>" {
				t.Fatalf("classifier response content = %#v, want original allow response", decoded.Content)
			}
			calls, gotModel := capture.snapshot()
			if calls != 1 || gotModel != model.ProviderModel {
				t.Fatalf("selected provider calls=%d model=%q, want one call with %q", calls, gotModel, model.ProviderModel)
			}
			if calls := firstParty.snapshot(); calls != 0 {
				t.Fatalf("classifier used first-party Anthropic %d times", calls)
			}
		})
	}
}

func TestGatewayRoutesAutoModeClassifierCountTokensThroughActiveAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, model, capture := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
	defer firstPartyServer.Close()
	s := newGatewayStore(t, provider, model)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token",
		DefaultModelAlias: model.Alias, AnthropicBaseURL: firstPartyServer.URL,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postAutoModeClassifierEndpoint(t, ctx, server.URL(), "/v1/messages/count_tokens")
	defer response.Body.Close()
	var decoded struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.StatusCode != http.StatusOK || decoded.InputTokens != 13 {
		t.Fatalf("count_tokens status=%d input_tokens=%d, want 200 and 13", response.StatusCode, decoded.InputTokens)
	}
	calls, gotModel := capture.snapshot()
	if calls != 1 || gotModel != model.ProviderModel {
		t.Fatalf("selected count_tokens calls=%d model=%q, want one call with %q", calls, gotModel, model.ProviderModel)
	}
	if calls := firstParty.snapshot(); calls != 0 {
		t.Fatalf("classifier count_tokens used first-party Anthropic %d times", calls)
	}
}

func TestGatewayKeepsAutoModeClassifierSelectionPerClaudeSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, startup, capture := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
	defer firstPartyServer.Close()
	s := newGatewayStore(t, provider, startup)
	switched := startup
	switched.Alias = "switched"
	switched.ProviderModel = "provider-switched"
	if err := s.AddModel(ctx, switched); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token",
		DefaultModelAlias: startup.Alias, AnthropicBaseURL: firstPartyServer.URL,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessagesForSession(t, ctx, server.URL(), "session-a", `{"model":"switched","system":"ordinary","messages":[{"role":"user","content":"hello"}]}`)
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session-a switch status=%d, want 200", response.StatusCode)
	}

	response = postGatewayMessagesForSession(t, ctx, server.URL(), "session-b", autoModeClassifierBody(t))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session-b classifier status=%d, want 200", response.StatusCode)
	}
	if calls, gotModel := capture.snapshot(); calls != 2 || gotModel != startup.ProviderModel {
		t.Fatalf("session-b classifier calls=%d model=%q, want startup model %q", calls, gotModel, startup.ProviderModel)
	}

	response = postGatewayMessagesForSession(t, ctx, server.URL(), "session-a", autoModeClassifierBody(t))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session-a classifier status=%d, want 200", response.StatusCode)
	}
	if calls, gotModel := capture.snapshot(); calls != 3 || gotModel != switched.ProviderModel {
		t.Fatalf("session-a classifier calls=%d model=%q, want switched model %q", calls, gotModel, switched.ProviderModel)
	}
	if calls := firstParty.snapshot(); calls != 0 {
		t.Fatalf("per-session classifiers used first-party Anthropic %d times", calls)
	}
}

func TestGatewayExplicitClassifierAliasOverridesCachedSessionAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, startup, capture := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	s := newGatewayStore(t, provider, startup)
	switched := startup
	switched.Alias = "switched"
	switched.ProviderModel = "provider-switched"
	if err := s.AddModel(ctx, switched); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: startup.Alias,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessagesForSession(t, ctx, server.URL(), "session-a", autoModeClassifierBodyForModel(t, "anthropic.ccr.switched"))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit classifier switch status=%d, want 200", response.StatusCode)
	}
	if calls, gotModel := capture.snapshot(); calls != 1 || gotModel != switched.ProviderModel {
		t.Fatalf("explicit classifier switch calls=%d model=%q, want %q", calls, gotModel, switched.ProviderModel)
	}

	response = postGatewayMessagesForSession(t, ctx, server.URL(), "session-a", autoModeClassifierBody(t))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cached classifier after explicit switch status=%d, want 200", response.StatusCode)
	}
	if calls, gotModel := capture.snapshot(); calls != 2 || gotModel != switched.ProviderModel {
		t.Fatalf("cached classifier after explicit switch calls=%d model=%q, want %q", calls, gotModel, switched.ProviderModel)
	}
}

func TestGatewayDoesNotShareHeaderlessModelObservations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, startup, capture := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	s := newGatewayStore(t, provider, startup)
	switched := startup
	switched.Alias = "switched"
	switched.ProviderModel = "provider-switched"
	if err := s.AddModel(ctx, switched); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", DefaultModelAlias: startup.Alias,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postGatewayMessagesForSession(t, ctx, server.URL(), "", `{"model":"switched","system":"ordinary","messages":[{"role":"user","content":"hello"}]}`)
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("headerless model request status=%d, want 200", response.StatusCode)
	}
	response = postGatewayMessagesForSession(t, ctx, server.URL(), "", autoModeClassifierBody(t))
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("headerless classifier status=%d, want 200", response.StatusCode)
	}
	if calls, gotModel := capture.snapshot(); calls != 2 || gotModel != startup.ProviderModel {
		t.Fatalf("headerless classifier calls=%d model=%q, want unshared startup model %q", calls, gotModel, startup.ProviderModel)
	}
}

func TestGatewayAutoModeClassifierProviderFailureDoesNotFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var providerCalls int
	var providerMu sync.Mutex
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerMu.Lock()
		providerCalls++
		providerMu.Unlock()
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	defer providerServer.Close()
	firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
	defer firstPartyServer.Close()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: providerServer.URL},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "provider-model", Status: "degraded"},
	)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token",
		DefaultModelAlias: "active", AnthropicBaseURL: firstPartyServer.URL,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postClassifierRequest(t, ctx, server.URL(), autoModeClassifierBody(t))
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("classifier failure status=%d body=%s, want 503", response.StatusCode, body)
	}
	providerMu.Lock()
	calls := providerCalls
	providerMu.Unlock()
	if calls != 1 || firstParty.snapshot() != 0 {
		t.Fatalf("provider calls=%d first-party calls=%d, want 1 and 0", calls, firstParty.snapshot())
	}
}

func TestGatewayRejectsAutoModeClassifierWithoutModelBeforeRouting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, model, capture := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
	defer firstPartyServer.Close()
	s := newGatewayStore(t, provider, model)
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token",
		DefaultModelAlias: model.Alias, AnthropicBaseURL: firstPartyServer.URL,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	response := postClassifierRequest(t, ctx, server.URL(), `{
		"model":"   ",
		"system":"You are a security monitor for autonomous AI coding agents. Classify this action.",
		"messages":[]
	}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read classifier rejection: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "message request model is required") {
		t.Fatalf("classifier rejection status=%d body=%s, want visible 400", response.StatusCode, body)
	}
	if calls, _ := capture.snapshot(); calls != 0 || firstParty.snapshot() != 0 {
		t.Fatalf("blank-model classifier provider calls=%d first-party calls=%d, want zero", calls, firstParty.snapshot())
	}
}

func TestGatewayTracesAutoModeClassifierOperationsWithoutTracker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	providerServer, provider, model, _ := newAutoModeClassifierProvider(t, "openai-chat")
	defer providerServer.Close()
	firstPartyServer, firstParty := newAutoModeFirstPartyProbe(t)
	defer firstPartyServer.Close()
	s := newGatewayStore(t, provider, model)
	launchID, err := s.CreateLaunch(ctx, model.Alias, "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "local-token", Recorder: recorder,
		DefaultModelAlias: model.Alias, AnthropicBaseURL: firstPartyServer.URL,
	})
	defer func() { _ = server.Shutdown(ctx) }()

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		response := postAutoModeClassifierEndpoint(t, ctx, server.URL(), path)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("classifier endpoint %s status=%d, want 200", path, response.StatusCode)
		}
	}

	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	wantOperations := map[string]bool{
		"auto_mode_classifier":              false,
		"auto_mode_classifier_count_tokens": false,
	}
	for _, event := range events {
		if event.Kind != "route" {
			continue
		}
		route := event.Route
		if _, ok := wantOperations[route.Operation]; !ok {
			continue
		}
		if route.RequestedModel != "claude-sonnet-5" || route.ModelAlias != model.Alias ||
			route.ProviderModel != model.ProviderModel || route.Status != "succeeded" {
			t.Fatalf("classifier trace = %#v", route)
		}
		wantOperations[route.Operation] = true
	}
	for operation, seen := range wantOperations {
		if !seen {
			t.Fatalf("classifier trace operation %q not recorded: %#v", operation, events)
		}
	}
	if calls := firstParty.snapshot(); calls != 0 {
		t.Fatalf("traced classifier requests used first-party Anthropic %d times", calls)
	}
}

type autoModeClassifierCapture struct {
	mu    sync.Mutex
	calls int
	model string
}

func (c *autoModeClassifierCapture) record(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.model = model
}

func (c *autoModeClassifierCapture) snapshot() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.model
}

type autoModeFirstPartyProbe struct {
	mu    sync.Mutex
	calls int
}

func (p *autoModeFirstPartyProbe) record() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *autoModeFirstPartyProbe) snapshot() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newAutoModeClassifierProvider(t *testing.T, protocol string) (*httptest.Server, store.Provider, store.Model, *autoModeClassifierCapture) {
	t.Helper()
	capture := &autoModeClassifierCapture{}
	provider := store.Provider{
		Name: "fixture", Type: "openai-compatible", SupportsCountTokens: true,
	}
	model := store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "provider-model", Status: "degraded"}
	switch protocol {
	case "openai-responses":
		provider.SupportsResponses = true
		model.CapabilityOverrides = modelcap.Values{Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true)}
	case "anthropic-compatible":
		provider.Type = "anthropic-compatible"
	default:
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode selected provider request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		capture.record(payload.Model)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			_, _ = fmt.Fprint(w, `{"input_tokens":13}`)
		case "/v1/chat/completions":
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-classifier","choices":[{"message":{"content":"<block>no</block>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)
		case "/v1/responses":
			_, _ = fmt.Fprintf(w, `{"id":"resp-classifier","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<block>no</block>"}]}],"usage":{"input_tokens":9,"output_tokens":3}}`, payload.Model)
		case "/v1/messages":
			_, _ = fmt.Fprintf(w, `{"id":"msg-classifier","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"<block>no</block>"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`, payload.Model)
		default:
			http.NotFound(w, r)
		}
	}))
	provider.BaseURL = server.URL
	return server, provider, model, capture
}

func newAutoModeFirstPartyProbe(t *testing.T) (*httptest.Server, *autoModeFirstPartyProbe) {
	t.Helper()
	probe := &autoModeFirstPartyProbe{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.record()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			_, _ = fmt.Fprint(w, `{"input_tokens":17}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"msg-first-party","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"<block>no</block>"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`)
	}))
	return server, probe
}

func postAutoModeClassifierEndpoint(t *testing.T, ctx context.Context, gatewayURL, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+path, strings.NewReader(autoModeClassifierBody(t)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("classifier request error = %v", err)
	}
	return response
}

func postGatewayMessagesForSession(t *testing.T, ctx context.Context, gatewayURL, sessionID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	req.Header.Set(claudeCodeSessionIDHeader, sessionID)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway messages request error: %v", err)
	}
	return response
}

func autoModeClassifierBody(t *testing.T) string {
	t.Helper()
	return autoModeClassifierBodyForModel(t, "claude-sonnet-5")
}

func autoModeClassifierBodyForModel(t *testing.T, model string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 64,
		"system": []any{
			map[string]any{"type": "text", "text": autoModeClassifierSystemPrefix + " Classify this action."},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "<tool_use>Agent</tool_use>"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal(classifier request) error = %v", err)
	}
	return string(body)
}
