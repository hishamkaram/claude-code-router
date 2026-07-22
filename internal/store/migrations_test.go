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
