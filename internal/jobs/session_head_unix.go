//go:build unix

package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionStatus resolves the authoritative head without starting work. Legacy
// registration uses the same nonblocking lease as admission; established heads
// can be observed while their owner holds that lease.
func (r *Registry) SessionStatus(ctx context.Context, sid string) (Record, error) {
	head, exists, err := r.SessionHead(ctx, sid)
	if err != nil {
		return Record{}, err
	}
	if !exists {
		head, err = r.registerLegacySession(ctx, sid)
		if err != nil {
			return Record{}, err
		}
	}
	if head == "" {
		return Record{}, fmt.Errorf("session has no execution head")
	}
	record, err := (Store{Root: r.root}).StatusContext(ctx, head)
	if err != nil {
		return Record{}, fmt.Errorf("reading authoritative session head: %w", err)
	}
	if record.SessionID != sid {
		return Record{}, fmt.Errorf("session head identity mismatch")
	}
	return record, nil
}

func (r *Registry) registerLegacySession(ctx context.Context, sid string) (string, error) {
	lease, err := r.LockSession(sid)
	if err != nil {
		return "", err
	}
	defer func() { _ = lease.Close() }()
	head, exists, err := r.SessionHead(ctx, sid)
	if err != nil || exists {
		return head, err
	}
	return r.bootstrapSession(ctx, lease)
}

// ResumeHead resolves only registered CCR sessions. Existing authority always
// wins, including missing-artifact tombstones; no older ancestor is selected.
func (r *Registry) ResumeHead(ctx context.Context, lease *SessionLease, expected string) (Record, error) {
	if lease == nil {
		return Record{}, fmt.Errorf("resume requires a session lease")
	}
	if err := r.validateLease(lease, lease.sessionID); err != nil {
		return Record{}, err
	}
	head, exists, err := r.SessionHead(ctx, lease.sessionID)
	if err != nil {
		return Record{}, err
	}
	if !exists {
		head, err = r.bootstrapSession(ctx, lease)
		if err != nil {
			return Record{}, err
		}
	}
	if head == "" {
		return Record{}, fmt.Errorf("session has no execution head")
	}
	if expected != "" && expected != head {
		return Record{}, ErrSessionHeadChanged
	}
	predecessor, err := (Store{Root: r.root}).StatusContext(ctx, head)
	if err != nil {
		return Record{}, fmt.Errorf("reading authoritative session head: %w", err)
	}
	if predecessor.SessionID != lease.sessionID {
		return Record{}, fmt.Errorf("session head identity mismatch")
	}
	if !predecessor.Stopped() {
		return Record{}, fmt.Errorf("session head lacks positive stopped evidence")
	}
	return predecessor, nil
}

func (r *Registry) bootstrapSession(ctx context.Context, lease *SessionLease) (string, error) {
	var bindings int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM admissions WHERE session_id=?", lease.sessionID).Scan(&bindings); err != nil {
		return "", fmt.Errorf("checking established session bindings: %w", err)
	}
	if bindings != 0 {
		return "", fmt.Errorf("established session authority is missing")
	}
	head, err := legacySessionCandidate(Store{Root: r.root}, lease.sessionID)
	if err != nil {
		return "", err
	}
	err = r.transaction(ctx, func(conn *sql.Conn) error {
		_, writeErr := conn.ExecContext(ctx, "INSERT INTO session_heads(session_id, head_job_id, updated_at) VALUES (?,?,?)",
			lease.sessionID, head, time.Now().UTC().Format(time.RFC3339Nano))
		if writeErr != nil {
			return fmt.Errorf("bootstrapping legacy session authority: %w", writeErr)
		}
		return nil
	})
	return head, err
}

func legacySessionCandidate(s Store, sid string) (string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return "", fmt.Errorf("reading legacy session candidates: %w", err)
	}
	var head string
	for _, entry := range entries {
		if !entry.IsDir() || ValidateID(entry.Name()) != nil {
			continue
		}
		r, err := s.Read(entry.Name())
		if err != nil {
			return "", fmt.Errorf("unreadable legacy candidate %s: %w", filepath.Base(entry.Name()), err)
		}
		if r.SessionID != sid {
			continue
		}
		if r.SchemaVersion != 1 || head != "" {
			return "", fmt.Errorf("session authority is missing or legacy candidates are ambiguous")
		}
		head = r.JobID
	}
	if head == "" {
		return "", fmt.Errorf("unknown session: only detached sessions registered in this job store can resume")
	}
	return head, nil
}
