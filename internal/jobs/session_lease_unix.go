//go:build unix

package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

var ErrSessionBusy = errors.New("session is owned by an active CCR job")

// SessionLease coordinates CCR owners within one canonical registry. Its file
// may be inherited by the detached owner, but never by the workload itself.
// Closing one inherited descriptor must not explicitly unlock the shared lease.
type SessionLease struct {
	file      *os.File
	root      string
	sessionID string
}

func ValidateSessionID(id string) error {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id || u.Version() != 4 {
		return fmt.Errorf("invalid session ID: expected a canonical version-4 UUID")
	}
	return nil
}

func (r *Registry) LockSession(id string) (*SessionLease, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	dir := filepath.Join(r.root, "session-leases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating session lease directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening session lease: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrSessionBusy
		}
		return nil, fmt.Errorf("acquiring session lease: %w", err)
	}
	return &SessionLease{file: f, root: r.root, sessionID: id}, nil
}

func (l *SessionLease) File() *os.File { return l.file }
func (l *SessionLease) Close() error   { return l.file.Close() }

func (r *Registry) validateLease(l *SessionLease, id string) error {
	if l == nil || l.file == nil || l.root != r.root || l.sessionID != id {
		return fmt.Errorf("session lease does not match admission namespace and identity")
	}
	actual, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("checking session lease: %w", err)
	}
	expected, err := os.Stat(filepath.Join(r.root, "session-leases", id+".lock"))
	if err != nil || !os.SameFile(actual, expected) {
		return fmt.Errorf("session lease file identity changed")
	}
	return nil
}

// AdoptSessionLease validates an inherited lease without releasing it. The
// caller transfers descriptor ownership even when validation fails.
func (r *Registry) AdoptSessionLease(f *os.File, id string) (*SessionLease, error) {
	l := &SessionLease{file: f, root: r.root, sessionID: id}
	if err := ValidateSessionID(id); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := r.validateLease(l, id); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("adopting session lease: %w", err)
	}
	unix.CloseOnExec(int(f.Fd()))
	return l, nil
}
