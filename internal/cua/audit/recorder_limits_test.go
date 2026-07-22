package audit

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestRecorderCapsRetainedEvents(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	recorder := NewRecorder(Config{Retention: time.Hour, MaxEvents: 2, Now: func() time.Time { return now }})
	for index := 0; index < 3; index++ {
		if err := recorder.Record(context.Background(), cua.AuditEvent{Action: cua.ActionWait, Risk: cua.RiskLow, Decision: cua.DecisionApprove, Status: "approved"}); err != nil {
			t.Fatalf("Record(%d) error = %v", index, err)
		}
	}
	if recorder.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", recorder.Len())
	}
}
