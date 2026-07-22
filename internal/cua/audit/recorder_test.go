package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestRecorderStoresOnlyRedactedAuditMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	recorder := NewRecorder(Config{Now: func() time.Time { return now }})
	err := recorder.Record(context.Background(), cua.AuditEvent{
		Executor: "external:sk-secret-password",
		Action:   cua.ActionType,
		Risk:     cua.RiskHigh,
		Decision: cua.DecisionDeny,
		Status:   "typed password bearer sk-secret",
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	events := recorder.Events()
	if len(events) != 1 {
		t.Fatalf("Events() len = %d, want 1", len(events))
	}
	event := events[0]
	if !event.At.Equal(now) {
		t.Fatalf("event At = %v, want %v", event.At, now)
	}
	if event.Executor != string(cua.ExecutorExternal) {
		t.Fatalf("event Executor = %q, want external", event.Executor)
	}
	if event.Status != "other" {
		t.Fatalf("event Status = %q, want other", event.Status)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"sk-secret", "password", "bearer", "typed"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit events retained forbidden content %q in %s", forbidden, encoded)
		}
	}
}

func TestRecorderPrunesThirtyDayRetention(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	recorder := NewRecorder(Config{Now: func() time.Time { return now }})
	oldEvent := cua.AuditEvent{
		At:       now.Add(-(DefaultRetention + time.Second)),
		Executor: "docker",
		Action:   cua.ActionScreenshot,
		Risk:     cua.RiskLow,
		Decision: cua.DecisionApprove,
		Status:   "approved",
	}
	recentEvent := oldEvent
	recentEvent.At = now.Add(-DefaultRetention + time.Second)
	recentEvent.Action = cua.ActionScroll
	if err := recorder.Record(context.Background(), oldEvent); err != nil {
		t.Fatalf("Record(old) error = %v", err)
	}
	if err := recorder.Record(context.Background(), recentEvent); err != nil {
		t.Fatalf("Record(recent) error = %v", err)
	}
	events := recorder.Events()
	if len(events) != 1 {
		t.Fatalf("Events() len = %d, want 1", len(events))
	}
	if events[0].Action != cua.ActionScroll {
		t.Fatalf("retained action = %q, want %q", events[0].Action, cua.ActionScroll)
	}
}

func TestRecorderReturnsCopiesAndHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	recorder := NewRecorder(Config{})
	if err := recorder.Record(context.Background(), cua.AuditEvent{
		Executor: "docker",
		Action:   cua.ActionWait,
		Risk:     cua.RiskLow,
		Decision: cua.DecisionApprove,
		Status:   "approved",
	}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	events := recorder.Events()
	events[0].Status = "mutated"
	if got := recorder.Events()[0].Status; got != "approved" {
		t.Fatalf("stored status = %q after caller mutation, want approved", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recorder.Record(ctx, cua.AuditEvent{}); err == nil {
		t.Fatal("Record() with canceled context unexpectedly succeeded")
	}
}

func TestRecorderEventsPrunesExpiredEntriesWithoutAnotherRecord(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	recorder := NewRecorder(Config{
		Retention: time.Hour,
		Now:       func() time.Time { return now },
	})
	if err := recorder.Record(context.Background(), cua.AuditEvent{
		Executor: "docker", Action: cua.ActionWait, Risk: cua.RiskLow,
		Decision: cua.DecisionApprove, Status: "approved",
	}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	now = now.Add(2 * time.Hour)
	if events := recorder.Events(); len(events) != 0 {
		t.Fatalf("Events() = %#v, want expired event pruned", events)
	}
}
