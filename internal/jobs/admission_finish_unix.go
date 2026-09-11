//go:build linux || darwin

package jobs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
)

// FinalOutcome is collected only after process cleanup, output observation, and
// gateway request accounting have joined. It never infers history from stderr.
type FinalOutcome struct {
	RunError            error
	Cleanup             Cleanup
	ExitCode            *int
	RequestEntries      uint64
	Stream              StreamResult
	ObservationRequired bool
	ObservationError    error
	Evidence            *ResultEvidence
}

func (o *Owner) FinishAdmission(ctx context.Context, registry *Registry, outcome FinalOutcome) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	admission, err := registry.LookupJob(ctx, o.record.JobID)
	if err != nil {
		return err
	}
	if admission.SessionID != o.record.SessionID || admission.SubmissionID != o.record.SubmissionID {
		return fmt.Errorf("owner finalization contradicts admission authority")
	}
	if admission.State == AdmissionPrepared {
		reason := failureReason(outcome)
		if _, abortErr := registry.abortPrepared(ctx, admission.SubmissionID, false, reason); abortErr != nil {
			return abortErr
		}
		admission, err = registry.LookupJob(ctx, o.record.JobID)
		if err != nil {
			return err
		}
	}
	if admission.State == AdmissionAborted {
		applyAdmission(&o.record, admission)
		return o.store.Write(o.record)
	}
	if admission.State != AdmissionPossible {
		return ErrAdmissionClosed
	}
	o.record = finalizeExecution(o.record, outcome)
	if err := o.store.Write(o.record); err != nil {
		return err
	}
	return registry.Finish(ctx, o.record.JobID)
}

func failureReason(outcome FinalOutcome) string {
	if identityContradiction(outcome.ObservationError) != nil {
		return ReasonSessionIdentityMismatch
	}
	var observed *OutputError
	if errors.As(outcome.ObservationError, &observed) {
		if observed.ReasonCode == ReasonSessionIdentityMismatch {
			return ReasonSessionIdentityMismatch
		}
		if !observed.Incomplete {
			return ReasonObservationFailed
		}
	} else if outcome.ObservationError != nil {
		return ReasonObservationFailed
	}
	if !outcome.Stream.Initialized && outcome.RequestEntries == 0 && outcome.RunError != nil {
		return ReasonStartupFailed
	}
	if outcome.ObservationError != nil {
		return ReasonObservationFailed
	}
	return ReasonExecutionFailed
}

func finalizeExecution(record Record, outcome FinalOutcome) Record {
	record.Status, record.ReasonCode = "failed", failureReason(outcome)
	record.Cleanup, record.ExitCode = outcome.Cleanup, outcome.ExitCode
	record.WorkloadDisposition = DispositionUnknown
	record.ResultEvidence = nil
	if record.Stopped() {
		record.WorkloadDisposition = DispositionStopped
	}
	if observed := identityContradiction(outcome.ObservationError); observed != nil {
		record.ObservedSessionID = observed.ObservedSessionID
		return record // Cancellation errors never replace an identity contradiction.
	}
	if !record.Stopped() {
		record.ReasonCode = ReasonObservationFailed
		return record
	}
	if record.CancelRequested {
		record.Status = statusCancelled
		return record
	}
	if outcome.RunError != nil || outcome.ObservationError != nil || *outcome.ExitCode != 0 {
		return record
	}
	if outcome.ObservationRequired {
		if !validOutcomeEvidence(record.SessionID, outcome) {
			record.ReasonCode = ReasonObservationFailed
			return record
		}
		evidenceCopy := *outcome.Evidence
		record.ResultEvidence = &evidenceCopy
	}
	record.Status, record.ReasonCode = "completed", ""
	return record
}

func validOutcomeEvidence(session string, outcome FinalOutcome) bool {
	evidence := outcome.Evidence
	if evidence == nil || !evidence.Successful || !outcome.Stream.Successful {
		return false
	}
	if evidence.SessionID != session || evidence.SessionID != outcome.Stream.SessionID {
		return false
	}
	if evidence.Model == "" || evidence.Model != outcome.Stream.Model || evidence.Boundary <= 0 {
		return false
	}
	digest, err := hex.DecodeString(evidence.SHA256)
	return err == nil && len(digest) == 32
}

// Join order must not let a generic observation or cancellation error hide a
// contradictory session identity discovered by another observation barrier.
func identityContradiction(err error) *OutputError {
	var observed *OutputError
	if errors.As(err, &observed) && observed.ReasonCode == ReasonSessionIdentityMismatch {
		return observed
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, child := range joined.Unwrap() {
			if found := identityContradiction(child); found != nil {
				return found
			}
		}
	}
	return nil
}
