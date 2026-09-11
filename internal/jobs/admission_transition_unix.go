//go:build unix

package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CommitExecution is the one-way execution boundary. It must commit before
// writing any bytes to the workload gate. Even a failed gate write after this
// commit must never make the admission replayable.
func (r *Registry) CommitExecution(ctx context.Context, lease *SessionLease, submission, executionDigest string) error {
	return r.transaction(ctx, func(conn *sql.Conn) error {
		a, err := scanAdmission(conn.QueryRowContext(ctx,
			"SELECT "+admissionColumns+" FROM admissions WHERE submission_id=?", submission))
		if err != nil {
			return err
		}
		if err := r.validateLease(lease, a.SessionID); err != nil {
			return err
		}
		if a.State != AdmissionPrepared {
			return ErrAdmissionClosed
		}
		if a.ExecutionDigest != executionDigest {
			return fmt.Errorf("prepared execution configuration changed")
		}
		var head sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT head_job_id FROM session_heads WHERE session_id=?", a.SessionID).Scan(&head); err != nil {
			return fmt.Errorf("reading execution session head: %w", err)
		}
		if (a.ResumedFrom == "" && head.Valid) || (a.ResumedFrom != "" && (!head.Valid || head.String != a.ResumedFrom)) {
			return ErrSessionHeadChanged
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, "UPDATE admissions SET state=?, updated_at=? WHERE submission_id=?",
			AdmissionPossible, now, submission); err != nil {
			return fmt.Errorf("committing execution intent: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_heads SET head_job_id=?, updated_at=? WHERE session_id=?",
			a.JobID, now, a.SessionID); err != nil {
			return fmt.Errorf("advancing execution session head: %w", err)
		}
		return nil
	})
}

// AbortPrepared atomically prevents a prepared admission from ever executing.
// A false result means execution already became possible; cancellation must
// then use the live owner and positive cleanup evidence instead.
func (r *Registry) AbortPrepared(ctx context.Context, submission string) (bool, error) {
	return r.abortPrepared(ctx, submission, false, ReasonAdmissionInterrupted)
}

// CancelPrepared prevents revival even if its owner is still initializing.
func (r *Registry) CancelPrepared(ctx context.Context, submission string) (bool, error) {
	return r.abortPrepared(ctx, submission, true, ReasonAdmissionInterrupted)
}

func (r *Registry) abortPrepared(ctx context.Context, submission string, cancelRequested bool, reason string) (bool, error) {
	var aborted bool
	err := r.transaction(ctx, func(conn *sql.Conn) error {
		a, err := scanAdmission(conn.QueryRowContext(ctx,
			"SELECT "+admissionColumns+" FROM admissions WHERE submission_id=?", submission))
		if err != nil {
			return err
		}
		if a.State == AdmissionAborted {
			aborted = true
			return nil
		}
		if a.State != AdmissionPrepared {
			return nil
		}
		_, err = conn.ExecContext(ctx, "UPDATE admissions SET state=?, reason_code=?, cancel_requested=?, updated_at=? WHERE submission_id=?",
			AdmissionAborted, reason, cancelRequested, time.Now().UTC().Format(time.RFC3339Nano), submission)
		if err != nil {
			return fmt.Errorf("aborting prepared admission: %w", err)
		}
		aborted = true
		return nil
	})
	return aborted, err
}

// Finish records publication after the terminal job record is synchronized.
// It does not clear the session head or permit the submission to execute again.
func (r *Registry) Finish(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE admissions SET state=?, updated_at=? WHERE job_id=? AND state=?",
		AdmissionFinished, time.Now().UTC().Format(time.RFC3339Nano), id, AdmissionPossible)
	if err != nil {
		return fmt.Errorf("recording finalized admission: %w", err)
	}
	return nil
}

// SessionHead distinguishes absent authority from a reserved session with no
// execution head. A recorded job ID remains authoritative after its deletion.
func (r *Registry) SessionHead(ctx context.Context, id string) (head string, exists bool, err error) {
	var value sql.NullString
	err = r.db.QueryRowContext(ctx, "SELECT head_job_id FROM session_heads WHERE session_id=?", id).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading session authority: %w", err)
	}
	if value.Valid {
		if err := ValidateID(value.String); err != nil {
			return "", true, fmt.Errorf("corrupt session authority: %w", err)
		}
	}
	return value.String, true, nil
}
