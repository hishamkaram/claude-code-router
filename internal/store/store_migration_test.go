package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateAndProviderModelRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := openMigratedTestStore(t, ctx)
	defer closeTestStore(t, s)

	assertProviderModelRoundTrip(t, ctx, s)
	assertSessionAndAgentRoundTrip(t, ctx, s)
	assertConformanceRoundTrip(t, ctx, s)
	assertRemoveProviderCascadesModels(t, ctx, s)
}

func openMigratedTestStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()

	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	return s
}

func closeTestStore(t *testing.T, s *Store) {
	t.Helper()

	if closeErr := s.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
}

func assertProviderModelRoundTrip(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()

	if addProviderErr := s.AddProvider(ctx, Provider{Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api", SecretRef: "env:OPENROUTER_API_KEY"}); addProviderErr != nil {
		t.Fatalf("AddProvider() error = %v", addProviderErr)
	}
	exists, err := s.ProviderExists(ctx, "openrouter")
	if err != nil {
		t.Fatalf("ProviderExists() error = %v", err)
	}
	if !exists {
		t.Fatalf("ProviderExists() = false, want true")
	}
	exists, err = s.ProviderExists(ctx, "missing")
	if err != nil {
		t.Fatalf("ProviderExists(missing) error = %v", err)
	}
	if exists {
		t.Fatalf("ProviderExists(missing) = true, want false")
	}
	if addModelErr := s.AddModel(ctx, Model{Alias: "qwen", ProviderName: "openrouter", ProviderModel: "qwen/qwen3-coder", Status: "degraded"}); addModelErr != nil {
		t.Fatalf("AddModel() error = %v", addModelErr)
	}
	if _, updateProviderErr := s.UpdateProvider(ctx, Provider{Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1", SecretRef: "env:OPENROUTER_UPDATED"}); updateProviderErr != nil {
		t.Fatalf("UpdateProvider() error = %v", updateProviderErr)
	}
	provider, err := s.GetProvider(ctx, "openrouter")
	if err != nil {
		t.Fatalf("GetProvider(openrouter) error = %v", err)
	}
	if provider.BaseURL != "https://openrouter.ai/api/v1" || provider.SecretRef != "env:OPENROUTER_UPDATED" ||
		provider.Protocol != "openai-compatible" || !provider.SupportsModelDiscovery || provider.SupportsCountTokens {
		t.Fatalf("GetProvider(openrouter) after update = %#v", provider)
	}
	if updateModelErr := s.UpdateModel(ctx, Model{Alias: "qwen", ProviderName: "openrouter", ProviderModel: "qwen/qwen3-coder-plus", Status: "full"}); updateModelErr != nil {
		t.Fatalf("UpdateModel() error = %v", updateModelErr)
	}
	model, err := s.GetModel(ctx, "qwen")
	if err != nil {
		t.Fatalf("GetModel(qwen) error = %v", err)
	}
	if model.ProviderModel != "qwen/qwen3-coder-plus" || model.Status != "full" {
		t.Fatalf("GetModel(qwen) after update = %#v", model)
	}
	modelExists, err := s.ModelExists(ctx, "qwen")
	if err != nil {
		t.Fatalf("ModelExists(qwen) error = %v", err)
	}
	if !modelExists {
		t.Fatalf("ModelExists(qwen) = false, want true")
	}
	modelExists, err = s.ModelExists(ctx, "missing")
	if err != nil {
		t.Fatalf("ModelExists(missing) error = %v", err)
	}
	if modelExists {
		t.Fatalf("ModelExists(missing) = true, want false")
	}

	providers, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders() error = %v", err)
	}
	if len(providers) != 1 || providers[0].SecretRef != "env:OPENROUTER_UPDATED" {
		t.Fatalf("ListProviders() = %#v", providers)
	}

	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0].Alias != "qwen" || models[0].ProviderName != "openrouter" {
		t.Fatalf("ListModels() = %#v", models)
	}
}

