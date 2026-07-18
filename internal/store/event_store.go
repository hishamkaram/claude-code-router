package store

import (
	"context"
	"fmt"
	"time"
)

type TokenUsage struct {
	Observed         bool
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

type RouteEvent struct {
	EventID        int64
	LaunchID       int64
	SessionID      int64
	RequestID      string
	Operation      string
	RequestedModel string
	RouteKind      string
	ModelAlias     string
	ProviderName   string
	ProviderModel  string
	Protocol       string
	Streaming      bool
	Tools          bool
	Thinking       bool
	Status         string
	HTTPStatus     int
	ErrorClass     string
	LatencyMS      int64
	Usage          TokenUsage
	OccurredAt     string
	CompletedAt    string
}

type LifecycleEvent struct {
	EventID     int64
	LaunchID    int64
	SessionID   int64
	Name        string
	Status      string
	ExternalID  string
	ActorName   string
	ActorKind   string
	TeamName    string
	Reason      string
	OccurredAt  string
	CompletedAt string
}

type TraceEvent struct {
	ID          int64
	Kind        string
	Name        string
	Status      string
	LaunchID    int64
	SessionID   int64
	Route       RouteEvent
	Lifecycle   LifecycleEvent
	OccurredAt  string
	CompletedAt string
}

type TraceFilter struct {
	LaunchID        int64
	ClaudeSessionID string
	Kind            string
	Since           string
	AfterID         int64
	OldestFirst     bool
	Limit           int
}

func (s *Store) BeginRouteEvent(ctx context.Context, event RouteEvent) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.BeginRouteEvent: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := runtimeTimestamp()
	result, err := tx.ExecContext(ctx, `
INSERT INTO event_log (
  launch_id, session_id, kind, name, status, occurred_at, completed_at
)
VALUES (?, ?, 'route', ?, 'started', ?, '')
`, event.LaunchID, nullableRuntimeID(event.SessionID), event.Operation, now)
	if err != nil {
		return 0, fmt.Errorf("store.BeginRouteEvent: inserting event log: %w", err)
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.BeginRouteEvent: reading event id: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO route_events (
  event_id, request_id, requested_model, route_kind, model_alias, provider_name,
  provider_model, protocol, streaming, tools, thinking
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, eventID, event.RequestID, event.RequestedModel, event.RouteKind,
		event.ModelAlias, event.ProviderName, event.ProviderModel, event.Protocol,
		boolToInt(event.Streaming), boolToInt(event.Tools), boolToInt(event.Thinking))
	if err != nil {
		return 0, fmt.Errorf("store.BeginRouteEvent: inserting route event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.BeginRouteEvent: committing event: %w", err)
	}
	return eventID, nil
}

func (s *Store) ResolveRouteEvent(ctx context.Context, eventID, sessionID int64, event RouteEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ResolveRouteEvent: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
UPDATE event_log
SET session_id = COALESCE(?, session_id)
WHERE id = ? AND kind = 'route'
`, nullableRuntimeID(sessionID), eventID)
	if err != nil {
		return fmt.Errorf("store.ResolveRouteEvent: updating event log: %w", err)
	}
	if affectedErr := requireAffected("store.ResolveRouteEvent", "event", eventID, result); affectedErr != nil {
		return affectedErr
	}
	_, err = tx.ExecContext(ctx, `
UPDATE route_events
SET route_kind = ?, model_alias = ?, provider_name = ?, provider_model = ?, protocol = ?
WHERE event_id = ?
`, event.RouteKind, event.ModelAlias, event.ProviderName, event.ProviderModel,
		event.Protocol, eventID)
	if err != nil {
		return fmt.Errorf("store.ResolveRouteEvent: updating route details: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.ResolveRouteEvent: committing event: %w", err)
	}
	return nil
}

func (s *Store) CompleteRouteEvent(ctx context.Context, eventID int64, status string, httpStatus int, errorClass string, latency time.Duration, usage TokenUsage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.CompleteRouteEvent: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := runtimeTimestamp()
	result, err := tx.ExecContext(ctx, `
UPDATE event_log SET status = ?, completed_at = ? WHERE id = ? AND kind = 'route'
`, status, now, eventID)
	if err != nil {
		return fmt.Errorf("store.CompleteRouteEvent: updating event log: %w", err)
	}
	if affectedErr := requireAffected("store.CompleteRouteEvent", "event", eventID, result); affectedErr != nil {
		return affectedErr
	}
	_, err = tx.ExecContext(ctx, `
UPDATE route_events
SET http_status = ?, error_class = ?, latency_ms = ?, usage_observed = ?,
  input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ?
WHERE event_id = ?
`, httpStatus, errorClass, latency.Milliseconds(), boolToInt(usage.Observed),
		usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens,
		usage.CacheWriteTokens, eventID)
	if err != nil {
		return fmt.Errorf("store.CompleteRouteEvent: updating route details: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.CompleteRouteEvent: committing event: %w", err)
	}
	return nil
}

func (s *Store) RecordLifecycleEvent(ctx context.Context, event LifecycleEvent) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.RecordLifecycleEvent: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := runtimeTimestamp()
	result, err := tx.ExecContext(ctx, `
INSERT INTO event_log (
  launch_id, session_id, kind, name, status, occurred_at, completed_at
)
VALUES (?, ?, 'lifecycle', ?, ?, ?, ?)
`, event.LaunchID, nullableRuntimeID(event.SessionID), event.Name, event.Status, now, now)
	if err != nil {
		return 0, fmt.Errorf("store.RecordLifecycleEvent: inserting event log: %w", err)
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.RecordLifecycleEvent: reading event id: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO lifecycle_events (
  event_id, external_id, actor_name, actor_kind, team_name, reason
)
VALUES (?, ?, ?, ?, ?, ?)
`, eventID, event.ExternalID, event.ActorName, event.ActorKind, event.TeamName, event.Reason)
	if err != nil {
		return 0, fmt.Errorf("store.RecordLifecycleEvent: inserting lifecycle details: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.RecordLifecycleEvent: committing event: %w", err)
	}
	return eventID, nil
}

