package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func copyClaudeProfileDirectoryWithOptions(source, destination, relative string, visited map[string]struct{}, options claudeProfileCopyOptions) error {
	if err := ensureClaudeProfileDirectory(destination); err != nil {
		return fmt.Errorf("creating private Claude Code profile directory %s: %w", destination, err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading Claude Code profile directory %s: %w", source, err)
	}
	for _, entry := range entries {
		childRelative := entry.Name()
		if relative != "" {
			childRelative = filepath.Join(relative, entry.Name())
		}
		if err := copyClaudeProfilePathVisitedWithOptions(
			filepath.Join(source, entry.Name()),
			filepath.Join(destination, entry.Name()),
			childRelative,
			visited,
			options,
		); err != nil {
			return err
		}
	}
	return nil
}
