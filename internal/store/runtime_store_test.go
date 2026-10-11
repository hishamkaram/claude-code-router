package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestFindResumeRouteUsesObservedSessionRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	sessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "fixture", "model-v1", "openai-compatible", "sha256:fixture"); updateErr != nil {
		t.Fatal(updateErr)
	}
	route, routeErr := s.FindResumeRoute(ctx, "native-session")
	if routeErr != nil {
		t.Fatal(routeErr)
	}
	if route.SourceLaunchID != launchID || route.SourceAuthMode != "provider-only" ||
		route.ModelAlias != "coder" || route.ProviderName != "fixture" || route.ProviderModel != "model-v1" ||
		route.Protocol != "openai-compatible" || route.ConfigurationFingerprint != "sha256:fixture" {
		t.Fatalf("FindResumeRoute() = %#v", route)
	}
}

func TestFindResumeRouteRefusesObservedRouteWithoutProtocol(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	sessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "fixture", "model-v1", "", "sha256:fixture"); updateErr != nil {
		t.Fatal(updateErr)
	}
	if _, routeErr := s.FindResumeRoute(ctx, "native-session"); !errors.Is(routeErr, ErrResumeRouteUnavailable) {
		t.Fatalf("FindResumeRoute() error = %v, want ErrResumeRouteUnavailable", routeErr)
	}
}

func TestFindResumeRouteRefusesObservedRouteWithoutFingerprint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	sessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "fixture", "model-v1", "openai-compatible", ""); updateErr != nil {
		t.Fatal(updateErr)
	}
	if _, routeErr := s.FindResumeRoute(ctx, "native-session"); !errors.Is(routeErr, ErrResumeRouteUnavailable) {
		t.Fatalf("FindResumeRoute() error = %v, want ErrResumeRouteUnavailable", routeErr)
	}
}

func TestFindResumeRouteSkipsNewerUnobservedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	oldLaunchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	oldSessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: oldLaunchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, oldSessionID, "registered", "coder", "fixture", "model-v1", "openai-compatible", "sha256:fixture"); updateErr != nil {
		t.Fatal(updateErr)
	}
	newLaunchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	if _, upsertErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: newLaunchID, ClaudeSessionID: "native-session", Source: "resume"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	route, routeErr := s.FindResumeRoute(ctx, "native-session")
	if routeErr != nil {
		t.Fatal(routeErr)
	}
	if route.SourceLaunchID != oldLaunchID || route.ModelAlias != "coder" {
		t.Fatalf("FindResumeRoute() = %#v", route)
	}
}

func TestFindResumeRouteRefusesNewerNonProviderRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	oldLaunchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	oldSessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: oldLaunchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, oldSessionID, "registered", "coder", "fixture", "model-v1", "openai-compatible", "sha256:fixture"); updateErr != nil {
		t.Fatal(updateErr)
	}
	newLaunchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	newSessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: newLaunchID, ClaudeSessionID: "native-session", Source: "resume"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, newSessionID, "first-party-anthropic", "", "", "", "anthropic", ""); updateErr != nil {
		t.Fatal(updateErr)
	}
	if _, routeErr := s.FindResumeRoute(ctx, "native-session"); !errors.Is(routeErr, ErrResumeRouteUnavailable) {
		t.Fatalf("FindResumeRoute() error = %v, want ErrResumeRouteUnavailable", routeErr)
	}
}

func TestFindResumeRouteRefusesUnobservedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	if _, upsertErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "hook-recovery"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	if _, routeErr := s.FindResumeRoute(ctx, "native-session"); !errors.Is(routeErr, ErrResumeRouteUnavailable) {
		t.Fatalf("FindResumeRoute() error = %v, want ErrResumeRouteUnavailable", routeErr)
	}
}

