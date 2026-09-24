package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestActiveChildDoesNotCaptureParentSubagentMention(t *testing.T) {
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
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	childRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", initial)
	if validationErr != nil || !childRoute.agentChildRouted {
		t.Fatalf("initial child route = %#v, error = %#v", childRoute, validationErr)
	}
	childRoute.commitAgentChild()

	parent := anthropicRequest{
		Model:    "sonnet",
		System:   "ordinary parent system",
		Messages: []anthropicMessage{{Role: "user", Content: "Summarize what the subagent did."}},
	}
	parentRoute, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", parent)
	if validationErr != nil || !parentRoute.firstPartyAnthropic || parentRoute.agentChildRouted {
		t.Fatalf("unmarked parent request was not preserved = %#v, error = %#v", parentRoute, validationErr)
	}
}

func TestActiveChildDoesNotRejectParentContinuationWithChildToolHistory(t *testing.T) {
	t.Parallel()
	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})
	initial := selection.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the child task.",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	})
	if initial.alias != "active" || initial.rollback == nil {
		t.Fatalf("initial child reservation = %#v; want active route", initial)
	}

	parent := anthropicRequest{
		Model:  "sonnet",
		System: "ordinary parent system",
		Messages: []anthropicMessage{
			{Role: "user", Content: "Start the child task."},
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "name": "Agent",
				"input": map[string]any{"prompt": "child task"},
			}}},
			{Role: "user", Content: []any{map[string]any{
				"type": "tool_result", "content": "child completed",
			}}},
			{Role: "user", Content: "Continue the parent task."},
		},
	}
	reservation := selection.reserveAgentChild("session-a", parent)
	if reservation.pending || reservation.identified {
		t.Fatalf("parent continuation with retained child tool history was identified as a child: %#v", reservation)
	}
}

func TestActiveDynamicWorkflowDoesNotRejectParentWorkflowContinuation(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "active", []agentChildDescriptor{{workflow: true}})
	child := selection.reserveAgentChild("session-a", anthropicRequest{
		Model:  "sonnet",
		System: "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: "run the dynamically generated worker",
		}},
	})
	if child.alias != "active" || child.rollback == nil {
		t.Fatalf("dynamic workflow child reservation = %#v; want active CCR route", child)
	}

	parent := anthropicRequest{
		Model:  "sonnet",
		System: "ordinary parent system",
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "name": "Workflow",
				"input": map[string]any{"script": "await agent(dynamicPrompt)"},
			}}},
			{Role: "user", Content: "Continue the parent workflow."},
		},
	}
	reservation := selection.reserveAgentChild("session-a", parent)
	if reservation.pending || reservation.identified || reservation.alias != "" {
		t.Fatalf("parent workflow continuation was identified as dynamic child: %#v", reservation)
	}
}

func TestAgentChildContinuationIdentityIgnoresMutableSystemEnvelope(t *testing.T) {
	t.Parallel()

	descriptor := agentChildDescriptor{model: "haiku", prompt: "child task", description: "research"}
	initial := anthropicRequest{
		Model:    "sonnet",
		System:   "cc_is_subagent=true; child execution envelope version=1",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	continued := anthropicRequest{
		Model:    "haiku",
		System:   "cc_is_subagent=true; child execution envelope version=2; refreshed context",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	}
	identity := agentChildContinuationIdentity(initial, descriptor)
	if identity == "" || identity != agentChildContinuationIdentity(continued, descriptor) {
		t.Fatalf("continuation identity changed with mutable system envelope: initial=%q continued=%q", identity, agentChildContinuationIdentity(continued, descriptor))
	}
	if !agentChildContinuationMatches(continued, pendingAgentChild{descriptor: descriptor, continuationIdentity: identity}) {
		t.Fatal("continuation with the stable child marker did not match its reservation")
	}
}

func TestActiveChildDoesNotRejectOrdinaryParentHistory(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "active", []agentChildDescriptor{{prompt: "child task"}})
	initial := selection.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "child task"}},
	})
	if initial.alias != "active" || initial.rollback == nil {
		t.Fatalf("initial child reservation = %#v; want active route", initial)
	}
	initial.rollback()

	// This is ordinary native parent history. It has multiple messages but no
	// child prompt, child tool transcript, or native child identity marker.
	parent := anthropicRequest{
		Model:  "sonnet",
		System: "ordinary parent system",
		Messages: []anthropicMessage{
			{Role: "user", Content: "Discuss the repository architecture."},
			{Role: "assistant", Content: "The repository has several boundaries."},
			{Role: "user", Content: "Continue with the deployment risks."},
		},
	}
	reservation := selection.reserveAgentChild("session-a", parent)
	if reservation.pending || reservation.identified {
		t.Fatalf("ordinary parent history was identified as a child: %#v", reservation)
	}
}

