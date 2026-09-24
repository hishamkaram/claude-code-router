package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func rejectClaudeProfileDestinationSymlink(destination string) error {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting persistent Claude Code profile destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("persistent Claude Code profile destination %s must not be a symlink", destination)
	}
	if !info.IsDir() {
		return fmt.Errorf("persistent Claude Code profile destination %s is not a directory", destination)
	}
	return nil
}

func migrateClaudeProfileDirectory(source, destination string) error {
	if filepath.Clean(source) == filepath.Clean(destination) {
		return nil
	}
	exists, err := validateClaudeProfileMigrationSource(source)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if destinationErr := validateClaudeProfileMigrationDestination(destination); destinationErr != nil {
		return destinationErr
	}
	if parentErr := os.MkdirAll(filepath.Dir(destination), 0o700); parentErr != nil {
		return fmt.Errorf("creating detached Claude profile migration parent: %w", parentErr)
	}
	moved, err := tryRenameClaudeProfile(source, destination)
	if moved {
		return err
	}
	return copyClaudeProfileAcrossStorageRoots(source, destination, err)
}

func validateClaudeProfileMigrationSource(source string) (bool, error) {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting detached Claude profile for migration: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("detached Claude profile migration source must be a directory")
	}
	if !claudeProfileMarkerOwned(source) {
		return false, fmt.Errorf("detached Claude profile %s is not a CCR-owned profile; refusing migration", source)
	}
	return true, nil
}

func validateClaudeProfileMigrationDestination(destination string) error {
	exists, err := inspectClaudeProfileMigrationDestination(destination)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("detached Claude profile migration destination %s already exists", destination)
	}
	return nil
}

func inspectClaudeProfileMigrationDestination(destination string) (bool, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting detached Claude profile migration destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("detached Claude profile migration destination %s is not a CCR-owned directory", destination)
	}
	if !claudeProfileMarkerOwned(destination) {
		return false, fmt.Errorf("detached Claude profile migration destination %s is not CCR-owned", destination)
	}
	return true, nil
}

func tryRenameClaudeProfile(source, destination string) (bool, error) {
	if renameErr := os.Rename(source, destination); renameErr != nil {
		// Cross-device moves and some user storage providers do not support
		// rename. The caller will use the verified copy-and-commit path.
		return false, renameErr
	}
	if chmodErr := os.Chmod(destination, 0o700); chmodErr != nil {
		return true, fmt.Errorf("restricting migrated detached Claude profile: %w", chmodErr)
	}
	return true, nil
}

func copyClaudeProfileAcrossStorageRoots(source, destination string, renameErr error) error {
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".ccr-profile-migration-*")
	if err != nil {
		return fmt.Errorf("creating detached Claude profile migration staging directory: %w", err)
	}
	if copyErr := copyClaudeProfileStateDirectory(source, temporary); copyErr != nil {
		return claudeProfileMigrationStageError("copying detached Claude profile across storage roots", copyErr, temporary, renameErr)
	}
	if chmodErr := os.Chmod(temporary, 0o700); chmodErr != nil {
		return claudeProfileMigrationStageError("restricting staged detached Claude profile", chmodErr, temporary, renameErr)
	}
	if commitErr := os.Rename(temporary, destination); commitErr != nil {
		return claudeProfileMigrationStageError("committing detached Claude profile migration", commitErr, temporary, renameErr)
	}
	return removeMigratedClaudeProfileSource(source, renameErr)
}

func claudeProfileMigrationStageError(message string, cause error, temporary string, renameErr error) error {
	return errors.Join(
		claudeProfileMigrationRenameError(renameErr),
		fmt.Errorf("%s: %w", message, cause),
		cleanupClaudeProfileMigrationStaging(temporary),
	)
}

func claudeProfileMigrationRenameError(renameErr error) error {
	if renameErr == nil {
		return nil
	}
	return fmt.Errorf("renaming detached Claude profile before copy: %w", renameErr)
}

func cleanupClaudeProfileMigrationStaging(temporary string) error {
	if err := os.RemoveAll(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing detached Claude profile migration staging directory: %w", err)
	}
	return nil
}

