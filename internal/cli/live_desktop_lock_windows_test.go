//go:build live && windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockLiveDesktopFile takes a non-blocking exclusive lock on the first byte
// so live desktop CUA tests never share the real desktop across processes.
func tryLockLiveDesktopFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
}

func unlockLiveDesktopFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}
