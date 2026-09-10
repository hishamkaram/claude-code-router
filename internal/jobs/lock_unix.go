//go:build unix

package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lock is the ownership lease. An unlocked record never grants process authority.
func (s Store) Lock(id string) (*os.File, bool, error) {
	dir, err := s.Directory(id)
	if err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("opening job ownership lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("locking job owner: %w", err)
	}
	return f, true, nil
}

func (s Store) Status(id string) (Record, error) {
	r, err := s.Read(id)
	if err != nil || r.Terminal() {
		return r, err
	}
	lock, acquired, err := s.Lock(id)
	if err != nil || !acquired {
		return r, err
	}
	defer func() { _ = lock.Close() }()
	r, err = s.Read(id)
	if err != nil || r.Terminal() {
		return r, err
	}
	r.Status, r.Reason = "failed", "job owner was lost; workload was not replayed"
	r.Cleanup = Cleanup{Coverage: "unknown", Reason: "owner lost; no cancellation authority recovered from metadata"}
	return r, s.Write(r)
}