func assertSessionAndAgentRoundTrip(t *testing.T, ctx context.Context, s *Store) int64 {
	t.Helper()

	sessionID, err := s.AddSession(ctx, Session{GatewayURL: "http://127.0.0.1:1234", PID: 1234, ModelAlias: "qwen"})
	if err != nil {
		t.Fatalf("AddSession() error = %v", err)
	}
	sessions, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID || sessions[0].GatewayURL != "http://127.0.0.1:1234" {
		t.Fatalf("ListSessions() = %#v", sessions)
	}

	agentID, err := s.AddAgent(ctx, Agent{SessionID: sessionID, Name: "worker-1", Kind: "subagent", ModelAlias: "qwen", Status: "observed"})
	if err != nil {
		t.Fatalf("AddAgent() error = %v", err)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].ID != agentID || agents[0].Name != "worker-1" {
		t.Fatalf("ListAgents() = %#v", agents)
	}
	return sessionID
}

func assertConformanceRoundTrip(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()

	recordID, err := s.AddConformanceRecord(ctx, ConformanceRecord{Alias: "qwen", Status: "local-verified", LiveVerified: false, Details: "ok"})
	if err != nil {
		t.Fatalf("AddConformanceRecord() error = %v", err)
	}
	records, err := s.ListConformanceRecords(ctx)
	if err != nil {
		t.Fatalf("ListConformanceRecords() error = %v", err)
	}
	if len(records) != 1 || records[0].ID != recordID || records[0].LiveVerified {
		t.Fatalf("ListConformanceRecords() = %#v", records)
	}
}

func assertRemoveProviderCascadesModels(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()

	removedModels, err := s.RemoveProvider(ctx, "openrouter")
	if err != nil {
		t.Fatalf("RemoveProvider(openrouter) error = %v", err)
	}
	if removedModels != 1 {
		t.Fatalf("RemoveProvider(openrouter) removed %d models, want 1", removedModels)
	}
	exists, err := s.ProviderExists(ctx, "openrouter")
	if err != nil {
		t.Fatalf("ProviderExists(openrouter) after remove error = %v", err)
	}
	if exists {
		t.Fatalf("ProviderExists(openrouter) after remove = true, want false")
	}
	modelExists, err := s.ModelExists(ctx, "qwen")
	if err != nil {
		t.Fatalf("ModelExists(qwen) after provider remove error = %v", err)
	}
	if modelExists {
		t.Fatalf("ModelExists(qwen) after provider remove = true, want false")
	}
}

func TestMigrateV2AddsProviderCapabilities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, seedErr := db.ExecContext(ctx, `
CREATE TABLE schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT INTO schema_version (id, version) VALUES (1, 2);
CREATE TABLE providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE models (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL UNIQUE,
  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  provider_model TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_url TEXT NOT NULL,
  pid INTEGER NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE conformance_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL,
  status TEXT NOT NULL,
  live_verified INTEGER NOT NULL DEFAULT 0,
  details TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
INSERT INTO providers (name, type, base_url, secret_ref, created_at)
VALUES
  ('anthropic', 'anthropic', 'https://api.anthropic.com', 'env:ANTHROPIC_API_KEY', 'now'),
  ('litellm', 'litellm', 'http://localhost:4000', '', 'now'),
  ('unsupported', 'unsupported', 'http://localhost:5000', '', 'now');
`); seedErr != nil {
		t.Fatalf("seed v2 schema error = %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("seed db Close() error = %v", closeErr)
	}

	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	anthropic, err := s.GetProvider(ctx, "anthropic")
	if err != nil {
		t.Fatalf("GetProvider(anthropic) error = %v", err)
	}
	if anthropic.Protocol != "anthropic-compatible" || anthropic.Mode != "full" || !anthropic.SupportsCountTokens {
		t.Fatalf("migrated anthropic provider = %#v", anthropic)
	}
	litellm, err := s.GetProvider(ctx, "litellm")
	if err != nil {
		t.Fatalf("GetProvider(litellm) error = %v", err)
	}
	if litellm.Protocol != "openai-compatible" || litellm.Mode != "degraded" || litellm.SupportsCountTokens || !litellm.SupportsModelDiscovery {
		t.Fatalf("migrated litellm provider = %#v", litellm)
	}
	unsupported, err := s.GetProvider(ctx, "unsupported")
	if err != nil {
		t.Fatalf("GetProvider(unsupported) error = %v", err)
	}
	if unsupported.Protocol != "" || unsupported.SupportsTools || unsupported.Mode != "degraded" {
		t.Fatalf("migrated unsupported provider = %#v", unsupported)
	}
}

