package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeTemporaryProfileNeverUsesSourceTree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if claudePathContains(root, profile.configDir) {
		t.Fatalf("private profile %q is inside source tree %q", profile.configDir, root)
	}
}

func TestClaudeTemporaryProfileFallsBackOutsideBroadSource(t *testing.T) {
	source := os.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	t.Setenv("HOME", source)
	t.Setenv("XDG_CONFIG_HOME", source)
	t.Setenv("XDG_CACHE_HOME", source)
	t.Setenv("XDG_RUNTIME_DIR", source)

	destination, err := createClaudeTemporaryProfile(source)
	if err != nil {
		t.Fatalf("createClaudeTemporaryProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destination) })
	if claudePathContains(source, destination) {
		t.Fatalf("temporary profile %q is inside broad source profile %q", destination, source)
	}
}

func TestForegroundClaudeProfileFindsRootOutsideBroadSourceProfile(t *testing.T) {
	source := os.TempDir()
	durableRoot, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	durableData, err := os.MkdirTemp(durableRoot, ".ccr-durable-profile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(durableData) })
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	t.Setenv("HOME", source)
	t.Setenv("XDG_CONFIG_HOME", source)
	t.Setenv("XDG_CACHE_HOME", source)
	t.Setenv("XDG_DATA_HOME", durableData)
	t.Setenv("XDG_RUNTIME_DIR", source)

	destination, err := foregroundClaudeProfileDestination("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("foregroundClaudeProfileDestination() error = %v", err)
	}
	if claudePathContains(source, destination) {
		t.Fatalf("foreground profile %q is inside broad source profile %q", destination, source)
	}
}

func TestForegroundClaudeProfileDestinationSeparatesNativeSources(t *testing.T) {
	base := t.TempDir()
	firstSource := t.TempDir()
	secondSource := t.TempDir()
	first, ok := claudeForegroundProfileDestination(base, firstSource, "session")
	if !ok {
		t.Fatal("first foreground profile destination was not available")
	}
	second, ok := claudeForegroundProfileDestination(base, secondSource, "session")
	if !ok {
		t.Fatal("second foreground profile destination was not available")
	}
	if first == second {
		t.Fatalf("foreground profile destinations reused across native sources: %q", first)
	}
}

func TestForegroundClaudeProfileDestinationSkipsSymlinkedStorageRoot(t *testing.T) {
	realData := t.TempDir()
	linkParent := t.TempDir()
	linkData := filepath.Join(linkParent, "data-link")
	if err := os.Symlink(realData, linkData); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	fallback, err := os.MkdirTemp(".", ".ccr-profile-storage-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fallback) })
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	t.Setenv("XDG_DATA_HOME", linkData)
	t.Setenv("XDG_CONFIG_HOME", fallback)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))

	destination, err := foregroundClaudeProfileDestination("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("foregroundClaudeProfileDestination() error = %v", err)
	}
	if claudePathContains(realData, destination) || strings.HasPrefix(filepath.Clean(destination), filepath.Clean(linkData)+string(os.PathSeparator)) {
		t.Fatalf("selected symlinked storage root: %q", destination)
	}
	if !claudePathContains(fallback, destination) {
		t.Fatalf("destination = %q, want fallback root %q", destination, fallback)
	}
}

func TestClaudeTemporaryProfileRejectsCanonicalSourceTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	cacheRoot := t.TempDir()
	configParent := t.TempDir()
	configLink := filepath.Join(configParent, "native-link")
	if err := os.Symlink(cacheRoot, configLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", configLink)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if claudePathContains(cacheRoot, profile.configDir) {
		t.Fatalf("private profile %q is inside canonical source tree %q", profile.configDir, cacheRoot)
	}
}

func TestClaudeProfileCopyRejectsSymlinkCycles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.Symlink(".", filepath.Join(source, "loop")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	profile, err := prepareClaudeLaunchProfile()
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfile() error = %v", err)
	}
	t.Cleanup(func() { _ = profile.cleanup() })
	if _, err := os.Stat(filepath.Join(profile.configDir, "loop")); !os.IsNotExist(err) {
		t.Fatalf("private profile copied symlink cycle: %v", err)
	}
}

func TestCopyClaudeProfileStreamContentsRejectsInPlaceChange(t *testing.T) {
	source := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(source, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	before, err := input.Stat()
	if err != nil {
		t.Fatal(err)
	}
	appendFile, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := appendFile.WriteString("appended\n"); writeErr != nil {
		_ = appendFile.Close()
		t.Fatal(writeErr)
	}
	if closeErr := appendFile.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	temporary, err := os.CreateTemp(t.TempDir(), "copy-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if err := copyClaudeProfileStreamContents(input, temporary, source, before); err == nil || !strings.Contains(err.Error(), "changed while being copied") {
		t.Fatalf("copyClaudeProfileStreamContents() error = %v, want in-place change refusal", err)
	}
}
