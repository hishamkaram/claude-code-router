package jobs

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func observeChildExit(ctx context.Context, pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("opening child exit observer: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for child exit event: %w", err)
		}
		var info unix.Siginfo
		err = unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("observing owned child exit: %w", err)
		}
		return nil
	}
}
