package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func claudeProfileRefreshPaths() []string {
	return []string{
		"CLAUDE.md",
		"agents",
		"commands",
		"hooks",
		"plugins",
		"rules",
		"skills",
	}
}

// refreshClaudeProfileAssets keeps the mutable launch profile's immutable
// customization inputs aligned with the native profile. A linked asset sees
// source changes automatically; a copied asset is rebuilt in a sibling staging
// directory and committed only after the complete snapshot succeeds.
func refreshClaudeProfileAssets(source, destination string, options claudeProfileCopyOptions) error {
	for _, relative := range claudeProfileRefreshPaths() {
		if err := refreshClaudeProfileAsset(source, destination, relative, options); err != nil {
			return fmt.Errorf("refreshing Claude Code profile asset %s: %w", relative, err)
		}
	}
	return nil
}

func refreshClaudeProfileAsset(source, destination, relative string, options claudeProfileCopyOptions) error {
	sourcePath := filepath.Join(source, relative)
	destinationPath := filepath.Join(destination, relative)
	if _, err := os.Lstat(sourcePath); errors.Is(err, os.ErrNotExist) {
		return removeClaudeProfileAsset(destinationPath)
	} else if err != nil {
		return fmt.Errorf("inspecting native Claude Code asset: %w", err)
	}

	parent := filepath.Dir(destinationPath)
	if err := ensureClaudeProfileDirectory(parent); err != nil {
		return fmt.Errorf("restricting Claude Code asset destination: %w", err)
	}
	stagingRoot, err := os.MkdirTemp(parent, ".ccr-profile-asset-refresh-*")
	if err != nil {
		return fmt.Errorf("creating Claude Code asset staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()

	stagedPath := filepath.Join(stagingRoot, filepath.Base(destinationPath))
	if err := copyClaudeProfilePathVisitedWithOptions(
		sourcePath,
		stagedPath,
		relative,
		make(map[string]struct{}),
		options,
	); err != nil {
		return fmt.Errorf("copying native Claude Code asset snapshot: %w", err)
	}
	if _, err := os.Lstat(stagedPath); errors.Is(err, os.ErrNotExist) {
		return removeClaudeProfileAsset(destinationPath)
	} else if err != nil {
		return fmt.Errorf("checking staged Claude Code asset: %w", err)
	}
	return commitClaudeProfileAsset(stagedPath, destinationPath)
}

func removeClaudeProfileAsset(path string) error {
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale Claude Code profile asset: %w", err)
	}
	return nil
}

// commitClaudeProfileAsset replaces the owned destination while retaining a
// same-directory backup long enough to restore it if the staged rename fails.
// The backup keeps a transient refresh failure from destroying a resumable
// profile, while the final rename avoids exposing a partially copied tree.
func commitClaudeProfileAsset(staged, destination string) error {
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	backup, existed, err := moveClaudeProfileAssetDestinationAside(destination)
	if err != nil {
		return err
	}
	if err := os.Rename(staged, destination); err != nil {
		if existed {
			if restoreErr := os.Rename(backup, destination); restoreErr != nil {
				return errors.Join(
					fmt.Errorf("committing refreshed Claude Code profile asset: %w", err),
					fmt.Errorf("restoring previous Claude Code profile asset: %w", restoreErr),
				)
			}
		}
		return fmt.Errorf("committing refreshed Claude Code profile asset: %w", err)
	}
	if existed {
		if err := os.RemoveAll(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing replaced Claude Code profile asset backup: %w", err)
		}
	}
	return nil
}

func moveClaudeProfileAssetDestinationAside(destination string) (backup string, existed bool, err error) {
	if _, statErr := os.Lstat(destination); errors.Is(statErr, os.ErrNotExist) {
		return "", false, nil
	} else if statErr != nil {
		return "", false, fmt.Errorf("inspecting existing Claude Code profile asset: %w", statErr)
	}
	backup, err = temporaryClaudeProfileAssetPath(filepath.Dir(destination))
	if err != nil {
		return "", false, err
	}
	if err := os.Rename(destination, backup); err != nil {
		_ = os.RemoveAll(backup)
		return "", false, fmt.Errorf("staging existing Claude Code profile asset: %w", err)
	}
	return backup, true, nil
}

func temporaryClaudeProfileAssetPath(parent string) (string, error) {
	temporary, err := os.MkdirTemp(parent, ".ccr-profile-asset-backup-*")
	if err != nil {
		return "", fmt.Errorf("creating Claude Code profile asset backup path: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		return "", fmt.Errorf("preparing Claude Code profile asset backup path: %w", err)
	}
	return temporary, nil
}
