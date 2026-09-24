package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestAgentChildTokenCountDoesNotConsumePendingRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}})
	h.activeModel.observe("session-a", messageRoute{model: store.Model{Alias: "parent"}})
	childRequest := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	route, validationErr := h.selectRouteForTokenCountRequest(ctx, "session-a", childRequest)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" || !route.agentChildRouted {
		t.Fatalf("Agent token-count route = %#v, error = %#v; want active CCR route without consuming inheritance", route, validationErr)
	}
	h.activeModel.observe("session-a", route)
	if got := h.activeModel.currentAlias("session-a"); got != "parent" {
		t.Fatalf("parent alias after child token count = %q, want parent", got)
	}
	route, validationErr = h.selectMessageRouteForRequest(ctx, "session-a", childRequest)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("Agent message route after token count = %#v, error = %#v; want active alias", route, validationErr)
	}
}

func TestUnmarkedAgentChildTokenCountUsesPendingAliasWithoutConsumingRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}})
	childRequest := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	route, validationErr := h.selectRouteForTokenCountRequest(ctx, "session-a", childRequest)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" || !route.agentChildRouted {
		t.Fatalf("unmarked child token-count route = %#v, error = %#v; want active CCR alias", route, validationErr)
	}
	route, validationErr = h.selectMessageRouteForRequest(ctx, "session-a", childRequest)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("unmarked child route after token-count probe = %#v, error = %#v; want active CCR alias", route, validationErr)
	}
}

func TestAgentChildTokenCountPeekDoesNotPinAliasBeforeMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "before", ProviderName: "fixture", ProviderModel: "model-before", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "after", ProviderName: "fixture", ProviderModel: "model-after", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(after) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("before")}
	if _, err := h.activeModel.registerAgentChildDescriptorsForNewWork("session-a", []agentChildDescriptor{{prompt: "child task"}}); err != nil {
		t.Fatalf("registerAgentChildDescriptorsForNewWork() error = %v", err)
	}
	h.activeModel.observe("session-a", messageRoute{model: store.Model{Alias: "before"}})
	childRequest := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	peek, validationErr := h.selectRouteForTokenCountRequest(ctx, "session-a", childRequest)
	if validationErr != nil || peek.model.Alias != "before" {
		t.Fatalf("child token-count peek = %#v, error = %#v; want before alias", peek, validationErr)
	}
	h.activeModel.observe("session-a", messageRoute{model: store.Model{Alias: "after"}})
	message, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", childRequest)
	if validationErr != nil || message.model.Alias != "after" {
		t.Fatalf("child message route after /model change = %#v, error = %#v; want after alias", message, validationErr)
	}
}

func TestNativeTokenCountWithoutChildMarkerIsNotRejectedByActiveAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	route, validationErr := h.selectRouteForTokenCountRequest(ctx, "session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "native model probe"}},
	})
	if validationErr != nil || !route.firstPartyAnthropic || route.agentChildRouted {
		t.Fatalf("native token-count route = %#v, error = %#v; want ordinary first-party route", route, validationErr)
	}
}

func TestAgentChildTokenCountProbePreservesSiblingReservations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{
		{prompt: "first child"},
		{prompt: "second child"},
	})
	firstRequest := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the first child.",
		Messages: []anthropicMessage{{Role: "user", Content: "first child"}},
	}
	if _, validationErr := h.selectRouteForTokenCountRequest(ctx, "session-a", firstRequest); validationErr != nil {
		t.Fatalf("first child token-count probe error = %#v", validationErr)
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", firstRequest)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("first child route after token-count probe = %#v, error = %#v; want active alias", route, validationErr)
	}
}

func TestAgentChildClassifierProbeDoesNotConsumePendingRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{}})
	classifierRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", autoModeClassifierRequest("sonnet"))
	if validationErr != nil || classifierRoute.firstPartyAnthropic || classifierRoute.model.Alias != "active" {
		t.Fatalf("classifier route = %#v, error = %#v; want active alias without consuming child reservation", classifierRoute, validationErr)
	}
	childRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || childRoute.firstPartyAnthropic || childRoute.model.Alias != "active" {
		t.Fatalf("child route after classifier probe = %#v, error = %#v; want active alias", childRoute, validationErr)
	}
}

func TestAgentChildRouteFailureRestoresPendingRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{}})
	if err := s.RemoveModel(ctx, "active"); err != nil {
		t.Fatalf("RemoveModel(active) error = %v", err)
	}
	if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"}); validationErr == nil {
		t.Fatal("child route unexpectedly succeeded after active alias removal")
	}
	if err := s.AddModel(ctx, store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(active) error = %v", err)
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("restored Agent child route = %#v, error = %#v", route, validationErr)
	}
}

func TestAgentChildProviderFailureRestoresPendingRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s, HTTPClient: &http.Client{}}, activeModel: newActiveModelSelection("active")}
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{}})
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil {
		t.Fatalf("selectMessageRouteForRequest(sonnet) error = %#v", validationErr)
	}
	h.handleOpenAIChat(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", nil),
		anthropicRequest{Model: "sonnet", Messages: []anthropicMessage{{Role: "user", Content: "hello"}}},
		&route,
		&routeCompletionState{},
	)
	retryRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || retryRoute.firstPartyAnthropic || retryRoute.model.Alias != "active" {
		t.Fatalf("retry route after provider failure = %#v, error = %#v; want restored active alias", retryRoute, validationErr)
	}
}
