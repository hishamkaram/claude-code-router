package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

func TestOpenReadOnlyHandlesEscapedPathsAndRejectsWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr #1.db")
	writable, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if migrateErr := writable.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	if addErr := writable.AddProvider(ctx, Provider{Name: "fixture", Type: "local", BaseURL: "http://localhost:4000"}); addErr != nil {
		t.Fatalf("AddProvider() error = %v", addErr)
	}
	if closeErr := writable.Close(); closeErr != nil {
		t.Fatalf("Close(writable) error = %v", closeErr)
	}
	readOnly, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer func() { _ = readOnly.Close() }()
	providers, err := readOnly.ListProviders(ctx)
	if err != nil || len(providers) != 1 || providers[0].Name != "fixture" {
		t.Fatalf("ListProviders() = %#v, error = %v", providers, err)
	}
	if err := readOnly.AddProvider(ctx, Provider{Name: "forbidden", Type: "local", BaseURL: "http://localhost:5000"}); err == nil {
		t.Fatal("AddProvider() error = nil on read-only store")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenReadOnly(context.Background(), filepath.Join(root, "ccr.db")); err == nil {
		t.Fatal("OpenReadOnly() error = nil for missing database")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly() created parent directory: %v", err)
	}
}

func TestMigrateAndProviderModelRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
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

	removedModels, err := s.RemoveProvider(ctx, "openrouter")
	if err != nil {
		t.Fatalf("RemoveProvider(openrouter) error = %v", err)
	}
	if removedModels != 1 {
		t.Fatalf("RemoveProvider(openrouter) removed %d models, want 1", removedModels)
	}
	exists, err = s.ProviderExists(ctx, "openrouter")
	if err != nil {
		t.Fatalf("ProviderExists(openrouter) after remove error = %v", err)
	}
	if exists {
		t.Fatalf("ProviderExists(openrouter) after remove = true, want false")
	}
	modelExists, err = s.ModelExists(ctx, "qwen")
	if err != nil {
		t.Fatalf("ModelExists(qwen) after provider remove error = %v", err)
	}
	if modelExists {
		t.Fatalf("ModelExists(qwen) after provider remove = true, want false")
	}
}

func TestModelCapabilitiesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(modelcap.Values{
		Kind:                modelcap.KindChat,
		ContextWindowTokens: modelcap.Int64(1_000_000),
		SupportsTools:       modelcap.Bool(true),
	}, "litellm:/model/info")
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	want := Model{
		Alias:                   "glm-5-2",
		ProviderName:            "litellm",
		ProviderModel:           "glm-5.2[1m]",
		Status:                  "full",
		DiscoveredCapabilities:  discovered,
		CapabilityOverrides:     modelcap.Values{MaxOutputTokens: modelcap.Int64(64_000)},
		CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}
	if err := s.AddModel(ctx, want); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	got, getErr := s.GetModel(ctx, want.Alias)
	if getErr != nil {
		t.Fatalf("GetModel() error = %v", getErr)
	}
	if got.DiscoveredCapabilities.Values.ContextWindowTokens == nil ||
		*got.DiscoveredCapabilities.Values.ContextWindowTokens != 1_000_000 ||
		got.DiscoveredCapabilities.Sources["supports_tools"] != "litellm:/model/info" ||
		got.CapabilityOverrides.MaxOutputTokens == nil || *got.CapabilityOverrides.MaxOutputTokens != 64_000 ||
		got.CapabilitiesRefreshedAt != want.CapabilitiesRefreshedAt {
		t.Fatalf("GetModel() = %#v", got)
	}
	got.Status = "degraded"
	if err := s.UpdateModel(ctx, got); err != nil {
		t.Fatalf("UpdateModel() error = %v", err)
	}
	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	if len(models) != 1 || models[0].CapabilityOverrides.MaxOutputTokens == nil ||
		*models[0].CapabilityOverrides.MaxOutputTokens != 64_000 {
		t.Fatalf("ListModels() = %#v", models)
	}
}

