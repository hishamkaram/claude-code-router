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

const CurrentSchemaVersion = 2

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

type Session struct {
	ID         int64
	GatewayURL string
	PID        int
	ModelAlias string
	CreatedAt  string
}

type Agent struct {
	ID         int64
	SessionID  int64
	Name       string
	Kind       string
	ModelAlias string
	Status     string
	CreatedAt  string
}

type ConformanceRecord struct {
	ID           int64
	Alias        string
	Status       string
	LiveVerified bool
	Details      string
	CreatedAt    string
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
	if _, err := s.db.ExecContext(ctx, bootstrapSchemaSQL); err != nil {
		return fmt.Errorf("store.Migrate: applying schema bootstrap: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("store.Migrate: reading schema version: %w", err)
	}
	switch version {
	case 1:
		if _, err := s.db.ExecContext(ctx, migrateV1ToV2SQL); err != nil {
			return fmt.Errorf("store.Migrate: migrating schema from version 1 to 2: %w", err)
		}
	case CurrentSchemaVersion:
		if _, err := s.db.ExecContext(ctx, currentSchemaSQL); err != nil {
			return fmt.Errorf("store.Migrate: ensuring current schema: %w", err)
		}
	default:
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

func (s *Store) UpdateProvider(ctx context.Context, provider Provider) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE providers
SET type = ?, base_url = ?, secret_ref = ?
WHERE name = ?
`, provider.Type, provider.BaseURL, provider.SecretRef, provider.Name)
	if err != nil {
		return fmt.Errorf("store.UpdateProvider: updating provider %q: %w", provider.Name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdateProvider: reading rows affected for provider %q: %w", provider.Name, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.UpdateProvider: provider %q does not exist", provider.Name)
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

func (s *Store) RemoveProvider(ctx context.Context, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: beginning transaction for provider %q: %w", name, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var providerID int64
	queryErr := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE name = ?`, name).Scan(&providerID)
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return 0, fmt.Errorf("store.RemoveProvider: provider %q does not exist", name)
		}
		return 0, fmt.Errorf("store.RemoveProvider: reading provider %q: %w", name, queryErr)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM models WHERE provider_id = ?`, providerID)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: deleting models for provider %q: %w", name, err)
	}
	modelsRemoved, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: reading model rows affected for provider %q: %w", name, err)
	}

	result, err = tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, providerID)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: deleting provider %q: %w", name, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: reading provider rows affected for %q: %w", name, err)
	}
	if removed != 1 {
		return 0, fmt.Errorf("store.RemoveProvider: provider %q does not exist", name)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: committing removal for provider %q: %w", name, err)
	}
	return modelsRemoved, nil
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

func (s *Store) GetModel(ctx context.Context, alias string) (Model, error) {
	var model Model
	err := s.db.QueryRowContext(ctx, `
SELECT models.id, models.alias, providers.name, models.provider_model, models.status, models.created_at
FROM models
JOIN providers ON providers.id = models.provider_id
WHERE models.alias = ?
`, alias).Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status, &model.CreatedAt)
	if err != nil {
		return Model{}, fmt.Errorf("store.GetModel: reading model %q: %w", alias, err)
	}
	return model, nil
}

func (s *Store) UpdateModel(ctx context.Context, model Model) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: beginning transaction for model %q: %w", model.Alias, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var providerID int64
	queryErr := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE name = ?`, model.ProviderName).Scan(&providerID)
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return fmt.Errorf("store.UpdateModel: provider %q does not exist", model.ProviderName)
		}
		return fmt.Errorf("store.UpdateModel: reading provider %q: %w", model.ProviderName, queryErr)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE models
SET provider_id = ?, provider_model = ?, status = ?
WHERE alias = ?
`, providerID, model.ProviderModel, model.Status, model.Alias)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: updating model %q: %w", model.Alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdateModel: reading rows affected for model %q: %w", model.Alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.UpdateModel: model %q does not exist", model.Alias)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.UpdateModel: committing update for model %q: %w", model.Alias, err)
	}
	return nil
}

func (s *Store) RemoveModel(ctx context.Context, alias string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE alias = ?`, alias)
	if err != nil {
		return fmt.Errorf("store.RemoveModel: deleting model %q: %w", alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RemoveModel: reading rows affected for model %q: %w", alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.RemoveModel: model %q does not exist", alias)
	}
	return nil
}

