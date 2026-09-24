package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

//nolint:unparam // routing tests intentionally exercise one isolated Claude session namespace.
func registerAgentChildForTest(t *testing.T, selection *activeModelSelection, sessionID, spawnAlias string, descriptors []agentChildDescriptor) func() {
	t.Helper()
	rollback, err := selection.registerAgentChildDescriptorsAtAliasChecked(sessionID, spawnAlias, descriptors)
	if err != nil {
		t.Fatalf("registerAgentChildDescriptorsAtAliasChecked() error = %v", err)
	}
	return rollback
}

func TestAgentChildRequestUsesSpawnAliasOnceAfterModelSwitch(t *testing.T) {
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
	_ = registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{}})

	if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "switched"}); validationErr != nil {
		t.Fatalf("selectMessageRouteForRequest(switched) error = %#v", validationErr)
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "startup" {
		t.Fatalf("pending Agent route = %#v, error = %#v; want spawn-time alias", route, validationErr)
	}
	nativeRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || !nativeRoute.firstPartyAnthropic {
		t.Fatalf("standalone native request was not preserved: %#v, error = %#v", nativeRoute, validationErr)
	}
}

func TestNewAgentChildResolvesAliasAtFirstRequestAfterModelSwitch(t *testing.T) {
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
	if _, err := h.activeModel.registerAgentChildDescriptorsForNewWork("session-a", []agentChildDescriptor{{prompt: "new child work"}}); err != nil {
		t.Fatalf("registerAgentChildDescriptorsForNewWork() error = %v", err)
	}

	if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "switched"}); validationErr != nil {
		t.Fatalf("selectMessageRouteForRequest(switched) error = %#v", validationErr)
	}
	child := anthropicRequest{
		Model:  "sonnet",
		System: "cc_is_subagent=true",
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: "new child work",
		}},
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "switched" {
		t.Fatalf("new child route = %#v, error = %#v; want active alias at first request", route, validationErr)
	}

	if _, startupValidationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "startup"}); startupValidationErr != nil {
		t.Fatalf("selectMessageRouteForRequest(startup) error = %#v", startupValidationErr)
	}
	continuation, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child)
	if validationErr != nil || continuation.firstPartyAnthropic || continuation.model.Alias != "switched" {
		t.Fatalf("child continuation route = %#v, error = %#v; want child spawn alias", continuation, validationErr)
	}
}

func TestFirstPartyAgentChildMarkerDoesNotInferDefaultAlias(t *testing.T) {
	selection := newActiveModelSelection("configured")
	if marker := selection.agentChildMarker("", ""); marker != nil {
		t.Fatal("first-party child marker is non-nil without a CCR spawn alias")
	}
	if marker := selection.agentChildMarker("session-a", ""); marker != nil {
		t.Fatal("first-party child marker is non-nil without a CCR spawn alias")
	}
	if got := selection.defaultAlias; got != "configured" {
		t.Fatalf("default alias changed while constructing first-party marker: %q", got)
	}
}

func TestAgentChildReservationMatchesItsPromptAndPreservesAliasAcrossUnrelatedRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	if cleanup := registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "Return the child marker", description: "Return the child marker"}}); cleanup == nil {
		t.Fatal("registerAgentChildDescriptors() returned nil")
	}
	unrelated := anthropicRequest{Model: "sonnet", Messages: []anthropicMessage{{Role: "user", Content: "ordinary top-level work"}}}
	if route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", unrelated); validationErr != nil || !route.firstPartyAnthropic {
		t.Fatalf("unrelated standalone parent route = %#v, error = %#v; want native route", route, validationErr)
	}
	child := anthropicRequest{Model: "haiku", Messages: []anthropicMessage{{Role: "user", Content: "Return the child marker"}}}
	if route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child); validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("correlated child route = %#v, error = %#v; want active alias", route, validationErr)
	}
	if route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child); validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" {
		t.Fatalf("unmarked repeated child turn did not fail closed: route = %#v, error = %#v", route, validationErr)
	}
}