func (s *Store) ListTraceEvents(ctx context.Context, filter TraceFilter) ([]TraceEvent, error) {
	since := filter.Since
	if since != "" {
		var err error
		since, err = normalizeRuntimeTimestamp(since)
		if err != nil {
			return nil, fmt.Errorf("store.ListTraceEvents: invalid since timestamp: %w", err)
		}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	order := "DESC"
	if filter.OldestFirst || filter.AfterID > 0 {
		order = "ASC"
	}
	rows, err := s.db.QueryContext(ctx, traceSelectSQL+`
WHERE (? = 0 OR event_log.launch_id = ?)
  AND (? = '' OR sessions.claude_session_id = ?)
  AND (? = '' OR event_log.kind = ?)
  AND (? = '' OR event_log.occurred_at >= ?)
  AND (? = 0 OR event_log.id > ?)
ORDER BY event_log.id `+order+`
LIMIT ?`, filter.LaunchID, filter.LaunchID, filter.ClaudeSessionID,
		filter.ClaudeSessionID, filter.Kind, filter.Kind, since, since,
		filter.AfterID, filter.AfterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListTraceEvents: querying events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []TraceEvent
	for rows.Next() {
		event, scanErr := scanTraceEvent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store.ListTraceEvents: scanning event: %w", scanErr)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTraceEvents: iterating events: %w", err)
	}
	return events, nil
}

func (s *Store) PruneEvents(ctx context.Context, retention time.Duration, maxEvents int) (int64, error) {
	if retention <= 0 || maxEvents <= 0 {
		return 0, fmt.Errorf("store.PruneEvents: positive retention and max events are required")
	}
	cutoff := formatRuntimeTimestamp(time.Now().Add(-retention))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.PruneEvents: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	oldResult, err := tx.ExecContext(ctx, `DELETE FROM event_log WHERE occurred_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store.PruneEvents: deleting expired events: %w", err)
	}
	overflowResult, err := tx.ExecContext(ctx, `
DELETE FROM event_log
WHERE id NOT IN (SELECT id FROM event_log ORDER BY id DESC LIMIT ?)
`, maxEvents)
	if err != nil {
		return 0, fmt.Errorf("store.PruneEvents: deleting overflow events: %w", err)
	}
	oldCount, err := oldResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.PruneEvents: reading expired count: %w", err)
	}
	overflowCount, err := overflowResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.PruneEvents: reading overflow count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.PruneEvents: committing prune: %w", err)
	}
	return oldCount + overflowCount, nil
}

func (s *Store) PurgeEvents(ctx context.Context, before string, all bool) (int64, error) {
	if !all && before == "" {
		return 0, fmt.Errorf("store.PurgeEvents: before is required unless all is set")
	}
	query := `DELETE FROM event_log`
	var args []any
	if !all {
		normalized, err := normalizeRuntimeTimestamp(before)
		if err != nil {
			return 0, fmt.Errorf("store.PurgeEvents: invalid before timestamp: %w", err)
		}
		query += ` WHERE occurred_at < ?`
		args = append(args, normalized)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store.PurgeEvents: deleting events: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.PurgeEvents: reading deleted count: %w", err)
	}
	return count, nil
}

const traceSelectSQL = `
SELECT
  event_log.id, event_log.launch_id, COALESCE(event_log.session_id, 0),
  event_log.kind, event_log.name, event_log.status, event_log.occurred_at,
  event_log.completed_at,
  COALESCE(route_events.request_id, ''),
  COALESCE(route_events.requested_model, ''),
  COALESCE(route_events.route_kind, ''),
  COALESCE(route_events.model_alias, ''),
  COALESCE(route_events.provider_name, ''),
  COALESCE(route_events.provider_model, ''),
  COALESCE(route_events.protocol, ''),
  COALESCE(route_events.streaming, 0),
  COALESCE(route_events.tools, 0),
  COALESCE(route_events.thinking, 0),
  COALESCE(route_events.http_status, 0),
  COALESCE(route_events.error_class, ''),
  COALESCE(route_events.latency_ms, 0),
  COALESCE(route_events.usage_observed, 0),
  COALESCE(route_events.input_tokens, 0),
  COALESCE(route_events.output_tokens, 0),
  COALESCE(route_events.cache_read_tokens, 0),
  COALESCE(route_events.cache_write_tokens, 0),
  COALESCE(lifecycle_events.external_id, ''),
  COALESCE(lifecycle_events.actor_name, ''),
  COALESCE(lifecycle_events.actor_kind, ''),
  COALESCE(lifecycle_events.team_name, ''),
  COALESCE(lifecycle_events.reason, '')
FROM event_log
LEFT JOIN sessions ON sessions.id = event_log.session_id
LEFT JOIN route_events ON route_events.event_id = event_log.id
LEFT JOIN lifecycle_events ON lifecycle_events.event_id = event_log.id`

func scanTraceEvent(row rowScanner) (TraceEvent, error) {
	var event TraceEvent
	var streaming, tools, thinking, usageObserved int
	err := row.Scan(
		&event.ID, &event.LaunchID, &event.SessionID, &event.Kind, &event.Name,
		&event.Status, &event.OccurredAt, &event.CompletedAt,
		&event.Route.RequestID, &event.Route.RequestedModel, &event.Route.RouteKind,
		&event.Route.ModelAlias, &event.Route.ProviderName, &event.Route.ProviderModel,
		&event.Route.Protocol, &streaming, &tools, &thinking, &event.Route.HTTPStatus,
		&event.Route.ErrorClass, &event.Route.LatencyMS, &usageObserved,
		&event.Route.Usage.InputTokens, &event.Route.Usage.OutputTokens,
		&event.Route.Usage.CacheReadTokens, &event.Route.Usage.CacheWriteTokens,
		&event.Lifecycle.ExternalID, &event.Lifecycle.ActorName,
		&event.Lifecycle.ActorKind, &event.Lifecycle.TeamName, &event.Lifecycle.Reason,
	)
	event.Route.EventID = event.ID
	event.Route.LaunchID = event.LaunchID
	event.Route.SessionID = event.SessionID
	event.Route.Operation = event.Name
	event.Route.Status = event.Status
	event.Route.OccurredAt = event.OccurredAt
	event.Route.CompletedAt = event.CompletedAt
	event.Route.Streaming = intToBool(streaming)
	event.Route.Tools = intToBool(tools)
	event.Route.Thinking = intToBool(thinking)
	event.Route.Usage.Observed = intToBool(usageObserved)
	event.Lifecycle.EventID = event.ID
	event.Lifecycle.LaunchID = event.LaunchID
	event.Lifecycle.SessionID = event.SessionID
	event.Lifecycle.Name = event.Name
	event.Lifecycle.Status = event.Status
	event.Lifecycle.OccurredAt = event.OccurredAt
	event.Lifecycle.CompletedAt = event.CompletedAt
	return event, err
}

func nullableRuntimeID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
