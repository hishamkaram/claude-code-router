//go:build !windows

package cli

import "os"

func replaceClaudeProfileFile(temporary, destination string) error {
	return os.Rename(temporary, destination)
}