func TestAgentChildParentTranscriptDoesNotConsumeMatchingPromptReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "parent", ProviderName: "fixture", ProviderModel: "model-parent", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "child", ProviderName: "fixture", ProviderModel: "model-child", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(child) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("parent")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "child", []agentChildDescriptor{{prompt: "shared child prompt"}})
	parent := anthropicRequest{
		Model:  "sonnet",
		System: autoModeClassifierSystemPrefix,
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "name": "Agent", "input": map[string]any{"prompt": "shared child prompt"},
			}}},
			{Role: "user", Content: "shared child prompt"},
		},
	}
	reservation := h.activeModel.reserveAgentChild("session-a", parent)
	if reservation.pending || reservation.identified || reservation.alias != "" {
		t.Fatalf("parent transcript matched child reservation: %#v", reservation)
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", parent)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "parent" {
		t.Fatalf("parent transcript route = %#v, error = %#v; want selected parent alias", route, validationErr)
	}
	h.activeModel.mu.RLock()
	entries := h.activeModel.pendingAgentChildren["session-a"]
	h.activeModel.mu.RUnlock()
	if len(entries) != 1 || entries[0].active {
		t.Fatalf("parent transcript consumed child reservation: %#v", entries)
	}
}

func TestAgentChildReservationPersistsAcrossMarkedContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	if cleanup := registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}}); cleanup == nil {
		t.Fatal("registerAgentChildDescriptors() returned nil")
	}
	initial := anthropicRequest{
		Model:    "sonnet",
		System:   "Native Claude preamble.\nYou are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	if route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", initial); validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("initial child route = %#v, error = %#v; want active alias", route, validationErr)
	}
	continuation := anthropicRequest{
		Model:  "haiku",
		System: "Native Claude preamble.\nYou are a subagent continuing the child task.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	}
	if route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", continuation); validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("continuation child route = %#v, error = %#v; want active alias", route, validationErr)
	}
}

func TestAgentChildRollbackDoesNotUndoNewerActivation(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})
	request := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	first := selection.reserveAgentChild("session-a", request)
	second := selection.reserveAgentChild("session-a", request)
	if first.alias != "active" || second.alias != "active" || first.rollback == nil || second.rollback == nil {
		t.Fatalf("overlapping child reservations = %#v, %#v; want both routed with rollback", first, second)
	}

	first.rollback()
	selection.mu.RLock()
	entry := selection.pendingAgentChildren["session-a"][0]
	selection.mu.RUnlock()
	if !entry.active {
		t.Fatal("older failed request rollback deactivated newer child activation")
	}

	second.rollback()
	selection.mu.RLock()
	entry = selection.pendingAgentChildren["session-a"][0]
	selection.mu.RUnlock()
	if !entry.active {
		t.Fatal("newer failed request rollback deactivated the active child reservation")
	}
}

func TestUnmarkedAgentChildContinuationRefusesAnthropicFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}})
	initial := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	initialRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", initial)
	if validationErr != nil || !initialRoute.agentChildRouted || initialRoute.firstPartyAnthropic {
		t.Fatalf("unmarked initial child route = %#v, error = %#v; want CCR route", initialRoute, validationErr)
	}
	initialRoute.commitAgentChild()

	continuation := anthropicRequest{
		Model: "haiku",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "user", Content: "continue"},
		},
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", continuation)
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" ||
		!strings.Contains(validationErr.message, "could not be correlated") ||
		!strings.Contains(validationErr.message, "rather than falling back") {
		t.Fatalf("unmarked continuation route = %#v, error = %#v; want visible fail-closed refusal", route, validationErr)
	}
}

func TestMarkerlessActiveChildWithoutPromptRefusesAnthropicFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})
	initial := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	initialRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", initial)
	if validationErr != nil || !initialRoute.agentChildRouted || initialRoute.firstPartyAnthropic {
		t.Fatalf("markerless initial route = %#v, error = %#v; want CCR route", initialRoute, validationErr)
	}
	initialRoute.commitAgentChild()

	compacted := anthropicRequest{
		Model:    "haiku",
		Messages: []anthropicMessage{{Role: "user", Content: "continue after context compaction"}},
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", compacted)
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" ||
		!strings.Contains(validationErr.message, "could not be correlated") {
		t.Fatalf("markerless compacted continuation = %#v, error = %#v; want visible fail-closed refusal", route, validationErr)
	}
}

