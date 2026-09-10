package jobs

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func observeChildExit(ctx context.Context, pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("opening child exit observer: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	change := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	_, err = unix.Kevent(fd, change, nil, nil)
	// Only this owner may reap the child. ESRCH here means it already exited.
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registering child exit observer: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		events := make([]unix.Kevent_t, 1)
		timeout := unix.Timespec{Nsec: 100_000_000}
		n, err := unix.Kevent(fd, nil, events, &timeout)
		if err == nil && n == 0 {
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for child exit: %w", err)
		}
		return nil
	}
}
