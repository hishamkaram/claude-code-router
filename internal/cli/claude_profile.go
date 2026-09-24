package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type claudeLaunchProfile struct {
	configDir    string
	cleanup      func() error
	degradations []string
}

type claudeProfileCopyOptions struct {
	preserveAnthropicAPIKey bool
	providerSecretEnvNames  []string
	degradations            *[]string
	sourceDir               string
	storageRoot             string
}

func prepareClaudeLaunchProfile() (claudeLaunchProfile, error) {
	return prepareClaudeLaunchProfileAt("")
}

func prepareClaudeLaunchProfileAt(destination string) (claudeLaunchProfile, error) {
	return prepareClaudeLaunchProfileAtForSession(destination, "")
}

func prepareClaudeLaunchProfileAtForSession(destination, sessionID string) (claudeLaunchProfile, error) {
	return prepareClaudeLaunchProfileAtForSessionWithOptions(destination, sessionID, claudeProfileCopyOptions{})
}

func prepareClaudeLaunchProfileAtForSessionWithOptions(destination, sessionID string, options claudeProfileCopyOptions) (claudeLaunchProfile, error) {
	if options.degradations == nil {
		degradations := []string(nil)
		options.degradations = &degradations
	}
	source, err := claudeConfigDirForOverride(options.sourceDir)
	if err != nil {
		return claudeLaunchProfile{}, err
	}
	directory, persistent, created, err := claudeProfileDestinationWithStorageRoot(source, destination, options.storageRoot)
	if err != nil {
		return claudeLaunchProfile{}, err
	}
	cleanup := claudeProfileCleanup(directory, persistent)
	cleanupFailedInitialization := claudeProfileInitializationCleanup(directory, !persistent || created)
	collectClaudeProfileDegradations(source, options.degradations)
	reused, err := reconcileClaudeProfileMarker(source, directory, persistent, created, options)
	if err != nil {
		return claudeLaunchProfile{}, err
	}
	if reused {
		return claudeLaunchProfile{configDir: directory, cleanup: cleanup, degradations: copyClaudeProfileDegradations(options.degradations)}, nil
	}
	if err := ensureClaudeProfileDirectory(directory); err != nil {
		return claudeLaunchProfile{}, errors.Join(
			fmt.Errorf("restricting private Claude Code profile: %w", err),
			cleanupFailedInitialization(),
		)
	}
	if err := populateClaudeLaunchProfile(source, directory, sessionID, options); err != nil {
		return claudeLaunchProfile{}, errors.Join(err, cleanupFailedInitialization())
	}
	return claudeLaunchProfile{configDir: directory, cleanup: cleanup, degradations: copyClaudeProfileDegradations(options.degradations)}, nil
}

func reconcileClaudeProfileMarker(source, directory string, persistent, created bool, options claudeProfileCopyOptions) (bool, error) {
	marker := filepath.Join(directory, ".ccr-profile-ready")
	markerErr := checkClaudeProfileMarker(marker, source)
	if markerErr == nil {
		return false, nil
	}
	if errors.Is(markerErr, os.ErrNotExist) {
		return reconcileMissingClaudeProfileMarker(source, directory, persistent, created)
	}
	if !errors.Is(markerErr, errClaudeProfileSourceChanged) {
		return false, fmt.Errorf("checking private Claude Code profile readiness: %w", markerErr)
	}
	if persistent {
		return true, refreshPersistentClaudeProfileAfterSourceChange(source, directory, options)
	}
	return false, replaceChangedClaudeProfile(directory)
}

func reconcileMissingClaudeProfileMarker(source, directory string, persistent, created bool) (bool, error) {
	if !persistent || created {
		return false, nil
	}
	if claudeProfileInitializationMarkerOwned(directory) {
		if err := resetInterruptedClaudeProfile(directory); err != nil {
			return false, err
		}
		return false, nil
	}
	adoptable, err := claudeProfileContainsOnlyOwnedAssetLinks(source, directory)
	if err != nil {
		return false, err
	}
	if adoptable {
		return false, nil
	}
	empty, err := claudeProfileDirectoryEmpty(directory)
	if err != nil {
		return false, err
	}
	if empty {
		return false, nil
	}
	return false, fmt.Errorf("persistent Claude Code profile %s has no CCR readiness marker; refusing reuse", directory)
}