func TestFindResumeRouteKeepsSelectedRouteAfterFailedRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	sessionID, sessionErr := s.UpsertClaudeSession(ctx, ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "startup"})
	if sessionErr != nil {
		t.Fatal(sessionErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "fixture", "model-v1", "openai-compatible", "sha256:fixture"); updateErr != nil {
		t.Fatal(updateErr)
	}
	eventID, beginErr := s.BeginRouteEvent(ctx, RouteEvent{
		LaunchID: launchID, SessionID: sessionID, RequestID: "failed-request", Operation: "messages",
		RouteKind: "registered", ModelAlias: "coder", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	})
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if completeErr := s.CompleteRouteEvent(ctx, eventID, "failed", 503, "provider_http", time.Millisecond, TokenUsage{}, nil); completeErr != nil {
		t.Fatal(completeErr)
	}
	route, routeErr := s.FindResumeRoute(ctx, "native-session")
	if routeErr != nil {
		t.Fatal(routeErr)
	}
	if route.SourceLaunchID != launchID || route.ModelAlias != "coder" {
		t.Fatalf("FindResumeRoute() = %#v", route)
	}
}

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
	if routeErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", "coder", "local", "model-v1", "openai-compatible", "sha256:fixture"); routeErr != nil {
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
	if session.ActiveModelAlias != "coder" || session.ActiveProviderModel != "model-v1" ||
		session.ActiveRouteFingerprint != "sha256:fixture" {
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

func TestLaunchAuthMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	legacyID, err := s.CreateLaunch(ctx, "coder", "pending", "pending")
	if err != nil {
		t.Fatalf("CreateLaunch() error = %v", err)
	}
	legacy, err := s.GetLaunch(ctx, legacyID)
	if err != nil {
		t.Fatalf("GetLaunch(legacy) error = %v", err)
	}
	if legacy.AuthMode != "" || legacy.ClaudeAccountName != "" {
		t.Fatalf("legacy launch auth metadata = %q, %q", legacy.AuthMode, legacy.ClaudeAccountName)
	}

	authID, err := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "subscription-pool", "work")
	if err != nil {
		t.Fatalf("CreateLaunchWithAuth() error = %v", err)
	}
	authLaunch, err := s.GetLaunch(ctx, authID)
	if err != nil {
		t.Fatalf("GetLaunch(auth) error = %v", err)
	}
	if authLaunch.AuthMode != "subscription-pool" || authLaunch.ClaudeAccountName != "work" {
		t.Fatalf("auth launch metadata = %#v", authLaunch)
	}
	providerOnlyID, err := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "provider-only", "")
	if err != nil {
		t.Fatalf("CreateLaunchWithAuth(provider-only) error = %v", err)
	}
	providerOnlyLaunch, err := s.GetLaunch(ctx, providerOnlyID)
	if err != nil {
		t.Fatalf("GetLaunch(provider-only) error = %v", err)
	}
	if providerOnlyLaunch.AuthMode != "provider-only" || providerOnlyLaunch.ClaudeAccountName != "" {
		t.Fatalf("provider-only launch metadata = %#v", providerOnlyLaunch)
	}
	if setErr := s.SetLaunchClaudeAccount(ctx, authID, "personal"); setErr != nil {
		t.Fatalf("SetLaunchClaudeAccount() error = %v", setErr)
	}
	authLaunch, err = s.GetLaunch(ctx, authID)
	if err != nil || authLaunch.ClaudeAccountName != "personal" {
		t.Fatalf("rotated launch auth metadata = %#v, %v", authLaunch, err)
	}
	if setErr := s.SetLaunchClaudeAccount(ctx, authID, "bad/name"); setErr == nil {
		t.Fatal("SetLaunchClaudeAccount() error = nil for invalid account name")
	}
	if _, err := s.CreateLaunchWithAuth(ctx, "coder", "pending", "pending", "claude-account", "bad/name"); err == nil {
		t.Fatal("CreateLaunchWithAuth() error = nil for invalid account name")
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
