// Package audit records redacted computer-use approval metadata.
package audit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const DefaultRetention = 30 * 24 * time.Hour

type Config struct {
	Retention time.Duration
	MaxEvents int
	Now       func() time.Time
}

type Recorder struct {
	retention time.Duration
	maxEvents int
	now       func() time.Time

	mu     sync.Mutex
	events []cua.AuditEvent
}

func NewRecorder(config Config) *Recorder {
	retention := config.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	maxEvents := config.MaxEvents
	if maxEvents <= 0 {
		maxEvents = 10_000
	}
	return &Recorder{retention: retention, maxEvents: maxEvents, now: now}
}

func (r *Recorder) Record(ctx context.Context, event cua.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit.Recorder.Record: context canceled: %w", err)
	}
	if event.At.IsZero() {
		event.At = r.now().UTC()
	}
	safeEvent := cua.AuditEvent{
		At:       event.At.UTC(),
		Executor: safeExecutor(event.Executor),
		Action:   safeAction(event.Action),
		Risk:     safeRisk(event.Risk),
		Decision: safeDecision(event.Decision),
		Status:   safeStatus(event.Status),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, safeEvent)
	r.pruneLocked(r.now())
	return nil
}

func (r *Recorder) Events() []cua.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	events := make([]cua.AuditEvent, len(r.events))
	copy(events, r.events)
	return events
}

func (r *Recorder) Prune(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pruneLocked(now)
}

func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *Recorder) pruneLocked(now time.Time) int {
	cutoff := now.Add(-r.retention)
	kept := r.events[:0]
	for _, event := range r.events {
		if !event.At.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	if overflow := len(kept) - r.maxEvents; overflow > 0 {
		kept = append([]cua.AuditEvent(nil), kept[overflow:]...)
	}
	pruned := len(r.events) - len(kept)
	r.events = kept
	return pruned
}

func safeExecutor(executor string) string {
	executor = strings.ToLower(strings.TrimSpace(executor))
	switch executor {
	case "", string(cua.ExecutorDocker), string(cua.ExecutorLocalBrowser), string(cua.ExecutorMacOSPreview):
		return executor
	default:
		if strings.HasPrefix(executor, string(cua.ExecutorExternal)+":") {
			return string(cua.ExecutorExternal)
		}
		return "other"
	}
}

func safeAction(action cua.ActionKind) cua.ActionKind {
	switch action {
	case cua.ActionScreenshot, cua.ActionClick, cua.ActionDoubleClick, cua.ActionDrag, cua.ActionMove,
		cua.ActionType, cua.ActionKeypress, cua.ActionScroll, cua.ActionWait:
		return action
	default:
		return ""
	}
}

func safeRisk(risk cua.Risk) cua.Risk {
	switch risk {
	case cua.RiskLow, cua.RiskHigh:
		return risk
	default:
		return ""
	}
}

func safeDecision(decision cua.Decision) cua.Decision {
	switch decision {
	case cua.DecisionApprove, cua.DecisionDeny:
		return decision
	default:
		return ""
	}
}

func safeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "", "approved", "denied", "remembered", "timeout", "shutdown", "error":
		return status
	default:
		return "other"
	}
}
