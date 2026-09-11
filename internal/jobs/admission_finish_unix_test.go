//go:build linux || darwin

package jobs

import (
	"errors"
	"strings"
	"testing"
)

func TestFinalOutcomeRequiresStopAndCommittedIdentityEvidence(t *testing.T) {
	zero := 0
	original := Record{SessionID: "session", Status: "running"}
	valid := FinalOutcome{
		Cleanup: Cleanup{Coverage: "partial", Survivors: []Survivor{}}, ExitCode: &zero,
		ObservationRequired: true,
		Stream:              StreamResult{Initialized: true, Successful: true, SessionID: "session", Model: "model"},
		Evidence:            &ResultEvidence{Boundary: 100, SHA256: strings.Repeat("a", 64), SessionID: "session", Model: "model", Successful: true},
	}
	got := finalizeExecution(original, valid)
	if got.Status != "completed" || got.WorkloadDisposition != DispositionStopped || got.ResultEvidence == nil {
		t.Fatalf("valid result rejected: %+v", got)
	}
	for _, test := range []struct {
		name   string
		mutate func(*FinalOutcome)
	}{
		{"exit", func(o *FinalOutcome) { o.ExitCode = nil }},
		{"coverage", func(o *FinalOutcome) { o.Cleanup.Coverage = "unknown" }},
		{"missing-survivor-observation", func(o *FinalOutcome) { o.Cleanup.Survivors = nil }},
		{"survivor", func(o *FinalOutcome) { o.Cleanup.Survivors = []Survivor{{PID: 123}} }},
		{"evidence", func(o *FinalOutcome) { o.Evidence = nil }},
		{"session", func(o *FinalOutcome) { o.Stream.SessionID = "other" }},
		{"model", func(o *FinalOutcome) { o.Stream.Model = "other" }},
		{"result", func(o *FinalOutcome) { o.Stream.Successful = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := valid
			test.mutate(&outcome)
			record := finalizeExecution(original, outcome)
			if record.Status == "completed" || record.ResultEvidence != nil {
				t.Fatalf("accepted incomplete proof: %+v", record)
			}
		})
	}
}

func TestFailureClassificationUsesRequestEntryAndStructuredObservations(t *testing.T) {
	failure := errors.New("execution failed")
	for _, test := range []struct {
		name    string
		outcome FinalOutcome
		reason  string
	}{
		{"setup", FinalOutcome{RunError: failure}, ReasonStartupFailed},
		{"missing-history-before-init", FinalOutcome{RunError: failure, ObservationError: &OutputError{ReasonCode: ReasonObservationFailed, Incomplete: true}}, ReasonStartupFailed},
		{"request-before-init", FinalOutcome{RunError: failure, RequestEntries: 1}, ReasonExecutionFailed},
		{"initialized", FinalOutcome{RunError: failure, Stream: StreamResult{Initialized: true}}, ReasonExecutionFailed},
		{"malformed", FinalOutcome{RunError: failure, ObservationError: &OutputError{ReasonCode: ReasonObservationFailed}}, ReasonObservationFailed},
		{"incomplete-after-init", FinalOutcome{RunError: failure, Stream: StreamResult{Initialized: true}, ObservationError: &OutputError{ReasonCode: ReasonObservationFailed, Incomplete: true}}, ReasonObservationFailed},
		{"identity", FinalOutcome{RunError: failure, ObservationError: &OutputError{ReasonCode: ReasonSessionIdentityMismatch, ObservedSessionID: "other"}}, ReasonSessionIdentityMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := failureReason(test.outcome); got != test.reason {
				t.Fatalf("reason=%s want=%s", got, test.reason)
			}
		})
	}
	record := finalizeExecution(Record{SessionID: "admitted", CancelRequested: true}, FinalOutcome{RunError: failure, ObservationError: &OutputError{ReasonCode: ReasonSessionIdentityMismatch, ObservedSessionID: "other"}})
	if record.ReasonCode != ReasonSessionIdentityMismatch || record.ObservedSessionID != "other" || record.SessionID != "admitted" {
		t.Fatalf("lost contradiction on cancellation: %+v", record)
	}
}

func TestNeverStartedFinalizationPreservesStartupFailureWithoutExitEvidence(t *testing.T) {
	registry := testRegistry(t, t.TempDir())
	admission := testAdmission()
	lease := reserveAdmission(t, registry, admission)
	record, lock, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	owner := NewOwner(Store{Root: registry.root}, record, func() {})
	if finishErr := owner.FinishAdmission(t.Context(), registry, FinalOutcome{RunError: errors.New("bad alias")}); finishErr != nil {
		t.Fatal(finishErr)
	}
	got, err := registry.JobStatus(t.Context(), admission.JobID)
	if err != nil || got.ReasonCode != ReasonStartupFailed || got.WorkloadDisposition != DispositionNotStarted || got.ExitCode != nil || got.Status != "failed" {
		t.Fatalf("fabricated or lost outcome: %+v %v", got, err)
	}
	if err := registry.CommitExecution(t.Context(), lease, admission.SubmissionID, admission.ExecutionDigest); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("revived startup failure: %v", err)
	}
}

func TestJoinedIdentityContradictionSurvivesEarlierObservationErrors(t *testing.T) {
	mismatch := &OutputError{ReasonCode: ReasonSessionIdentityMismatch, ObservedSessionID: "contradictory"}
	for _, earlier := range []error{errors.New("observer unavailable"), &OutputError{ReasonCode: ReasonObservationFailed}, &OutputError{ReasonCode: ReasonObservationFailed, Incomplete: true}} {
		outcome := FinalOutcome{ObservationError: errors.Join(earlier, mismatch), RunError: errors.New("canceled")}
		record := finalizeExecution(Record{SessionID: "admitted", CancelRequested: true}, outcome)
		if record.ReasonCode != ReasonSessionIdentityMismatch || record.ObservedSessionID != "contradictory" || record.SessionID != "admitted" {
			t.Fatalf("joined error hid contradiction: %+v", record)
		}
	}
}

func TestStructuredZeroTurnFailureBeforeInitializationIsStartupFailure(t *testing.T) {
	for _, test := range []struct {
		event string
		want  string
	}{
		{`{"type":"result","subtype":"error_during_execution","session_id":"session","is_error":true,"num_turns":0}`, ReasonStartupFailed},
		{`{"type":"result","subtype":"error_during_execution","session_id":"session","is_error":true,"num_turns":1}`, ReasonObservationFailed},
		{`{"type":"result","subtype":"success","session_id":"session","is_error":false,"num_turns":0,"result":"wrong"}`, ReasonObservationFailed},
		{`{"type":"result","subtype":"unknown","session_id":"session","is_error":true,"num_turns":0}`, ReasonObservationFailed},
	} {
		stream, err := parseOutput(t.Context(), strings.NewReader(test.event+"\n"), "session", "model")
		got := failureReason(FinalOutcome{RunError: errors.New("exit 1"), Stream: stream, ObservationError: err})
		if err == nil || stream.Successful || got != test.want {
			t.Fatalf("pre-init event classification=%s want=%s result=%+v err=%v", got, test.want, stream, err)
		}
	}
}
