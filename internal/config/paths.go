package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const AppName = "claude-code-router"

func DefaultDataDir() (string, error) {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, AppName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config.DefaultDataDir: finding user home: %w", err)
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}

func DefaultDBPath() (string, error) {
	dir, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ccr.db"), nil
}
