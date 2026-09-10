package jobs

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// EnableSubreaper is only called in the dedicated owner, never in the caller CLI.
func EnableSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enabling orphan reaping: %w", err)
	}
	return nil
}

// ReapOrphans runs after explicit child waits; it never races another reaper.
func ReapOrphans() error {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) || (err == nil && pid == 0) {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reaping adopted child: %w", err)
		}
	}
}