func replaceChangedClaudeProfile(directory string) error {
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("replacing private Claude Code profile after native source change: %w", err)
	}
	if err := ensureClaudeProfileDirectory(directory); err != nil {
		return fmt.Errorf("recreating private Claude Code profile after native source change: %w", err)
	}
	return nil
}

func claudeProfileDirectoryEmpty(directory string) (bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, fmt.Errorf("inspecting incomplete Claude Code profile %s: %w", directory, err)
	}
	return len(entries) == 0, nil
}

func claudeProfileContainsOnlyOwnedAssetLinks(source, directory string) (bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, fmt.Errorf("inspecting interrupted Claude Code profile %s: %w", directory, err)
	}
	for _, entry := range entries {
		relative := filepath.ToSlash(entry.Name())
		if !shouldShareClaudeProfileAssetDirectory(relative) || entry.Type()&os.ModeSymlink == 0 {
			return false, nil
		}
		owned, err := claudeProfileAssetLinkMatches(source, directory, relative)
		if err != nil {
			return false, err
		}
		if !owned {
			return false, nil
		}
	}
	return true, nil
}

func claudeProfileAssetLinkMatches(source, directory, relative string) (bool, error) {
	sourcePath := filepath.Join(source, filepath.FromSlash(relative))
	destinationPath := filepath.Join(directory, filepath.FromSlash(relative))
	if err := validateClaudeProfileAssetTree(sourcePath); err != nil {
		if errors.Is(err, errClaudeProfileAssetLinkUnavailable) {
			return false, nil
		}
		return false, err
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting native Claude Code asset %s: %w", relative, err)
	}
	destinationInfo, err := os.Stat(destinationPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("resolving interrupted Claude Code asset %s: %w", relative, err)
	}
	return sourceInfo.IsDir() && destinationInfo.IsDir() && os.SameFile(sourceInfo, destinationInfo), nil
}

func resetInterruptedClaudeProfile(directory string) error {
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("clearing interrupted Claude Code profile %s: %w", directory, err)
	}
	if err := ensureClaudeProfileDirectory(directory); err != nil {
		return fmt.Errorf("recreating interrupted Claude Code profile %s: %w", directory, err)
	}
	return nil
}

func refreshPersistentClaudeProfileAfterSourceChange(source, directory string, options claudeProfileCopyOptions) error {
	if !claudeProfileMarkerOwned(directory) {
		return fmt.Errorf("persistent Claude Code profile %s is not CCR-owned; refusing reuse", directory)
	}
	// A persistent CCR profile owns its conversation history. Its native source
	// is only the bootstrap template used at first creation; a later change to
	// CLAUDE_CONFIG_DIR must never replace the profile that a detached --resume
	// continuation depends on.
	if err := ensureClaudeProfileDirectory(directory); err != nil {
		return fmt.Errorf("restricting persistent Claude Code profile after native source change: %w", err)
	}
	return refreshClaudeLaunchProfileOptions(source, directory, options)
}

func claudeProfileDestinationWithStorageRoot(source, destination, storageRoot string) (directory string, persistent, created bool, err error) {
	if strings.TrimSpace(destination) == "" {
		directory, err = createClaudeTemporaryProfileWithStorageRoot(source, storageRoot)
		return directory, false, true, err
	}
	if claudePathContains(source, destination) {
		return "", false, false, fmt.Errorf("private Claude Code profile destination %s is inside source profile %s", destination, source)
	}
	created, err = createClaudePersistentProfileDirectory(destination)
	if err != nil {
		return "", false, false, err
	}
	if err := rejectClaudeProfileDestinationSymlink(destination); err != nil {
		return "", false, created, err
	}
	if err := ensureClaudeProfileDirectory(destination); err != nil {
		return "", false, created, fmt.Errorf("creating persistent Claude Code profile: %w", err)
	}
	if err := rejectClaudeProfileDestinationSymlink(destination); err != nil {
		return "", false, created, err
	}
	return destination, true, created, nil
}

func createClaudePersistentProfileDirectory(destination string) (bool, error) {
	if _, err := os.Lstat(destination); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspecting persistent Claude Code profile destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return false, fmt.Errorf("creating persistent Claude Code profile parent: %w", err)
	}
	if err := os.Mkdir(destination, 0o700); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("creating persistent Claude Code profile destination: %w", err)
	}
	// Another CCR process won the create race. Treat the directory as shared
	// state and never remove it if this process later fails initialization.
	return false, nil
}

func claudeProfileCleanup(directory string, persistent bool) func() error {
	return func() error {
		if persistent {
			return nil
		}
		if err := os.RemoveAll(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing private Claude Code profile: %w", err)
		}
		return nil
	}
}

