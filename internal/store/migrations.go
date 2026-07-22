package store

import (
	"context"
	"database/sql"
	"fmt"
)

type providerMetadataColumn struct {
	name       string
	definition string
}

type migrationColumn struct {
	name       string
	definition string
}

func (s *Store) migrateV2ToV3(ctx context.Context) error {
	columns := [...]providerMetadataColumn{
		{name: "protocol", definition: "protocol TEXT NOT NULL DEFAULT ''"},
		{name: "supports_tools", definition: "supports_tools INTEGER NOT NULL DEFAULT 0"},
		{name: "supports_streaming", definition: "supports_streaming INTEGER NOT NULL DEFAULT 0"},
		{name: "supports_thinking", definition: "supports_thinking INTEGER NOT NULL DEFAULT 0"},
		{name: "supports_model_discovery", definition: "supports_model_discovery INTEGER NOT NULL DEFAULT 0"},
		{name: "supports_count_tokens", definition: "supports_count_tokens INTEGER NOT NULL DEFAULT 0"},
		{name: "mode", definition: "mode TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		exists, err := s.providerColumnExists(ctx, column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE providers ADD COLUMN "+column.definition); err != nil {
			return fmt.Errorf("adding providers.%s column: %w", column.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, migrateV2ToV3DataSQL); err != nil {
		return fmt.Errorf("backfilling provider capabilities: %w", err)
	}
	return nil
}

func (s *Store) migrateV3ToV4(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting v3 to v4 migration: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.ExecContext(ctx, legacyV2SchemaSQL); err != nil {
		return fmt.Errorf("ensuring legacy runtime tables: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE sessions RENAME TO launches`); err != nil {
		return fmt.Errorf("renaming legacy sessions table: %w", err)
	}
	launchColumns := [...]string{
		"state TEXT NOT NULL DEFAULT 'legacy'",
		"lifecycle_state TEXT NOT NULL DEFAULT 'unobserved'",
		"statusline_state TEXT NOT NULL DEFAULT 'not-configured'",
		"started_at TEXT NOT NULL DEFAULT ''",
		"ended_at TEXT NOT NULL DEFAULT ''",
		"exit_code INTEGER",
		"end_reason TEXT NOT NULL DEFAULT ''",
	}
	if err := addMigrationColumns(ctx, tx, "launches", launchColumns[:]); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE launches SET state = 'legacy', started_at = created_at`); err != nil {
		return fmt.Errorf("backfilling legacy launches: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE agents RENAME TO agents_v3`); err != nil {
		return fmt.Errorf("renaming legacy agents table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, migrateV3ToV4CreateSQL); err != nil {
		return fmt.Errorf("creating v4 runtime tables: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO agents (
  id, launch_id, session_id, external_id, name, kind, model_alias, status,
  created_at, updated_at, ended_at
)
SELECT
  agents_v3.id,
  CASE WHEN EXISTS (SELECT 1 FROM launches WHERE launches.id = agents_v3.session_id)
    THEN agents_v3.session_id ELSE NULL END,
  NULL,
  'legacy-' || agents_v3.id,
  agents_v3.name,
  agents_v3.kind,
  agents_v3.model_alias,
  agents_v3.status,
  agents_v3.created_at,
  agents_v3.created_at,
  ''
FROM agents_v3
`); err != nil {
		return fmt.Errorf("migrating legacy agents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE agents_v3`); err != nil {
		return fmt.Errorf("dropping legacy agents table: %w", err)
	}

	conformanceColumns := [...]string{
		"scope TEXT NOT NULL DEFAULT 'provider'",
		"provider_name TEXT NOT NULL DEFAULT ''",
		"provider_model TEXT NOT NULL DEFAULT ''",
		"protocol TEXT NOT NULL DEFAULT ''",
		"claude_version TEXT NOT NULL DEFAULT ''",
		"started_at TEXT NOT NULL DEFAULT ''",
		"completed_at TEXT NOT NULL DEFAULT ''",
	}
	if err := addMigrationColumns(ctx, tx, "conformance_runs", conformanceColumns[:]); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conformance_runs
SET started_at = created_at, completed_at = created_at
`); err != nil {
		return fmt.Errorf("backfilling conformance timestamps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 4 WHERE id = 1 AND version = 3`); err != nil {
		return fmt.Errorf("updating schema version to 4: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing v3 to v4 migration: %w", err)
	}
	return nil
}

func (s *Store) migrateV4ToV5(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting v4 to v5 migration: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	columns := [...]migrationColumn{
		{name: "discovered_capabilities", definition: "discovered_capabilities TEXT NOT NULL DEFAULT '{}'"},
		{name: "capability_overrides", definition: "capability_overrides TEXT NOT NULL DEFAULT '{}'"},
		{name: "capabilities_refreshed_at", definition: "capabilities_refreshed_at TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		exists, err := tableColumnExists(ctx, tx, "models", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE models ADD COLUMN "+column.definition); err != nil {
			return fmt.Errorf("adding models.%s column: %w", column.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 5 WHERE id = 1 AND version = 4`); err != nil {
		return fmt.Errorf("updating schema version to 5: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing v4 to v5 migration: %w", err)
	}
	return nil
}

func (s *Store) migrateV5ToV6(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting v5 to v6 migration: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	exists, err := tableColumnExists(ctx, tx, "providers", "supports_responses")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE providers ADD COLUMN supports_responses INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("adding providers.supports_responses column: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 6 WHERE id = 1 AND version = 5`); err != nil {
		return fmt.Errorf("updating schema version to 6: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing v5 to v6 migration: %w", err)
	}
	return nil
}

func tableColumnExists(ctx context.Context, tx *sql.Tx, table, name string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("reading %s table columns: %w", table, err)
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("scanning %s table column: %w", table, err)
		}
		if columnName == name {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating %s table columns: %w", table, err)
	}
	return false, nil
}

func addMigrationColumns(ctx context.Context, tx *sql.Tx, table string, definitions []string) error {
	for _, definition := range definitions {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+definition); err != nil {
			return fmt.Errorf("adding %s column %q: %w", table, definition, err)
		}
	}
	return nil
}

func (s *Store) providerColumnExists(ctx context.Context, name string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(providers)`)
	if err != nil {
		return false, fmt.Errorf("reading providers table columns: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var cid int
		var columnName string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("scanning providers table column: %w", err)
		}
		if columnName == name {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating providers table columns: %w", err)
	}
	return false, nil
}