func TestAgentChildReservationExpiresWithoutRoutingUnrelatedRequest(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("active")
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:         1,
		descriptor: agentChildDescriptor{prompt: "expired"},
		expiresAt:  time.Now().Add(-time.Second),
	}}
	s.mu.Unlock()
	reservation := s.reserveAgentChild("session-a", anthropicRequest{Model: "sonnet", Messages: []anthropicMessage{{Role: "user", Content: "expired"}}})
	if reservation.pending || reservation.expired || reservation.ambiguous {
		t.Fatal("expired Agent reservation was still used")
	}
}

func TestAgentChildReservationRollbackIsIdempotent(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	rollback, err := selection.registerAgentChildDescriptorsAtAliasChecked(
		"session-a", "active", []agentChildDescriptor{{prompt: "child task"}},
	)
	if err != nil {
		t.Fatalf("registerAgentChildDescriptorsAtAliasChecked() error = %v", err)
	}
	rollback()
	rollback()
	selection.mu.RLock()
	defer selection.mu.RUnlock()
	if len(selection.pendingAgentChildren) != 0 || len(selection.childRoutingUntil) != 0 {
		t.Fatalf("reservation state after repeated rollback = pending:%#v routing:%#v; want empty", selection.pendingAgentChildren, selection.childRoutingUntil)
	}
}

func TestExpiredAgentChildReservationRefusesAnthropicFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	now := time.Now()
	h.activeModel.mu.Lock()
	h.activeModel.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:          1,
		descriptor:  agentChildDescriptor{prompt: "delayed child"},
		expiresAt:   now.Add(-time.Second),
		rejectUntil: now.Add(time.Minute),
	}}
	h.activeModel.mu.Unlock()

	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "delayed child"}},
	})
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" {
		t.Fatalf("expired child route = %#v, error = %#v; want visible service-unavailable refusal", route, validationErr)
	}
}

func TestChildTombstoneDoesNotRejectOrdinaryParentRequest(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	selection.mu.Lock()
	selection.childRoutingUntil["session-a"] = time.Now().Add(time.Minute)
	selection.mu.Unlock()
	reservation := selection.reserveAgentChild("session-a", anthropicRequest{
		Model:    "sonnet",
		System:   "ordinary parent system",
		Messages: []anthropicMessage{{Role: "user", Content: "continue the parent conversation"}},
	})
	if reservation.pending || reservation.identified || reservation.expired || reservation.ambiguous {
		t.Fatalf("ordinary parent request was blocked by child tombstone: %#v", reservation)
	}
}

func TestActiveAgentChildReservationSurvivesLongIdleUntilSessionEnd(t *testing.T) {
	t.Parallel()

	s := newActiveModelSelection("active")
	descriptor := agentChildDescriptor{prompt: "long-running child"}
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the long-running child.",
		Messages: []anthropicMessage{{Role: "user", Content: "long-running child"}},
	}
	identity := agentChildContinuationIdentity(req, descriptor)
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "active",
		continuationIdentity: identity,
		active:               true,
		expiresAt:            time.Time{},
		rejectUntil:          time.Time{},
	}}
	s.mu.Unlock()

	reservation := s.reserveAgentChild("session-a", req)
	if reservation.alias != "active" || reservation.expired || reservation.ambiguous {
		t.Fatalf("active child reservation = %#v, want active continuation", reservation)
	}
	s.mu.RLock()
	entry := s.pendingAgentChildren["session-a"][0]
	s.mu.RUnlock()
	if !entry.expiresAt.After(time.Now()) || !entry.rejectUntil.After(entry.expiresAt) {
		t.Fatalf("active child lease = expires:%s reject:%s, want bounded renewable lease", entry.expiresAt, entry.rejectUntil)
	}
	s.observeAgentChildLifecycle("session-a", "SessionEnd")
	s.mu.RLock()
	_, exists := s.pendingAgentChildren["session-a"]
	s.mu.RUnlock()
	if exists {
		t.Fatal("SessionEnd did not release the active child reservation")
	}
}

