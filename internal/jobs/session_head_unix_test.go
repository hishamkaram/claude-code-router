//go:build unix

package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func legacyPredecessor(t *testing.T, r *Registry, status string) Record {
	t.Helper()
	s := Store{Root: r.root}
	record, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	record.Status, record.ExitCode = status, &code
	record.Cleanup = Cleanup{Coverage: "partial", Survivors: []Survivor{}}
	if err := s.Write(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLegacySessionBootstrapRequiresPositiveStoppedEvidence(t *testing.T) {
	for _, status := range []string{"completed", "failed", statusCancelled, "running"} {
		t.Run(status, func(t *testing.T) {
			r := testRegistry(t, t.TempDir())
			old := legacyPredecessor(t, r, status)
			lease, leaseErr := r.LockSession(old.SessionID)
			if leaseErr != nil {
				t.Fatal(leaseErr)
			}
			defer func() { _ = lease.Close() }()
			head, err := r.ResumeHead(t.Context(), lease, old.JobID)
			if status == "running" {
				if err == nil {
					t.Fatal("accepted predecessor without stopped evidence")
				}
				return
			}
			if err != nil || head.JobID != old.JobID || head.Cleanup.Coverage != "partial" {
				t.Fatalf("head=%+v err=%v", head, err)
			}
		})
	}
}

func TestSessionHeadNeverFallsBackAfterArtifactDeletion(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	old := legacyPredecessor(t, r, "completed")
	lease, leaseErr := r.LockSession(old.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer func() { _ = lease.Close() }()
	if _, err := r.ResumeHead(t.Context(), lease, old.JobID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(r.root, old.JobID)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeHead(t.Context(), lease, old.JobID); err == nil {
		t.Fatal("deleted head regained resume authority")
	}
	head, exists, err := r.SessionHead(t.Context(), old.SessionID)
	if err != nil || !exists || head != old.JobID {
		t.Fatalf("lost head tombstone: %s %v %v", head, exists, err)
	}
}

func TestUnknownAmbiguousAndIncompleteLegacySessionsFailClosed(t *testing.T) {
	for _, scenario := range []string{"unknown", "ambiguous", "missing-survivors"} {
		t.Run(scenario, func(t *testing.T) {
			r := testRegistry(t, t.TempDir())
			sid := uuid.NewString()
			if scenario != "unknown" {
				old := legacyPredecessor(t, r, "completed")
				sid = old.SessionID
				if scenario == "ambiguous" {
					second := legacyPredecessor(t, r, "completed")
					second.SessionID = sid
					if err := (Store{Root: r.root}).Write(second); err != nil {
						t.Fatal(err)
					}
				} else {
					old.Cleanup.Survivors = nil
					data, err := json.Marshal(old)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(r.root, old.JobID, "status.json"), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			lease, leaseErr := r.LockSession(sid)
			if leaseErr != nil {
				t.Fatal(leaseErr)
			}
			defer func() { _ = lease.Close() }()
			if _, err := r.ResumeHead(t.Context(), lease, ""); err == nil {
				t.Fatal("accepted unsafe session")
			}
		})
	}
}

func TestResumeReservationRetainsParentUntilExecutionIntent(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	old := legacyPredecessor(t, r, "failed")
	lease, leaseErr := r.LockSession(old.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer func() { _ = lease.Close() }()
	if _, err := r.ResumeHead(t.Context(), lease, "ccr-"+uuid.NewString()); !errors.Is(err, ErrSessionHeadChanged) {
		t.Fatalf("stale parent: %v", err)
	}
	a := testAdmission()
	a.SessionID, a.RequestedResumeSession = old.SessionID, old.SessionID
	a.ExpectedParentJob, a.ResumedFrom, a.ResumedFromStatus = old.JobID, old.JobID, old.Status
	if _, err := r.Reserve(t.Context(), lease, a); err != nil {
		t.Fatal(err)
	}
	if aborted, err := r.AbortPrepared(t.Context(), a.SubmissionID); err != nil || !aborted {
		t.Fatalf("abort=%v err=%v", aborted, err)
	}
	head, _, err := r.SessionHead(t.Context(), old.SessionID)
	if err != nil || head != old.JobID {
		t.Fatalf("aborted resume changed execution head: %s %v", head, err)
	}
	a.SubmissionID, a.JobID = uuid.NewString(), "ccr-"+uuid.NewString()
	if _, err := r.Reserve(t.Context(), lease, a); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeHead(t.Context(), lease, old.JobID); !errors.Is(err, ErrSessionHeadChanged) {
		t.Fatalf("selected old ancestor after execution became possible: %v", err)
	}
}

func TestMissingEstablishedHeadCannotBootstrapLegacyAncestor(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	old := legacyPredecessor(t, r, "completed")
	lease, leaseErr := r.LockSession(old.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer func() { _ = lease.Close() }()
	if _, err := r.ResumeHead(t.Context(), lease, old.JobID); err != nil {
		t.Fatal(err)
	}
	a := testAdmission()
	a.SessionID, a.RequestedResumeSession, a.ResumedFrom, a.ResumedFromStatus = old.SessionID, old.SessionID, old.JobID, old.Status
	if _, err := r.Reserve(t.Context(), lease, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(t.Context(), "DELETE FROM session_heads WHERE session_id=?", old.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeHead(t.Context(), lease, ""); err == nil || !strings.Contains(err.Error(), "authority is missing") {
		t.Fatalf("missing established head: %v", err)
	}
}