func TestUpdateProviderInvalidatesCapabilitiesWhenSourceChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	provider := Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(modelcap.Values{
		ContextWindowTokens: modelcap.Int64(1_000_000), SupportsTools: modelcap.Bool(false),
	}, "litellm:/model/info")
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	model := Model{
		Alias: "glm", ProviderName: provider.Name, ProviderModel: "glm-5.2", Status: "degraded",
		DiscoveredCapabilities: discovered, CapabilityOverrides: modelcap.Values{MaxOutputTokens: modelcap.Int64(64_000)},
		CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}

	storedProvider, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	noChange, err := s.UpdateProvider(ctx, storedProvider)
	if err != nil {
		t.Fatalf("no-op UpdateProvider() error = %v", err)
	}
	if noChange.CapabilitySnapshotsInvalidated != 0 {
		t.Fatalf("no-op UpdateProvider() invalidated %d snapshots", noChange.CapabilitySnapshotsInvalidated)
	}
	beforeSourceChange, err := s.GetModel(ctx, model.Alias)
	if err != nil {
		t.Fatalf("GetModel() before source change error = %v", err)
	}
	if modelcap.IsZeroSnapshot(beforeSourceChange.DiscoveredCapabilities) {
		t.Fatal("no-op provider update cleared discovered capabilities")
	}

	storedProvider.BaseURL = "http://localhost:5000"
	result, err := s.UpdateProvider(ctx, storedProvider)
	if err != nil {
		t.Fatalf("source-changing UpdateProvider() error = %v", err)
	}
	if result.CapabilitySnapshotsInvalidated != 1 {
		t.Fatalf("UpdateProvider() invalidated %d snapshots, want 1", result.CapabilitySnapshotsInvalidated)
	}
	updatedModel, err := s.GetModel(ctx, model.Alias)
	if err != nil {
		t.Fatalf("GetModel() after source change error = %v", err)
	}
	if !modelcap.IsZeroSnapshot(updatedModel.DiscoveredCapabilities) || updatedModel.CapabilitiesRefreshedAt != "" {
		t.Fatalf("stale discovered capabilities survived provider update: %#v", updatedModel)
	}
	if updatedModel.CapabilityOverrides.MaxOutputTokens == nil || *updatedModel.CapabilityOverrides.MaxOutputTokens != 64_000 {
		t.Fatalf("provider update cleared explicit overrides: %#v", updatedModel.CapabilityOverrides)
	}
}

func TestUpdateProviderRollsBackWhenCapabilityInvalidationFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	provider := Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(
		modelcap.Values{ContextWindowTokens: modelcap.Int64(1_000_000)}, "litellm:/model/info",
	)
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	if err := s.AddModel(ctx, Model{
		Alias: "glm", ProviderName: provider.Name, ProviderModel: "glm-5.2", Status: "degraded",
		DiscoveredCapabilities: discovered, CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TRIGGER reject_capability_invalidation
BEFORE UPDATE OF discovered_capabilities ON models
BEGIN
  SELECT RAISE(ABORT, 'invalidation rejected');
END;
`); err != nil {
		t.Fatalf("creating invalidation trigger: %v", err)
	}
	storedProvider, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	storedProvider.BaseURL = "http://localhost:5000"
	_, updateErr := s.UpdateProvider(ctx, storedProvider)
	if updateErr == nil || !strings.Contains(updateErr.Error(), "invalidating model capabilities") {
		t.Fatalf("UpdateProvider() error = %v", updateErr)
	}
	rolledBack, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() after rollback error = %v", err)
	}
	if rolledBack.BaseURL != provider.BaseURL {
		t.Fatalf("provider base URL = %q after rollback, want %q", rolledBack.BaseURL, provider.BaseURL)
	}
}

func TestModelCapabilityValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	err := s.AddModel(ctx, Model{
		Alias:               "invalid",
		ProviderName:        "litellm",
		ProviderModel:       "invalid",
		Status:              "degraded",
		CapabilityOverrides: modelcap.Values{ContextWindowTokens: modelcap.Int64(0)},
	})
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("AddModel() error = %v", err)
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
