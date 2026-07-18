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

const migrateV3ToV4CreateSQL = `
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  claude_session_id TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'active',
  active_route_kind TEXT NOT NULL DEFAULT '',
  active_model_alias TEXT NOT NULL DEFAULT '',
  active_provider_name TEXT NOT NULL DEFAULT '',
  active_provider_model TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ended_at TEXT NOT NULL DEFAULT '',
  end_reason TEXT NOT NULL DEFAULT '',
  UNIQUE(launch_id, claude_session_id)
);

CREATE TABLE agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  ended_at TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id, external_id, kind)
);

CREATE TABLE tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL,
  teammate_name TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id, external_id)
);

CREATE TABLE event_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE route_events (
  event_id INTEGER PRIMARY KEY REFERENCES event_log(id) ON DELETE CASCADE,
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

CREATE TABLE lifecycle_events (
  event_id INTEGER PRIMARY KEY REFERENCES event_log(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL DEFAULT '',
  actor_kind TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE conformance_checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES conformance_runs(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  evidence TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX idx_sessions_launch_state ON sessions(launch_id, state);
CREATE INDEX idx_agents_launch_status ON agents(launch_id, status);
CREATE INDEX idx_tasks_launch_status ON tasks(launch_id, status);
CREATE INDEX idx_event_log_launch_time ON event_log(launch_id, occurred_at DESC);
CREATE INDEX idx_event_log_session_time ON event_log(session_id, occurred_at DESC);
CREATE INDEX idx_event_log_kind_time ON event_log(kind, occurred_at DESC);
CREATE INDEX idx_conformance_checks_run ON conformance_checks(run_id, id);
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

CREATE TABLE IF NOT EXISTS launches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_url TEXT NOT NULL DEFAULT '',
  pid INTEGER NOT NULL DEFAULT 0,
  model_alias TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'starting',
  lifecycle_state TEXT NOT NULL DEFAULT 'pending',
  statusline_state TEXT NOT NULL DEFAULT 'not-configured',
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  ended_at TEXT NOT NULL DEFAULT '',
  exit_code INTEGER,
  end_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  claude_session_id TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'active',
  active_route_kind TEXT NOT NULL DEFAULT '',
  active_model_alias TEXT NOT NULL DEFAULT '',
  active_provider_name TEXT NOT NULL DEFAULT '',
  active_provider_model TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ended_at TEXT NOT NULL DEFAULT '',
  end_reason TEXT NOT NULL DEFAULT '',
  UNIQUE(launch_id, claude_session_id)
);

CREATE TABLE IF NOT EXISTS agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  ended_at TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id, external_id, kind)
);

CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL,
  teammate_name TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  model_alias TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT '',
  UNIQUE(session_id, external_id)
);

CREATE TABLE IF NOT EXISTS event_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  launch_id INTEGER NOT NULL REFERENCES launches(id) ON DELETE CASCADE,
  session_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS route_events (
  event_id INTEGER PRIMARY KEY REFERENCES event_log(id) ON DELETE CASCADE,
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

CREATE TABLE IF NOT EXISTS lifecycle_events (
  event_id INTEGER PRIMARY KEY REFERENCES event_log(id) ON DELETE CASCADE,
  external_id TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL DEFAULT '',
  actor_kind TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS conformance_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  alias TEXT NOT NULL,
  status TEXT NOT NULL,
  live_verified INTEGER NOT NULL DEFAULT 0,
  details TEXT NOT NULL DEFAULT '',
  scope TEXT NOT NULL DEFAULT 'provider',
  provider_name TEXT NOT NULL DEFAULT '',
  provider_model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  claude_version TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT '',
  completed_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conformance_checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER NOT NULL REFERENCES conformance_runs(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  evidence TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_launch_state ON sessions(launch_id, state);
CREATE INDEX IF NOT EXISTS idx_agents_launch_status ON agents(launch_id, status);
CREATE INDEX IF NOT EXISTS idx_tasks_launch_status ON tasks(launch_id, status);
CREATE INDEX IF NOT EXISTS idx_event_log_launch_time ON event_log(launch_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_event_log_session_time ON event_log(session_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_event_log_kind_time ON event_log(kind, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_conformance_checks_run ON conformance_checks(run_id, id);
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
