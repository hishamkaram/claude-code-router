package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestRuntimeInspectionCommandsEmitVersionedJSON(t *testing.T) {
	t.Parallel()
	dbPath, launchID, sessionID := seedRuntimeInspectionStore(t)

	statusOut, _, err := runCommand(t, "--db", dbPath, "status", "--json")
	if err != nil {
		t.Fatalf("status --json error = %v", err)
	}
	var status statusDocument
	if decodeErr := json.Unmarshal([]byte(statusOut), &status); decodeErr != nil {
		t.Fatalf("status JSON error = %v\n%s", decodeErr, statusOut)
	}
	if status.SchemaVersion != 1 || status.LatestLaunch == nil ||
		status.LatestLaunch.ID != launchID || status.LastRoute == nil ||
		status.LastRoute.ModelAlias != "coder" {
		t.Fatalf("status document = %#v", status)
	}
	if strings.Contains(statusOut, "PROVIDER_KEY") {
		t.Fatalf("status JSON contains secret reference: %s", statusOut)
	}

	sessionsOut, _, err := runCommand(t, "--db", dbPath, "sessions", "--json", "--launch", "1")
	if err != nil {
		t.Fatalf("sessions --json error = %v", err)
	}
	var sessions sessionsDocument
	if decodeErr := json.Unmarshal([]byte(sessionsOut), &sessions); decodeErr != nil {
		t.Fatalf("sessions JSON error = %v\n%s", decodeErr, sessionsOut)
	}
	if sessions.SchemaVersion != 1 || len(sessions.Launches) != 1 ||
		len(sessions.Launches[0].Sessions) != 1 ||
		sessions.Launches[0].Sessions[0].ID != sessionID {
		t.Fatalf("sessions document = %#v", sessions)
	}

	agentsOut, _, err := runCommand(t, "--db", dbPath, "agents", "--json",
		"--launch", "1", "--session", "1")
	if err != nil {
		t.Fatalf("agents --json error = %v", err)
	}
	var agents agentsDocument
	if decodeErr := json.Unmarshal([]byte(agentsOut), &agents); decodeErr != nil {
		t.Fatalf("agents JSON error = %v\n%s", decodeErr, agentsOut)
	}
	if agents.SchemaVersion != 1 || len(agents.Agents) != 1 || len(agents.Tasks) != 1 {
		t.Fatalf("agents document = %#v", agents)
	}

	traceOut, _, err := runCommand(t, "--db", dbPath, "trace", "--json", "--launch", "1")
	if err != nil {
		t.Fatalf("trace --json error = %v", err)
	}
	var trace traceDocument
	if err := json.Unmarshal([]byte(traceOut), &trace); err != nil {
		t.Fatalf("trace JSON error = %v\n%s", err, traceOut)
	}
	if trace.SchemaVersion != 1 || len(trace.Events) != 2 || trace.Events[1].Route == nil ||
		!trace.Events[1].Route.Usage.Observed {
		t.Fatalf("trace document = %#v", trace)
	}
	for _, forbidden := range []string{"prompt text", "response text", "tool argument", "PROVIDER_KEY"} {
		if strings.Contains(traceOut, forbidden) {
			t.Fatalf("trace JSON contains %q: %s", forbidden, traceOut)
		}
	}
}

func TestStatusFindsLatestRouteBeyondLifecycleEventPage(t *testing.T) {
	t.Parallel()
	dbPath, launchID, sessionID := seedRuntimeInspectionStore(t)
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	for index := 0; index < 60; index++ {
		if _, recordErr := s.RecordLifecycleEvent(ctx, store.LifecycleEvent{
			LaunchID: launchID, SessionID: sessionID, Name: "TaskCreated",
			Status: "pending", ExternalID: fmt.Sprintf("task-%d", index),
		}); recordErr != nil {
			_ = s.Close()
			t.Fatalf("RecordLifecycleEvent(%d) error = %v", index, recordErr)
		}
	}
	if closeErr := s.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	out, _, err := runCommand(t, "--db", dbPath, "status", "--json")
	if err != nil {
		t.Fatalf("status --json error = %v", err)
	}
	var status statusDocument
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("status JSON error = %v", err)
	}
	if status.LastRoute == nil || status.LastRoute.ModelAlias != "coder" {
		t.Fatalf("status last route = %#v", status.LastRoute)
	}
}

