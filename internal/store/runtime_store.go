package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Launch struct {
	ID                int64
	GatewayURL        string
	PID               int
	ModelAlias        string
	State             string
	LifecycleState    string
	StatuslineState   string
	AuthMode          string
	ClaudeAccountName string
	CreatedAt         string
	StartedAt         string
	EndedAt           string
	ExitCode          *int
	EndReason         string
}

type ClaudeSession struct {
	ID                  int64
	LaunchID            int64
	ClaudeSessionID     string
	Source              string
	State               string
	ActiveRouteKind     string
	ActiveModelAlias    string
	ActiveProviderName  string
	ActiveProviderModel string
	StartedAt           string
	LastSeenAt          string
	EndedAt             string
	EndReason           string
}

type Task struct {
	ID           int64
	LaunchID     int64
	SessionID    int64
	ExternalID   string
	TeammateName string
	TeamName     string
	ModelAlias   string
	Status       string
	CreatedAt    string
	UpdatedAt    string
	CompletedAt  string
}

func (s *Store) CreateLaunch(ctx context.Context, modelAlias, lifecycleState, statuslineState string) (int64, error) {
	return s.CreateLaunchWithAuth(ctx, modelAlias, lifecycleState, statuslineState, "", "")
}

func (s *Store) CreateLaunchWithAuth(
	ctx context.Context,
	modelAlias, lifecycleState, statuslineState, authMode, claudeAccountName string,
) (int64, error) {
	if claudeAccountName != "" {
		if err := validateClaudeAccountName(claudeAccountName); err != nil {
			return 0, fmt.Errorf("store.CreateLaunchWithAuth: %w", err)
		}
	}
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO launches (
  gateway_url, pid, model_alias, state, lifecycle_state, statusline_state,
  auth_mode, claude_account_name, created_at, started_at
)
VALUES ('', 0, ?, 'starting', ?, ?, ?, ?, ?, '')
`, modelAlias, lifecycleState, statuslineState, authMode, claudeAccountName, now)
	if err != nil {
		return 0, fmt.Errorf("store.CreateLaunchWithAuth: inserting launch: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.CreateLaunchWithAuth: reading launch id: %w", err)
	}
	return id, nil
}

func (s *Store) ActivateLaunch(ctx context.Context, id int64, gatewayURL string, pid int) error {
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
UPDATE launches
SET gateway_url = ?, pid = ?, state = 'running', started_at = ?
WHERE id = ?
`, gatewayURL, pid, now, id)
	if err != nil {
		return fmt.Errorf("store.ActivateLaunch: updating launch %d: %w", id, err)
	}
	return requireAffected("store.ActivateLaunch", "launch", id, result)
}

func (s *Store) FinishLaunch(ctx context.Context, id int64, state, reason string, exitCode *int) error {
	now := runtimeTimestamp()
	var code any
	if exitCode != nil {
		code = *exitCode
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE launches
SET state = ?, ended_at = ?, exit_code = ?, end_reason = ?
WHERE id = ?
`, state, now, code, reason, id)
	if err != nil {
		return fmt.Errorf("store.FinishLaunch: updating launch %d: %w", id, err)
	}
	return requireAffected("store.FinishLaunch", "launch", id, result)
}

func (s *Store) SetLaunchLifecycleState(ctx context.Context, id int64, state string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE launches SET lifecycle_state = ? WHERE id = ?`, state, id)
	if err != nil {
		return fmt.Errorf("store.SetLaunchLifecycleState: updating launch %d: %w", id, err)
	}
	return requireAffected("store.SetLaunchLifecycleState", "launch", id, result)
}

