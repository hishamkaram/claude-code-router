package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 1

type Store struct {
	db *sql.DB
}

type Provider struct {
	ID        int64
	Name      string
	Type      string
	BaseURL   string
	SecretRef string
	CreatedAt string
}

type Model struct {
	ID            int64
	Alias         string
	ProviderName  string
	ProviderModel string
	Status        string
	CreatedAt     string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store.Open: database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store.Open: creating database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store.Open: opening sqlite database: %w", err)
	}
	store := &Store{db: db}
	if err := store.db.PingContext(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("store.Open: pinging sqlite database: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store.Close: closing sqlite database: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store.Migrate: applying schema: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("store.Migrate: reading schema version: %w", err)
	}
	if version != CurrentSchemaVersion {
		return fmt.Errorf("store.Migrate: unsupported schema version %d", version)
	}
	return nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store.SchemaVersion: reading schema version: %w", err)
	}
	return version, nil
}

func (s *Store) AddProvider(ctx context.Context, provider Provider) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO providers (name, type, base_url, secret_ref, created_at)
VALUES (?, ?, ?, ?, ?)
`, provider.Name, provider.Type, provider.BaseURL, provider.SecretRef, now)
	if err != nil {
		return fmt.Errorf("store.AddProvider: inserting provider %q: %w", provider.Name, err)
	}
	return nil
}

func (s *Store) GetProvider(ctx context.Context, name string) (Provider, error) {
	var provider Provider
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, type, base_url, secret_ref, created_at
FROM providers
WHERE name = ?
`, name).Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef, &provider.CreatedAt)
	if err != nil {
		return Provider{}, fmt.Errorf("store.GetProvider: reading provider %q: %w", name, err)
	}
	return provider, nil
}

func (s *Store) ProviderExists(ctx context.Context, name string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM providers
WHERE name = ?
`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.ProviderExists: reading provider %q: %w", name, err)
	}
	return true, nil
}

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, type, base_url, secret_ref, created_at
FROM providers
ORDER BY name
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListProviders: querying providers: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var providers []Provider
	for rows.Next() {
		var provider Provider
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef, &provider.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListProviders: scanning provider: %w", err)
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListProviders: iterating providers: %w", err)
	}
	return providers, nil
}

func (s *Store) AddModel(ctx context.Context, model Model) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO models (alias, provider_id, provider_model, status, created_at)
SELECT ?, providers.id, ?, ?, ?
FROM providers
WHERE providers.name = ?
`, model.Alias, model.ProviderModel, model.Status, now, model.ProviderName)
	if err != nil {
		return fmt.Errorf("store.AddModel: inserting model %q: %w", model.Alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.AddModel: reading rows affected for %q: %w", model.Alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.AddModel: provider %q does not exist", model.ProviderName)
	}
	return nil
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT models.id, models.alias, providers.name, models.provider_model, models.status, models.created_at
FROM models
JOIN providers ON providers.id = models.provider_id
ORDER BY models.alias
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListModels: querying models: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var models []Model
	for rows.Next() {
		var model Model
		if err := rows.Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status, &model.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListModels: scanning model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListModels: iterating models: %w", err)
	}
	return models, nil
}

const schemaSQL = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);

INSERT INTO schema_version (id, version)
VALUES (1, 1)
ON CONFLICT(id) DO NOTHING;

CREATE TABLE IF NOT EXISTS providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS models (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL UNIQUE,
  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  provider_model TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`
