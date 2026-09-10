//go:build linux || darwin

package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	s := Store{Root: filepath.Join(t.TempDir(), "jobs")}
	r, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	code := 7
	r.Status, r.ExitCode = "failed", &code
	r.Cleanup = Cleanup{Coverage: "partial", Reason: "escape cannot be excluded"}
	if writeErr := s.Write(r); writeErr != nil {
		t.Fatal(writeErr)
	}
	got, err := s.Read(r.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.ExitCode == nil || *got.ExitCode != 7 || got.SessionID != r.SessionID || got.Cleanup.Coverage != "partial" || got.Cleanup.Survivors == nil {
		t.Fatalf("record mismatch: %+v", got)
	}
	dir, err := s.Directory(r.JobID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record permissions: %o", info.Mode().Perm())
	}
}
