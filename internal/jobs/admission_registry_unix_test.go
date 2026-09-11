//go:build unix

package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func testRegistry(t *testing.T, root string) *Registry {
	t.Helper()
	r, err := OpenRegistry(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func testAdmission() Admission {
	return Admission{
		SubmissionID: uuid.NewString(), JobID: "ccr-" + uuid.NewString(),
		SessionID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64),
	}
}

func reserveAdmission(t *testing.T, r *Registry, a Admission) *SessionLease {
	t.Helper()
	lease, leaseErr := r.LockSession(a.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := r.Reserve(t.Context(), lease, a); err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestAdmissionReplaySurvivesReopenAndIgnoresCurrentConfiguration(t *testing.T) {
	root := t.TempDir()
	r := testRegistry(t, root)
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(t.Context(), a.JobID); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = testRegistry(t, root)
	a.ExecutionDigest = strings.Repeat("c", 64)
	replay, err := r.Reserve(t.Context(), lease, a)
	if err != nil || replay.JobID != a.JobID || replay.State != AdmissionFinished {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	a.RequestDigest = strings.Repeat("d", 64)
	if _, err := r.Reserve(t.Context(), lease, a); !errors.Is(err, ErrSubmissionConflict) {
		t.Fatalf("conflicting request: %v", err)
	}
}

func TestAdmissionExecutionBoundaryCannotBeCrossedTwice(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { results <- r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest) })
	}
	workers.Wait()
	close(results)
	successes, rejected := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAdmissionClosed):
			rejected++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("success=%d rejected=%d", successes, rejected)
	}
	head, exists, err := r.SessionHead(t.Context(), a.SessionID)
	if err != nil || !exists || head != a.JobID {
		t.Fatalf("head=%s exists=%v err=%v", head, exists, err)
	}
}

func TestPreparedCancellationWinsBeforeExecutionAndRetainsBinding(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	aborted, err := r.AbortPrepared(t.Context(), a.SubmissionID)
	if err != nil || !aborted {
		t.Fatalf("abort=%v err=%v", aborted, err)
	}
	if commitErr := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); !errors.Is(commitErr, ErrAdmissionClosed) {
		t.Fatalf("revived canceled preparation: %v", commitErr)
	}
	replay, err := r.Lookup(t.Context(), a.SubmissionID)
	if err != nil || replay.State != AdmissionAborted || replay.JobID != a.JobID {
		t.Fatalf("lost cancellation tombstone: %+v %v", replay, err)
	}
	head, exists, err := r.SessionHead(t.Context(), a.SessionID)
	if err != nil || !exists || head != "" {
		t.Fatalf("unexecuted preparation advanced head: %s %v %v", head, exists, err)
	}
}

func TestSessionLeaseIsNonblockingAndNamespacesAreIndependent(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	if _, err := r.LockSession(a.SessionID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("concurrent session owner: %v", err)
	}
	other := testRegistry(t, t.TempDir())
	independent, err := other.LockSession(a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = independent.Close() }()
	if _, err := other.Reserve(t.Context(), lease, a); err == nil {
		t.Fatal("accepted lease from another namespace")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); err == nil {
		t.Fatal("accepted closed lease")
	}
}

func TestPreparedConfigurationMismatchDoesNotAdvanceHead(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	lease := reserveAdmission(t, r, a)
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, strings.Repeat("c", 64)); err == nil {
		t.Fatal("accepted changed execution configuration")
	}
	got, err := r.Lookup(t.Context(), a.SubmissionID)
	if err != nil || got.State != AdmissionPrepared {
		t.Fatalf("changed prepared binding: %+v %v", got, err)
	}
	if err := r.CommitExecution(t.Context(), lease, a.SubmissionID, a.ExecutionDigest); err != nil {
		t.Fatal(err)
	}
	if aborted, err := r.AbortPrepared(t.Context(), a.SubmissionID); err != nil || aborted {
		t.Fatalf("revoked execution intent as never-started: %v %v", aborted, err)
	}
}

func TestReservationRollbackDoesNotLeaveFreshSessionAuthority(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	a := testAdmission()
	reserveAdmission(t, r, a)
	duplicate := testAdmission()
	duplicate.JobID = a.JobID
	lease, leaseErr := r.LockSession(duplicate.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer func() { _ = lease.Close() }()
	if _, err := r.Reserve(t.Context(), lease, duplicate); err == nil {
		t.Fatal("accepted duplicate job identity")
	}
	if _, exists, err := r.SessionHead(t.Context(), duplicate.SessionID); err != nil || exists {
		t.Fatalf("reservation did not roll back: exists=%v err=%v", exists, err)
	}
}

func TestRegistryCancellationDoesNotPoisonConnection(t *testing.T) {
	r := testRegistry(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	err := r.transaction(ctx, func(_ *sql.Conn) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result: %v", err)
	}
	a := testAdmission()
	reserveAdmission(t, r, a)
}
