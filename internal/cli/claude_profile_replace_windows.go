//go:build windows

package cli

import "golang.org/x/sys/windows"

func replaceClaudeProfileFile(temporary, destination string) error {
	// MoveFileEx with replacement preserves the committed destination when the
	// move cannot be completed. A remove-then-rename fallback would turn a
	// transient sharing or permission error into permanent profile loss.
	return windows.Rename(temporary, destination)
}
