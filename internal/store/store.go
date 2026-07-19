package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 5

type Store struct {
	db *sql.DB
}

type Provider struct {
	ID                     int64
	Name                   string
	Type                   string
	BaseURL                string
	SecretRef              string
	Protocol               string
	SupportsTools          bool
	SupportsStreaming      bool
	SupportsThinking       bool
	SupportsModelDiscovery bool
	SupportsCountTokens    bool
	Mode                   string
	CreatedAt              string
}

type ProviderUpdateResult struct {
	CapabilitySnapshotsInvalidated int64
}

type Model struct {
	ID                      int64
	Alias                   string
	ProviderName            string
	ProviderModel           string
	Status                  string
	DiscoveredCapabilities  modelcap.Snapshot
	CapabilityOverrides     modelcap.Values
	CapabilitiesRefreshedAt string
	CreatedAt               string
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
	LaunchID   int64
	SessionID  int64
	ExternalID string
	Name       string
	Kind       string
	ModelAlias string
	Status     string
	CreatedAt  string
	UpdatedAt  string
	EndedAt    string
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
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.db.PingContext(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("store.Open: pinging sqlite database: %w", err)
	}
	pragmas := [...]string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	}
	for _, pragma := range pragmas {
		if _, err := store.db.ExecContext(ctx, pragma); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("store.Open: applying %q: %w", pragma, err)
		}
	}
	return store, nil
}

// OpenReadOnly opens an existing database without creating directories,
// changing journal settings, or permitting writes.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store.OpenReadOnly: database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store.OpenReadOnly: resolving database path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("store.OpenReadOnly: inspecting database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store.OpenReadOnly: database path must be a regular file")
	}
	databaseURL := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	databaseURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return nil, fmt.Errorf("store.OpenReadOnly: opening sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.db.PingContext(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("store.OpenReadOnly: pinging sqlite database: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("store.OpenReadOnly: enforcing query-only mode: %w", err)
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
		return s.migrateFromV1(ctx)
	case 2:
		return s.migrateFromV2(ctx)
	case 3:
		return s.migrateFromV3(ctx)
	case 4:
		return s.migrateFromV4(ctx)
	case CurrentSchemaVersion:
		return s.ensureCurrentSchema(ctx)
	default:
		return fmt.Errorf("store.Migrate: unsupported schema version %d", version)
	}
}

