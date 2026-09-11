//go:build linux || darwin

package jobs

import (
	"os"

	"golang.org/x/sys/unix"
)

// Nonblocking open rejects substituted FIFOs without waiting for a writer.
// Validate the opened descriptor, avoiding a stat/open replacement race.
func openOutputFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &OutputError{ReasonCode: ReasonObservationFailed}
	}
	return f, nil
}
