package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestIsAutoModeClassifierRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "plain system string",
			body: `{"model":"claude-sonnet-5","system":"  You are a security monitor for autonomous AI coding agents. Classify this action.","messages":[]}`,
			want: true,
		},
		{
			name: "text block after unrelated block",
			body: `{"model":"claude-sonnet-5","system":[{"type":"text","text":"unrelated"},{"type":"text","text":"You are a security monitor for autonomous AI coding agents.\nClassify this action."}],"messages":[]}`,
			want: true,
		},
		{
			name: "marker in middle of system block",
			body: `{"model":"claude-sonnet-5","system":[{"type":"text","text":"context before You are a security monitor for autonomous AI coding agents."}],"messages":[]}`,
		},
		{
			name: "marker in non-text block",
			body: `{"model":"claude-sonnet-5","system":[{"type":"metadata","text":"You are a security monitor for autonomous AI coding agents."}],"messages":[]}`,
		},
		{
			name: "marker only in user message",
			body: `{"model":"claude-sonnet-5","system":"ordinary","messages":[{"role":"user","content":"You are a security monitor for autonomous AI coding agents."}]}`,
		},
		{
			name: "no system",
			body: `{"model":"claude-sonnet-5","messages":[]}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var req anthropicRequest
			if err := json.Unmarshal([]byte(test.body), &req); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if got := isAutoModeClassifierRequest(req); got != test.want {
				t.Fatalf("isAutoModeClassifierRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAutoModeClassifierFollowsStartupAndLatestConfiguredAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "startup", ProviderName: "fixture", ProviderModel: "model-startup", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "switched", ProviderName: "fixture", ProviderModel: "model-switched", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("startup")}

	assertClassifierRoute(t, &h, ctx, "session-a", "startup", "model-startup")
	if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "switched"}); validationErr != nil {
		t.Fatalf("selectMessageRouteForRequest(switched) error = %#v", validationErr)
	}
	assertClassifierRoute(t, &h, ctx, "session-a", "switched", "model-switched")
	assertClassifierRoute(t, &h, ctx, "session-b", "startup", "model-startup")
}

func TestAutoModeClassifierUsesActiveAliasForFirstPartyClassifierModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	request := autoModeClassifierRequest("claude-sonnet-5")
	route, validationErr := h.selectRouteForRequest(ctx, "session-a", request)
	if validationErr != nil || route.model.Alias != "active" {
		t.Fatalf("first-party classifier route = %#v, error = %#v", route, validationErr)
	}
	assertClassifierRoute(t, &h, ctx, "session-a", "active", "model-active")
}

func TestAutoModeClassifierPrefersExplicitAliasAndUpdatesSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "switched", ProviderName: "fixture", ProviderModel: "model-switched", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	for _, requestedModel := range []string{"switched", "anthropic.ccr.switched"} {
		requestedModel := requestedModel
		t.Run(requestedModel, func(t *testing.T) {
			h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
			route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", autoModeClassifierRequest(requestedModel))
			if validationErr != nil || route.model.Alias != "switched" || route.responseModel != requestedModel {
				t.Fatalf("explicit classifier route = %#v, error = %#v", route, validationErr)
			}
			assertClassifierRoute(t, &h, ctx, "session-a", "switched", "model-switched")
		})
	}
}

func TestCountTokensRouteDoesNotChangeActiveAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	if _, validationErr := h.selectRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"}); validationErr != nil {
		t.Fatalf("selectRouteForRequest(sonnet) error = %#v", validationErr)
	}
	assertClassifierRoute(t, &h, ctx, "session-a", "active", "model-active")
}

func TestAutoModeClassifierUsesFirstPartyOnlyWithoutActiveConfiguredAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "configured", ProviderName: "fixture", ProviderModel: "model-configured", Status: "degraded"},
	)
	tests := []struct {
		name            string
		clearWithSonnet bool
	}{
		{name: "no startup alias"},
		{
			name:            "normal first-party request clears configured alias",
			clearWithSonnet: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("configured")}
			if test.clearWithSonnet {
				if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"}); validationErr != nil {
					t.Fatalf("selectMessageRouteForRequest(sonnet) error = %#v", validationErr)
				}
			} else {
				h.activeModel = newActiveModelSelection("")
			}
			route, validationErr := h.selectRouteForRequest(ctx, "session-a", autoModeClassifierRequest("claude-sonnet-5"))
			if validationErr != nil {
				t.Fatalf("selectRouteForRequest(classifier) error = %#v", validationErr)
			}
			if !route.firstPartyAnthropic || route.responseModel != "claude-sonnet-5" {
				t.Fatalf("classifier route = %#v, want intentional first-party route", route)
			}
		})
	}
}

func TestAutoModeClassifierFailsClosedWhenActiveAliasCannotRoute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		model      store.Model
		mutate     func(context.Context, *store.Store, store.Model) error
		wantStatus int
		wantReason string
	}{
		{
			name:  "blocked",
			model: store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
			mutate: func(ctx context.Context, s *store.Store, model store.Model) error {
				model.Status = "blocked"
				return s.UpdateModel(ctx, model)
			},
			wantStatus: http.StatusForbidden,
			wantReason: "is blocked",
		},
		{
			name:       "deleted",
			model:      store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
			mutate:     func(ctx context.Context, s *store.Store, _ store.Model) error { return s.RemoveModel(ctx, "active") },
			wantStatus: http.StatusInternalServerError,
			wantReason: "reading requested model alias",
		},
		{
			name:       "unroutable provider control model",
			model:      store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "all-proxy-models", Status: "degraded"},
			mutate:     func(context.Context, *store.Store, store.Model) error { return nil },
			wantStatus: http.StatusBadRequest,
			wantReason: "control model",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := newGatewayStore(t, store.Provider{Name: "fixture", Type: "litellm", BaseURL: "http://127.0.0.1:1"}, test.model)
			if err := test.mutate(ctx, s, test.model); err != nil {
				t.Fatalf("mutate active model error = %v", err)
			}
			h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
			_, validationErr := h.selectRouteForRequest(ctx, "session-a", autoModeClassifierRequest("claude-sonnet-5"))
			if validationErr == nil {
				t.Fatal("classifier route succeeded, want fail-closed rejection")
			}
			if validationErr.status != test.wantStatus || !strings.Contains(validationErr.message, test.wantReason) ||
				!strings.Contains(validationErr.message, "first-party Anthropic fallback was refused") {
				t.Fatalf("classifier error = %#v, want status %d and reasons %q", validationErr, test.wantStatus, test.wantReason)
			}
		})
	}
}

func TestActiveModelSelectionConcurrentAccess(t *testing.T) {
	t.Parallel()
	selection := newActiveModelSelection("startup")
	configured := messageRoute{model: store.Model{Alias: "configured"}}
	firstParty := messageRoute{firstPartyAnthropic: true}
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			sessionID := "session-a"
			if index%2 != 0 {
				sessionID = "session-b"
			}
			for iteration := 0; iteration < 100; iteration++ {
				if index%2 == 0 {
					selection.observe(sessionID, configured)
				} else {
					selection.observe(sessionID, firstParty)
				}
				_ = selection.currentAlias(sessionID)
			}
		}(index)
	}
	wait.Wait()
}

func TestActiveModelSelectionDoesNotShareBlankSessionID(t *testing.T) {
	t.Parallel()
	configured := messageRoute{model: store.Model{Alias: "configured"}}
	selection := newActiveModelSelection("startup")
	selection.observe("", configured)
	if got := selection.currentAlias(""); got != "startup" {
		t.Fatalf("blank session alias = %q, want startup", got)
	}
	if got := selection.currentAlias("session-a"); got != "startup" {
		t.Fatalf("unobserved named session alias = %q, want startup", got)
	}
	selection.observe("session-a", configured)
	if got := selection.currentAlias("session-a"); got != "configured" {
		t.Fatalf("observed named session alias = %q, want configured", got)
	}
	if got := selection.currentAlias(""); got != "startup" {
		t.Fatalf("blank session alias after named observation = %q, want startup", got)
	}
}

func assertClassifierRoute(t *testing.T, h *handler, ctx context.Context, sessionID, wantAlias, wantProviderModel string) {
	t.Helper()
	route, validationErr := h.selectRouteForRequest(ctx, sessionID, autoModeClassifierRequest("claude-sonnet-5"))
	if validationErr != nil {
		t.Fatalf("selectRouteForRequest(classifier) error = %#v", validationErr)
	}
	if route.firstPartyAnthropic || route.model.Alias != wantAlias || route.model.ProviderModel != wantProviderModel ||
		route.responseModel != "claude-sonnet-5" {
		t.Fatalf("classifier route = %#v, want alias=%q providerModel=%q responseModel=claude-sonnet-5", route, wantAlias, wantProviderModel)
	}
}

func autoModeClassifierRequest(model string) anthropicRequest {
	return anthropicRequest{
		Model:  model,
		System: autoModeClassifierSystemPrefix + " Classify this action.",
	}
}
