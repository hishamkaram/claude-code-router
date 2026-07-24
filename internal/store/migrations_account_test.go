package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrateV6ToV7ResumesAfterPartialAccountDDL(t *testing.T) {
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
INSERT INTO schema_version (id, version) VALUES (1, 6);
CREATE TABLE launches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_url TEXT NOT NULL DEFAULT '',
  pid INTEGER NOT NULL DEFAULT 0,
  model_alias TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'starting',
  lifecycle_state TEXT NOT NULL DEFAULT 'pending',
  statusline_state TEXT NOT NULL DEFAULT 'not-configured',
  auth_mode TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  ended_at TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  end_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE claude_accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  access_token_ref TEXT NOT NULL
);
`); seedErr != nil {
		_ = db.Close()
		t.Fatalf("seeding partial v7 DDL: %v", seedErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("closing seeded database: %v", closeErr)
	}

	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
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
	assertColumnsExist(t, ctx, s, "launches", []string{"auth_mode", "claude_account_name"})
	assertColumnsExist(t, ctx, s, "claude_accounts", []string{
		"name", "access_token_ref", "refresh_token_ref", "expires_at", "scopes_json",
		"enabled", "cooldown_until", "created_at", "updated_at", "last_used_at",
		"last_refresh_at", "last_error",
	})
	assertIndexExists(t, ctx, s, "idx_claude_accounts_name")

	if _, err := s.AddClaudeAccount(ctx, testClaudeAccount("resumed", testAccountTime(24*time.Hour))); err != nil {
		t.Fatalf("AddClaudeAccount() after migration error = %v", err)
	}
}

func assertColumnsExist(t *testing.T, ctx context.Context, s *Store, table string, columns []string) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, column := range columns {
		exists, existsErr := tableColumnExists(ctx, tx, table, column)
		if existsErr != nil {
			t.Fatalf("tableColumnExists(%s.%s) error = %v", table, column, existsErr)
		}
		if !exists {
			t.Fatalf("missing column %s.%s", table, column)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func assertIndexExists(t *testing.T, ctx context.Context, s *Store, name string) {
	t.Helper()
	var found string
	if err := s.db.QueryRowContext(ctx, `
SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?
`, name).Scan(&found); err != nil {
		t.Fatalf("index %s missing: %v", name, err)
	}
}
