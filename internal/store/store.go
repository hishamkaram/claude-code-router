package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 7

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
	SupportsResponses      bool
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
	case 5:
		return s.migrateFromV5(ctx)
	case 6:
		return s.migrateFromV6(ctx)
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
	if err := s.migrateV5ToV6(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 6: %w", err)
	}
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 1 to 7: %w", err)
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
	if err := s.migrateV5ToV6(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 2 to 6: %w", err)
	}
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 2 to 7: %w", err)
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
	if err := s.migrateV5ToV6(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 3 to 6: %w", err)
	}
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 3 to 7: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV4(ctx context.Context) error {
	if err := s.migrateV4ToV5(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 4 to 5: %w", err)
	}
	if err := s.migrateV5ToV6(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 4 to 6: %w", err)
	}
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 4 to 7: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV5(ctx context.Context) error {
	if err := s.migrateV5ToV6(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 5 to 6: %w", err)
	}
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 5 to 7: %w", err)
	}
	return nil
}

func (s *Store) migrateFromV6(ctx context.Context) error {
	if err := s.migrateV6ToV7(ctx); err != nil {
		return fmt.Errorf("store.Migrate: migrating schema from version 6 to 7: %w", err)
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
