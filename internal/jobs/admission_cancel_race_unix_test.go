//go:build linux || darwin

package jobs

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
)

func TestCancellationDuringTerminalPublication(t *testing.T) {
	registry := testRegistry(t, t.TempDir())
	admission := testAdmission()
	lease := reserveAdmission(t, registry, admission)
	record, lock, err := registry.MaterializePrepared(t.Context(), lease, admission.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if commitErr := registry.CommitExecution(t.Context(), lease, admission.SubmissionID, admission.ExecutionDigest); commitErr != nil {
		t.Fatal(commitErr)
	}
	owner := NewOwner(Store{Root: registry.root}, record, func() {})
	var requests sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-start
			for range 1000 {
				response := httptest.NewRecorder()
				owner.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/cancel/"+admission.JobID, nil))
				if response.Code != http.StatusOK {
					t.Errorf("owned cancellation returned %d", response.Code)
					return
				}
			}
		}()
	}
	close(start)
	runtime.Gosched()
	zero := 0
	err = owner.FinishAdmission(t.Context(), registry, FinalOutcome{Cleanup: Cleanup{Coverage: "partial", Survivors: []Survivor{}}, ExitCode: &zero})
	requests.Wait()
	if err != nil {
		t.Fatal(err)
	}
	final, err := registry.JobStatus(t.Context(), admission.JobID)
	if err != nil || !final.Stopped() {
		t.Fatalf("lost terminal evidence: %+v %v", final, err)
	}
}
