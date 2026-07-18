package store

import (
	"context"
	"fmt"
)

type ConformanceRun struct {
	ID            int64
	Alias         string
	Status        string
	LiveVerified  bool
	Details       string
	Scope         string
	ProviderName  string
	ProviderModel string
	Protocol      string
	ClaudeVersion string
	StartedAt     string
	CompletedAt   string
	CreatedAt     string
}

type ConformanceCheck struct {
	ID        int64
	RunID     int64
	Name      string
	Status    string
	LatencyMS int64
	Evidence  string
	CreatedAt string
}

func (s *Store) CreateConformanceRun(ctx context.Context, run ConformanceRun) (int64, error) {
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO conformance_runs (
  alias, status, live_verified, details, scope, provider_name, provider_model,
  protocol, claude_version, started_at, completed_at, created_at
)
VALUES (?, 'running', 0, '', ?, ?, ?, ?, ?, ?, '', ?)
`, run.Alias, run.Scope, run.ProviderName, run.ProviderModel, run.Protocol,
		run.ClaudeVersion, now, now)
	if err != nil {
		return 0, fmt.Errorf("store.CreateConformanceRun: inserting run for alias %q: %w", run.Alias, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.CreateConformanceRun: reading run id: %w", err)
	}
	return id, nil
}

func (s *Store) AddConformanceCheck(ctx context.Context, check ConformanceCheck) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO conformance_checks (run_id, name, status, latency_ms, evidence, created_at)
VALUES (?, ?, ?, ?, ?, ?)
`, check.RunID, check.Name, check.Status, check.LatencyMS, check.Evidence, runtimeTimestamp())
	if err != nil {
		return 0, fmt.Errorf("store.AddConformanceCheck: inserting check %q: %w", check.Name, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.AddConformanceCheck: reading check id: %w", err)
	}
	return id, nil
}

func (s *Store) CompleteConformanceRun(ctx context.Context, id int64, status string, liveVerified bool, details string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE conformance_runs
SET status = ?, live_verified = ?, details = ?, completed_at = ?
WHERE id = ?
`, status, boolToInt(liveVerified), details, runtimeTimestamp(), id)
	if err != nil {
		return fmt.Errorf("store.CompleteConformanceRun: updating run %d: %w", id, err)
	}
	return requireAffected("store.CompleteConformanceRun", "conformance run", id, result)
}

func (s *Store) ListConformanceRuns(ctx context.Context, alias string, limit int) ([]ConformanceRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, alias, status, live_verified, details, scope, provider_name,
  provider_model, protocol, claude_version, started_at, completed_at, created_at
FROM conformance_runs
WHERE (? = '' OR alias = ?)
ORDER BY id DESC
LIMIT ?
`, alias, alias, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListConformanceRuns: querying runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []ConformanceRun
	for rows.Next() {
		var run ConformanceRun
		var liveVerified int
		if err := rows.Scan(&run.ID, &run.Alias, &run.Status, &liveVerified,
			&run.Details, &run.Scope, &run.ProviderName, &run.ProviderModel,
			&run.Protocol, &run.ClaudeVersion, &run.StartedAt, &run.CompletedAt,
			&run.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListConformanceRuns: scanning run: %w", err)
		}
		run.LiveVerified = intToBool(liveVerified)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListConformanceRuns: iterating runs: %w", err)
	}
	return runs, nil
}

func (s *Store) ListConformanceChecks(ctx context.Context, runID int64) ([]ConformanceCheck, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, run_id, name, status, latency_ms, evidence, created_at
FROM conformance_checks
WHERE run_id = ?
ORDER BY id
`, runID)
	if err != nil {
		return nil, fmt.Errorf("store.ListConformanceChecks: querying checks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var checks []ConformanceCheck
	for rows.Next() {
		var check ConformanceCheck
		if err := rows.Scan(&check.ID, &check.RunID, &check.Name, &check.Status,
			&check.LatencyMS, &check.Evidence, &check.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListConformanceChecks: scanning check: %w", err)
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListConformanceChecks: iterating checks: %w", err)
	}
	return checks, nil
}