func claudeProfileInitializationCleanup(directory string, owned bool) func() error {
	return func() error {
		if !owned {
			return nil
		}
		if err := os.RemoveAll(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing incomplete private Claude Code profile: %w", err)
		}
		return nil
	}
}

func populateClaudeLaunchProfile(source, destination, sessionID string, options claudeProfileCopyOptions) error {
	marker := filepath.Join(destination, ".ccr-profile-ready")
	markerErr := checkClaudeProfileMarker(marker, source)
	if markerErr == nil {
		return refreshClaudeLaunchProfileOptions(source, destination, options)
	}
	if !errors.Is(markerErr, os.ErrNotExist) {
		return fmt.Errorf("checking private Claude Code profile readiness: %w", markerErr)
	}
	initializingMarker := filepath.Join(destination, claudeProfileInitializationMarkerName)
	initializingContents, err := claudeProfileInitializationMarkerContents(source)
	if err != nil {
		return err
	}
	if writeErr := writeClaudeProfileFile(initializingMarker, initializingContents, 0o600); writeErr != nil {
		return fmt.Errorf("marking private Claude Code profile initialization: %w", writeErr)
	}
	if copyErr := copyClaudeProfileTreeWithOptions(source, destination, options); copyErr != nil {
		return copyErr
	}
	bootstrapSource, err := currentClaudeBootstrapStatePath(source)
	if err != nil {
		return err
	}
	if copyErr := copyClaudeBootstrapState(bootstrapSource, filepath.Join(destination, ".claude.json")); copyErr != nil {
		return copyErr
	}
	if strings.TrimSpace(sessionID) != "" {
		if sessionErr := copyClaudeSessionState(source, destination, sessionID); sessionErr != nil {
			return sessionErr
		}
	}
	markerContents, err := claudeProfileMarkerContents(source)
	if err != nil {
		return err
	}
	if err := writeClaudeProfileFile(marker, markerContents, 0o600); err != nil {
		return fmt.Errorf("marking private Claude Code profile ready: %w", err)
	}
	if err := os.Remove(initializingMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing private Claude Code profile initialization marker: %w", err)
	}
	return nil
}

func refreshClaudeLaunchProfileOptions(source, destination string, options claudeProfileCopyOptions) error {
	if err := refreshClaudeProfileAssets(source, destination, options); err != nil {
		return err
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if err := refreshClaudeSettingsFile(filepath.Join(source, name), filepath.Join(destination, name), options); err != nil {
			return err
		}
	}
	return nil
}

func createClaudeTemporaryProfile(source string) (string, error) {
	return createClaudeTemporaryProfileWithStorageRoot(source, "")
}

func createClaudeTemporaryProfileWithStorageRoot(source, storageRoot string) (string, error) {
	// Try every approved CCR storage root. A user can intentionally point
	// CLAUDE_CONFIG_DIR at a broad directory such as /tmp or $HOME; limiting
	// temporary profiles to the cache and system temp directories would then
	// leave no safe location even though CCR has other writable roots.
	parents := claudeProfileStorageBases()
	if strings.TrimSpace(storageRoot) != "" {
		parents = []string{storageRoot}
	}
	var lastErr error
	for _, parent := range parents {
		directory, err := os.MkdirTemp(parent, "ccr-claude-profile-*")
		if err != nil {
			lastErr = err
			continue
		}
		if claudePathContains(source, directory) {
			_ = os.RemoveAll(directory)
			lastErr = fmt.Errorf("private Claude Code profile destination %s is inside source profile %s", directory, source)
			continue
		}
		return directory, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no safe directory is available for a private Claude Code profile")
	}
	return "", fmt.Errorf("creating private Claude Code profile: %w", lastErr)
}

