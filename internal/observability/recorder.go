package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	DefaultRetention = 30 * 24 * time.Hour
	DefaultMaxEvents = 10_000
	pruneInterval    = 100
)

type Config struct {
	Store     *store.Store
	LaunchID  int64
	Enabled   bool
	Retention time.Duration
	MaxEvents int
	Logger    *slog.Logger
}

type RouteStart struct {
	SessionID      int64
	Operation      string
	RequestedModel string
	Streaming      bool
	Tools          bool
	Thinking       bool
}

type RouteResolution struct {
	SessionID     int64
	RouteKind     string
	ModelAlias    string
	ProviderName  string
	ProviderModel string
	Protocol      string
}

type RouteResult struct {
	Status     string
	HTTPStatus int
	ErrorClass string
	Usage      TokenUsage
}

type TokenUsage = store.TokenUsage

type LifecycleEvent = store.LifecycleEvent

type Snapshot struct {
	Enabled       bool   `json:"enabled"`
	Healthy       bool   `json:"healthy"`
	WriteFailures uint64 `json:"write_failures"`
	PrunedEvents  int64  `json:"pruned_events"`
	LastError     string `json:"last_error,omitempty"`
	LastFailureAt string `json:"last_failure_at,omitempty"`
}

type Recorder struct {
	store     *store.Store
	launchID  int64
	enabled   bool
	retention time.Duration
	maxEvents int
	logger    *slog.Logger

	mu            sync.RWMutex
	writeCount    uint64
	writeFailures uint64
	prunedEvents  int64
	lastError     string
	lastFailureAt string
}

func NewRecorder(ctx context.Context, config Config) *Recorder {
	retention := config.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	maxEvents := config.MaxEvents
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	recorder := &Recorder{
		store: config.Store, launchID: config.LaunchID, enabled: config.Enabled,
		retention: retention, maxEvents: maxEvents, logger: logger,
	}
	if recorder.enabled {
		if recorder.store == nil || recorder.launchID == 0 {
			recorder.recordFailure("initialize", fmt.Errorf("store and launch id are required"))
		} else {
			recorder.prune(ctx)
		}
	}
	return recorder
}

func (r *Recorder) BeginRoute(ctx context.Context, start RouteStart) *RouteSpan {
	span := &RouteSpan{recorder: r, startedAt: time.Now()}
	requestID, err := newRequestID()
	if err != nil {
		r.recordFailure("generate request id", err)
		return span
	}
	span.requestID = requestID
	if !r.canWrite() {
		return span
	}
	eventID, err := r.store.BeginRouteEvent(ctx, store.RouteEvent{
		LaunchID: r.launchID, SessionID: start.SessionID, RequestID: requestID,
		Operation: start.Operation, RequestedModel: start.RequestedModel,
		Streaming: start.Streaming, Tools: start.Tools, Thinking: start.Thinking,
	})
	if err != nil {
		r.recordFailure("begin route", err)
		return span
	}
	span.eventID = eventID
	r.afterWrite(ctx)
	return span
}

func (r *Recorder) RecordLifecycle(ctx context.Context, event LifecycleEvent) {
	if !r.canWrite() {
		return
	}
	event.LaunchID = r.launchID
	if _, err := r.store.RecordLifecycleEvent(ctx, event); err != nil {
		r.recordFailure("record lifecycle", err)
		return
	}
	r.afterWrite(ctx)
}

func (r *Recorder) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Snapshot{
		Enabled: r.enabled, Healthy: !r.enabled || r.writeFailures == 0,
		WriteFailures: r.writeFailures, PrunedEvents: r.prunedEvents,
		LastError: r.lastError, LastFailureAt: r.lastFailureAt,
	}
}

func (r *Recorder) canWrite() bool {
	return r != nil && r.enabled && r.store != nil && r.launchID != 0
}

func (r *Recorder) afterWrite(ctx context.Context) {
	r.mu.Lock()
	r.writeCount++
	shouldPrune := r.writeCount%pruneInterval == 0
	r.mu.Unlock()
	if shouldPrune {
		r.prune(ctx)
	}
}

func (r *Recorder) prune(ctx context.Context) {
	deleted, err := r.store.PruneEvents(ctx, r.retention, r.maxEvents)
	if err != nil {
		r.recordFailure("prune history", err)
		return
	}
	r.mu.Lock()
	r.prunedEvents += deleted
	r.mu.Unlock()
}

func (r *Recorder) recordFailure(operation string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.writeFailures++
	r.lastError = operation + ": persistence unavailable"
	r.lastFailureAt = time.Now().UTC().Format(time.RFC3339Nano)
	r.mu.Unlock()
	r.logger.Warn("observability write failed", "operation", operation, "error", err)
}

type RouteSpan struct {
	recorder  *Recorder
	eventID   int64
	requestID string
	startedAt time.Time
	mu        sync.Mutex
	completed bool
}

func (s *RouteSpan) RequestID() string {
	if s == nil {
		return ""
	}
	return s.requestID
}

func (s *RouteSpan) Resolve(ctx context.Context, resolution RouteResolution) {
	if s == nil || s.eventID == 0 || !s.recorder.canWrite() {
		return
	}
	err := s.recorder.store.ResolveRouteEvent(ctx, s.eventID, resolution.SessionID, store.RouteEvent{
		RouteKind: resolution.RouteKind, ModelAlias: resolution.ModelAlias,
		ProviderName: resolution.ProviderName, ProviderModel: resolution.ProviderModel,
		Protocol: resolution.Protocol,
	})
	if err != nil {
		s.recorder.recordFailure("resolve route", err)
		return
	}
	s.recorder.afterWrite(ctx)
}

func (s *RouteSpan) Complete(ctx context.Context, result RouteResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.completed {
		s.mu.Unlock()
		return
	}
	s.completed = true
	s.mu.Unlock()
	if s.eventID == 0 || !s.recorder.canWrite() {
		return
	}
	err := s.recorder.store.CompleteRouteEvent(ctx, s.eventID, result.Status,
		result.HTTPStatus, result.ErrorClass, time.Since(s.startedAt), result.Usage)
	if err != nil {
		s.recorder.recordFailure("complete route", err)
		return
	}
	s.recorder.afterWrite(ctx)
}

func newRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("reading cryptographic randomness: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}
