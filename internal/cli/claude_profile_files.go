package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var errClaudeProfileAssetLinkUnavailable = errors.New("claude Code profile asset links are unavailable")

func copyClaudeProfileFile(source, destination string, mode fs.FileMode) error {
	data, err := readClaudeProfileFile(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading Claude Code profile file %s: %w", source, err)
	}
	if err := writeClaudeProfileFile(destination, data, mode); err != nil {
		return fmt.Errorf("writing private Claude Code profile file %s: %w", destination, err)
	}
	return nil
}

func shareClaudeProfileDirectory(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("claude Code profile asset %s is not a regular directory", source)
	}
	if err := validateClaudeProfileAssetTree(source); err != nil {
		return err
	}
	if err := ensureClaudeProfileDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	if adopted, adoptErr := adoptClaudeProfileAssetLink(source, destination); adoptErr != nil {
		return adoptErr
	} else if adopted {
		return nil
	}
	if err := rejectClaudeProfilePathSymlinks(destination); err != nil {
		return err
	}
	if err := os.Symlink(source, destination); err != nil {
		return fmt.Errorf("%w: linking %s to %s: %w", errClaudeProfileAssetLinkUnavailable, source, destination, err)
	}
	return nil
}

// validateClaudeProfileAssetTree keeps a directory link limited to the same
// immutable tree that CCR inspected. A nested link would otherwise bypass the
// copy walk's safe omission and expose an arbitrary native path to Claude Code.
func validateClaudeProfileAssetTree(source string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("reading claude Code profile asset %s: %w", source, err)
	}
	for _, entry := range entries {
		path := filepath.Join(source, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspecting claude Code profile asset %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: nested link %s requires isolated copy", errClaudeProfileAssetLinkUnavailable, path)
		}
		if info.IsDir() {
			if err := validateClaudeProfileAssetTree(path); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("claude Code profile asset %s is not a regular file", path)
		}
	}
	return nil
}

func adoptClaudeProfileAssetLink(source, destination string) (bool, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting claude Code profile asset destination %s: %w", destination, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false, fmt.Errorf("inspecting claude Code profile asset %s: %w", source, err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		return false, fmt.Errorf("resolving claude Code profile asset destination %s: %w", destination, err)
	}
	if os.SameFile(sourceInfo, destinationInfo) {
		// A process may have stopped after publishing an immutable asset link but
		// before writing the ready marker. The link is already the exact intended
		// state; adopt it and let the enclosing initialization publish readiness.
		return true, nil
	}
	return false, fmt.Errorf("claude Code profile asset destination %s points outside the native profile asset", destination)
}

// ensureClaudeProfileDirectory creates a destination directory only through
// non-symlink path components. CLAUDE_CONFIG_DIR is user-configurable and a
// persistent profile can be reused after another same-user process has
// modified its contents, so following an existing destination symlink would
// break profile isolation.
func ensureClaudeProfileDirectory(path string) error {
	if err := rejectClaudeProfilePathSymlinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("claude Code profile destination %s is not a directory", path)
	}
	return os.Chmod(path, 0o700)
}

// rejectClaudeProfilePathSymlinks checks every existing component, including
// the final one. It intentionally stops at the first missing component; the
// caller can create that suffix and validate it again afterwards.
func rejectClaudeProfilePathSymlinks(path string) error {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("resolving Claude Code profile destination %s: %w", path, err)
	}
	volume := filepath.VolumeName(absPath)
	remainder := strings.TrimPrefix(absPath, volume)
	current := volume
	if strings.HasPrefix(remainder, string(os.PathSeparator)) {
		current += string(os.PathSeparator)
		remainder = strings.TrimPrefix(remainder, string(os.PathSeparator))
	}
	for _, component := range strings.Split(remainder, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("inspecting Claude Code profile destination %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("claude Code profile destination %s must not contain symlink %s", path, current)
		}
		if !info.IsDir() && current != absPath {
			return fmt.Errorf("claude Code profile destination component %s is not a directory", current)
		}
	}
	return nil
}

// writeClaudeProfileFile writes through a private temporary file and commits
// with rename. Rename never follows the final destination symlink, while the
// component checks prevent an existing nested directory symlink from redirecting
// the temporary file into another tree.
func writeClaudeProfileFile(destination string, data []byte, mode fs.FileMode) (retErr error) {
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(destination); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".ccr-profile-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if cleanupErr := os.Remove(temporaryPath); retErr == nil && cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			retErr = cleanupErr
		}
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := replaceClaudeProfileFile(temporaryPath, destination); err != nil {
		return err
	}
	return nil
}

// copyClaudeProfileFileStream copies a large profile file without retaining its
// contents in memory. It keeps the same identity checks as readClaudeProfileFile
// so a Claude Code session that is replaced during the snapshot cannot be
// silently copied from the wrong file.
func copyClaudeProfileFileStream(source, destination string, mode fs.FileMode) (retErr error) {
	input, before, sourceErr := openClaudeProfileStreamSource(source)
	if sourceErr != nil {
		return sourceErr
	}
	defer func() {
		if closeErr := input.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(destination); err != nil {
		return err
	}
	temporary, createErr := os.CreateTemp(filepath.Dir(destination), ".ccr-profile-write-*")
	if createErr != nil {
		return createErr
	}
	temporaryPath := temporary.Name()
	defer func() {
		if cleanupErr := os.Remove(temporaryPath); retErr == nil && cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			retErr = cleanupErr
		}
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := copyClaudeProfileStreamContents(input, temporary, source, before); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	return replaceClaudeProfileFile(temporaryPath, destination)
}

func openClaudeProfileStreamSource(source string) (*os.File, fs.FileInfo, error) {
	before, err := os.Lstat(source)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("claude Code profile entry %s is not a regular file", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return nil, nil, err
	}
	opened, statErr := input.Stat()
	if statErr != nil {
		_ = input.Close()
		return nil, nil, fmt.Errorf("inspecting opened Claude Code profile file %s: %w", source, statErr)
	}
	if !os.SameFile(before, opened) {
		_ = input.Close()
		return nil, nil, fmt.Errorf("claude Code profile file %s changed while being copied", source)
	}
	return input, before, nil
}

func copyClaudeProfileStreamContents(input, temporary *os.File, source string, before fs.FileInfo) error {
	if _, err := io.Copy(temporary, input); err != nil {
		return err
	}
	inputAfter, err := input.Stat()
	if err != nil {
		return fmt.Errorf("inspecting opened Claude Code profile file %s after copying: %w", source, err)
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !sameClaudeProfileFileSnapshot(before, inputAfter) || !sameClaudeProfileFileSnapshot(before, after) {
		return fmt.Errorf("claude Code profile file %s changed while being copied", source)
	}
	return nil
}

func sameClaudeProfileFileSnapshot(before, after fs.FileInfo) bool {
	if before == nil || after == nil || !os.SameFile(before, after) {
		return false
	}
	return before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) && before.Mode().Perm() == after.Mode().Perm()
}

func readClaudeProfileFile(source string) ([]byte, error) {
	before, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("claude Code profile entry %s is not a regular file", source)
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting opened Claude Code profile file %s: %w", source, err)
	}
	after, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, fmt.Errorf("claude Code profile file %s changed while being copied", source)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return data, nil
}