func claudePathContains(root, path string) bool {
	canonicalRoot, err := canonicalClaudePath(root)
	if err != nil {
		return true
	}
	canonicalPath, err := canonicalClaudePath(path)
	if err != nil {
		return true
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil {
		rootVolume := filepath.VolumeName(canonicalRoot)
		pathVolume := filepath.VolumeName(canonicalPath)
		if rootVolume != "" && pathVolume != "" && !strings.EqualFold(rootVolume, pathVolume) {
			return false
		}
		return true
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func canonicalClaudePath(path string) (string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	current := absPath
	var suffix []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absPath, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func currentClaudeConfigDir() (string, error) {
	return claudeConfigDirForOverride("")
}

func claudeConfigDirForOverride(override string) (string, error) {
	if configured := strings.TrimSpace(override); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolving Claude Code profile override: %w", err)
		}
		return absolute, nil
	}
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolving CLAUDE_CONFIG_DIR: %w", err)
		}
		return absolute, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating Claude Code home: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

func copyClaudeProfileTreeWithOptions(source, destination string, options claudeProfileCopyOptions) error {
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting Claude Code profile %s: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("claude Code profile %s is not a directory", source)
	}
	if claudePathContains(source, destination) {
		return fmt.Errorf("private Claude Code profile destination %s is inside source profile %s", destination, source)
	}
	// CLAUDE_CONFIG_DIR itself may be a symlink. Resolve only this trusted root;
	// nested links are skipped so they cannot import external credentials or
	// create cycles in the isolated profile.
	if resolved, resolveErr := filepath.EvalSymlinks(source); resolveErr == nil {
		source = resolved
	}
	return copyClaudeProfilePathWithOptions(source, destination, "", options)
}

func collectClaudeProfileDegradations(source string, degradations *[]string) {
	if degradations == nil {
		return
	}
	for _, name := range []string{"agents", "commands", "hooks", "rules", "skills"} {
		info, err := os.Lstat(filepath.Join(source, name))
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			recordClaudeProfileDegradation(degradations, name)
		}
	}
}

func copyClaudeProfileDegradations(degradations *[]string) []string {
	if degradations == nil || len(*degradations) == 0 {
		return nil
	}
	return append([]string(nil), (*degradations)...)
}

func recordClaudeProfileDegradation(degradations *[]string, relative string) {
	if degradations == nil || !isClaudeProfileCustomizationPath(relative) {
		return
	}
	relative = filepath.ToSlash(filepath.Clean(relative))
	for _, existing := range *degradations {
		if existing == relative {
			return
		}
	}
	*degradations = append(*degradations, relative)
}

func isClaudeProfileCustomizationPath(relative string) bool {
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || clean == "" {
		return false
	}
	name := strings.Split(clean, "/")[0]
	switch name {
	case "CLAUDE.md", "settings.json", "settings.local.json", "agents", "commands", "hooks", "plugins", "rules", "skills":
		return true
	default:
		return false
	}
}

func copyClaudeSessionState(source, destination, sessionID string) error {
	projects := filepath.Join(source, "projects")
	if err := ensureClaudeProjectsDirectory(projects); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %q", errClaudeSessionNotFound, strings.TrimSpace(sessionID))
		}
		return fmt.Errorf("inspecting Claude Code projects: %w", err)
	}
	want := strings.TrimSpace(sessionID) + ".jsonl"
	var copied bool
	err := filepath.WalkDir(projects, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking Claude Code projects: %w", walkErr)
		}
		if entry.IsDir() || entry.Name() != want || !entry.Type().IsRegular() {
			return nil
		}
		var err error
		copied, err = copyClaudeSessionFile(source, destination, path, entry)
		return err
	})
	if err != nil {
		return err
	}
	if !copied {
		return fmt.Errorf("%w: %q", errClaudeSessionNotFound, strings.TrimSpace(sessionID))
	}
	return nil
}

func ensureClaudeProjectsDirectory(projects string) error {
	_, err := os.Stat(projects)
	return err
}

func copyClaudeSessionFile(source, destination, path string, entry fs.DirEntry) (bool, error) {
	relative, err := filepath.Rel(source, path)
	if err != nil {
		return false, fmt.Errorf("resolving Claude Code session path: %w", err)
	}
	destinationPath := filepath.Join(destination, relative)
	if err := ensureClaudeProfileDirectory(filepath.Dir(destinationPath)); err != nil {
		return false, fmt.Errorf("creating private Claude Code session directory: %w", err)
	}
	mode := fs.FileMode(0o600)
	if info, infoErr := entry.Info(); infoErr == nil {
		mode = info.Mode().Perm()
	}
	if err := copyClaudeProfileFileStream(path, destinationPath, mode); err != nil {
		return false, fmt.Errorf("copying private Claude Code session state: %w", err)
	}
	if err := copyClaudeSessionSidecars(source, destination, path); err != nil {
		return false, err
	}
	return true, nil
}

