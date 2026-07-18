package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRuntimeStateRoundTripAndAbandonment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	launchID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	if activateErr := s.ActivateLaunch(ctx, launchID, "http://127.0.0.1:43123", 1234); activateErr != nil {
		t.Fatalf("ActivateLaunch() error = %v", activateErr)
	}
	sessionID, err := s.UpsertClaudeSession(ctx, ClaudeSession{
		LaunchID: launchID, ClaudeSessionID: "claude-session-1", Source: "startup",
	})
	if err != nil {
		t.Fatalf("UpsertClaudeSession() error = %v", err)
	}
	if routeErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "local", "model-v1"); routeErr != nil {
		t.Fatalf("UpdateClaudeSessionRoute() error = %v", routeErr)
	}

	if _, agentErr := s.UpsertAgent(ctx, Agent{
		LaunchID: launchID, SessionID: sessionID, ExternalID: "agent-1",
		Name: "Explore", Kind: "subagent", ModelAlias: "coder", Status: "running",
	}); agentErr != nil {
		t.Fatalf("UpsertAgent() error = %v", agentErr)
	}
	if _, taskErr := s.UpsertTask(ctx, Task{
		LaunchID: launchID, SessionID: sessionID, ExternalID: "task-1",
		TeammateName: "worker", TeamName: "backend", ModelAlias: "coder", Status: "pending",
	}); taskErr != nil {
		t.Fatalf("UpsertTask() error = %v", taskErr)
	}

	session, err := s.GetClaudeSession(ctx, launchID, "claude-session-1")
	if err != nil {
		t.Fatalf("GetClaudeSession() error = %v", err)
	}
	if session.ActiveModelAlias != "coder" || session.ActiveProviderModel != "model-v1" {
		t.Fatalf("GetClaudeSession() = %#v", session)
	}
	agents, err := s.ListRuntimeAgents(ctx, launchID, sessionID, true)
	if err != nil {
		t.Fatalf("ListRuntimeAgents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].ExternalID != "agent-1" {
		t.Fatalf("ListRuntimeAgents() = %#v", agents)
	}
	tasks, err := s.ListTasks(ctx, launchID, sessionID, true)
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ExternalID != "task-1" {
		t.Fatalf("ListTasks() = %#v", tasks)
	}

	if abandonErr := s.AbandonLaunchRuntime(ctx, launchID); abandonErr != nil {
		t.Fatalf("AbandonLaunchRuntime() error = %v", abandonErr)
	}
	sessions, err := s.ListClaudeSessions(ctx, launchID, false)
	if err != nil {
		t.Fatalf("ListClaudeSessions() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].State != "abandoned" {
		t.Fatalf("ListClaudeSessions() = %#v", sessions)
	}
	agents, err = s.ListRuntimeAgents(ctx, launchID, sessionID, false)
	if err != nil {
		t.Fatalf("ListRuntimeAgents(all) error = %v", err)
	}
	if agents[0].Status != "abandoned" {
		t.Fatalf("agent status = %q, want abandoned", agents[0].Status)
	}
	tasks, err = s.ListTasks(ctx, launchID, sessionID, false)
	if err != nil {
		t.Fatalf("ListTasks(all) error = %v", err)
	}
	if tasks[0].Status != "abandoned" {
		t.Fatalf("task status = %q, want abandoned", tasks[0].Status)
	}
}

func TestMigrateV3PreservesRuntimeAndConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, schemaErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+legacyV2SchemaSQL+`
UPDATE schema_version SET version = 2 WHERE id = 1;
`); schemaErr != nil {
		t.Fatalf("creating v2 schema: %v", schemaErr)
	}
	if migrateErr := s.migrateV2ToV3(ctx); migrateErr != nil {
		t.Fatalf("migrateV2ToV3() error = %v", migrateErr)
	}
	if _, seedErr := s.db.ExecContext(ctx, `
INSERT INTO providers (name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, mode, created_at)
VALUES ('fixture', 'openai-compatible', 'http://127.0.0.1:1', '',
  'openai-compatible', 1, 1, 0, 1, 0, 'degraded', '2026-01-01T00:00:00Z');
INSERT INTO models (alias, provider_id, provider_model, status, created_at)
VALUES ('coder', 1, 'model-v1', 'degraded', '2026-01-01T00:00:00Z');
INSERT INTO sessions (gateway_url, pid, model_alias, created_at)
VALUES ('http://127.0.0.1:43123', 1234, 'coder', '2026-01-01T00:00:00Z');
INSERT INTO agents (session_id, name, kind, model_alias, status, created_at)
VALUES (1, 'worker', 'subagent', 'coder', 'observed', '2026-01-01T00:00:00Z');
INSERT INTO conformance_runs (alias, status, live_verified, details, created_at)
VALUES ('coder', 'local-verified', 0, 'ok', '2026-01-01T00:00:00Z');
`); seedErr != nil {
		t.Fatalf("seeding v3 data: %v", seedErr)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}

	provider, err := s.GetProvider(ctx, "fixture")
	if err != nil || provider.Protocol != "openai-compatible" {
		t.Fatalf("GetProvider() = %#v, %v", provider, err)
	}
	model, err := s.GetModel(ctx, "coder")
	if err != nil || model.ProviderModel != "model-v1" {
		t.Fatalf("GetModel() = %#v, %v", model, err)
	}
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 1 || launches[0].State != "legacy" {
		t.Fatalf("ListLaunches() = %#v, %v", launches, err)
	}
	agents, err := s.ListRuntimeAgents(ctx, launches[0].ID, 0, false)
	if err != nil || len(agents) != 1 || agents[0].Name != "worker" {
		t.Fatalf("ListRuntimeAgents() = %#v, %v", agents, err)
	}
	records, err := s.ListConformanceRecords(ctx)
	if err != nil || len(records) != 1 || records[0].Details != "ok" {
		t.Fatalf("ListConformanceRecords() = %#v, %v", records, err)
	}
}

func openMigratedStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return s
}
