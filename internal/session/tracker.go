package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

var ErrInvalidHook = errors.New("invalid lifecycle hook")

type Config struct {
	Store             *store.Store
	Recorder          *observability.Recorder
	LaunchID          int64
	Enabled           bool
	DefaultModelAlias string
}

// HookEvent deliberately excludes transcript paths, messages, task text, tool
// arguments, and raw errors. JSON decoding discards those fields immediately.
type HookEvent struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
	Reason        string `json:"reason"`
	AgentID       string `json:"agent_id"`
	AgentType     string `json:"agent_type"`
	TaskID        string `json:"task_id"`
	TeammateName  string `json:"teammate_name"`
	TeamName      string `json:"team_name"`
}

type Route struct {
	Kind          string `json:"kind"`
	ModelAlias    string `json:"model_alias"`
	ProviderName  string `json:"provider_name"`
	ProviderModel string `json:"provider_model"`
	Protocol      string `json:"protocol"`
}

type SessionSnapshot struct {
	ID              int64  `json:"id,omitempty"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	State           string `json:"state,omitempty"`
	Source          string `json:"source,omitempty"`
}

type Snapshot struct {
	SchemaVersion    int                    `json:"schema_version"`
	LaunchID         int64                  `json:"launch_id"`
	LifecycleEnabled bool                   `json:"lifecycle_enabled"`
	LifecycleState   string                 `json:"lifecycle_state"`
	CurrentSession   SessionSnapshot        `json:"current_session"`
	Route            Route                  `json:"route"`
	ActiveAgents     int                    `json:"active_agents"`
	ActiveTasks      int                    `json:"active_tasks"`
	LastEvent        string                 `json:"last_event,omitempty"`
	LastError        string                 `json:"last_error,omitempty"`
	UpdatedAt        string                 `json:"updated_at,omitempty"`
	Observability    observability.Snapshot `json:"observability"`
}

type Tracker struct {
	store    *store.Store
	recorder *observability.Recorder
	launchID int64
	enabled  bool

	mu             sync.RWMutex
	lifecycleState string
	currentSession SessionSnapshot
	route          Route
	agents         map[string]string
	tasks          map[string]string
	lastEvent      string
	lastError      string
	updatedAt      string
	hadFailure     bool
}

func NewTracker(config Config) (*Tracker, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("session.NewTracker: store is required")
	}
	if config.LaunchID == 0 {
		return nil, fmt.Errorf("session.NewTracker: launch id is required")
	}
	state := "pending"
	if !config.Enabled {
		state = "disabled"
	}
	return &Tracker{
		store: config.Store, recorder: config.Recorder, launchID: config.LaunchID,
		enabled: config.Enabled, lifecycleState: state,
		route:  Route{ModelAlias: config.DefaultModelAlias},
		agents: make(map[string]string), tasks: make(map[string]string),
	}, nil
}

func (t *Tracker) HandleHook(ctx context.Context, event HookEvent) error {
	if !t.enabled {
		return fmt.Errorf("%w: lifecycle tracking is disabled", ErrInvalidHook)
	}
	if err := ValidateHookEvent(event); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	session, err := t.sessionForEvent(ctx, event)
	if err != nil {
		t.markFailureLocked(ctx)
		return fmt.Errorf("session.Tracker.HandleHook: resolving session: %w", err)
	}
	lifecycle, err := t.applyEventLocked(ctx, session, event)
	if err != nil {
		t.markFailureLocked(ctx)
		return fmt.Errorf("session.Tracker.HandleHook: applying %s: %w", event.HookEventName, err)
	}
	t.lastEvent = event.HookEventName
	t.updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if t.recorder != nil {
		t.recorder.RecordLifecycle(ctx, lifecycle)
	}
	return nil
}

func (t *Tracker) ObserveRoute(ctx context.Context, route Route) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.route = route
	t.updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if t.currentSession.ID == 0 {
		return
	}
	if err := t.store.UpdateClaudeSessionRoute(ctx, t.currentSession.ID, route.Kind,
		route.ModelAlias, route.ProviderName, route.ProviderModel); err != nil {
		t.markFailureLocked(ctx)
	}
}

func (t *Tracker) CurrentSessionID() int64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentSession.ID
}

func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{SchemaVersion: 1}
	}
	t.mu.RLock()
	snapshot := Snapshot{
		SchemaVersion: 1, LaunchID: t.launchID, LifecycleEnabled: t.enabled,
		LifecycleState: t.lifecycleState, CurrentSession: t.currentSession,
		Route: t.route, ActiveAgents: activeCount(t.agents),
		ActiveTasks: activeCount(t.tasks), LastEvent: t.lastEvent,
		LastError: t.lastError, UpdatedAt: t.updatedAt,
	}
	t.mu.RUnlock()
	if t.recorder != nil {
		snapshot.Observability = t.recorder.Snapshot()
	} else {
		snapshot.Observability.Healthy = true
	}
	return snapshot
}

func (t *Tracker) Finalize(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if err := t.store.AbandonLaunchRuntime(ctx, t.launchID); err != nil {
		t.mu.Lock()
		t.markFailureLocked(ctx)
		t.mu.Unlock()
		return fmt.Errorf("session.Tracker.Finalize: %w", err)
	}
	t.mu.Lock()
	if err := t.finalizeLifecycleStateLocked(ctx); err != nil {
		t.mu.Unlock()
		return err
	}
	for id, status := range t.agents {
		if status == "running" || status == "idle" || status == "pending" {
			t.agents[id] = "abandoned"
		}
	}
	for id, status := range t.tasks {
		if status == "running" || status == "pending" {
			t.tasks[id] = "abandoned"
		}
	}
	if t.currentSession.State == "active" {
		t.currentSession.State = "abandoned"
	}
	t.updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	t.mu.Unlock()
	return nil
}

func (t *Tracker) finalizeLifecycleStateLocked(ctx context.Context) error {
	finalLifecycleState := ""
	if t.enabled {
		switch t.lifecycleState {
		case "pending":
			finalLifecycleState = "unobserved"
		case "active":
			finalLifecycleState = "abandoned"
		}
	}
	if finalLifecycleState != "" {
		t.lifecycleState = finalLifecycleState
		if err := t.store.SetLaunchLifecycleState(ctx, t.launchID, finalLifecycleState); err != nil {
			t.markFailureLocked(ctx)
			return fmt.Errorf("session.Tracker.Finalize: marking lifecycle %s: %w", finalLifecycleState, err)
		}
	}
	return nil
}

func ValidateHookEvent(event HookEvent) error {
	if !isLifecycleName(event.HookEventName) {
		return fmt.Errorf("%w: unsupported hook event", ErrInvalidHook)
	}
	if err := validateExternalID("session_id", event.SessionID, true); err != nil {
		return err
	}
	switch event.HookEventName {
	case "SubagentStart", "SubagentStop":
		return validateExternalID("agent_id", event.AgentID, true)
	case "TaskCreated", "TaskCompleted":
		return validateExternalID("task_id", event.TaskID, true)
	case "TeammateIdle":
		if strings.TrimSpace(event.TeammateName) == "" {
			return fmt.Errorf("%w: teammate_name is required", ErrInvalidHook)
		}
	}
	for name, value := range map[string]string{
		"source": event.Source, "reason": event.Reason, "agent_type": event.AgentType,
		"teammate_name": event.TeammateName, "team_name": event.TeamName,
	} {
		if len(value) > 256 {
			return fmt.Errorf("%w: %s exceeds 256 bytes", ErrInvalidHook, name)
		}
	}
	return nil
}

func (t *Tracker) sessionForEvent(ctx context.Context, event HookEvent) (store.ClaudeSession, error) {
	if event.HookEventName == "SessionStart" {
		id, err := t.store.UpsertClaudeSession(ctx, store.ClaudeSession{
			LaunchID: t.launchID, ClaudeSessionID: event.SessionID,
			Source: normalizeSource(event.Source),
		})
		if err != nil {
			return store.ClaudeSession{}, err
		}
		session, err := t.store.GetClaudeSession(ctx, t.launchID, event.SessionID)
		if err != nil {
			return store.ClaudeSession{}, err
		}
		session.ID = id
		return session, nil
	}
	session, err := t.store.GetClaudeSession(ctx, t.launchID, event.SessionID)
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.ClaudeSession{}, err
	}
	_, err = t.store.UpsertClaudeSession(ctx, store.ClaudeSession{
		LaunchID: t.launchID, ClaudeSessionID: event.SessionID, Source: "hook-recovery",
	})
	if err != nil {
		return store.ClaudeSession{}, err
	}
	return t.store.GetClaudeSession(ctx, t.launchID, event.SessionID)
}

func (t *Tracker) applyEventLocked(ctx context.Context, current store.ClaudeSession, event HookEvent) (observability.LifecycleEvent, error) {
	lifecycle := observability.LifecycleEvent{
		SessionID: current.ID, Name: event.HookEventName,
		Status: lifecycleStatus(event.HookEventName),
	}
	var err error
	switch event.HookEventName {
	case "SessionStart", "SessionEnd":
		err = t.applySessionEventLocked(ctx, current, event, &lifecycle)
	case "SubagentStart", "SubagentStop", "TeammateIdle":
		err = t.applyAgentEventLocked(ctx, current, event, &lifecycle)
	case "TaskCreated", "TaskCompleted":
		err = t.applyTaskEventLocked(ctx, current, event, &lifecycle)
	case "StopFailure":
		lifecycle.Reason = "stop_failure"
		t.hadFailure = true
		t.lifecycleState = "error"
		t.lastError = "Claude reported a stop failure"
		err = t.store.SetLaunchLifecycleState(ctx, t.launchID, "error")
	}
	if err != nil {
		return lifecycle, fmt.Errorf("apply %s lifecycle event: %w", event.HookEventName, err)
	}
	return lifecycle, nil
}

func (t *Tracker) applySessionEventLocked(ctx context.Context, current store.ClaudeSession, event HookEvent, lifecycle *observability.LifecycleEvent) error {
	if event.HookEventName == "SessionStart" {
		t.currentSession = SessionSnapshot{
			ID: current.ID, ClaudeSessionID: current.ClaudeSessionID,
			State: "active", Source: current.Source,
		}
		if err := t.persistRouteLocked(ctx); err != nil {
			return err
		}
		if t.hadFailure {
			return nil
		}
		t.lifecycleState = "active"
		return t.store.SetLaunchLifecycleState(ctx, t.launchID, "active")
	}

	reason := normalizeEndReason(event.Reason)
	lifecycle.Reason = reason
	if err := t.store.EndClaudeSession(ctx, current.ID, "ended", reason); err != nil {
		return err
	}
	if t.currentSession.ID == current.ID {
		t.currentSession.State = "ended"
	}
	if t.hadFailure {
		return nil
	}
	t.lifecycleState = "observed"
	return t.store.SetLaunchLifecycleState(ctx, t.launchID, "observed")
}

func (t *Tracker) applyAgentEventLocked(ctx context.Context, current store.ClaudeSession, event HookEvent, lifecycle *observability.LifecycleEvent) error {
	if event.HookEventName == "TeammateIdle" {
		lifecycle.ExternalID = teammateID(event.TeamName, event.TeammateName)
		lifecycle.ActorName, lifecycle.ActorKind = safeName(event.TeammateName), "teammate"
		lifecycle.TeamName = safeName(event.TeamName)
		agent := t.runtimeAgent(current.ID, lifecycle.ExternalID, "teammate", "idle")
		agent.Name = lifecycle.ActorName
		if _, err := t.store.UpsertAgent(ctx, agent); err != nil {
			return err
		}
		t.agents[lifecycle.ExternalID] = "idle"
		return nil
	}

	status := "running"
	if event.HookEventName == "SubagentStop" {
		status = "completed"
	}
	lifecycle.ExternalID, lifecycle.ActorKind = event.AgentID, safeActorKind(event.AgentType)
	if status == "completed" {
		if err := t.finishAgentLocked(ctx, current.ID, event.AgentID, lifecycle.ActorKind); err != nil {
			return err
		}
	} else if _, err := t.store.UpsertAgent(ctx, t.runtimeAgent(current.ID, event.AgentID, lifecycle.ActorKind, status)); err != nil {
		return err
	}
	t.agents[event.AgentID] = status
	return nil
}

func (t *Tracker) applyTaskEventLocked(ctx context.Context, current store.ClaudeSession, event HookEvent, lifecycle *observability.LifecycleEvent) error {
	status := "pending"
	if event.HookEventName == "TaskCompleted" {
		status = "completed"
		lifecycle.ActorName, lifecycle.TeamName = safeName(event.TeammateName), safeName(event.TeamName)
	}
	lifecycle.ExternalID = event.TaskID
	if status == "completed" {
		if err := t.finishTaskLocked(ctx, current.ID, event); err != nil {
			return err
		}
	} else if _, err := t.store.UpsertTask(ctx, t.runtimeTask(current.ID, event, status)); err != nil {
		return err
	}
	t.tasks[event.TaskID] = status
	return nil
}

func (t *Tracker) finishAgentLocked(ctx context.Context, sessionID int64, externalID, actorKind string) error {
	const kind = "subagent"
	finished, err := t.store.FinishAgent(ctx, sessionID, externalID, kind, "completed")
	if err != nil || finished {
		return err
	}
	agent := t.runtimeAgent(sessionID, externalID, actorKind, "completed")
	agent.Kind = kind
	if _, upsertErr := t.store.UpsertAgent(ctx, agent); upsertErr != nil {
		return upsertErr
	}
	finished, err = t.store.FinishAgent(ctx, sessionID, externalID, kind, "completed")
	if err != nil {
		return err
	}
	if !finished {
		return fmt.Errorf("finishing recovered agent %q: no matching row", externalID)
	}
	return nil
}

func (t *Tracker) finishTaskLocked(ctx context.Context, sessionID int64, event HookEvent) error {
	teammateName, teamName := safeName(event.TeammateName), safeName(event.TeamName)
	finished, err := t.store.FinishTask(ctx, sessionID, event.TaskID, teammateName, teamName, "completed")
	if err != nil || finished {
		return err
	}
	if _, upsertErr := t.store.UpsertTask(ctx, t.runtimeTask(sessionID, event, "completed")); upsertErr != nil {
		return upsertErr
	}
	finished, err = t.store.FinishTask(ctx, sessionID, event.TaskID, teammateName, teamName, "completed")
	if err != nil {
		return err
	}
	if !finished {
		return fmt.Errorf("finishing recovered task %q: no matching row", event.TaskID)
	}
	return nil
}

func (t *Tracker) runtimeAgent(sessionID int64, externalID, actorKind, status string) store.Agent {
	name := actorKind
	if name == "" {
		name = "subagent"
	}
	return store.Agent{
		LaunchID: t.launchID, SessionID: sessionID, ExternalID: externalID,
		Name: name, Kind: normalizeAgentKind(actorKind), ModelAlias: t.route.ModelAlias,
		Status: status,
	}
}

func (t *Tracker) runtimeTask(sessionID int64, event HookEvent, status string) store.Task {
	return store.Task{
		LaunchID: t.launchID, SessionID: sessionID, ExternalID: event.TaskID,
		TeammateName: safeName(event.TeammateName), TeamName: safeName(event.TeamName),
		ModelAlias: t.route.ModelAlias, Status: status,
	}
}

func (t *Tracker) persistRouteLocked(ctx context.Context) error {
	if t.currentSession.ID == 0 || t.route.ModelAlias == "" {
		return nil
	}
	return t.store.UpdateClaudeSessionRoute(ctx, t.currentSession.ID, t.route.Kind,
		t.route.ModelAlias, t.route.ProviderName, t.route.ProviderModel)
}

func (t *Tracker) markFailureLocked(ctx context.Context) {
	t.hadFailure = true
	t.lifecycleState = "error"
	t.lastError = "lifecycle persistence unavailable"
	t.updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = t.store.SetLaunchLifecycleState(ctx, t.launchID, "error")
}

func validateExternalID(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s is required", ErrInvalidHook, name)
		}
		return nil
	}
	if len(value) > 128 || !isExternalID(value) {
		return fmt.Errorf("%w: %s has an invalid format", ErrInvalidHook, name)
	}
	return nil
}

func isLifecycleName(value string) bool {
	switch value {
	case "SessionStart", "SessionEnd", "SubagentStart", "SubagentStop",
		"TaskCreated", "TaskCompleted", "TeammateIdle", "StopFailure":
		return true
	default:
		return false
	}
}

func isExternalID(value string) bool {
	for index := range len(value) {
		character := value[index]
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("._:@-", rune(character)) {
			continue
		}
		return false
	}
	return value != ""
}

func normalizeSource(source string) string {
	switch source {
	case "startup", "resume", "clear", "compact":
		return source
	default:
		return "unknown"
	}
}

func normalizeEndReason(reason string) string {
	switch reason {
	case "clear", "logout", "prompt_input_exit", "other":
		return reason
	default:
		return "other"
	}
}

func normalizeAgentKind(actorKind string) string {
	if actorKind == "teammate" {
		return actorKind
	}
	return "subagent"
}

func safeActorKind(value string) string {
	value = safeName(value)
	if value == "" {
		return "subagent"
	}
	return value
}

func safeName(value string) string {
	return strings.TrimSpace(value)
}

func teammateID(team, name string) string {
	return "teammate:" + safeName(team) + ":" + safeName(name)
}

func lifecycleStatus(name string) string {
	switch name {
	case "SessionStart":
		return "active"
	case "SessionEnd", "SubagentStop", "TaskCompleted":
		return "completed"
	case "SubagentStart":
		return "running"
	case "TaskCreated":
		return "pending"
	case "TeammateIdle":
		return "idle"
	case "StopFailure":
		return "failed"
	default:
		return "observed"
	}
}

func activeCount(items map[string]string) int {
	count := 0
	for _, status := range items {
		if status == "running" || status == "pending" || status == "idle" {
			count++
		}
	}
	return count
}