func TestAgentChildLifecycleRefreshesPendingBackgroundReservation(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("active")
	now := time.Now()
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:          1,
		descriptor:  agentChildDescriptor{prompt: "background child"},
		spawnAlias:  "active",
		expiresAt:   now.Add(-time.Second),
		rejectUntil: now.Add(time.Second),
	}}
	s.mu.Unlock()

	s.observeAgentChildLifecycle("session-a", "SubagentStart")
	s.mu.RLock()
	entry := s.pendingAgentChildren["session-a"][0]
	s.mu.RUnlock()
	if !entry.expiresAt.After(time.Now()) || !entry.rejectUntil.After(entry.expiresAt) {
		t.Fatalf("pending background reservation was not refreshed: %#v", entry)
	}
}

func TestAgentChildLifecycleDoesNotRefreshAfterChildWorkEnds(t *testing.T) {
	t.Parallel()
	for _, eventName := range []string{"SessionStart", "SubagentStop", "TaskCompleted", "TeammateIdle", "StopFailure"} {
		s := newActiveModelSelection("active")
		now := time.Now()
		s.mu.Lock()
		s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
			id:          1,
			descriptor:  agentChildDescriptor{prompt: "completed child"},
			spawnAlias:  "active",
			expiresAt:   now.Add(-time.Second),
			rejectUntil: now.Add(time.Second),
		}}
		s.mu.Unlock()

		s.observeAgentChildLifecycle("session-a", eventName)
		s.mu.RLock()
		entry := s.pendingAgentChildren["session-a"][0]
		s.mu.RUnlock()
		if entry.expiresAt.After(now) {
			t.Errorf("%s refreshed an unstarted reservation: %#v", eventName, entry)
		}
	}
}

func TestExpiredActiveSiblingSurvivesInitialChildSelection(t *testing.T) {
	t.Parallel()

	now := time.Now()
	descriptor := agentChildDescriptor{prompt: "shared child prompt"}
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the shared child prompt.",
		Messages: []anthropicMessage{{Role: "user", Content: "shared child prompt"}},
	}
	active := pendingAgentChild{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "active",
		continuationIdentity: agentChildContinuationIdentity(req, descriptor),
		active:               true,
		expiresAt:            now.Add(-time.Second),
		rejectUntil:          now.Add(time.Minute),
	}
	initial := pendingAgentChild{
		id:          2,
		descriptor:  descriptor,
		spawnAlias:  "active",
		expiresAt:   now.Add(time.Minute),
		rejectUntil: now.Add(time.Minute),
	}
	kept, selected, expired, ambiguous := selectPendingAgentChild([]pendingAgentChild{active, initial}, req, now)
	if selected.id != initial.id || expired || ambiguous {
		t.Fatalf("selection = selected:%#v expired:%v ambiguous:%v; want initial child selected", selected, expired, ambiguous)
	}
	if len(kept) != 2 || !containsPendingAgentChild(kept, active.id) || !containsPendingAgentChild(kept, initial.id) {
		t.Fatalf("kept reservations = %#v; want initial and expired active sibling retained", kept)
	}
}

