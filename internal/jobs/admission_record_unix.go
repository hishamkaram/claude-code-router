//go:build unix

package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaterializePrepared creates the recoverable job artifacts after reservation.
// The caller holds the session lease; this method takes a nonblocking job lease before
// handoff. Neither acquisition waits inside a registry transaction.
func (r *Registry) MaterializePrepared(ctx context.Context, lease *SessionLease, submission string) (Record, *os.File, error) {
	admission, err := r.Lookup(ctx, submission)
	if err != nil {
		return Record{}, nil, err
	}
	if leaseErr := r.validateLease(lease, admission.SessionID); leaseErr != nil {
		return Record{}, nil, err
	}
	if admission.State != AdmissionPrepared {
		return Record{}, nil, ErrAdmissionClosed
	}
	s := Store{Root: r.root}
	directory, err := s.Directory(admission.JobID)
	if err != nil {
		return Record{}, nil, err
	}
	if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
		return Record{}, nil, fmt.Errorf("creating prepared job directory: %w", mkdirErr)
	}
	if syncErr := syncDirectory(r.root); syncErr != nil {
		return Record{}, nil, err
	}
	lock, acquired, err := s.Lock(admission.JobID)
	if err != nil {
		return Record{}, nil, err
	}
	if !acquired {
		return Record{}, nil, fmt.Errorf("prepared job owner is still active")
	}
	record, err := s.materializePrepared(admission, directory)
	if err != nil {
		_ = lock.Close()
		return Record{}, nil, err
	}
	return record, lock, nil
}

func (s Store) materializePrepared(a Admission, directory string) (Record, error) {
	existing, err := s.Read(a.JobID)
	if err == nil {
		if existing.SchemaVersion != 2 || existing.SubmissionID != a.SubmissionID || existing.SessionID != a.SessionID || existing.Terminal() || existing.Log != filepath.Join(directory, "stdout.log") || existing.ErrorLog != filepath.Join(directory, "stderr.log") {
			return Record{}, fmt.Errorf("prepared job artifacts contradict admission")
		}
		applyAdmission(&existing, a)
		return existing, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	record := Record{
		SchemaVersion: 2, JobID: a.JobID, SessionID: a.SessionID,
		Status: "running", Containment: "pending", CreatedAt: time.Now().UTC(),
		Log: filepath.Join(directory, "stdout.log"), ErrorLog: filepath.Join(directory, "stderr.log"),
		Cleanup: Cleanup{Coverage: "unknown", Reason: "execution has not been admitted"},
	}
	applyAdmission(&record, a)
	if err := s.Write(record); err != nil {
		return Record{}, err
	}
	return record, nil
}
