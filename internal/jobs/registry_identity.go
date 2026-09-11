package jobs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const registryIdentity = "ccr-admissions/1\n"

// The establishment marker is not an admission journal. It only prevents a
// missing database from being silently recreated and resurrecting old heads.
// No admission can be returned until the initialized database and marker are
// durable. All submission and head transactions remain exclusively in SQLite.
func checkRegistryIdentity(root, database string) (bool, error) {
	info, statErr := os.Lstat(database)
	if statErr != nil && !os.IsNotExist(statErr) {
		return false, fmt.Errorf("checking admission registry: %w", statErr)
	}
	if statErr == nil && !info.Mode().IsRegular() {
		return false, fmt.Errorf("admission registry is not a regular file")
	}
	marker, err := openOutputFile(filepath.Join(root, "admissions.identity"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading registry identity: %w", err)
	}
	defer func() { _ = marker.Close() }()
	data, err := io.ReadAll(io.LimitReader(marker, 64))
	if err != nil || string(data) != registryIdentity {
		return false, fmt.Errorf("corrupt admission registry identity")
	}
	info, err = os.Lstat(database)
	if err != nil {
		return false, fmt.Errorf("established admission registry is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("established admission registry is not a regular file")
	}
	return true, nil
}

func establishRegistryIdentity(root string) error {
	file, err := os.CreateTemp(root, ".registry-identity-*")
	if err != nil {
		return fmt.Errorf("creating registry identity: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.WriteString(registryIdentity); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing registry identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("synchronizing registry identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing registry identity: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(root, "admissions.identity")); err != nil {
		return fmt.Errorf("publishing registry identity: %w", err)
	}
	return syncDirectory(root)
}

// Synchronize every ancestor even when it already exists: it may have been
// created by an interrupted or concurrent opener which never reached its
// durability barrier. No registry transaction or execution can precede this.
func prepareRegistryRoot(root string, syncParent func(string) error) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating admission root: %w", err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalizing admission root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving admission root: %w", err)
	}
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		if err := syncParent(parent); err != nil {
			return "", fmt.Errorf("synchronizing admission ancestor %s: %w", parent, err)
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return root, nil
}
