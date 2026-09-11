//go:build unix

package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// JobStatus reports registry authority without starting or recovering work.
// In particular, an absent prepared owner does not abort a recoverable admission.
func (r *Registry) JobStatus(ctx context.Context, id string) (Record, error) {
	a, err := r.LookupJob(ctx, id)
	if err != nil {
		return Record{}, err
	}
	s := Store{Root: r.root}
	record, err := s.Read(id)
	if errors.Is(err, os.ErrNotExist) {
		return admissionTombstone(a), nil
	}
	if err != nil {
		return Record{}, err
	}
	if !matchesAdmission(record, a) {
		return Record{}, fmt.Errorf("job record contradicts admission authority")
	}
	applyAdmission(&record, a)
	if a.State == AdmissionAborted || a.State == AdmissionPrepared || record.Terminal() {
		return record, nil
	}
	lock, acquired, err := s.Lock(id)
	if err != nil || !acquired {
		return record, err
	}
	defer func() { _ = lock.Close() }()
	// The owner may have finalized between the first read and our lease acquisition.
	record, err = s.Read(id)
	if err != nil {
		return Record{}, err
	}
	if !matchesAdmission(record, a) {
		return Record{}, fmt.Errorf("job record contradicts admission authority")
	}
	applyAdmission(&record, a)
	if record.Terminal() {
		return record, nil
	}
	record.Status, record.ReasonCode = "failed", ReasonOwnerLost
	record.Reason = "job owner was lost; execution will not be replayed"
	record.WorkloadDisposition = DispositionUnknown
	record.Cleanup = Cleanup{Coverage: "unknown", Reason: "owner lost; no cancellation authority recovered from metadata"}
	return record, s.Write(record)
}

func applyAdmission(record *Record, a Admission) {
	record.SubmissionID, record.AdmissionState = a.SubmissionID, a.State
	record.RequestedResumeSession = a.RequestedResumeSession
	record.ResumedFrom, record.ResumedFromStatus = a.ResumedFrom, a.ResumedFromStatus
	record.WorkloadDisposition = DispositionUnknown
	if record.Stopped() {
		record.WorkloadDisposition = DispositionStopped
	}
	if a.State == AdmissionAborted {
		record.Status, record.ReasonCode = "failed", a.ReasonCode
		if a.CancelRequested {
			record.Status, record.CancelRequested = statusCancelled, true
		}
		record.WorkloadDisposition = DispositionNotStarted
		record.ExitCode, record.ResultEvidence = nil, nil
		record.Cleanup = Cleanup{Coverage: "unknown", Reason: "execution gate was never released"}
	}
}

func admissionTombstone(a Admission) Record {
	record := Record{
		SchemaVersion: 2, JobID: a.JobID, SessionID: a.SessionID,
		Status: "failed", WorkloadDisposition: DispositionUnknown,
		ReasonCode: ReasonObservationFailed, Reason: "job artifacts are unavailable; admission binding retained",
		Cleanup: Cleanup{Coverage: "unknown", Reason: "job artifacts unavailable"},
	}
	applyAdmission(&record, a)
	if a.State == AdmissionPrepared {
		record.Status = "running"
		record.ReasonCode = ReasonAdmissionInterrupted
	}
	return record
}

func (s Store) cancelPrepared(ctx context.Context, record Record) (Record, bool, error) {
	if record.SchemaVersion != 2 || record.AdmissionState != AdmissionPrepared {
		return record, false, nil
	}
	registry, err := OpenRegistry(ctx, s.Root)
	if err != nil {
		return record, false, err
	}
	defer func() { _ = registry.Close() }()
	aborted, err := registry.CancelPrepared(ctx, record.SubmissionID)
	if err != nil || !aborted {
		return record, false, err
	}
	record, err = registry.JobStatus(ctx, record.JobID)
	return record, true, err
}

func matchesAdmission(record Record, a Admission) bool {
	return record.SessionID == a.SessionID && record.SubmissionID == a.SubmissionID && record.SchemaVersion == 2
}
