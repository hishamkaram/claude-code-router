package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestTrackerNormalizesLifecycleState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := trackerStore(t, ctx)
	recorder := observability.NewRecorder(ctx, observability.Config{
		Store: s, LaunchID: launchID, Enabled: true,
	})
	tracker, err := NewTracker(Config{
		Store: s, Recorder: recorder, LaunchID: launchID,
		Enabled: true, DefaultModelAlias: "coder",
	})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	tracker.ObserveRoute(ctx, Route{
		Kind: "registered", ModelAlias: "coder", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	})
	events := []HookEvent{
		{SessionID: "session-1", HookEventName: "SessionStart", Source: "startup"},
		{SessionID: "session-1", HookEventName: "SubagentStart", AgentID: "agent-1", AgentType: "Explore"},
		{SessionID: "session-1", HookEventName: "TaskCreated", TaskID: "task-1"},
		{SessionID: "session-1", HookEventName: "TeammateIdle", TeammateName: "worker", TeamName: "backend"},
		{SessionID: "session-1", HookEventName: "SubagentStop", AgentID: "agent-1", AgentType: "Explore"},
		{SessionID: "session-1", HookEventName: "TaskCompleted", TaskID: "task-1", TeammateName: "worker", TeamName: "backend"},
		{SessionID: "session-1", HookEventName: "SessionEnd", Reason: "clear"},
	}
	for _, event := range events {
		if hookErr := tracker.HandleHook(ctx, event); hookErr != nil {
			t.Fatalf("HandleHook(%s) error = %v", event.HookEventName, hookErr)
		}
	}

	snapshot := tracker.Snapshot()
	if snapshot.LifecycleState != "observed" || snapshot.CurrentSession.State != "ended" ||
		snapshot.Route.ModelAlias != "coder" || snapshot.ActiveTasks != 0 || snapshot.ActiveAgents != 1 {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	sessions, err := s.ListClaudeSessions(ctx, launchID, false)
	if err != nil || len(sessions) != 1 || sessions[0].ActiveProviderModel != "model-v1" {
		t.Fatalf("ListClaudeSessions() = %#v, %v", sessions, err)
	}
	agents, err := s.ListRuntimeAgents(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(agents) != 2 {
		t.Fatalf("ListRuntimeAgents() = %#v, %v", agents, err)
	}
	tasks, err := s.ListTasks(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(tasks) != 1 || tasks[0].Status != "completed" ||
		tasks[0].TeammateName != "worker" || tasks[0].TeamName != "backend" {
		t.Fatalf("ListTasks() = %#v, %v", tasks, err)
	}
	traces, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 20})
	if err != nil || len(traces) != len(events) {
		t.Fatalf("ListTraceEvents() length = %d, error = %v", len(traces), err)
	}
}