func TestAgentChildContinuationWithIndistinguishableSiblingIsAmbiguous(t *testing.T) {
	t.Parallel()
	now := time.Now()
	descriptor := agentChildDescriptor{prompt: "shared child prompt"}
	continuation := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the shared child prompt.",
		Messages: []anthropicMessage{{Role: "user", Content: "shared child prompt"}},
	}
	active := pendingAgentChild{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "existing-child",
		continuationIdentity: agentChildContinuationIdentity(continuation, descriptor),
		active:               true,
		expiresAt:            now.Add(time.Minute),
		rejectUntil:          now.Add(time.Minute),
	}
	sibling := pendingAgentChild{
		id:          2,
		descriptor:  descriptor,
		spawnAlias:  "new-sibling",
		expiresAt:   now.Add(time.Minute),
		rejectUntil: now.Add(time.Minute),
	}
	kept, selected, expired, ambiguous := selectPendingAgentChild([]pendingAgentChild{active, sibling}, continuation, now)
	if selected.id != 0 || expired || !ambiguous {
		t.Fatalf("selection = selected:%#v expired:%v ambiguous:%v; want visible ambiguity", selected, expired, ambiguous)
	}
	if len(kept) != 2 || !containsPendingAgentChild(kept, active.id) || !containsPendingAgentChild(kept, sibling.id) {
		t.Fatalf("kept reservations = %#v; want both indistinguishable reservations preserved", kept)
	}
}

func containsPendingAgentChild(entries []pendingAgentChild, id uint64) bool {
	for _, entry := range entries {
		if entry.id == id {
			return true
		}
	}
	return false
}

func TestUnmatchedIdentifiedAgentChildRefusesAnthropicFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "", []agentChildDescriptor{{prompt: "expected child task"}})

	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{
		Model:  "haiku",
		System: "Claude Code subagent execution envelope",
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: "wrapped child request without its original prompt",
		}},
	})
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" ||
		!strings.Contains(validationErr.message, "could not be correlated") ||
		!strings.Contains(validationErr.message, "rather than falling back") {
		t.Fatalf("unmatched identified child route = %#v, error = %#v; want visible refusal", route, validationErr)
	}
}

func TestExpiredAgentChildReservationsAreEvictedByCleanup(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("active")
	now := time.Now()
	s.mu.Lock()
	s.pendingAgentChildren["expired"] = []pendingAgentChild{{rejectUntil: now.Add(-time.Second)}}
	s.pendingAgentChildren["live"] = []pendingAgentChild{{rejectUntil: now.Add(time.Minute)}}
	s.childRoutingUntil["expired"] = now.Add(-time.Second)
	s.childRoutingUntil["live"] = now.Add(time.Minute)
	s.mu.Unlock()
	s.cleanupExpiredAgentChildren(now)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, exists := s.pendingAgentChildren["expired"]; exists {
		t.Fatal("expired Agent reservations were not evicted")
	}
	if len(s.pendingAgentChildren["live"]) != 1 {
		t.Fatal("live Agent reservation was evicted")
	}
	if _, exists := s.childRoutingUntil["expired"]; exists {
		t.Fatal("expired Agent routing tombstone was not evicted")
	}
	if !s.childRoutingUntil["live"].After(now) {
		t.Fatal("live Agent routing tombstone was evicted")
	}
}

func TestExpiredActiveAgentChildReservationRemainsFailClosedAfterCleanup(t *testing.T) {
	t.Parallel()
	h := handler{cfg: Config{}}
	h.activeModel.defaultAlias = "active"
	h.activeModel.pendingAgentChildren = make(map[string][]pendingAgentChild)
	h.activeModel.childRoutingUntil = make(map[string]time.Time)
	h.activeModel.childRoutingFailClosed = make(map[string]time.Time)
	s := &h.activeModel
	now := time.Now()
	descriptor := agentChildDescriptor{prompt: "delayed child"}
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the delayed child.",
		Messages: []anthropicMessage{{Role: "user", Content: "delayed child"}},
	}
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "active",
		continuationIdentity: agentChildContinuationIdentity(req, descriptor),
		active:               true,
		expiresAt:            now.Add(-time.Second),
		rejectUntil:          now.Add(-time.Second),
	}}
	s.mu.Unlock()
	s.cleanupExpiredAgentChildren(now.Add(agentChildActiveLeaseTTL + agentChildReservationRetention + time.Minute))

	s.mu.RLock()
	entries := s.pendingAgentChildren["session-a"]
	_, failClosed := s.childRoutingFailClosed["session-a"]
	s.mu.RUnlock()
	if len(entries) != 0 || !failClosed {
		t.Fatalf("active expired reservation = %#v fail_closed=%v; want descriptor eviction with bounded marker", entries, failClosed)
	}

	route, validationErr := h.selectMessageRouteForRequest(context.Background(), "session-a", req)
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable || route.model.Alias != "" {
		t.Fatalf("expired active child route = %#v, error = %#v; want visible refusal", route, validationErr)
	}
}

