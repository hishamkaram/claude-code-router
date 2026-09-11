//go:build linux || darwin

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openJobOutput(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("job output must be a regular file")
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("resetting prepared job output: %w", err)
	}
	return file, nil
}