func (s *Store) migrateFromV1(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, migrateV1ToV2SQL); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 2: %w", err)
	}
	if err := s.migrateV2ToV3(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 3: %w", err)
	}
	if err := s.migrateV3ToV4(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 4: %w", err)
	}
	if err := s.migrateV4ToV5(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 5: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV2(ctx context.Context) error {
	if err := s.migrateV2ToV3(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 2 to 3: %w", err)
	}
	if err := s.migrateV3ToV4(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 2 to 4: %w", err)
	}
	if err := s.migrateV4ToV5(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 2 to 5: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV3(ctx context.Context) error {
	if err := s.migrateV3ToV4(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 3 to 4: %w", err)
	}
	if err := s.migrateV4ToV5(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 3 to 5: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV4(ctx context.Context) error {
	if err := s.migrateV4ToV5(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 4 to 5: %w", err)
	}
	return nil
}

func (s *Store) ensureCurrentSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, currentSchemaSQL); err != nil {
		return fmt.Errorf("store.Migrate: ensuring current schema: %w", err)
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
	provider = providerWithMetadataDefaults(provider)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO providers (
  name, type, base_url, secret_ref, protocol, supports_tools, supports_streaming,
  supports_thinking, supports_model_discovery, supports_count_tokens, mode, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, provider.Name, provider.Type, provider.BaseURL, provider.SecretRef, provider.Protocol, boolToInt(provider.SupportsTools),
		boolToInt(provider.SupportsStreaming), boolToInt(provider.SupportsThinking), boolToInt(provider.SupportsModelDiscovery),
		boolToInt(provider.SupportsCountTokens), provider.Mode, now)
	if err != nil {
		return fmt.Errorf("store.AddProvider: inserting provider %q: %w", provider.Name, err)
	}
	return nil
}

func (s *Store) UpdateProvider(ctx context.Context, provider Provider) (ProviderUpdateResult, error) {
	provider = providerWithMetadataDefaults(provider)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: starting transaction for provider %q: %w", provider.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing Provider
	err = tx.QueryRowContext(ctx, `
SELECT id, type, base_url, secret_ref, protocol
FROM providers
WHERE name = ?
`, provider.Name).Scan(&existing.ID, &existing.Type, &existing.BaseURL, &existing.SecretRef, &existing.Protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: provider %q does not exist", provider.Name)
	}
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading provider %q before update: %w", provider.Name, err)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE providers
SET type = ?, base_url = ?, secret_ref = ?, protocol = ?, supports_tools = ?,
  supports_streaming = ?, supports_thinking = ?, supports_model_discovery = ?,
  supports_count_tokens = ?, mode = ?
WHERE name = ?
`, provider.Type, provider.BaseURL, provider.SecretRef, provider.Protocol, boolToInt(provider.SupportsTools),
		boolToInt(provider.SupportsStreaming), boolToInt(provider.SupportsThinking), boolToInt(provider.SupportsModelDiscovery),
		boolToInt(provider.SupportsCountTokens), provider.Mode, provider.Name)
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: updating provider %q: %w", provider.Name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading rows affected for provider %q: %w", provider.Name, err)
	}
	if affected == 0 {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: provider %q does not exist", provider.Name)
	}

	updateResult := ProviderUpdateResult{}
	if providerCapabilitySourceChanged(existing, provider) {
		invalidated, invalidateErr := tx.ExecContext(ctx, `
UPDATE models
SET discovered_capabilities = '{}', capabilities_refreshed_at = ''
WHERE provider_id = ?
  AND (capabilities_refreshed_at <> '' OR discovered_capabilities NOT IN ('{}', '{"values":{}}'))
`, existing.ID)
		if invalidateErr != nil {
			return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: invalidating model capabilities for provider %q: %w", provider.Name, invalidateErr)
		}
		updateResult.CapabilitySnapshotsInvalidated, err = invalidated.RowsAffected()
		if err != nil {
			return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading invalidated model count for provider %q: %w", provider.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: committing provider %q update: %w", provider.Name, err)
	}
	return updateResult, nil
}

func providerCapabilitySourceChanged(existing, updated Provider) bool {
	return existing.Type != updated.Type || existing.BaseURL != updated.BaseURL ||
		existing.SecretRef != updated.SecretRef || existing.Protocol != updated.Protocol
}

func (s *Store) GetProvider(ctx context.Context, name string) (Provider, error) {
	var provider Provider
	var supportsTools, supportsStreaming, supportsThinking, supportsModelDiscovery, supportsCountTokens int
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, mode, created_at
FROM providers
WHERE name = ?
`, name).Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef, &provider.Protocol,
		&supportsTools, &supportsStreaming, &supportsThinking, &supportsModelDiscovery, &supportsCountTokens,
		&provider.Mode, &provider.CreatedAt)
	if err != nil {
		return Provider{}, fmt.Errorf("store.GetProvider: reading provider %q: %w", name, err)
	}
	provider.SupportsTools = intToBool(supportsTools)
	provider.SupportsStreaming = intToBool(supportsStreaming)
	provider.SupportsThinking = intToBool(supportsThinking)
	provider.SupportsModelDiscovery = intToBool(supportsModelDiscovery)
	provider.SupportsCountTokens = intToBool(supportsCountTokens)
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
SELECT id, name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, mode, created_at
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
		var supportsTools, supportsStreaming, supportsThinking, supportsModelDiscovery, supportsCountTokens int
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef,
			&provider.Protocol, &supportsTools, &supportsStreaming, &supportsThinking, &supportsModelDiscovery,
			&supportsCountTokens, &provider.Mode, &provider.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListProviders: scanning provider: %w", err)
		}
		provider.SupportsTools = intToBool(supportsTools)
		provider.SupportsStreaming = intToBool(supportsStreaming)
		provider.SupportsThinking = intToBool(supportsThinking)
		provider.SupportsModelDiscovery = intToBool(supportsModelDiscovery)
		provider.SupportsCountTokens = intToBool(supportsCountTokens)
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
	discoveredJSON, overridesJSON, err := encodeModelCapabilities(model)
	if err != nil {
		return fmt.Errorf("store.AddModel: encoding capabilities for model %q: %w", model.Alias, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO models (
  alias, provider_id, provider_model, status, discovered_capabilities,
  capability_overrides, capabilities_refreshed_at, created_at
)
SELECT ?, providers.id, ?, ?, ?, ?, ?, ?
FROM providers
WHERE providers.name = ?
`, model.Alias, model.ProviderModel, model.Status, discoveredJSON, overridesJSON,
		model.CapabilitiesRefreshedAt, now, model.ProviderName)
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
	var discoveredJSON, overridesJSON string
	err := s.db.QueryRowContext(ctx, `
SELECT models.id, models.alias, providers.name, models.provider_model, models.status,
  models.discovered_capabilities, models.capability_overrides,
  models.capabilities_refreshed_at, models.created_at
FROM models
JOIN providers ON providers.id = models.provider_id
WHERE models.alias = ?
`, alias).Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status,
		&discoveredJSON, &overridesJSON, &model.CapabilitiesRefreshedAt, &model.CreatedAt)
	if err != nil {
		return Model{}, fmt.Errorf("store.GetModel: reading model %q: %w", alias, err)
	}
	if err := decodeModelCapabilities(&model, discoveredJSON, overridesJSON); err != nil {
		return Model{}, fmt.Errorf("store.GetModel: decoding capabilities for model %q: %w", alias, err)
	}
	return model, nil
}

func (s *Store) UpdateModel(ctx context.Context, model Model) error {
	discoveredJSON, overridesJSON, err := encodeModelCapabilities(model)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: encoding capabilities for model %q: %w", model.Alias, err)
	}
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
SET provider_id = ?, provider_model = ?, status = ?, discovered_capabilities = ?,
  capability_overrides = ?, capabilities_refreshed_at = ?
WHERE alias = ?
`, providerID, model.ProviderModel, model.Status, discoveredJSON, overridesJSON,
		model.CapabilitiesRefreshedAt, model.Alias)
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
SELECT models.id, models.alias, providers.name, models.provider_model, models.status,
  models.discovered_capabilities, models.capability_overrides,
  models.capabilities_refreshed_at, models.created_at
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
		var discoveredJSON, overridesJSON string
		if err := rows.Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status,
			&discoveredJSON, &overridesJSON, &model.CapabilitiesRefreshedAt, &model.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListModels: scanning model: %w", err)
		}
		if err := decodeModelCapabilities(&model, discoveredJSON, overridesJSON); err != nil {
			return nil, fmt.Errorf("store.ListModels: decoding capabilities for model %q: %w", model.Alias, err)
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
INSERT INTO launches (
  gateway_url, pid, model_alias, state, lifecycle_state, statusline_state,
  created_at, started_at
)
VALUES (?, ?, ?, 'running', 'unobserved', 'not-configured', ?, ?)
`, session.GatewayURL, session.PID, session.ModelAlias, now, now)
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
FROM launches
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
	externalID := agent.ExternalID
	if externalID == "" {
		externalID = fmt.Sprintf("legacy-%d", time.Now().UTC().UnixNano())
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO agents (
  launch_id, session_id, external_id, name, kind, model_alias, status,
  created_at, updated_at
)
VALUES (NULL, NULL, ?, ?, ?, ?, ?, ?, ?)
`, externalID, agent.Name, agent.Kind, agent.ModelAlias, agent.Status, now, now)
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
SELECT id, COALESCE(launch_id, 0), COALESCE(session_id, 0), external_id,
  name, kind, model_alias, status, created_at, updated_at, ended_at
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
		if err := rows.Scan(&agent.ID, &agent.LaunchID, &agent.SessionID, &agent.ExternalID,
			&agent.Name, &agent.Kind, &agent.ModelAlias, &agent.Status, &agent.CreatedAt,
			&agent.UpdatedAt, &agent.EndedAt); err != nil {
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