func TestExpiredPendingAgentChildReservationRemainsFailClosedAfterCleanup(t *testing.T) {
	t.Parallel()
	s := newActiveModelSelection("")
	now := time.Now()
	request := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the delayed child.",
		Messages: []anthropicMessage{{Role: "user", Content: "delayed child"}},
	}
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:          1,
		descriptor:  agentChildDescriptor{prompt: "delayed child"},
		spawnAlias:  "open-source",
		expiresAt:   now.Add(-time.Second),
		rejectUntil: now.Add(-time.Second),
	}}
	s.mu.Unlock()

	s.cleanupExpiredAgentChildren(now.Add(agentChildReservationTTL + agentChildReservationRetention + time.Minute))

	s.mu.RLock()
	entries := s.pendingAgentChildren["session-a"]
	_, failClosed := s.childRoutingFailClosed["session-a"]
	s.mu.RUnlock()
	if len(entries) != 0 || !failClosed {
		t.Fatalf("pending expired reservation = %#v fail_closed=%v; want descriptor eviction with bounded marker", entries, failClosed)
	}

	reservation := s.reserveAgentChild("session-a", request)
	if !reservation.identified || reservation.pending || reservation.expired {
		t.Fatalf("expired pending child reservation = %#v; want identified fail-closed reservation", reservation)
	}
}

func TestUnreservedIdentifiedChildRefusesWhenCCRAliasIsActive(t *testing.T) {
	t.Parallel()

	h := handler{cfg: Config{}}
	h.activeModel = newActiveModelSelection("active")
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing work after the provider reservation was lost.",
		Messages: []anthropicMessage{{Role: "user", Content: "continue the child"}},
	}
	reservation := h.activeModel.reserveAgentChild("session-a", req)
	if !reservation.identified || reservation.pending {
		t.Fatalf("unreserved identified child reservation = %#v; want bounded fail-closed identification", reservation)
	}
	_, validationErr := h.selectMessageRouteForRequest(t.Context(), "session-a", req)
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable {
		t.Fatalf("unreserved identified child validation error = %#v; want visible refusal", validationErr)
	}
}

func TestIdentifiedChildWithoutSessionCorrelationRefuses(t *testing.T) {
	t.Parallel()

	h := handler{cfg: Config{}, activeModel: newActiveModelSelection("active")}
	req := anthropicRequest{
		Model:  "sonnet",
		System: "You are a subagent continuing work after an Agent spawn.",
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: "continue the child",
		}},
	}
	reservation := h.activeModel.reserveAgentChild("", req)
	if !reservation.identified || reservation.pending {
		t.Fatalf("uncorrelated identified child reservation = %#v; want visible refusal", reservation)
	}
	_, validationErr := h.selectMessageRouteForRequest(t.Context(), "", req)
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable ||
		!strings.Contains(validationErr.message, "could not be correlated") {
		t.Fatalf("uncorrelated identified child validation error = %#v; want session-correlation refusal", validationErr)
	}
}

func TestOrdinaryParentSystemMentionDoesNotIdentifyChild(t *testing.T) {
	t.Parallel()

	selection := newActiveModelSelection("active")
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "The parent should explain the subagent plan before continuing.",
		Messages: []anthropicMessage{{Role: "user", Content: "summarize the plan"}},
	}
	reservation := selection.reserveAgentChild("session-a", req)
	if reservation.identified || reservation.pending {
		t.Fatalf("ordinary parent system mention = %#v; want no child identification", reservation)
	}
}
