package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Register the repository's existing SQLite driver.
)

// Admission is a permanent submission binding. It contains digests and public
// identities only; prompts and credentials never enter the registry.
type Admission struct {
	SubmissionID           string `json:"submission_id"`
	RequestDigest          string `json:"-"`
	ExecutionDigest        string `json:"-"`
	JobID                  string `json:"job_id"`
	SessionID              string `json:"session_id"`
	RequestedResumeSession string `json:"requested_resume_session,omitempty"`
	ExpectedParentJob      string `json:"expected_parent_job,omitempty"`
	ResumedFrom            string `json:"resumed_from,omitempty"`
	ResumedFromStatus      string `json:"resumed_from_status,omitempty"`
	State                  string `json:"admission_state"`
	ReasonCode             string `json:"reason_code,omitempty"`
	CancelRequested        bool   `json:"cancel_requested"`
}

const (
	AdmissionPrepared = "prepared"
	AdmissionPossible = "execution_possible"
	AdmissionFinished = "finished"
	AdmissionAborted  = "aborted"
)

var (
	ErrSubmissionConflict = errors.New("submission ID is already bound to a different request")
	ErrAdmissionNotFound  = errors.New("submission admission not found")
	ErrSessionHeadChanged = errors.New("session head changed")
	ErrAdmissionClosed    = errors.New("admission can no longer start execution")
)

// Registry serializes short SQLite transactions. Process leases must already
// be held by callers; no transaction may wait for a process lease.
type Registry struct {
	db   *sql.DB
	root string
}

// OpenRegistry creates private, fully synchronized admission bookkeeping in
// the canonical job root. Bindings and heads outlive removable job artifacts.
func OpenRegistry(ctx context.Context, root string) (*Registry, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating admission root: %w", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing admission root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving admission root: %w", err)
	}
	path := filepath.Join(root, "admissions.sqlite")
	established, err := checkRegistryIdentity(root, path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening private admission registry: %w", err)
	}
	if modeErr := f.Chmod(0o600); modeErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("securing admission registry: %w", modeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		return nil, fmt.Errorf("closing admission registry file: %w", closeErr)
	}
	databaseURL := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := databaseURL.Query()
	query.Set("_pragma", "busy_timeout(5000)")
	databaseURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return nil, fmt.Errorf("opening admission database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	r := &Registry{db: db, root: root}
	if err := r.initialize(ctx, established); err != nil {
		_ = db.Close()
		return nil, err
	}
	if !established {
		if identityErr := establishRegistryIdentity(root); identityErr != nil {
			_ = db.Close()
			return nil, identityErr
		}
	}
	return r, nil
}

func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) initialize(ctx context.Context, established bool) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON",
	} {
		if _, err := r.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configuring admission database: %w", err)
		}
	}
	return r.transaction(ctx, func(conn *sql.Conn) error {
		var version int
		if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("reading admission schema: %w", err)
		}
		if established && version != 1 {
			return fmt.Errorf("established admission schema is missing or corrupt: %d", version)
		}
		if version != 0 && version != 1 {
			return fmt.Errorf("unsupported admission schema %d", version)
		}
		if version == 1 {
			return nil
		}
		_, err := conn.ExecContext(ctx, admissionSchema)
		if err != nil {
			return fmt.Errorf("creating admission schema: %w", err)
		}
		return nil
	})
}

const admissionSchema = `
CREATE TABLE admissions (
 submission_id TEXT PRIMARY KEY NOT NULL,
 request_digest TEXT NOT NULL,
 execution_digest TEXT NOT NULL,
 job_id TEXT UNIQUE NOT NULL,
 session_id TEXT NOT NULL,
 requested_resume_session TEXT NOT NULL,
 expected_parent_job TEXT NOT NULL,
 resumed_from TEXT NOT NULL,
 resumed_from_status TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','execution_possible','finished','aborted')),
 reason_code TEXT NOT NULL DEFAULT '',
 cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0,1)),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE TABLE session_heads (
 session_id TEXT PRIMARY KEY NOT NULL,
 head_job_id TEXT,
 updated_at TEXT NOT NULL
);
PRAGMA user_version=1;`

func (r *Registry) transaction(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring admission connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("beginning admission transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, _ = conn.ExecContext(rollbackCtx, "ROLLBACK")
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("committing admission transaction: %w", err)
	}
	return nil
}

const admissionColumns = `submission_id, request_digest, execution_digest, job_id,
 session_id, requested_resume_session, expected_parent_job, resumed_from,
 resumed_from_status, state, reason_code, cancel_requested`

func scanAdmission(row *sql.Row) (Admission, error) {
	var a Admission
	err := row.Scan(&a.SubmissionID, &a.RequestDigest, &a.ExecutionDigest, &a.JobID,
		&a.SessionID, &a.RequestedResumeSession, &a.ExpectedParentJob, &a.ResumedFrom,
		&a.ResumedFromStatus, &a.State, &a.ReasonCode, &a.CancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return Admission{}, ErrAdmissionNotFound
	}
	if err != nil {
		return Admission{}, fmt.Errorf("reading admission binding: %w", err)
	}
	return a, nil
}

// Lookup never starts work, repairs artifacts, or consults current route configuration.
func (r *Registry) Lookup(ctx context.Context, submission string) (Admission, error) {
	return scanAdmission(r.db.QueryRowContext(ctx,
		"SELECT "+admissionColumns+" FROM admissions WHERE submission_id=?", submission))
}

func (r *Registry) LookupJob(ctx context.Context, id string) (Admission, error) {
	return scanAdmission(r.db.QueryRowContext(ctx,
		"SELECT "+admissionColumns+" FROM admissions WHERE job_id=?", id))
}