func TestAgentChildReservationRecognizesClaudeSubagentBillingMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}})
	child := anthropicRequest{
		Model:  "claude-sonnet-5",
		System: "x-anthropic-billing-header: cc_version=2.1.278; cc_entrypoint=sdk-cli; cc_is_subagent=true;",
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: "child task",
		}},
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("billing-marked child route = %#v, error = %#v; want active CCR route", route, validationErr)
	}
}

func TestAgentChildReservationUsesLatestUserBeforeTrailingSystemMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "child task"}})
	child := anthropicRequest{
		Model:  "claude-sonnet-5",
		System: "x-anthropic-billing-header: cc_version=2.1.278; cc_entrypoint=sdk-cli; cc_is_subagent=true;",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "system", Content: "native child billing context"},
		},
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child)
	if validationErr != nil || route.firstPartyAnthropic || route.model.Alias != "active" {
		t.Fatalf("trailing-system child route = %#v, error = %#v; want active CCR route", route, validationErr)
	}
}

func TestAgentChildReservationRollbackDoesNotEraseAnotherReservation(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("active")
	first := registerAgentChildForTest(t, &s, "session-a", "", []agentChildDescriptor{{}})
	if first == nil {
		t.Fatal("first registration returned nil")
	}
	second := registerAgentChildForTest(t, &s, "session-a", "", []agentChildDescriptor{{}})
	if second == nil {
		t.Fatal("second registration returned nil")
	}
	first()
	reservation := s.reserveAgentChild("session-a", anthropicRequest{Model: "sonnet"})
	if !reservation.pending || reservation.expired || reservation.ambiguous || reservation.rollback == nil {
		t.Fatal("second reservation was removed by the first rollback")
	}
}

func TestEquivalentChildReservationsAreAmbiguousWithoutChildIdentity(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "spawned", []agentChildDescriptor{
		{prompt: "same child", description: "same child"},
		{prompt: "same child", description: "same child"},
	})
	request := anthropicRequest{Model: "sonnet", Messages: []anthropicMessage{{Role: "user", Content: "same child"}}}
	reservation := selection.reserveAgentChild("session-a", request)
	if reservation.alias != "" || !reservation.ambiguous || reservation.expired {
		t.Fatalf("same-alias child reservation = %#v, want visible ambiguity", reservation)
	}
	selection.mu.RLock()
	remaining := selection.pendingAgentChildren["session-a"]
	selection.mu.RUnlock()
	if len(remaining) != 2 {
		t.Fatalf("equivalent child reservations after first selection = %#v, want both retained", remaining)
	}
}

func TestEquivalentChildReservationsWithDifferentAliasesRemainAmbiguous(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "first", []agentChildDescriptor{{prompt: "same child", description: "same child"}})
	registerAgentChildForTest(t, &selection, "session-a", "second", []agentChildDescriptor{{prompt: "same child", description: "same child"}})
	reservation := selection.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "same child"}},
	})
	if !reservation.ambiguous || reservation.alias != "" {
		t.Fatalf("different-alias child reservation = %#v, want visible ambiguity refusal", reservation)
	}
}

func TestAgentChildRegistrationRequiresSessionCorrelation(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	rollback, err := selection.registerAgentChildDescriptorsAtAliasChecked("", "active", []agentChildDescriptor{{prompt: "child task"}})
	if rollback != nil {
		t.Fatal("uncorrelated registration returned a rollback")
	}
	if err == nil || !strings.Contains(err.Error(), "session correlation") {
		t.Fatalf("uncorrelated registration error = %v, want session-correlation refusal", err)
	}
}

func TestAgentChildReservationPreservesActiveSibling(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("active")
	registerAgentChildForTest(t, &s, "session-a", "", []agentChildDescriptor{
		{prompt: "first child"},
		{prompt: "second child"},
	})
	firstReservation := s.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "first child"}},
	})
	if !firstReservation.pending || firstReservation.expired || firstReservation.ambiguous {
		t.Fatal("first child reservation was not activated")
	}

	secondReservation := s.reserveAgentChild("session-a", anthropicRequest{
		Model:    "haiku",
		System:   "You are a subagent continuing a child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "second child"}},
	})
	if !secondReservation.pending || secondReservation.expired || secondReservation.ambiguous || secondReservation.rollback == nil {
		t.Fatal("sibling child reservation was not selected")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.pendingAgentChildren["session-a"]
	if len(entries) != 2 {
		t.Fatalf("pending child reservations = %#v, want active sibling preserved", entries)
	}
	for _, entry := range entries {
		if !entry.active {
			t.Fatalf("pending child reservation %#v was not active", entry)
		}
	}
}

