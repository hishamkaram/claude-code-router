//go:build unix

package jobs

import (
	"errors"
	"os"
	"testing"
)

func TestPreparedArtifactsRecoverSameJobAndCannotReopenExecution(t *testing.T) {
	registry := testRegistry(t, t.TempDir())
	admission := testAdmission()
	lease := reserveAdmission(t, registry, admission)
	first, lock, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID != admission.JobID || first.SessionID != admission.SessionID || first.SchemaVersion != 2 {
		t.Fatalf("wrong artifact identity: %+v", first)
	}
	if _, _, activeErr := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID); activeErr == nil {
		t.Fatal("materialized through active job owner")
	}
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	recovered, lock, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.JobID != first.JobID || !recovered.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("recovery replaced original admission")
	}
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if commitErr := registry.CommitExecution(t.Context(), lease, admission.SubmissionID, admission.ExecutionDigest); commitErr != nil {
		t.Fatal(commitErr)
	}
	directory, err := (Store{Root: registry.root}).Directory(admission.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("reopened executed admission after artifact removal: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recreated executed artifact: %v", err)
	}
}

func TestCancelledReservationCannotMaterialize(t *testing.T) {
	registry := testRegistry(t, t.TempDir())
	admission := testAdmission()
	lease := reserveAdmission(t, registry, admission)
	if aborted, err := registry.CancelPrepared(t.Context(), admission.SubmissionID); err != nil || !aborted {
		t.Fatalf("cancel=%v %v", aborted, err)
	}
	if _, _, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("revived canceled admission: %v", err)
	}
}

func TestPreparedMaterializationPreservesLeaseAndSyncFailures(t *testing.T) {
	for _, failure := range []string{"lease", "directory-sync"} {
		t.Run(failure, func(t *testing.T) {
			registry := testRegistry(t, t.TempDir())
			admission := testAdmission()
			lease := reserveAdmission(t, registry, admission)
			syncFailure := errors.New("injected directory synchronization failure")
			synchronize := syncDirectory
			if failure == "lease" {
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				synchronize = func(string) error { return syncFailure }
			}
			record, lock, err := registry.materializePrepared(t.Context(), lease, admission.SubmissionID, synchronize)
			if err == nil || record.JobID != "" || lock != nil {
				t.Fatalf("failed materialization reported success: record=%+v lock=%v err=%v", record, lock, err)
			}
			if failure == "directory-sync" && !errors.Is(err, syncFailure) {
				t.Fatalf("lost synchronization cause: %v", err)
			}
			bound, lookupErr := registry.Lookup(t.Context(), admission.SubmissionID)
			if lookupErr != nil || bound.State != AdmissionPrepared {
				t.Fatalf("failure changed execution reservation: %+v %v", bound, lookupErr)
			}
		})
	}
}
