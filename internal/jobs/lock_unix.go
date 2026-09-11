//go:build unix

package jobs

import (
	"context"
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
	return s.StatusContext(context.Background(), id)
}

func (s Store) StatusContext(ctx context.Context, id string) (Record, error) {
	r, err := s.Read(id)
	if errors.Is(err, os.ErrNotExist) {
		if _, registryErr := os.Stat(filepath.Join(s.Root, "admissions.sqlite")); registryErr == nil {
			registry, openErr := OpenRegistry(ctx, s.Root)
			if openErr != nil {
				return Record{}, openErr
			}
			defer func() { _ = registry.Close() }()
			result, lookupErr := registry.JobStatus(ctx, id)
			if !errors.Is(lookupErr, ErrAdmissionNotFound) {
				return result, lookupErr
			}
		}
	}
	if err == nil && r.SchemaVersion == 2 {
		registry, openErr := OpenRegistry(ctx, s.Root)
		if openErr != nil {
			return Record{}, openErr
		}
		defer func() { _ = registry.Close() }()
		return registry.JobStatus(ctx, id)
	}
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
