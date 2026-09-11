//go:build unix

package jobs

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ValidateSubmissionID accepts a bounded opaque token without control bytes.
func ValidateSubmissionID(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("submission ID must contain 1 to 128 printable ASCII bytes")
	}
	for _, b := range []byte(id) {
		if b < 33 || b > 126 {
			return fmt.Errorf("submission ID must contain 1 to 128 printable ASCII bytes")
		}
	}
	return nil
}

func validateAdmission(a Admission) error {
	if err := ValidateSubmissionID(a.SubmissionID); err != nil {
		return err
	}
	if err := ValidateID(a.JobID); err != nil {
		return err
	}
	if err := ValidateSessionID(a.SessionID); err != nil {
		return err
	}
	for _, digest := range []string{a.RequestDigest, a.ExecutionDigest} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("admission requires SHA-256 request and execution digests")
		}
	}
	return validateResumeAdmission(a)
}

func validateResumeAdmission(a Admission) error {
	if a.RequestedResumeSession == "" {
		if a.ExpectedParentJob != "" || a.ResumedFrom != "" || a.ResumedFromStatus != "" {
			return fmt.Errorf("fresh admission cannot name a predecessor")
		}
		return nil
	}
	if a.RequestedResumeSession != a.SessionID {
		return fmt.Errorf("requested resume session differs from admission")
	}
	if err := ValidateID(a.ResumedFrom); err != nil {
		return err
	}
	if a.ExpectedParentJob != "" && a.ExpectedParentJob != a.ResumedFrom {
		return ErrSessionHeadChanged
	}
	switch a.ResumedFromStatus {
	case "completed", "failed", statusCancelled:
		return nil
	default:
		return fmt.Errorf("resume predecessor is not terminal")
	}
}

// Reserve binds a submission exactly once. A matching replay returns its
// original binding without consulting or changing its current execution state.
// The caller must first verify positive stop evidence for ResumedFrom while
// holding the supplied session lease.
func (r *Registry) Reserve(ctx context.Context, lease *SessionLease, candidate Admission) (Admission, error) {
	if err := validateAdmission(candidate); err != nil {
		return Admission{}, err
	}
	if err := r.validateLease(lease, candidate.SessionID); err != nil {
		return Admission{}, err
	}
	var result Admission
	err := r.transaction(ctx, func(conn *sql.Conn) error {
		existing, err := scanAdmission(conn.QueryRowContext(ctx,
			"SELECT "+admissionColumns+" FROM admissions WHERE submission_id=?", candidate.SubmissionID))
		if err == nil {
			if existing.RequestDigest != candidate.RequestDigest {
				return ErrSubmissionConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrAdmissionNotFound) {
			return err
		}
		if headErr := reserveHead(ctx, conn, candidate); headErr != nil {
			return headErr
		}
		candidate.State, candidate.ReasonCode = AdmissionPrepared, ""
		candidate.CancelRequested = false
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = conn.ExecContext(ctx, `INSERT INTO admissions (`+admissionColumns+`, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, candidate.SubmissionID, candidate.RequestDigest, candidate.ExecutionDigest,
			candidate.JobID, candidate.SessionID, candidate.RequestedResumeSession, candidate.ExpectedParentJob,
			candidate.ResumedFrom, candidate.ResumedFromStatus, candidate.State, candidate.ReasonCode, false, now, now)
		if err != nil {
			return fmt.Errorf("reserving submission: %w", err)
		}
		result = candidate
		return nil
	})
	return result, err
}

func reserveHead(ctx context.Context, conn *sql.Conn, a Admission) error {
	var head sql.NullString
	err := conn.QueryRowContext(ctx, "SELECT head_job_id FROM session_heads WHERE session_id=?", a.SessionID).Scan(&head)
	if a.RequestedResumeSession != "" {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading resume session head: %w", err)
		}
		if err != nil || !head.Valid || head.String != a.ResumedFrom {
			return ErrSessionHeadChanged
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return fmt.Errorf("reading fresh session authority: %w", err)
		}
		return fmt.Errorf("fresh session already has an authority record")
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO session_heads(session_id, head_job_id, updated_at) VALUES (?,NULL,?)",
		a.SessionID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("reserving fresh session authority: %w", err)
	}
	return nil
}
