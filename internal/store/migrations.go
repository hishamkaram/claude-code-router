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
