package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRouteAndLifecycleEventRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	sessionID, err := s.UpsertClaudeSession(ctx, ClaudeSession{
		LaunchID: launchID, ClaudeSessionID: "claude-session-1", Source: "startup",
	})
	if err != nil {
		t.Fatalf("UpsertClaudeSession() error = %v", err)
	}
	eventID, err := s.BeginRouteEvent(ctx, RouteEvent{
		LaunchID: launchID, RequestID: "request-1", Operation: "messages",
		RequestedModel: "ccr-coder", Streaming: true, Tools: true,
	})
	if err != nil {
		t.Fatalf("BeginRouteEvent() error = %v", err)
	}
	if resolveErr := s.ResolveRouteEvent(ctx, eventID, sessionID, RouteEvent{
		RouteKind: "registered", ModelAlias: "coder", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	}); resolveErr != nil {
		t.Fatalf("ResolveRouteEvent() error = %v", resolveErr)
	}
	usage := TokenUsage{Observed: true, InputTokens: 12, OutputTokens: 7}
	if completeErr := s.CompleteRouteEvent(ctx, eventID, "succeeded", 200, "", 25*time.Millisecond, usage); completeErr != nil {
		t.Fatalf("CompleteRouteEvent() error = %v", completeErr)
	}
	if _, recordErr := s.RecordLifecycleEvent(ctx, LifecycleEvent{
		LaunchID: launchID, SessionID: sessionID, Name: "SubagentStart",
		Status: "running", ExternalID: "agent-1", ActorKind: "Explore",
	}); recordErr != nil {
		t.Fatalf("RecordLifecycleEvent() error = %v", recordErr)
	}

	events, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ListTraceEvents() length = %d, want 2", len(events))
	}
	route := events[1].Route
	if route.ModelAlias != "coder" || route.ProviderModel != "model-v1" ||
		route.HTTPStatus != 200 || !route.Usage.Observed || route.Usage.OutputTokens != 7 {
		t.Fatalf("route event = %#v", route)
	}
	if events[0].Lifecycle.ExternalID != "agent-1" {
		t.Fatalf("lifecycle event = %#v", events[0].Lifecycle)
	}
	routes, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, Kind: "route", Limit: 1})
	if err != nil || len(routes) != 1 || routes[0].ID != eventID {
		t.Fatalf("route-only ListTraceEvents() = %#v, error = %v", routes, err)
	}
}

func TestEventRetentionAndPurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	for i, requestID := range []string{"one", "two", "three", "four", "five"} {
		if _, beginErr := s.BeginRouteEvent(ctx, RouteEvent{
			LaunchID: launchID, RequestID: requestID, Operation: "messages",
		}); beginErr != nil {
			t.Fatalf("BeginRouteEvent(%d) error = %v", i, beginErr)
		}
	}
	deleted, err := s.PruneEvents(ctx, 30*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("PruneEvents() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("PruneEvents() deleted = %d, want 2", deleted)
	}
	events, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil || len(events) != 3 {
		t.Fatalf("ListTraceEvents() = %#v, %v", events, err)
	}

	deleted, err = s.PurgeEvents(ctx, "", true)
	if err != nil {
		t.Fatalf("PurgeEvents(all) error = %v", err)
	}
	if deleted != 3 {
		t.Fatalf("PurgeEvents(all) deleted = %d, want 3", deleted)
	}
	if _, err := s.PurgeEvents(ctx, "", false); err == nil {
		t.Fatal("PurgeEvents() error = nil, want validation error")
	}
}

func TestTraceTimeFiltersUseChronologicalTimestampOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	times := []time.Time{
		time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 18, 12, 0, 0, 500_000_000, time.UTC),
		time.Date(2026, 7, 18, 12, 0, 1, 0, time.UTC),
	}
	ids := make([]int64, 0, len(times))
	for index, occurredAt := range times {
		id, beginErr := s.BeginRouteEvent(ctx, RouteEvent{
			LaunchID: launchID, RequestID: fmt.Sprintf("request-%d", index), Operation: "messages",
		})
		if beginErr != nil {
			t.Fatalf("BeginRouteEvent(%d) error = %v", index, beginErr)
		}
		if _, updateErr := s.db.ExecContext(ctx,
			`UPDATE event_log SET occurred_at = ? WHERE id = ?`,
			formatRuntimeTimestamp(occurredAt), id); updateErr != nil {
			t.Fatalf("updating event %d timestamp: %v", id, updateErr)
		}
		ids = append(ids, id)
	}

	events, err := s.ListTraceEvents(ctx, TraceFilter{
		LaunchID: launchID, Since: "2026-07-18T12:00:00.25Z", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 2 || events[0].ID != ids[2] || events[1].ID != ids[1] {
		t.Fatalf("ListTraceEvents() = %#v, want events %d and %d", events, ids[2], ids[1])
	}
	deleted, err := s.PurgeEvents(ctx, "2026-07-18T12:00:00.75Z", false)
	if err != nil {
		t.Fatalf("PurgeEvents() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("PurgeEvents() deleted = %d, want 2", deleted)
	}
	events, err = s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil || len(events) != 1 || events[0].ID != ids[2] {
		t.Fatalf("remaining events = %#v, error = %v", events, err)
	}
}

func TestTraceTimeFiltersRejectInvalidTimestamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	if _, err := s.ListTraceEvents(ctx, TraceFilter{Since: "not-a-timestamp"}); err == nil {
		t.Fatal("ListTraceEvents() error = nil, want invalid timestamp error")
	}
	if _, err := s.PurgeEvents(ctx, "not-a-timestamp", false); err == nil {
		t.Fatal("PurgeEvents() error = nil, want invalid timestamp error")
	}
}

func TestListTraceEventsAfterIDPaginatesOldestFirstWithoutGaps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	for _, requestID := range []string{"one", "two", "three", "four", "five"} {
		if _, beginErr := s.BeginRouteEvent(ctx, RouteEvent{
			LaunchID: launchID, RequestID: requestID, Operation: "messages",
		}); beginErr != nil {
			t.Fatalf("BeginRouteEvent(%s) error = %v", requestID, beginErr)
		}
	}

	first, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, AfterID: 1, Limit: 2})
	if err != nil {
		t.Fatalf("ListTraceEvents(first page) error = %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("ListTraceEvents(first page) = %#v, want two events", first)
	}
	second, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID, AfterID: first[len(first)-1].ID, Limit: 2})
	if err != nil {
		t.Fatalf("ListTraceEvents(second page) error = %v", err)
	}
	if first[0].ID != 2 || first[1].ID != 3 || len(second) != 2 ||
		second[0].ID != 4 || second[1].ID != 5 {
		t.Fatalf("trace pages = %#v then %#v", first, second)
	}
}

func TestAbandonLaunchCompletesStartedRouteEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	if _, beginErr := s.BeginRouteEvent(ctx, RouteEvent{
		LaunchID: launchID, RequestID: "request-1", Operation: "messages",
	}); beginErr != nil {
		t.Fatalf("BeginRouteEvent() error = %v", beginErr)
	}
	if abandonErr := s.AbandonLaunchRuntime(ctx, launchID); abandonErr != nil {
		t.Fatalf("AbandonLaunchRuntime() error = %v", abandonErr)
	}
	events, err := s.ListTraceEvents(ctx, TraceFilter{LaunchID: launchID})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].Status != "abandoned" ||
		events[0].Route.ErrorClass != "launch_exit" || events[0].CompletedAt == "" {
		t.Fatalf("ListTraceEvents() = %#v", events)
	}
}