func TestTrackerRecoversMissingSessionAndRecordsStopFailureSafely(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := trackerStore(t, ctx)
	tracker, err := NewTracker(Config{Store: s, LaunchID: launchID, Enabled: true})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	if hookErr := tracker.HandleHook(ctx, HookEvent{
		SessionID: "session-1", HookEventName: "StopFailure",
	}); hookErr != nil {
		t.Fatalf("HandleHook() error = %v", hookErr)
	}
	snapshot := tracker.Snapshot()
	if snapshot.LifecycleState != "error" || snapshot.LastError != "Claude reported a stop failure" {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	sessions, err := s.ListClaudeSessions(ctx, launchID, false)
	if err != nil || len(sessions) != 1 || sessions[0].Source != "hook-recovery" {
		t.Fatalf("ListClaudeSessions() = %#v, %v", sessions, err)
	}
}

func TestTrackerCompletionPreservesSpawnRouteAndMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := trackerStore(t, ctx)
	tracker, err := NewTracker(Config{Store: s, LaunchID: launchID, Enabled: true})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	tracker.ObserveRoute(ctx, Route{ModelAlias: "spawn-model"})
	for _, event := range []HookEvent{
		{SessionID: "session-1", HookEventName: "SessionStart", Source: "startup"},
		{SessionID: "session-1", HookEventName: "SubagentStart", AgentID: "agent-1", AgentType: "Explore"},
		{SessionID: "session-1", HookEventName: "TaskCreated", TaskID: "task-1", TeammateName: "spawn-worker", TeamName: "spawn-team"},
	} {
		if hookErr := tracker.HandleHook(ctx, event); hookErr != nil {
			t.Fatalf("HandleHook(%s) error = %v", event.HookEventName, hookErr)
		}
	}
	tracker.ObserveRoute(ctx, Route{ModelAlias: "later-model"})
	for _, event := range []HookEvent{
		{SessionID: "session-1", HookEventName: "SubagentStop", AgentID: "agent-1", AgentType: "General"},
		{SessionID: "session-1", HookEventName: "TaskCompleted", TaskID: "task-1", TeammateName: "later-worker", TeamName: "later-team"},
	} {
		if hookErr := tracker.HandleHook(ctx, event); hookErr != nil {
			t.Fatalf("HandleHook(%s) error = %v", event.HookEventName, hookErr)
		}
	}
	sessions, err := s.ListClaudeSessions(ctx, launchID, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListClaudeSessions() = %#v, error = %v", sessions, err)
	}
	agents, err := s.ListRuntimeAgents(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(agents) != 1 {
		t.Fatalf("ListRuntimeAgents() = %#v, error = %v", agents, err)
	}
	if agents[0].ModelAlias != "spawn-model" || agents[0].Name != "Explore" || agents[0].Status != "completed" {
		t.Fatalf("completed agent = %#v", agents[0])
	}
	tasks, err := s.ListTasks(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasks() = %#v, error = %v", tasks, err)
	}
	if tasks[0].ModelAlias != "spawn-model" || tasks[0].TeammateName != "spawn-worker" ||
		tasks[0].TeamName != "spawn-team" || tasks[0].Status != "completed" {
		t.Fatalf("completed task = %#v", tasks[0])
	}
}

func TestTrackerCompletionRecoversMissingStartEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := trackerStore(t, ctx)
	tracker, err := NewTracker(Config{
		Store: s, LaunchID: launchID, Enabled: true, DefaultModelAlias: "recovered-model",
	})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	for _, event := range []HookEvent{
		{SessionID: "session-1", HookEventName: "SubagentStop", AgentID: "agent-1", AgentType: "Explore"},
		{SessionID: "session-1", HookEventName: "TaskCompleted", TaskID: "task-1", TeammateName: "worker", TeamName: "team"},
	} {
		if hookErr := tracker.HandleHook(ctx, event); hookErr != nil {
			t.Fatalf("HandleHook(%s) error = %v", event.HookEventName, hookErr)
		}
	}
	sessions, err := s.ListClaudeSessions(ctx, launchID, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListClaudeSessions() = %#v, error = %v", sessions, err)
	}
	agents, err := s.ListRuntimeAgents(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(agents) != 1 || agents[0].Status != "completed" || agents[0].EndedAt == "" {
		t.Fatalf("recovered agents = %#v, error = %v", agents, err)
	}
	tasks, err := s.ListTasks(ctx, launchID, sessions[0].ID, false)
	if err != nil || len(tasks) != 1 || tasks[0].Status != "completed" || tasks[0].CompletedAt == "" {
		t.Fatalf("recovered tasks = %#v, error = %v", tasks, err)
	}
}

func TestHookEventDecodingDropsSensitiveUnknownFields(t *testing.T) {
	t.Parallel()
	const body = `{
		"session_id":"session-1",
		"hook_event_name":"TaskCreated",
		"task_id":"task-1",
		"transcript_path":"SECRET_PATH",
		"description":"SECRET_DESCRIPTION",
		"last_assistant_message":"SECRET_RESPONSE"
	}`
	var event HookEvent
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&event); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, secret := range []string{"SECRET_PATH", "SECRET_DESCRIPTION", "SECRET_RESPONSE"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("encoded HookEvent contains %q: %s", secret, encoded)
		}
	}
}

func TestValidateHookEventRejectsMalformedIdentifiers(t *testing.T) {
	t.Parallel()
	tests := []HookEvent{
		{HookEventName: "SessionStart"},
		{SessionID: "session 1", HookEventName: "SessionStart"},
		{SessionID: "session-1", HookEventName: "SubagentStart"},
		{SessionID: "session-1", HookEventName: "TaskCreated", TaskID: "task secret"},
		{SessionID: "session-1", HookEventName: "unknown"},
	}
	for _, event := range tests {
		if err := ValidateHookEvent(event); !errorsIsInvalidHook(err) {
			t.Fatalf("ValidateHookEvent(%#v) error = %v", event, err)
		}
	}
}

func TestTrackerFinalizeAbandonsUnfinishedWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, launchID := trackerStore(t, ctx)
	tracker, err := NewTracker(Config{Store: s, LaunchID: launchID, Enabled: true})
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	for _, event := range []HookEvent{
		{SessionID: "session-1", HookEventName: "SessionStart", Source: "startup"},
		{SessionID: "session-1", HookEventName: "SubagentStart", AgentID: "agent-1"},
		{SessionID: "session-1", HookEventName: "TaskCreated", TaskID: "task-1"},
	} {
		if hookErr := tracker.HandleHook(ctx, event); hookErr != nil {
			t.Fatalf("HandleHook(%s) error = %v", event.HookEventName, hookErr)
		}
	}
	if finalizeErr := tracker.Finalize(ctx); finalizeErr != nil {
		t.Fatalf("Finalize() error = %v", finalizeErr)
	}
	snapshot := tracker.Snapshot()
	if snapshot.LifecycleState != "abandoned" || snapshot.CurrentSession.State != "abandoned" ||
		snapshot.ActiveAgents != 0 || snapshot.ActiveTasks != 0 {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 1 || launches[0].LifecycleState != "abandoned" {
		t.Fatalf("ListLaunches() = %#v, error = %v", launches, err)
	}
}

func errorsIsInvalidHook(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrInvalidHook.Error())
}

func trackerStore(t *testing.T, ctx context.Context) (*store.Store, int64) {
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
