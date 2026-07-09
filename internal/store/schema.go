package store

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

const migrateV1ToV2SQL = legacyV2SchemaSQL + `
UPDATE schema_version
SET version = 2
WHERE id = 1 AND version = 1;
`

const migrateV2ToV3DataSQL = `
UPDATE providers
SET
  protocol = CASE
    WHEN type IN ('anthropic', 'zai', 'anthropic-compatible') THEN 'anthropic-compatible'
    WHEN type IN ('litellm', 'local', 'openrouter', 'zai-openai', 'openai-compatible') THEN 'openai-compatible'
    ELSE ''
  END,
  supports_tools = CASE
    WHEN type IN ('anthropic', 'zai', 'anthropic-compatible', 'litellm', 'local', 'openrouter', 'zai-openai', 'openai-compatible') THEN 1
    ELSE 0
  END,
  supports_streaming = CASE
    WHEN type IN ('anthropic', 'zai', 'anthropic-compatible', 'litellm', 'local', 'openrouter', 'zai-openai', 'openai-compatible') THEN 1
    ELSE 0
  END,
  supports_thinking = CASE
    WHEN type IN ('anthropic', 'zai', 'anthropic-compatible', 'litellm', 'local', 'openrouter', 'zai-openai', 'openai-compatible') THEN 1
    ELSE 0
  END,
  supports_model_discovery = CASE
    WHEN type IN ('anthropic', 'litellm', 'local', 'openrouter', 'zai-openai', 'openai-compatible') THEN 1
    ELSE 0
  END,
  supports_count_tokens = CASE
    WHEN type IN ('anthropic', 'zai') THEN 1
    ELSE 0
  END,
  mode = CASE
    WHEN type IN ('anthropic', 'zai') THEN 'full'
    ELSE 'degraded'
  END;

UPDATE schema_version
SET version = 3
WHERE id = 1 AND version = 2;
`

const currentSchemaSQL = `
CREATE TABLE IF NOT EXISTS providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  supports_tools INTEGER NOT NULL DEFAULT 0,
  supports_streaming INTEGER NOT NULL DEFAULT 0,
  supports_thinking INTEGER NOT NULL DEFAULT 0,
  supports_model_discovery INTEGER NOT NULL DEFAULT 0,
  supports_count_tokens INTEGER NOT NULL DEFAULT 0,
  mode TEXT NOT NULL DEFAULT '',
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

const legacyV2SchemaSQL = `
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
