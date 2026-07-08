package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

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
	if updateProviderErr := s.UpdateProvider(ctx, Provider{Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1", SecretRef: "env:OPENROUTER_UPDATED"}); updateProviderErr != nil {
		t.Fatalf("UpdateProvider() error = %v", updateProviderErr)
	}
	provider, err := s.GetProvider(ctx, "openrouter")
	if err != nil {
		t.Fatalf("GetProvider(openrouter) error = %v", err)
	}
	if provider.BaseURL != "https://openrouter.ai/api/v1" || provider.SecretRef != "env:OPENROUTER_UPDATED" {
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
