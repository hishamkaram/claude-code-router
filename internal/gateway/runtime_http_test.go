package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestRuntimeEndpointsUseSeparateObserverAuthentication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(
		t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded"},
	)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	tracker, err := session.NewTracker(session.Config{
		Store: s, Recorder: recorder, LaunchID: launchID, Enabled: true,
	})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Token: "model-token", ObserverToken: "observer-token",
		Recorder: recorder, Tracker: tracker,
	})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	response := runtimeRequest(t, ctx, server.URL(), http.MethodPost, "/internal/v1/hooks",
		`{"session_id":"session-1","hook_event_name":"SessionStart","source":"startup"}`,
		"observer-token", "application/json")
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("hook status = %d, want 204", response.StatusCode)
	}

	response = runtimeRequest(t, ctx, server.URL(), http.MethodGet, "/internal/v1/status", "", "observer-token", "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint = %d, want 200", response.StatusCode)
	}
	var snapshot session.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatalf("status decode error = %v", err)
	}
	if snapshot.CurrentSession.ClaudeSessionID != "session-1" || snapshot.LifecycleState != "active" {
		t.Fatalf("status snapshot = %#v", snapshot)
	}

	response = runtimeRequest(t, ctx, server.URL(), http.MethodGet, "/internal/v1/status", "", "model-token", "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("model token status = %d, want 401", response.StatusCode)
	}
}

func TestRuntimeHookBoundaryValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(
		t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded"},
	)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	tracker, err := session.NewTracker(session.Config{Store: s, LaunchID: launchID, Enabled: true})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	h := &handler{cfg: Config{Tracker: tracker, ObserverToken: "observer-token"}}

	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		remoteAddr  string
		wantStatus  int
	}{
		{name: "method", method: http.MethodGet, contentType: "application/json", remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusMethodNotAllowed},
		{name: "content type", method: http.MethodPost, contentType: "text/plain", body: `{}`, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusUnsupportedMediaType},
		{name: "invalid body", method: http.MethodPost, contentType: "application/json", body: `{`, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusBadRequest},
		{name: "trailing body", method: http.MethodPost, contentType: "application/json", body: `{}` + `{}`, remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusBadRequest},
		{name: "oversized body", method: http.MethodPost, contentType: "application/json", body: fmt.Sprintf(`{"ignored":"%s"}`, strings.Repeat("x", maxHookBodyBytes)), remoteAddr: "127.0.0.1:1234", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "non loopback", method: http.MethodPost, contentType: "application/json", body: `{}`, remoteAddr: "192.0.2.1:1234", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, "http://127.0.0.1/internal/v1/hooks", strings.NewReader(test.body))
			req.RemoteAddr = test.remoteAddr
			req.Header.Set(observerTokenHeader, "observer-token")
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestGatewayTraceFollowsModelSwitchAndCapturesUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	}))
	t.Cleanup(provider.Close)
	s := newGatewayStore(
		t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: provider.URL},
		store.Model{Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "reviewer", ProviderName: "fixture", ProviderModel: "model-v2", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(reviewer) error = %v", err)
	}
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: s, LaunchID: launchID, Enabled: true})
	tracker, err := session.NewTracker(session.Config{
		Store: s, Recorder: recorder, LaunchID: launchID, Enabled: true,
		DefaultModelAlias: "coder",
	})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "model-token",
		ObserverToken: "observer-token", Recorder: recorder, Tracker: tracker,
	})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	response := runtimeRequest(t, ctx, server.URL(), http.MethodPost, "/internal/v1/hooks",
		`{"session_id":"session-1","hook_event_name":"SessionStart","source":"startup"}`,
		"observer-token", "application/json")
	_ = response.Body.Close()

	for _, alias := range []string{"coder", "reviewer"} {
		body := fmt.Sprintf(`{"model":%q,"max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, alias)
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
		if requestErr != nil {
			t.Fatalf("NewRequest(%s) error = %v", alias, requestErr)
		}
		req.Header.Set(ccrSessionTokenHeader, "model-token")
		response, requestErr = http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatalf("gateway request %s error = %v", alias, requestErr)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get(ccrRequestIDHeader) == "" {
			t.Fatalf("gateway %s status = %d, request id = %q", alias, response.StatusCode, response.Header.Get(ccrRequestIDHeader))
		}
	}

	traces, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	var routes []store.RouteEvent
	for _, trace := range traces {
		if trace.Kind == "route" {
			routes = append(routes, trace.Route)
		}
	}
	if len(routes) != 2 || routes[0].ModelAlias != "reviewer" || routes[1].ModelAlias != "coder" {
		t.Fatalf("route traces = %#v", routes)
	}
	for _, route := range routes {
		if route.Status != "succeeded" || !route.Usage.Observed ||
			route.Usage.InputTokens != 11 || route.Usage.OutputTokens != 5 {
			t.Fatalf("route trace = %#v", route)
		}
	}
	if snapshot := tracker.Snapshot(); snapshot.Route.ModelAlias != "reviewer" || snapshot.CurrentSession.ID == 0 {
		t.Fatalf("tracker Snapshot() = %#v", snapshot)
	}
}

func TestGatewayRoutingSurvivesDegradedRecorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(provider.Close)
	s := newGatewayStore(
		t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: provider.URL},
		store.Model{Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded"},
	)
	recorder := observability.NewRecorder(ctx, observability.Config{LaunchID: 99, Enabled: true})
	server := startGatewayWithConfig(t, ctx, Config{
		Store: s, Secrets: fakeGatewaySecrets{}, Token: "model-token",
		ObserverToken: "observer-token", Recorder: recorder,
	})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages",
		strings.NewReader(`{"model":"coder","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set(ccrSessionTokenHeader, "model-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", response.StatusCode)
	}
	if snapshot := recorder.Snapshot(); snapshot.Healthy || snapshot.WriteFailures == 0 {
		t.Fatalf("Recorder Snapshot() = %#v", snapshot)
	}
}

func runtimeRequest(t *testing.T, ctx context.Context, baseURL, method, path, body, token, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set(observerTokenHeader, token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("runtime request error = %v", err)
	}
	return response
}
