//go:build linux || darwin

package jobs

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnerLossIsDurableAndDoesNotReplay(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "jobs")}
	r, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	lock, ok, err := s.Lock(r.JobID)
	if err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	if writeErr := s.Write(r); writeErr != nil {
		t.Fatal(writeErr)
	}
	live, err := s.Status(r.JobID)
	if err != nil || live.Status != "running" {
		t.Fatalf("live: %+v %v", live, err)
	}
	lock.Close()
	lost, err := s.Status(r.JobID)
	if err != nil || lost.Status != "failed" || lost.Cleanup.Coverage != "unknown" {
		t.Fatalf("lost: %+v %v", lost, err)
	}
	stored, err := s.Read(r.JobID)
	if err != nil || stored.Status != "failed" {
		t.Fatalf("not durable: %+v %v", stored, err)
	}
	if _, err := s.Cancel(context.Background(), r.JobID); err != nil {
		t.Fatal(err)
	}
}

func TestCancelOnlyThroughOwnerAndIsIdempotent(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := Store{Root: filepath.Join(t.TempDir(), "jobs")}
	r, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	lock, ok, err := s.Lock(r.JobID)
	if err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	defer lock.Close()
	if writeErr := s.Write(r); writeErr != nil {
		t.Fatal(writeErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := NewOwner(s, r, cancel)
	listener, err := o.Listen()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: o, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	// A different shell's temporary directory must not hide the live owner.
	t.Setenv("TMPDIR", t.TempDir())
	for i := 0; i < 2; i++ {
		got, cancelErr := s.Cancel(context.Background(), r.JobID)
		if cancelErr != nil || !got.CancelRequested {
			t.Fatalf("cancel: %+v %v", got, cancelErr)
		}
	}
	if ctx.Err() == nil {
		t.Fatal("owner was not canceled")
	}
	if finishErr := o.Finish(context.Canceled, Cleanup{Coverage: "partial", Reason: "test"}, nil); finishErr != nil {
		t.Fatal(finishErr)
	}
	got, err := s.Cancel(context.Background(), r.JobID)
	if err != nil || got.Status != statusCancelled {
		t.Fatalf("terminal cancellation: %+v %v", got, err)
	}
}

func TestGroupObservationRejectsMalformedAndIgnoresZombies(t *testing.T) {
	for _, input := range []string{"", "1 2 S", "1 2 S\ntruncated\n", "1 invalid S\n"} {
		if _, err := parseGroupSnapshot(input, 2); err == nil {
			t.Errorf("accepted malformed table %q", input)
		}
	}
	got, err := parseGroupSnapshot("1 2 Z\n3 2 S\n4 8 S\n", 2)
	if err != nil || len(got) != 1 || got[0].PID != 3 {
		t.Fatalf("observation: %+v %v", got, err)
	}
}
