package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

const routeEventsV11TestSQL = `
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
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  stream_observed INTEGER NOT NULL DEFAULT 0,
  upstream_headers_ms INTEGER NOT NULL DEFAULT 0,
  first_upstream_event_ms INTEGER NOT NULL DEFAULT 0,
  first_downstream_event_ms INTEGER NOT NULL DEFAULT 0,
  upstream_event_count INTEGER NOT NULL DEFAULT 0,
  early_stream_commit INTEGER NOT NULL DEFAULT 0,
  stream_terminal_phase TEXT NOT NULL DEFAULT ''
);
`

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
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, seedErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+`
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
`); seedErr != nil {
		t.Fatalf("seeding v8 route-events schema: %v", seedErr)
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

func TestMigrateV9ToV10AddsActiveProtocol(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, seedErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+routeEventsV11TestSQL+`
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL,
  claude_session_id TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'active',
  active_route_kind TEXT NOT NULL DEFAULT '',
  active_model_alias TEXT NOT NULL DEFAULT '',
  active_provider_name TEXT NOT NULL DEFAULT '',
  active_provider_model TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT NOT NULL DEFAULT '',
  ended_at TEXT NOT NULL DEFAULT '',
  end_reason TEXT NOT NULL DEFAULT ''
);
UPDATE schema_version SET version = 9 WHERE id = 1;
`); seedErr != nil {
		t.Fatalf("seeding v9 sessions schema: %v", seedErr)
	}

	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	assertColumnsExist(t, ctx, s, "sessions", []string{"active_protocol", "active_route_fingerprint"})
	assertIndexExists(t, ctx, s, "idx_sessions_claude_session_id")
	version, versionErr := s.SchemaVersion(ctx)
	if versionErr != nil {
		t.Fatalf("SchemaVersion() error = %v", versionErr)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("second Migrate() error = %v", migrateErr)
	}
}

func TestMigrateV10ToV11AddsActiveRouteFingerprint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, seedErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+routeEventsV11TestSQL+`
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL,
  claude_session_id TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'active',
  active_route_kind TEXT NOT NULL DEFAULT '',
  active_model_alias TEXT NOT NULL DEFAULT '',
  active_provider_name TEXT NOT NULL DEFAULT '',
  active_provider_model TEXT NOT NULL DEFAULT '',
  active_protocol TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT NOT NULL DEFAULT '',
  ended_at TEXT NOT NULL DEFAULT '',
  end_reason TEXT NOT NULL DEFAULT ''
);
UPDATE schema_version SET version = 10 WHERE id = 1;
`); seedErr != nil {
		t.Fatalf("seeding v10 sessions schema: %v", seedErr)
	}

	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	assertColumnsExist(t, ctx, s, "sessions", []string{"active_route_fingerprint"})
	version, versionErr := s.SchemaVersion(ctx)
	if versionErr != nil {
		t.Fatalf("SchemaVersion() error = %v", versionErr)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("second Migrate() error = %v", migrateErr)
	}
}

func TestMigrateV11ToV12AddsClassifierDecisionEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, seedErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+routeEventsV11TestSQL+`
UPDATE schema_version SET version = 11 WHERE id = 1;
`); seedErr != nil {
		t.Fatalf("seeding v11 route-events schema: %v", seedErr)
	}

	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	assertColumnsExist(t, ctx, s, "route_events", []string{
		"classifier_selection_source", "classifier_active_alias",
	})
	version, versionErr := s.SchemaVersion(ctx)
	if versionErr != nil {
		t.Fatalf("SchemaVersion() error = %v", versionErr)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("second Migrate() error = %v", migrateErr)
	}
}

func TestMigrateV11ToV12ResumesAfterFirstClassifierColumn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, seedErr := s.db.ExecContext(ctx, bootstrapSchemaSQL+routeEventsV11TestSQL+`
ALTER TABLE route_events ADD COLUMN classifier_selection_source TEXT NOT NULL DEFAULT '';
UPDATE schema_version SET version = 11 WHERE id = 1;
`); seedErr != nil {
		t.Fatalf("seeding partial v11 route-events schema: %v", seedErr)
	}

	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	assertColumnsExist(t, ctx, s, "route_events", []string{
		"classifier_selection_source", "classifier_active_alias",
	})
}
