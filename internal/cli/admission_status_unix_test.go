//go:build linux || darwin

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

func TestAdmissionStatusRejectsConflictingOrMalformedIdentities(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, args := range [][]string{
		{"--submission-id="},
		{"--session-id="},
		{"--session-id=../outside"},
		{"--submission-id=contains space"},
		{"--submission-id=a", "--session-id=b"},
		{"ccr-" + uuid.NewString(), "--submission-id=a"},
	} {
		cmd := NewRootCommand(t.Context(), Dependencies{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"status"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted malformed status: %v", args)
		}
	}
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(s.Root); !os.IsNotExist(statErr) {
		t.Fatalf("invalid status created job storage: %v", statErr)
	}
}

func TestSubmissionStatusDoesNotRecoverPreparedExecution(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s, err := jobStore()
	if err != nil {
		t.Fatal(err)
	}
	r, err := jobs.OpenRegistry(t.Context(), s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	a := jobs.Admission{
		SubmissionID: uuid.NewString(), JobID: "ccr-" + uuid.NewString(),
		SessionID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64),
	}
	lease, leaseErr := r.LockSession(a.SessionID)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	defer func() { _ = lease.Close() }()
	if _, reserveErr := r.Reserve(t.Context(), lease, a); reserveErr != nil {
		t.Fatal(reserveErr)
	}
	cmd := NewRootCommand(t.Context(), Dependencies{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--submission-id=" + a.SubmissionID, "--json"})
	if executeErr := cmd.Execute(); executeErr != nil {
		t.Fatal(executeErr)
	}
	var got jobs.Record
	if decodeErr := json.Unmarshal(out.Bytes(), &got); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if got.JobID != a.JobID || got.SessionID != a.SessionID || got.AdmissionState != jobs.AdmissionPrepared || got.Status != "running" {
		t.Fatalf("incorrect prepared lookup: %+v", got)
	}
	binding, err := r.Lookup(t.Context(), a.SubmissionID)
	if err != nil || binding.State != jobs.AdmissionPrepared {
		t.Fatalf("status changed admission: %+v %v", binding, err)
	}
}