func copyClaudeSessionSidecars(source, destination, sessionPath string) error {
	sidecarPath := strings.TrimSuffix(sessionPath, filepath.Ext(sessionPath))
	info, err := os.Lstat(sidecarPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting Claude Code session sidecars: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("claude Code session sidecars %s are not a directory", sidecarPath)
	}
	relative, err := filepath.Rel(source, sidecarPath)
	if err != nil {
		return fmt.Errorf("resolving Claude Code session sidecars: %w", err)
	}
	return copyClaudeSessionSidecarPath(sidecarPath, filepath.Join(destination, relative))
}

func copyClaudeSessionSidecarPath(source, destination string) error {
	info, statErr := os.Lstat(source)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspecting Claude Code session sidecar %s: %w", source, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		return copyClaudeSessionSidecarDirectory(source, destination)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("claude Code session sidecar %s is not a regular file", source)
	}
	return copyClaudeSessionSidecarFile(source, destination, info.Mode().Perm())
}

func copyClaudeSessionSidecarDirectory(source, destination string) error {
	if mkdirErr := ensureClaudeProfileDirectory(destination); mkdirErr != nil {
		return fmt.Errorf("creating private Claude Code session sidecar directory: %w", mkdirErr)
	}
	entries, readErr := os.ReadDir(source)
	if readErr != nil {
		return fmt.Errorf("reading Claude Code session sidecars: %w", readErr)
	}
	for _, entry := range entries {
		if copyErr := copyClaudeSessionSidecarPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func copyClaudeSessionSidecarFile(source, destination string, mode fs.FileMode) error {
	data, readErr := readClaudeProfileFile(source)
	if readErr != nil {
		return fmt.Errorf("reading Claude Code session sidecar: %w", readErr)
	}
	if mkdirErr := ensureClaudeProfileDirectory(filepath.Dir(destination)); mkdirErr != nil {
		return fmt.Errorf("creating private Claude Code session sidecar directory: %w", mkdirErr)
	}
	if writeErr := writeClaudeProfileFile(destination, data, mode); writeErr != nil {
		return fmt.Errorf("writing private Claude Code session sidecar: %w", writeErr)
	}
	return nil
}

func copyClaudeProfilePathWithOptions(source, destination, relative string, options claudeProfileCopyOptions) error {
	return copyClaudeProfilePathVisitedWithOptions(source, destination, relative, make(map[string]struct{}), options)
}

func copyClaudeProfilePathVisitedWithOptions(source, destination, relative string, visited map[string]struct{}, options claudeProfileCopyOptions) error {
	if shouldSkipClaudeProfilePath(relative) {
		return nil
	}
	info, err := os.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Claude Code updates its profile with temporary-file renames while
			// this snapshot is in progress. A disappeared entry was not part of
			// the stable snapshot and is safe to omit.
			return nil
		}
		return fmt.Errorf("inspecting Claude Code profile entry %s: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Do not dereference nested profile links. A link can target credentials
		// outside the profile or create a recursive tree; omitting it is the safe
		// compatibility degradation for an optional customization.
		recordClaudeProfileDegradation(options.degradations, relative)
		return nil
	}
	if info.IsDir() {
		if shouldShareClaudeProfileAssetDirectory(relative) {
			if err := shareClaudeProfileDirectory(source, destination); err == nil {
				return nil
			} else if !errors.Is(err, errClaudeProfileAssetLinkUnavailable) {
				return fmt.Errorf("sharing Claude Code profile asset %s: %w", relative, err)
			}
			// Some filesystems and Windows installations do not permit directory
			// links. Preserve functionality with the existing isolated copy path;
			// the normal supported path remains shared and copy-free.
		}
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return fmt.Errorf("resolving Claude Code profile directory %s: %w", source, err)
		}
		if _, seen := visited[resolved]; seen {
			return fmt.Errorf("claude Code profile contains a symlink cycle through %s", source)
		}
		visited[resolved] = struct{}{}
		err = copyClaudeProfileDirectoryWithOptions(source, destination, relative, visited, options)
		delete(visited, resolved)
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("claude Code profile entry %s is not a regular file", source)
	}
	return copyClaudeRegularProfileFileWithOptions(source, destination, relative, info.Mode().Perm(), options)
}

func copyClaudeRegularProfileFileWithOptions(source, destination, relative string, mode fs.FileMode, options claudeProfileCopyOptions) error {
	if isClaudeSettingsFile(relative) {
		return copyClaudeSettingsFileWithOptions(source, destination, mode, options)
	}
	return copyClaudeProfileFile(source, destination, mode)
}
