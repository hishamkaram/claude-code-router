package store

import (
	"context"
	"testing"
)

func TestConformanceRunAndChecksRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	runID, err := s.CreateConformanceRun(ctx, ConformanceRun{
		Alias: "coder", Scope: "provider", ProviderName: "fixture",
		ProviderModel: "model-v1", Protocol: "openai-compatible",
	})
	if err != nil {
		t.Fatalf("CreateConformanceRun() error = %v", err)
	}
	for _, check := range []ConformanceCheck{
		{RunID: runID, Name: "configuration", Status: "passed", Evidence: "configured"},
		{RunID: runID, Name: "thinking", Status: "not_applicable", Evidence: "disabled"},
	} {
		if _, checkErr := s.AddConformanceCheck(ctx, check); checkErr != nil {
			t.Fatalf("AddConformanceCheck(%s) error = %v", check.Name, checkErr)
		}
	}
	if completeErr := s.CompleteConformanceRun(ctx, runID, "passed", true, "2 checks"); completeErr != nil {
		t.Fatalf("CompleteConformanceRun() error = %v", completeErr)
	}
	runs, err := s.ListConformanceRuns(ctx, "coder", 10)
	if err != nil {
		t.Fatalf("ListConformanceRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != runID || runs[0].Status != "passed" ||
		!runs[0].LiveVerified || runs[0].CompletedAt == "" {
		t.Fatalf("ListConformanceRuns() = %#v", runs)
	}
	checks, err := s.ListConformanceChecks(ctx, runID)
	if err != nil {
		t.Fatalf("ListConformanceChecks() error = %v", err)
	}
	if len(checks) != 2 || checks[1].Status != "not_applicable" {
		t.Fatalf("ListConformanceChecks() = %#v", checks)
	}
}
