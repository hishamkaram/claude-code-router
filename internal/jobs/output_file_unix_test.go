//go:build linux || darwin

package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOutputReadersRejectFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	go func() { _, _, err := CommitOutput(t.Context(), path, "session", "model"); done <- err }()
	go func() { _, err := ReadCommittedOutput(t.Context(), path, ResultEvidence{}); done <- err }()
	go func() { _, err := StartOutputObserver(t.Context(), path, "session", "model", nil); done <- err }()
	for range 3 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("accepted non-regular output")
			}
		case <-time.After(2 * time.Second):
			// Unblock broken readers so this regression does not leak goroutines.
			fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Fatal("output reader waited for FIFO writer")
		}
	}
}
