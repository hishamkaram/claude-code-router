// Package jobs owns durable detached-job state and subprocess containment.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Survivor struct {
	PID  int `json:"pid"`
	PGID int `json:"pgid,omitempty"`
}

const statusCancelled = "cancelled" //nolint:misspell // Preserve the public job status spelling.

type Cleanup struct {
	Coverage     string     `json:"coverage"`
	Survivors    []Survivor `json:"survivors"`
	Observed     []string   `json:"observed"`
	Unobservable []string   `json:"unobservable"`
	Reason       string     `json:"reason"`
}

// Normalize makes older or malformed reports pessimistic for every reader.
func (c *Cleanup) Normalize() {
	switch c.Coverage {
	case "complete", "partial", "unknown":
	default:
		c.Coverage = "unknown"
		c.Reason = "missing or unrecognized cleanup coverage"
	}
	if c.Coverage == "unknown" && c.Reason == "" {
		c.Reason = "cleanup observation unavailable"
	}
	if c.Survivors == nil {
		c.Survivors = []Survivor{}
	}
	if c.Observed == nil {
		c.Observed = []string{}
	}
	if c.Unobservable == nil {
		c.Unobservable = []string{}
	}
}

type Record struct {
	ExpectedModel          string          `json:"expected_model,omitempty"`
	SubmissionID           string          `json:"submission_id,omitempty"`
	RequestedResumeSession string          `json:"requested_resume_session,omitempty"`
	ResumedFrom            string          `json:"resumed_from,omitempty"`
	ResumedFromStatus      string          `json:"resumed_from_status,omitempty"`
	AdmissionState         string          `json:"admission_state,omitempty"`
	WorkloadDisposition    string          `json:"workload_disposition,omitempty"`
	ReasonCode             string          `json:"reason_code,omitempty"`
	ObservedSessionID      string          `json:"observed_session_id,omitempty"`
	ResultEvidence         *ResultEvidence `json:"result_evidence,omitempty"`
	SchemaVersion          int             `json:"schema_version"`
	JobID                  string          `json:"job_id"`
	SessionID              string          `json:"session_id"`
	Status                 string          `json:"status"`
	ExitCode               *int            `json:"exit_code"`
	Log                    string          `json:"log"`
	ErrorLog               string          `json:"error_log"`
	CancelRequested        bool            `json:"cancel_requested"`
	Containment            string          `json:"containment"`
	Cleanup                Cleanup         `json:"cleanup"`
	Reason                 string          `json:"reason,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

func (r Record) Terminal() bool { return r.Status != "running" }

// Stopped requires positive child-exit and cleanup evidence. Partial coverage
// remains explicit and does not prove the absence of escaped descendants.
func (r Record) Stopped() bool {
	switch r.Status {
	case "completed", "failed", statusCancelled:
	default:
		return false
	}
	return r.ExitCode != nil && (r.Cleanup.Coverage == "complete" || r.Cleanup.Coverage == "partial") &&
		r.Cleanup.Survivors != nil && len(r.Cleanup.Survivors) == 0
}

func ValidateID(id string) error {
	value, ok := strings.CutPrefix(id, "ccr-")
	u, err := uuid.Parse(value)
	if !ok || err != nil || u.String() != value || u.Version() != 4 {
		return fmt.Errorf("invalid job ID: expected ccr- followed by a canonical version-4 UUID")
	}
	return nil
}

type Store struct{ Root string }

func (s Store) Directory(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, id), nil
}

func (s Store) Create() (Record, error) {
	r := Record{SchemaVersion: 1, JobID: "ccr-" + uuid.NewString(), SessionID: uuid.NewString(), Status: "running", Containment: "pending", CreatedAt: time.Now().UTC()}
	r.Cleanup.Normalize()
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Record{}, fmt.Errorf("creating job root: %w", err)
	}
	dir, err := s.Directory(r.JobID)
	if err != nil {
		return Record{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Record{}, fmt.Errorf("creating job directory: %w", err)
	}
	r.Log = filepath.Join(dir, "stdout.log")
	r.ErrorLog = filepath.Join(dir, "stderr.log")
	if err := syncDirectory(s.Root); err != nil {
		return Record{}, err
	}
	return r, nil
}

func (s Store) Write(r Record) error {
	dir, err := s.Directory(r.JobID)
	if err != nil {
		return err
	}
	r.Cleanup.Normalize()
	r.UpdatedAt = time.Now().UTC()
	f, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("creating job record: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := json.NewEncoder(f).Encode(r); err != nil {
		_ = f.Close()
		return fmt.Errorf("encoding job record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("synchronizing job record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing job record: %w", err)
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "status.json")); err != nil {
		return fmt.Errorf("publishing job record: %w", err)
	}
	return syncDirectory(dir)
}

func (s Store) Read(id string) (Record, error) {
	dir, err := s.Directory(id)
	if err != nil {
		return Record{}, err
	}
	f, err := os.Open(filepath.Join(dir, "status.json"))
	if err != nil {
		return Record{}, fmt.Errorf("reading job %s: %w", id, err)
	}
	defer func() { _ = f.Close() }()
	var r Record
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		return Record{}, fmt.Errorf("decoding job record: %w", err)
	}
	if r.JobID != id || (r.SchemaVersion != 1 && r.SchemaVersion != 2) {
		return Record{}, fmt.Errorf("job record identity or schema mismatch")
	}
	switch r.Status {
	case "running", "completed", "failed", statusCancelled:
	default:
		return Record{}, fmt.Errorf("unrecognized job status")
	}
	if r.Cleanup.Survivors == nil {
		r.Cleanup.Coverage, r.Cleanup.Reason = "unknown", "cleanup survivor observation missing"
	}
	r.Cleanup.Normalize()
	return r, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening job directory: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("synchronizing job directory: %w", err)
	}
	return nil
}
