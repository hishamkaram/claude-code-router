package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	errClaudeProfileSourceChanged = errors.New("private Claude Code profile source identity changed")
	errClaudeSessionNotFound      = errors.New("session not found in native Claude Code profile")
)

const claudeProfileInitializationMarkerName = ".ccr-profile-initializing"

func checkClaudeProfileMarker(marker, source string) error {
	contents, err := readClaudeProfileFile(marker)
	if err != nil {
		return err
	}
	expected, err := claudeProfileMarkerContents(source)
	if err != nil {
		return err
	}
	if !bytes.Equal(contents, expected) {
		return fmt.Errorf("%w: refusing stale profile reuse", errClaudeProfileSourceChanged)
	}
	return nil
}

func claudeProfileMarkerOwned(directory string) bool {
	_, err := claudeProfileMarkerSource(directory)
	return err == nil
}

func claudeProfileMarkerSource(directory string) (string, error) {
	contents, err := readClaudeProfileFile(filepath.Join(directory, ".ccr-profile-ready"))
	if err != nil {
		return "", err
	}
	const prefix = "ccr private Claude profile\nsource="
	if !bytes.HasPrefix(contents, []byte(prefix)) || !bytes.HasSuffix(contents, []byte("\n")) {
		return "", errors.New("invalid CCR private Claude profile marker")
	}
	identity := bytes.TrimSuffix(bytes.TrimPrefix(contents, []byte(prefix)), []byte("\n"))
	if len(identity) == 0 || bytes.ContainsAny(identity, "\r\n") {
		return "", errors.New("invalid CCR private Claude profile marker source")
	}
	return string(identity), nil
}

func claudeProfileMarkerContents(source string) ([]byte, error) {
	identity, err := claudeProfileSourceIdentity(source)
	if err != nil {
		return nil, fmt.Errorf("identifying native Claude Code profile: %w", err)
	}
	return []byte("ccr private Claude profile\nsource=" + identity + "\n"), nil
}

func claudeProfileInitializationMarkerContents(source string) ([]byte, error) {
	identity, err := claudeProfileSourceIdentity(source)
	if err != nil {
		return nil, fmt.Errorf("identifying native Claude Code profile: %w", err)
	}
	return []byte("ccr private Claude profile initializing\nsource=" + identity + "\n"), nil
}

func claudeProfileInitializationMarkerOwned(directory string) bool {
	contents, err := readClaudeProfileFile(filepath.Join(directory, claudeProfileInitializationMarkerName))
	if err != nil {
		return false
	}
	const prefix = "ccr private Claude profile initializing\nsource="
	if !bytes.HasPrefix(contents, []byte(prefix)) || !bytes.HasSuffix(contents, []byte("\n")) {
		return false
	}
	identity := bytes.TrimSuffix(bytes.TrimPrefix(contents, []byte(prefix)), []byte("\n"))
	return len(identity) > 0 && !bytes.ContainsAny(identity, "\r\n")
}

func claudeProfileSourceIdentity(source string) (string, error) {
	return canonicalClaudePath(source)
}

func foregroundClaudeProfileDestination(sessionID string) (string, error) {
	return foregroundClaudeProfileDestinationWithOptions(sessionID, "", "")
}

func foregroundClaudeProfileDestinationWithOptions(sessionID, sourceOverride, storageRoot string) (string, error) {
	source, err := claudeConfigDirForOverride(sourceOverride)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	profileID := hex.EncodeToString(digest[:])
	bases := claudePersistentProfileStorageBases()
	if strings.TrimSpace(storageRoot) != "" {
		bases = []string{storageRoot}
	}
	for _, base := range bases {
		if destination, ok := claudeForegroundProfileDestination(base, source, profileID); ok {
			return destination, nil
		}
	}
	return "", fmt.Errorf("locating durable CCR private Claude profile storage outside source profile %s: configure XDG_DATA_HOME or another durable user data root outside the native profile", source)
}