func (s *Store) ModelExists(ctx context.Context, alias string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM models
WHERE alias = ?
`, alias).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.ModelExists: reading model %q: %w", alias, err)
	}
	return true, nil
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

func (s *Store) AddSession(ctx context.Context, session Session) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (gateway_url, pid, model_alias, created_at)
VALUES (?, ?, ?, ?)
`, session.GatewayURL, session.PID, session.ModelAlias, now)
	if err != nil {
		return 0, fmt.Errorf("store.AddSession: inserting session for pid %d: %w", session.PID, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.AddSession: reading inserted session id: %w", err)
	}
	return id, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, gateway_url, pid, model_alias, created_at
FROM sessions
ORDER BY id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListSessions: querying sessions: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var sessions []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.GatewayURL, &session.PID, &session.ModelAlias, &session.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListSessions: scanning session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListSessions: iterating sessions: %w", err)
	}
	return sessions, nil
}

func (s *Store) AddAgent(ctx context.Context, agent Agent) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO agents (session_id, name, kind, model_alias, status, created_at)
VALUES (?, ?, ?, ?, ?, ?)
`, agent.SessionID, agent.Name, agent.Kind, agent.ModelAlias, agent.Status, now)
	if err != nil {
		return 0, fmt.Errorf("store.AddAgent: inserting agent %q: %w", agent.Name, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.AddAgent: reading inserted agent id: %w", err)
	}
	return id, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, name, kind, model_alias, status, created_at
FROM agents
ORDER BY id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListAgents: querying agents: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var agents []Agent
	for rows.Next() {
		var agent Agent
		if err := rows.Scan(&agent.ID, &agent.SessionID, &agent.Name, &agent.Kind, &agent.ModelAlias, &agent.Status, &agent.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListAgents: scanning agent: %w", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListAgents: iterating agents: %w", err)
	}
	return agents, nil
}

func (s *Store) AddConformanceRecord(ctx context.Context, record ConformanceRecord) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	liveVerified := 0
	if record.LiveVerified {
		liveVerified = 1
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO conformance_runs (alias, status, live_verified, details, created_at)
VALUES (?, ?, ?, ?, ?)
`, record.Alias, record.Status, liveVerified, record.Details, now)
	if err != nil {
		return 0, fmt.Errorf("store.AddConformanceRecord: inserting record for alias %q: %w", record.Alias, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.AddConformanceRecord: reading inserted record id: %w", err)
	}
	return id, nil
}

func (s *Store) ListConformanceRecords(ctx context.Context) ([]ConformanceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, alias, status, live_verified, details, created_at
FROM conformance_runs
ORDER BY id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListConformanceRecords: querying records: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var records []ConformanceRecord
	for rows.Next() {
		var record ConformanceRecord
		var liveVerified int
		if err := rows.Scan(&record.ID, &record.Alias, &record.Status, &liveVerified, &record.Details, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListConformanceRecords: scanning record: %w", err)
		}
		record.LiveVerified = liveVerified == 1
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListConformanceRecords: iterating records: %w", err)
	}
	return records, nil
}

const bootstrapSchemaSQL = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);

INSERT INTO schema_version (id, version)
VALUES (1, 1)
ON CONFLICT(id) DO NOTHING;
`

const migrateV1ToV2SQL = currentSchemaSQL + `
UPDATE schema_version
SET version = 2
WHERE id = 1 AND version = 1;
`

const currentSchemaSQL = `
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

CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_url TEXT NOT NULL,
  pid INTEGER NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conformance_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL,
  status TEXT NOT NULL,
  live_verified INTEGER NOT NULL DEFAULT 0,
  details TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
`