func TestMigrateV2ProviderCapabilitiesCanResumeAfterPartialColumnAdds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, seedErr := db.ExecContext(ctx, `
CREATE TABLE schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT INTO schema_version (id, version) VALUES (1, 2);
CREATE TABLE providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  protocol TEXT NOT NULL DEFAULT '',
  supports_tools INTEGER NOT NULL DEFAULT 0
);
INSERT INTO providers (name, type, base_url, secret_ref, created_at)
VALUES ('zai', 'zai', 'https://api.z.ai/api/anthropic', 'env:ZAI_API_KEY', 'now');
`); seedErr != nil {
		t.Fatalf("seed partial v2 schema error = %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("seed db Close() error = %v", closeErr)
	}

	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	provider, err := s.GetProvider(ctx, "zai")
	if err != nil {
		t.Fatalf("GetProvider(zai) error = %v", err)
	}
	if provider.Protocol != "anthropic-compatible" || provider.Mode != "full" || !provider.SupportsCountTokens {
		t.Fatalf("migrated partially-updated provider = %#v", provider)
	}
}

func TestMigrateV1AddsProviderCapabilities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, seedErr := db.ExecContext(ctx, `
CREATE TABLE schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT INTO schema_version (id, version) VALUES (1, 1);
CREATE TABLE providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE models (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL UNIQUE,
  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  provider_model TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);
INSERT INTO providers (name, type, base_url, secret_ref, created_at)
VALUES ('litellm', 'litellm', 'http://localhost:4000', '', 'now');
INSERT INTO models (alias, provider_id, provider_model, status, created_at)
SELECT 'qwen', id, 'qwen/qwen3-coder', 'degraded', 'now'
FROM providers
WHERE name = 'litellm';
`); seedErr != nil {
		t.Fatalf("seed v1 schema error = %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("seed db Close() error = %v", closeErr)
	}

	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	provider, err := s.GetProvider(ctx, "litellm")
	if err != nil {
		t.Fatalf("GetProvider(litellm) error = %v", err)
	}
	if provider.Protocol != "openai-compatible" || provider.Mode != "degraded" || !provider.SupportsModelDiscovery {
		t.Fatalf("migrated litellm provider = %#v", provider)
	}
	model, err := s.GetModel(ctx, "qwen")
	if err != nil {
		t.Fatalf("GetModel(qwen) error = %v", err)
	}
	if model.ProviderName != "litellm" || model.ProviderModel != "qwen/qwen3-coder" {
		t.Fatalf("migrated model = %#v", model)
	}
}

func TestMigrateRejectsFutureSchemaVersionWithoutOverwrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, seedErr := db.ExecContext(ctx, `
CREATE TABLE schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT INTO schema_version (id, version) VALUES (1, 99);
`); seedErr != nil {
		t.Fatalf("seed future schema error = %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("seed db Close() error = %v", closeErr)
	}

	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()
	err = s.Migrate(ctx)
	if err == nil {
		t.Fatalf("Migrate() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "unsupported schema version 99") {
		t.Fatalf("Migrate() error = %v", err)
	}
	version, versionErr := s.SchemaVersion(ctx)
	if versionErr != nil {
		t.Fatalf("SchemaVersion() error = %v", versionErr)
	}
	if version != 99 {
		t.Fatalf("SchemaVersion() = %d, want 99", version)
	}
}
