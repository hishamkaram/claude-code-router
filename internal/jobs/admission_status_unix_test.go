//go:build unix

package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func admissionRecord(t *testing.T, r *Registry, a Admission) {
	t.Helper()
	dir := filepath.Join(r.root, a.JobID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: 2, JobID: a.JobID, SessionID: a.SessionID,
		SubmissionID: a.SubmissionID, Status: "running", WorkloadDisposition: DispositionUnknown,
	}
	if err := (Store{Root: r.root}).Write(record); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedStatusNeverStartsOrAbortsAdmission(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	reserveAdmission(t, r, a)
	for _, artifacts := range []bool{false, true} {
		if artifacts {
			admissionRecord(t, r, a)
		}
		got, err := r.JobStatus(t.Context(), a.JobID)
		if err != nil || got.Status != "running" || got.AdmissionState != AdmissionPrepared {
			t.Fatalf("prepared status: %+v %v", got, err)
		}
		binding, err := r.Lookup(t.Context(), a.SubmissionID)
		if err != nil || binding.State != AdmissionPrepared {
			t.Fatalf("status changed admission: %+v %v", binding, err)
		}
	}
}

func TestAbortedStatusHasNoInventedExitOrCleanup(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	reserveAdmission(t, r, a)
	admissionRecord(t, r, a)
	if _, err := r.AbortPrepared(t.Context(), a.SubmissionID); err != nil {
		t.Fatal(err)
	}
	got, err := (Store{Root: r.root}).StatusContext(t.Context(), a.JobID)
	if err != nil || got.WorkloadDisposition != DispositionNotStarted || !got.Terminal() || got.ExitCode != nil || got.Stopped() {
		t.Fatalf("invalid never-started evidence: %+v %v", got, err)
	}
}

func TestPossibleOwnerLossNeverBecomesReplayable(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	admissionRecord(t, r, a)
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); err != nil {
		t.Fatal(err)
	}
	got, err := r.JobStatus(t.Context(), a.JobID)
	if err != nil || got.Status != "failed" || got.ReasonCode != ReasonOwnerLost || got.WorkloadDisposition != DispositionUnknown {
		t.Fatalf("owner loss: %+v %v", got, err)
	}
	if replayErr := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); replayErr == nil {
		t.Fatal("owner loss permitted replay")
	}
	if removeErr := os.RemoveAll(filepath.Join(r.root, a.JobID)); removeErr != nil {
		t.Fatal(removeErr)
	}
	got, err = r.JobStatus(t.Context(), a.JobID)
	if err != nil || got.AdmissionState != AdmissionPossible || got.WorkloadDisposition != DispositionUnknown {
		t.Fatalf("deleted possible admission: %+v %v", got, err)
	}
}

func TestCancelPreparedWithoutLiveOwner(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	admissionRecord(t, r, a)
	got, err := (Store{Root: r.root}).Cancel(t.Context(), a.JobID)
	if err != nil || got.Status != statusCancelled || !got.CancelRequested || got.WorkloadDisposition != DispositionNotStarted {
		t.Fatalf("prepared cancellation: %+v %v", got, err)
	}
	if replayErr := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); replayErr == nil {
		t.Fatal("canceled admission revived")
	}
}

func TestCancelPreparedBeforeArtifactsExistPreventsRecovery(t *testing.T) {
	registry := testRegistry(t, t.TempDir())
	admission := testAdmission()
	lease := reserveAdmission(t, registry, admission)
	s := Store{Root: registry.root}
	record, err := s.Cancel(t.Context(), admission.JobID)
	if err != nil || record.Status != statusCancelled || record.WorkloadDisposition != DispositionNotStarted {
		t.Fatalf("prepared cancellation: %+v %v", record, err)
	}
	if _, _, materializeErr := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID); !errors.Is(materializeErr, ErrAdmissionClosed) {
		t.Fatalf("revived canceled reservation: %v", materializeErr)
	}
}