func TestAgentChildContinuationKeepsSpawnAliasAfterModelSwitch(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("startup")
	if cleanup := registerAgentChildForTest(t, &s, "session-a", "spawned", []agentChildDescriptor{{prompt: "child task"}}); cleanup == nil {
		t.Fatal("registerAgentChildDescriptorsAtAlias() returned nil")
	}

	firstReservation := s.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	})
	if !firstReservation.pending || firstReservation.expired || firstReservation.ambiguous || firstReservation.alias != "spawned" {
		t.Fatalf("initial child alias = %q, pending=%v expired=%v ambiguous=%v; want spawned", firstReservation.alias, firstReservation.pending, firstReservation.expired, firstReservation.ambiguous)
	}
	s.observe("session-a", messageRoute{model: store.Model{Alias: "new-model"}})

	continuationReservation := s.reserveAgentChild("session-a", anthropicRequest{
		Model:  "haiku",
		System: "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "content": "done"},
			}},
		},
	})
	if !continuationReservation.pending || continuationReservation.expired || continuationReservation.ambiguous || continuationReservation.alias != "spawned" {
		t.Fatalf("continuation child alias = %q, pending=%v expired=%v ambiguous=%v; want spawn-time alias", continuationReservation.alias, continuationReservation.pending, continuationReservation.expired, continuationReservation.ambiguous)
	}
}

func TestExplicitFirstPartyRequestClearsAliasWhileChildReservationIsActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})
	reservation := h.activeModel.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	})
	if reservation.alias != "active" || reservation.rollback == nil {
		t.Fatalf("child reservation = %#v, want active CCR route", reservation)
	}

	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil || !route.firstPartyAnthropic {
		t.Fatalf("explicit first-party route = %#v, error = %#v", route, validationErr)
	}
	if got := h.activeModel.currentAlias("session-a"); got != "" {
		t.Fatalf("active alias after explicit first-party request = %q, want cleared", got)
	}

	continuation := anthropicRequest{
		Model:  "haiku",
		System: "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "content": "done"},
			}},
		},
	}
	childRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", continuation)
	if validationErr != nil || childRoute.firstPartyAnthropic || childRoute.model.Alias != "active" {
		t.Fatalf("child continuation route = %#v, error = %#v; want spawn-time alias", childRoute, validationErr)
	}
}

func TestReservationRoutedChildDoesNotOverwriteNewParentAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "switched", ProviderName: "fixture", ProviderModel: "model-switched", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})

	child := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	childRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", child)
	if validationErr != nil || childRoute.firstPartyAnthropic || !childRoute.agentChildRouted || childRoute.model.Alias != "active" {
		t.Fatalf("child route = %#v, error = %#v; want reservation-routed active alias", childRoute, validationErr)
	}
	childRoute.commitAgentChild()

	if switchedRoute, switchValidationErr := h.selectMessageRouteForRequest(ctx, "session-a", autoModeClassifierRequest("switched")); switchValidationErr != nil || switchedRoute.model.Alias != "switched" {
		t.Fatalf("parent switch route = %#v, error = %#v", switchedRoute, switchValidationErr)
	}
	if got := h.activeModel.currentAlias("session-a"); got != "switched" {
		t.Fatalf("active alias after parent switch = %q, want switched", got)
	}

	continuation := anthropicRequest{
		Model:  "haiku",
		System: "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "child task"},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "content": "done"},
			}},
		},
	}
	continuationRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", continuation)
	if validationErr != nil || !continuationRoute.agentChildRouted || continuationRoute.model.Alias != "active" {
		t.Fatalf("child continuation route = %#v, error = %#v; want spawn-time active alias", continuationRoute, validationErr)
	}
	continuationRoute.commitAgentChild()
	if got := h.activeModel.currentAlias("session-a"); got != "switched" {
		t.Fatalf("active alias after child continuation = %q, want switched", got)
	}
}
