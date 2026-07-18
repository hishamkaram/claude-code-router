package observability

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestRecorderPersistsRedactedRouteAndLifecycleMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := recorderStore(t, ctx)
	recorder := NewRecorder(ctx, Config{Store: s, LaunchID: launchID, Enabled: true})
	span := recorder.BeginRoute(ctx, RouteStart{
		Operation: "messages", RequestedModel: "ccr-coder", Streaming: true,
		Tools: true, Thinking: true,
	})
	if span.RequestID() == "" {
		t.Fatal("RequestID() is empty")
	}
	span.Resolve(ctx, RouteResolution{
		RouteKind: "registered", ModelAlias: "coder", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	})
	span.Complete(ctx, RouteResult{
		Status: "succeeded", HTTPStatus: 200,
		Usage: TokenUsage{Observed: true, InputTokens: 10, OutputTokens: 4},
	})
	recorder.RecordLifecycle(ctx, LifecycleEvent{Name: "SessionStart", Status: "active"})

	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 10})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 2 || events[1].Route.ProviderModel != "model-v1" ||
		events[1].Route.Usage.InputTokens != 10 {
		t.Fatalf("ListTraceEvents() = %#v", events)
	}
	if snapshot := recorder.Snapshot(); !snapshot.Healthy || snapshot.WriteFailures != 0 {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
}

func TestRecorderDisabledWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := recorderStore(t, ctx)
	recorder := NewRecorder(ctx, Config{Store: s, LaunchID: launchID, Enabled: false})
	span := recorder.BeginRoute(ctx, RouteStart{Operation: "messages"})
	span.Complete(ctx, RouteResult{Status: "succeeded", HTTPStatus: 200})
	recorder.RecordLifecycle(ctx, LifecycleEvent{Name: "SessionStart", Status: "active"})
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ListTraceEvents() = %#v, want none", events)
	}
	if snapshot := recorder.Snapshot(); snapshot.Enabled || !snapshot.Healthy {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
}

func TestRecorderDegradesWithoutBreakingCallers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := recorderStore(t, ctx)
	recorder := NewRecorder(ctx, Config{Store: s, LaunchID: launchID, Enabled: true})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	span := recorder.BeginRoute(ctx, RouteStart{Operation: "messages"})
	span.Resolve(ctx, RouteResolution{ModelAlias: "coder"})
	span.Complete(ctx, RouteResult{Status: "failed", HTTPStatus: 500})
	snapshot := recorder.Snapshot()
	if snapshot.Healthy || snapshot.WriteFailures == 0 || snapshot.LastError == "" {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
}

func TestRouteSpanCompletesOnlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := recorderStore(t, ctx)
	recorder := NewRecorder(ctx, Config{Store: s, LaunchID: launchID, Enabled: true})
	span := recorder.BeginRoute(ctx, RouteStart{Operation: "messages"})
	span.Complete(ctx, RouteResult{Status: "succeeded", HTTPStatus: 200})
	span.Complete(ctx, RouteResult{Status: "failed", HTTPStatus: 500})
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].Status != "succeeded" || events[0].Route.HTTPStatus != 200 {
		t.Fatalf("ListTraceEvents() = %#v", events)
	}
}

func recorderStore(t *testing.T, ctx context.Context) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	return s, launchID
}