func TestTraceFiltersAndPurgeValidation(t *testing.T) {
	t.Parallel()
	dbPath, _, _ := seedRuntimeInspectionStore(t)
	if _, _, err := runCommand(t, "--db", dbPath, "trace", "--limit", "0"); err == nil ||
		!strings.Contains(err.Error(), "--limit") {
		t.Fatalf("trace --limit error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "trace", "--since", "tomorrow"); err == nil ||
		!strings.Contains(err.Error(), "invalid --since") {
		t.Fatalf("trace --since error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "trace", "purge", "--all"); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("trace purge --all error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "trace", "purge", "--all", "--yes", "--json")
	if err != nil {
		t.Fatalf("trace purge error = %v", err)
	}
	var result struct {
		SchemaVersion int   `json:"schema_version"`
		Deleted       int64 `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("purge JSON error = %v", err)
	}
	if result.SchemaVersion != 1 || result.Deleted != 2 {
		t.Fatalf("purge result = %#v", result)
	}
}

func TestParseTimeSelector(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  string
	}{
		{value: "", want: ""},
		{value: "24h", want: "2026-07-17T12:00:00Z"},
		{value: "2d", want: "2026-07-16T12:00:00Z"},
		{value: "2026-07-01T00:00:00Z", want: "2026-07-01T00:00:00Z"},
	}
	for _, test := range tests {
		got, err := parseTimeSelector(test.value, now)
		if err != nil || got != test.want {
			t.Fatalf("parseTimeSelector(%q) = %q, %v; want %q", test.value, got, err, test.want)
		}
	}
}

func seedRuntimeInspectionStore(t *testing.T) (string, int64, int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	if providerErr := s.AddProvider(ctx, store.Provider{
		Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:9999",
		SecretRef: "env:PROVIDER_KEY",
	}); providerErr != nil {
		t.Fatalf("AddProvider() error = %v", providerErr)
	}
	if modelErr := s.AddModel(ctx, store.Model{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
	}); modelErr != nil {
		t.Fatalf("AddModel() error = %v", modelErr)
	}
	launchID, err := s.CreateLaunch(ctx, "coder", "active", "injected")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	if activateErr := s.ActivateLaunch(ctx, launchID, "http://127.0.0.1:43123", 999999); activateErr != nil {
		t.Fatalf("ActivateLaunch() error = %v", activateErr)
	}
	sessionID, err := s.UpsertClaudeSession(ctx, store.ClaudeSession{
		LaunchID: launchID, ClaudeSessionID: "claude-session-1", Source: "startup",
	})
	if err != nil {
		t.Fatalf("UpsertClaudeSession() error = %v", err)
	}
	if routeErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "fixture", "model-v1"); routeErr != nil {
		t.Fatalf("UpdateClaudeSessionRoute() error = %v", routeErr)
	}
	if _, agentErr := s.UpsertAgent(ctx, store.Agent{
		LaunchID: launchID, SessionID: sessionID, ExternalID: "agent-1",
		Name: "Explore", Kind: "subagent", ModelAlias: "coder", Status: "running",
	}); agentErr != nil {
		t.Fatalf("UpsertAgent() error = %v", agentErr)
	}
	if _, taskErr := s.UpsertTask(ctx, store.Task{
		LaunchID: launchID, SessionID: sessionID, ExternalID: "task-1",
		ModelAlias: "coder", Status: "pending",
	}); taskErr != nil {
		t.Fatalf("UpsertTask() error = %v", taskErr)
	}
	eventID, err := s.BeginRouteEvent(ctx, store.RouteEvent{
		LaunchID: launchID, SessionID: sessionID, RequestID: "request-1",
		Operation: "messages", RequestedModel: "anthropic.ccr.coder", Tools: true,
	})
	if err != nil {
		t.Fatalf("BeginRouteEvent() error = %v", err)
	}
	if err := s.ResolveRouteEvent(ctx, eventID, sessionID, store.RouteEvent{
		RouteKind: "registered", ModelAlias: "coder", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	}); err != nil {
		t.Fatalf("ResolveRouteEvent() error = %v", err)
	}
	if err := s.CompleteRouteEvent(ctx, eventID, "succeeded", 200, "", 10*time.Millisecond,
		store.TokenUsage{Observed: true, InputTokens: 5, OutputTokens: 2}); err != nil {
		t.Fatalf("CompleteRouteEvent() error = %v", err)
	}
	if _, err := s.RecordLifecycleEvent(ctx, store.LifecycleEvent{
		LaunchID: launchID, SessionID: sessionID, Name: "SubagentStart",
		Status: "running", ExternalID: "agent-1", ActorKind: "Explore",
	}); err != nil {
		t.Fatalf("RecordLifecycleEvent() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return dbPath, launchID, sessionID
}
