package store

import (
	"context"
	"fmt"
	"time"
)

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