func claudeProfileStorageBases() []string {
	bases := make([]string, 0, 8)
	appendBase := func(base string) {
		if base = strings.TrimSpace(base); base != "" {
			for _, existing := range bases {
				if existing == base {
					return
				}
			}
			bases = append(bases, base)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		appendBase(home)
	}
	if base, err := os.UserConfigDir(); err == nil {
		appendBase(base)
	}
	if base, err := os.UserCacheDir(); err == nil {
		appendBase(base)
	}
	appendBase(os.Getenv("XDG_RUNTIME_DIR"))
	appendBase(os.TempDir())
	if workingDir, err := os.Getwd(); err == nil {
		appendBase(workingDir)
	}
	if runtime.GOOS != "windows" {
		appendBase("/var/tmp")
		appendBase("/var/cache")
	}
	return bases
}

// claudePersistentProfileStorageBases deliberately excludes runtime and
// temporary directories. Foreground session profiles and detached continuation
// profiles contain conversation state; placing them under XDG_RUNTIME_DIR,
// os.TempDir, /var/tmp, or a cache-only root would make a successful launch
// unresumable after logout, reboot, or cleanup.
func claudePersistentProfileStorageBases() []string {
	bases := make([]string, 0, 16)
	appendBase := func(base string) {
		if base = strings.TrimSpace(base); base == "" {
			return
		}
		if isVolatileClaudeProfileBase(base) {
			return
		}
		for _, existing := range bases {
			if existing == base {
				return
			}
		}
		bases = append(bases, base)
	}
	appendBase(os.Getenv("XDG_DATA_HOME"))
	appendBase(os.Getenv("XDG_CONFIG_HOME"))
	appendBase(os.Getenv("XDG_STATE_HOME"))
	appendClaudePersistentHomeBases(appendBase)
	if base, err := os.UserConfigDir(); err == nil {
		appendBase(base)
	}
	if runtime.GOOS != "windows" {
		// This optional system-wide root is durable. Normal users usually fail
		// the writability probe; that is preferable to a volatile fallback.
		appendBase("/var/lib")
	}
	return bases
}

// appendClaudePersistentHomeBases includes both the environment-selected home
// and the OS account home. os.UserHomeDir intentionally follows HOME, which is
// useful for Claude's native source profile but can point at a temporary
// sandbox used by a test runner, service supervisor, or CI job. The account
// database gives CCR a durable fallback without weakening the rule that
// conversation profiles cannot live in volatile storage.
func appendClaudePersistentHomeBases(appendBase func(string)) {
	appendHome := func(home string) {
		home = strings.TrimSpace(home)
		if home == "" {
			return
		}
		appendBase(filepath.Join(home, ".local", "share"))
		appendBase(filepath.Join(home, ".config"))
		appendBase(filepath.Join(home, ".local", "state"))
		appendBase(home)
	}
	if home, err := os.UserHomeDir(); err == nil {
		appendHome(home)
	}
	if account, err := osuser.Current(); err == nil {
		appendHome(account.HomeDir)
	}
}

func isVolatileClaudeProfileBase(base string) bool {
	volatileRoots := []string{os.TempDir(), os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("XDG_CACHE_HOME")}
	if runtime.GOOS != "windows" {
		volatileRoots = append(volatileRoots, "/var/tmp")
	}
	for _, root := range volatileRoots {
		root = strings.TrimSpace(root)
		if root != "" && claudePathContains(root, base) {
			return true
		}
	}
	return false
}

func claudeForegroundProfileDestination(base, source, profileID string) (string, bool) {
	identity, err := claudeProfileSourceIdentity(source)
	if err != nil {
		return "", false
	}
	sourceDigest := sha256.Sum256([]byte(identity))
	sourceID := hex.EncodeToString(sourceDigest[:])
	destination := filepath.Join(base, "claude-code-router", "claude-profiles", "foreground", sourceID, profileID)
	if claudePathContains(source, destination) || !claudeProfileBaseWritable(base) {
		return "", false
	}
	return destination, true
}

func claudeProfileBaseWritable(base string) bool {
	base = strings.TrimSpace(base)
	if base == "" {
		return false
	}
	// Profile files are later written through symlink-rejecting helpers. Reject
	// the same path here so a symlinked XDG/home root is skipped and the
	// resolver can choose the next durable CCR storage root instead of selecting
	// a destination that will fail during initialization.
	if err := rejectClaudeProfilePathSymlinks(base); err != nil {
		return false
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return false
	}
	if err := rejectClaudeProfilePathSymlinks(base); err != nil {
		return false
	}
	probe, err := os.CreateTemp(base, ".ccr-profile-write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	return closeErr == nil && removeErr == nil
}
