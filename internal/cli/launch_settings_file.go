package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writePrivateLaunchSettings(settings string) (path string, cleanup func() error, resultErr error) {
	if settings == "" {
		return "", func() error { return nil }, nil
	}
	directory, err := os.MkdirTemp("", "ccr-claude-settings-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating private Claude Code settings directory: %w", err)
	}
	cleanup = func() error {
		if err := os.RemoveAll(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing private Claude Code settings: %w", err)
		}
		return nil
	}
	path = filepath.Join(directory, "settings.json")
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		return "", nil, errors.Join(
			fmt.Errorf("writing private Claude Code settings: %w", err),
			cleanup(),
		)
	}
	return path, cleanup, nil
}
