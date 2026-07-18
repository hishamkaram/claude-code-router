package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writeAtomicFile(path string, data []byte, force bool) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".ccr-profile-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if force {
		if err := os.Rename(tempPath, path); err != nil {
			return err
		}
		return nil
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("file already exists (use --force to replace it)")
		}
		return err
	}
	return nil
}
