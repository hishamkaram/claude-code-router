//go:build unix

package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyBootstrapExcludesOnlyAuthoritativelyUnrelatedDirectories(t *testing.T) {
	for _, mode := range []string{"prepared", "aborted", "unregistered"} {
		for _, operation := range []string{"status", "resume"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				r := testRegistry(t, t.TempDir())
				old := legacyPredecessor(t, r, "completed")
				other := testAdmission()
				if mode != "unregistered" {
					reserveAdmission(t, r, other)
				}
				if mode == "aborted" {
					if _, err := r.AbortPrepared(t.Context(), other.SubmissionID); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(filepath.Join(r.root, other.JobID), 0o700); err != nil {
					t.Fatal(err)
				}
				var got Record
				var err error
				if operation == "status" {
					got, err = r.SessionStatus(t.Context(), old.SessionID)
				} else {
					lease, leaseErr := r.LockSession(old.SessionID)
					if leaseErr != nil {
						t.Fatal(leaseErr)
					}
					defer func() { _ = lease.Close() }()
					got, err = r.ResumeHead(t.Context(), lease, old.JobID)
				}
				if mode == "unregistered" {
					if err == nil {
						t.Fatal("unregistered ambiguous directory was ignored")
					}
					return
				}
				if err != nil || got.JobID != old.JobID {
					t.Fatalf("unrelated %s obstructed %s: %v", mode, operation, err)
				}
			})
		}
	}
}
