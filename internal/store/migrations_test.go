package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateV5ToV6ResumesAfterSupportsResponsesColumnWasAdded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	db, openErr := sql.Open("sqlite", dbPath)
	if openErr != nil {
		t.Fatalf("sql.Open() error = %v", openErr)
	}
	if _, seedErr := db.ExecContext(ctx, `
CREATE TABLE schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);
INSERT INTO schema_version (id, version) VALUES (1, 5);
CREATE TABLE providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  supports_responses INTEGER NOT NULL DEFAULT 0
);`); seedErr != nil {
		_ = db.Close()
		t.Fatalf("seeding partial v5 schema: %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("closing seeded database: %v", closeErr)
	}

	s, storeErr := Open(ctx, dbPath)
	if storeErr != nil {
		t.Fatalf("Open() error = %v", storeErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	version, versionErr := s.SchemaVersion(ctx)
	if versionErr != nil {
		t.Fatalf("SchemaVersion() error = %v", versionErr)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
}

func TestMigrateV8ToV9AddsStreamTelemetryColumns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.ExecContext(ctx, bootstrapSchemaSQL+`
CREATE TABLE route_events (
  event_id INTEGER PRIMARY KEY,
  request_id TEXT NOT NULL UNIQUE,
  requested_model TEXT NOT NULL DEFAULT '',
  route_kind TEXT NOT NULL DEFAULT '',
  model_alias TEXT NOT NULL DEFAULT '',
  provider_name TEXT NOT NULL DEFAULT '',
  provider_model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  streaming INTEGER NOT NULL DEFAULT 0,
  tools INTEGER NOT NULL DEFAULT 0,
  thinking INTEGER NOT NULL DEFAULT 0,
  http_status INTEGER NOT NULL DEFAULT 0,
  error_class TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER NOT NULL DEFAULT 0,
  usage_observed INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0
);
UPDATE schema_version SET version = 8 WHERE id = 1;
`); err != nil {
		t.Fatalf("seeding v8 route-events schema: %v", err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	assertColumnsExist(t, ctx, s, "route_events", []string{
		"stream_observed", "upstream_headers_ms", "first_upstream_event_ms",
		"first_downstream_event_ms", "upstream_event_count", "early_stream_commit",
		"stream_terminal_phase",
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
}