func removeMigratedClaudeProfileSource(source string, renameErr error) error {
	if removeErr := os.RemoveAll(source); removeErr == nil {
		return nil
	} else {
		return errors.Join(
			claudeProfileMigrationRenameError(renameErr),
			fmt.Errorf("removing migrated detached Claude profile source; committed destination is retained for recovery: %w", removeErr),
		)
	}
}

func copyClaudeProfileStateDirectory(source, destination string) error {
	nativeSource, err := claudeProfileMarkerSource(source)
	if err != nil {
		return fmt.Errorf("reading detached Claude profile asset source: %w", err)
	}
	if mkdirErr := os.MkdirAll(destination, 0o700); mkdirErr != nil {
		return mkdirErr
	}
	if chmodErr := os.Chmod(destination, 0o700); chmodErr != nil {
		return chmodErr
	}
	entries, readErr := os.ReadDir(source)
	if readErr != nil {
		return readErr
	}
	for _, entry := range entries {
		if copyErr := copyClaudeProfileStateEntry(source, destination, entry.Name(), entry.Name(), nativeSource); copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func copyClaudeProfileStateEntry(source, destination, name, relative, nativeSource string) error {
	childSource := filepath.Join(source, name)
	childDestination := filepath.Join(destination, name)
	info, statErr := os.Lstat(childSource)
	if statErr != nil {
		return statErr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !shouldShareClaudeProfileAssetDirectory(relative) {
			return nil
		}
		return copyClaudeManagedProfileAssetLink(childSource, childDestination, relative, nativeSource)
	}
	if info.IsDir() {
		if mkdirErr := os.MkdirAll(childDestination, 0o700); mkdirErr != nil {
			return mkdirErr
		}
		if chmodErr := os.Chmod(childDestination, 0o700); chmodErr != nil {
			return chmodErr
		}
		entries, readErr := os.ReadDir(childSource)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			childRelative := filepath.Join(relative, entry.Name())
			if copyErr := copyClaudeProfileStateEntry(childSource, childDestination, entry.Name(), childRelative, nativeSource); copyErr != nil {
				return copyErr
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("detached Claude profile entry %s is not a regular file", childSource)
	}
	// Session JSONL files can be much larger than the in-memory settings files.
	// Reuse the snapshot-checked streaming copier so a cross-filesystem profile
	// migration has the same bounded-memory and concurrent-write guarantees as a
	// normal profile bootstrap.
	return copyClaudeProfileFileStream(childSource, childDestination, info.Mode())
}

func copyClaudeManagedProfileAssetLink(source, destination, relative, nativeSource string) error {
	linkTarget, err := os.Readlink(source)
	if err != nil {
		return fmt.Errorf("reading detached Claude profile asset link %s: %w", source, err)
	}
	resolvedTarget := linkTarget
	if !filepath.IsAbs(resolvedTarget) {
		resolvedTarget = filepath.Join(filepath.Dir(source), resolvedTarget)
	}
	resolvedTarget, err = filepath.EvalSymlinks(resolvedTarget)
	if err != nil {
		return fmt.Errorf("resolving detached Claude profile asset link %s: %w", source, err)
	}
	expectedTarget := filepath.Join(nativeSource, filepath.FromSlash(filepath.ToSlash(relative)))
	expectedTarget, err = filepath.EvalSymlinks(expectedTarget)
	if err != nil {
		return fmt.Errorf("resolving native Claude profile asset %s: %w", relative, err)
	}
	resolvedInfo, err := os.Stat(resolvedTarget)
	if err != nil {
		return fmt.Errorf("inspecting detached Claude profile asset target %s: %w", resolvedTarget, err)
	}
	expectedInfo, err := os.Stat(expectedTarget)
	if err != nil {
		return fmt.Errorf("inspecting native Claude profile asset target %s: %w", expectedTarget, err)
	}
	if !resolvedInfo.IsDir() || !expectedInfo.IsDir() || !os.SameFile(resolvedInfo, expectedInfo) {
		return fmt.Errorf("detached Claude profile asset link %s does not target its CCR-managed native asset", relative)
	}
	if err := ensureClaudeProfileDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(destination); err != nil {
		return err
	}
	if err := os.Symlink(resolvedTarget, destination); err != nil {
		return fmt.Errorf("recreating detached Claude profile asset link %s: %w", relative, err)
	}
	return nil
}