func (s *Store) SetLaunchStatuslineState(ctx context.Context, id int64, state string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE launches SET statusline_state = ? WHERE id = ?`, state, id)
	if err != nil {
		return fmt.Errorf("store.SetLaunchStatuslineState: updating launch %d: %w", id, err)
	}
	return requireAffected("store.SetLaunchStatuslineState", "launch", id, result)
}

func (s *Store) GetLaunch(ctx context.Context, id int64) (Launch, error) {
	row := s.db.QueryRowContext(ctx, launchSelectSQL+` WHERE id = ?`, id)
	launch, err := scanLaunch(row)
	if err != nil {
		return Launch{}, fmt.Errorf("store.GetLaunch: reading launch %d: %w", id, err)
	}
	return launch, nil
}

func (s *Store) ListLaunches(ctx context.Context) ([]Launch, error) {
	rows, err := s.db.QueryContext(ctx, launchSelectSQL+` ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListLaunches: querying launches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var launches []Launch
	for rows.Next() {
		launch, scanErr := scanLaunch(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store.ListLaunches: scanning launch: %w", scanErr)
		}
		launches = append(launches, launch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListLaunches: iterating launches: %w", err)
	}
	return launches, nil
}

func (s *Store) UpsertClaudeSession(ctx context.Context, session ClaudeSession) (int64, error) {
	now := runtimeTimestamp()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (
  launch_id, claude_session_id, source, state, started_at, last_seen_at
)
VALUES (?, ?, ?, 'active', ?, ?)
ON CONFLICT(launch_id, claude_session_id) DO UPDATE SET
  source = excluded.source,
  state = 'active',
  last_seen_at = excluded.last_seen_at,
  ended_at = '',
  end_reason = ''
`, session.LaunchID, session.ClaudeSessionID, session.Source, now, now)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertClaudeSession: upserting Claude session: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `
SELECT id FROM sessions WHERE launch_id = ? AND claude_session_id = ?
`, session.LaunchID, session.ClaudeSessionID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store.UpsertClaudeSession: reading Claude session id: %w", err)
	}
	return id, nil
}

func (s *Store) UpdateClaudeSessionRoute(ctx context.Context, id int64, routeKind, alias, providerName, providerModel string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET active_route_kind = ?, active_model_alias = ?, active_provider_name = ?,
  active_provider_model = ?, last_seen_at = ?
WHERE id = ?
`, routeKind, alias, providerName, providerModel, runtimeTimestamp(), id)
	if err != nil {
		return fmt.Errorf("store.UpdateClaudeSessionRoute: updating session %d: %w", id, err)
	}
	return requireAffected("store.UpdateClaudeSessionRoute", "session", id, result)
}

func (s *Store) EndClaudeSession(ctx context.Context, id int64, state, reason string) error {
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
UPDATE sessions SET state = ?, last_seen_at = ?, ended_at = ?, end_reason = ?
WHERE id = ?
`, state, now, now, reason, id)
	if err != nil {
		return fmt.Errorf("store.EndClaudeSession: updating session %d: %w", id, err)
	}
	return requireAffected("store.EndClaudeSession", "session", id, result)
}

func (s *Store) ListClaudeSessions(ctx context.Context, launchID int64, activeOnly bool) ([]ClaudeSession, error) {
	query := claudeSessionSelectSQL + ` WHERE (? = 0 OR launch_id = ?)`
	if activeOnly {
		query += ` AND state = 'active'`
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, query, launchID, launchID)
	if err != nil {
		return nil, fmt.Errorf("store.ListClaudeSessions: querying sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []ClaudeSession
	for rows.Next() {
		var session ClaudeSession
		if err := scanClaudeSession(rows, &session); err != nil {
			return nil, fmt.Errorf("store.ListClaudeSessions: scanning session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListClaudeSessions: iterating sessions: %w", err)
	}
	return sessions, nil
}

func (s *Store) GetClaudeSession(ctx context.Context, launchID int64, claudeSessionID string) (ClaudeSession, error) {
	row := s.db.QueryRowContext(ctx, claudeSessionSelectSQL+`
 WHERE launch_id = ? AND claude_session_id = ?
`, launchID, claudeSessionID)
	var session ClaudeSession
	if err := scanClaudeSession(row, &session); err != nil {
		return ClaudeSession{}, fmt.Errorf("store.GetClaudeSession: reading Claude session %q: %w", claudeSessionID, err)
	}
	return session, nil
}

func (s *Store) UpsertAgent(ctx context.Context, agent Agent) (int64, error) {
	now := runtimeTimestamp()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO agents (
  launch_id, session_id, external_id, name, kind, model_alias, status,
  created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, external_id, kind) DO UPDATE SET
  name = excluded.name,
  model_alias = excluded.model_alias,
  status = excluded.status,
  updated_at = excluded.updated_at,
  ended_at = ''
`, agent.LaunchID, agent.SessionID, agent.ExternalID, agent.Name, agent.Kind,
		agent.ModelAlias, agent.Status, now, now)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertAgent: upserting agent %q: %w", agent.ExternalID, err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `
SELECT id FROM agents WHERE session_id = ? AND external_id = ? AND kind = ?
`, agent.SessionID, agent.ExternalID, agent.Kind).Scan(&id); err != nil {
		return 0, fmt.Errorf("store.UpsertAgent: reading agent id: %w", err)
	}
	return id, nil
}

func (s *Store) FinishAgent(ctx context.Context, sessionID int64, externalID, kind, status string) (bool, error) {
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
UPDATE agents SET status = ?, updated_at = ?, ended_at = ?
WHERE session_id = ? AND external_id = ? AND kind = ?
`, status, now, now, sessionID, externalID, kind)
	if err != nil {
		return false, fmt.Errorf("store.FinishAgent: updating agent %q: %w", externalID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.FinishAgent: reading affected rows: %w", err)
	}
	return affected > 0, nil
}

func (s *Store) ListRuntimeAgents(ctx context.Context, launchID, sessionID int64, activeOnly bool) ([]Agent, error) {
	query := `
SELECT id, COALESCE(launch_id, 0), COALESCE(session_id, 0), external_id,
  name, kind, model_alias, status, created_at, updated_at, ended_at
FROM agents
WHERE (? = 0 OR launch_id = ?) AND (? = 0 OR session_id = ?)`
	if activeOnly {
		query += ` AND status IN ('running', 'idle', 'pending')`
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, query, launchID, launchID, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ListRuntimeAgents: querying agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var agents []Agent
	for rows.Next() {
		var agent Agent
		if err := rows.Scan(&agent.ID, &agent.LaunchID, &agent.SessionID,
			&agent.ExternalID, &agent.Name, &agent.Kind, &agent.ModelAlias,
			&agent.Status, &agent.CreatedAt, &agent.UpdatedAt, &agent.EndedAt); err != nil {
			return nil, fmt.Errorf("store.ListRuntimeAgents: scanning agent: %w", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListRuntimeAgents: iterating agents: %w", err)
	}
	return agents, nil
}

func (s *Store) UpsertTask(ctx context.Context, task Task) (int64, error) {
	now := runtimeTimestamp()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tasks (
  launch_id, session_id, external_id, teammate_name, team_name, model_alias,
  status, created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, external_id) DO UPDATE SET
  teammate_name = excluded.teammate_name,
  team_name = excluded.team_name,
  model_alias = excluded.model_alias,
  status = excluded.status,
  updated_at = excluded.updated_at
`, task.LaunchID, task.SessionID, task.ExternalID, task.TeammateName, task.TeamName,
		task.ModelAlias, task.Status, now, now)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertTask: upserting task %q: %w", task.ExternalID, err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `
SELECT id FROM tasks WHERE session_id = ? AND external_id = ?
`, task.SessionID, task.ExternalID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store.UpsertTask: reading task id: %w", err)
	}
	return id, nil
}

func (s *Store) FinishTask(ctx context.Context, sessionID int64, externalID, teammateName, teamName, status string) (bool, error) {
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
UPDATE tasks SET
  teammate_name = CASE WHEN teammate_name = '' THEN ? ELSE teammate_name END,
  team_name = CASE WHEN team_name = '' THEN ? ELSE team_name END,
  status = ?, updated_at = ?, completed_at = ?
WHERE session_id = ? AND external_id = ?
`, teammateName, teamName, status, now, now, sessionID, externalID)
	if err != nil {
		return false, fmt.Errorf("store.FinishTask: updating task %q: %w", externalID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.FinishTask: reading affected rows: %w", err)
	}
	return affected > 0, nil
}

func (s *Store) ListTasks(ctx context.Context, launchID, sessionID int64, activeOnly bool) ([]Task, error) {
	query := `
SELECT id, launch_id, session_id, external_id, teammate_name, team_name,
  model_alias, status, created_at, updated_at, completed_at
FROM tasks
WHERE (? = 0 OR launch_id = ?) AND (? = 0 OR session_id = ?)`
	if activeOnly {
		query += ` AND status IN ('pending', 'running')`
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, query, launchID, launchID, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.ListTasks: querying tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.LaunchID, &task.SessionID,
			&task.ExternalID, &task.TeammateName, &task.TeamName, &task.ModelAlias,
			&task.Status, &task.CreatedAt, &task.UpdatedAt, &task.CompletedAt); err != nil {
			return nil, fmt.Errorf("store.ListTasks: scanning task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTasks: iterating tasks: %w", err)
	}
	return tasks, nil
}

func (s *Store) AbandonLaunchRuntime(ctx context.Context, launchID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.AbandonLaunchRuntime: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := runtimeTimestamp()
	statements := [...]string{
		`UPDATE sessions SET state = 'abandoned', last_seen_at = ?, ended_at = ?, end_reason = 'process_exit' WHERE launch_id = ? AND state = 'active'`,
		`UPDATE agents SET status = 'abandoned', updated_at = ?, ended_at = ? WHERE launch_id = ? AND status IN ('running', 'idle')`,
		`UPDATE tasks SET status = 'abandoned', updated_at = ?, completed_at = ? WHERE launch_id = ? AND status = 'pending'`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, now, now, launchID); err != nil {
			return fmt.Errorf("store.AbandonLaunchRuntime: finalizing launch %d: %w", launchID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE event_log
SET status = 'abandoned', completed_at = ?
WHERE launch_id = ? AND kind = 'route' AND status = 'started'
`, now, launchID); err != nil {
		return fmt.Errorf("store.AbandonLaunchRuntime: finalizing route events for launch %d: %w", launchID, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE route_events
SET error_class = 'launch_exit'
WHERE event_id IN (
  SELECT id FROM event_log WHERE launch_id = ? AND kind = 'route' AND status = 'abandoned'
)
`, launchID); err != nil {
		return fmt.Errorf("store.AbandonLaunchRuntime: annotating route events for launch %d: %w", launchID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.AbandonLaunchRuntime: committing launch %d: %w", launchID, err)
	}
	return nil
}

const launchSelectSQL = `
SELECT id, gateway_url, pid, model_alias, state, lifecycle_state,
  statusline_state, auth_mode, claude_account_name, created_at, started_at,
  ended_at, exit_code, end_reason
FROM launches`

const claudeSessionSelectSQL = `
SELECT id, launch_id, claude_session_id, source, state, active_route_kind,
  active_model_alias, active_provider_name, active_provider_model, started_at,
  last_seen_at, ended_at, end_reason
FROM sessions`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLaunch(row rowScanner) (Launch, error) {
	var launch Launch
	var exitCode sql.NullInt64
	err := row.Scan(&launch.ID, &launch.GatewayURL, &launch.PID, &launch.ModelAlias,
		&launch.State, &launch.LifecycleState, &launch.StatuslineState,
		&launch.AuthMode, &launch.ClaudeAccountName, &launch.CreatedAt,
		&launch.StartedAt, &launch.EndedAt, &exitCode, &launch.EndReason)
	if exitCode.Valid {
		code := int(exitCode.Int64)
		launch.ExitCode = &code
	}
	return launch, err
}

func scanClaudeSession(row rowScanner, session *ClaudeSession) error {
	return row.Scan(&session.ID, &session.LaunchID, &session.ClaudeSessionID,
		&session.Source, &session.State, &session.ActiveRouteKind,
		&session.ActiveModelAlias, &session.ActiveProviderName,
		&session.ActiveProviderModel, &session.StartedAt, &session.LastSeenAt,
		&session.EndedAt, &session.EndReason)
}

func requireAffected(operation, entity string, id any, result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: reading rows affected for %s %v: %w", operation, entity, id, err)
	}
	if affected == 0 {
		return fmt.Errorf("%s: %s %v does not exist", operation, entity, id)
	}
	return nil
}

func runtimeTimestamp() string {
	return formatRuntimeTimestamp(time.Now())
}

const runtimeTimestampLayout = "2006-01-02T15:04:05.000000000Z"

func formatRuntimeTimestamp(value time.Time) string {
	return value.UTC().Format(runtimeTimestampLayout)
}

func normalizeRuntimeTimestamp(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return formatRuntimeTimestamp(parsed), nil
}
