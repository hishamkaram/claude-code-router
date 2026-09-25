//go:build live && !windows

package cli

import (
	"os"
	"syscall"
)

// tryLockLiveDesktopFile takes a non-blocking exclusive advisory lock so live
// desktop CUA tests never share the real desktop across processes.
func tryLockLiveDesktopFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockLiveDesktopFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
