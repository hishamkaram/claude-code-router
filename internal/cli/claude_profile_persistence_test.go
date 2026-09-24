package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPersistentClaudeLaunchProfileAdoptsInterruptedAssetLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	asset := filepath.Join(source, "skills")
	if err := os.MkdirAll(asset, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset, "SKILL.md"), []byte("research"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(asset, filepath.Join(destination, "skills")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	profile, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("retrying interrupted persistent profile initialization: %v", err)
	}
	if cleanupErr := profile.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if _, markerErr := os.Stat(filepath.Join(destination, ".ccr-profile-ready")); markerErr != nil {
		t.Fatalf("ready marker was not published after adopting asset link: %v", markerErr)
	}
	info, infoErr := os.Lstat(filepath.Join(destination, "skills"))
	if infoErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("interrupted skills asset link was not retained: %v", infoErr)
	}
}

func TestPersistentClaudeLaunchProfilePreservesHistoryAcrossChangedNativeSource(t *testing.T) {
	firstSource := t.TempDir()
	secondSource := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", firstSource)
	first, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("first prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if writeErr := os.WriteFile(filepath.Join(destination, ".claude.json"), []byte(`{"session":"detached-history"}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", secondSource)
	second, err := prepareClaudeLaunchProfileAtForSession(destination, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("persistent profile was not reusable after native source change: %v", err)
	}
	if cleanupErr := second.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	history, err := os.ReadFile(filepath.Join(destination, ".claude.json"))
	if err != nil || string(history) != `{"session":"detached-history"}` {
		t.Fatalf("persistent detached history = %q, %v; want original history preserved", history, err)
	}
	marker, err := os.ReadFile(filepath.Join(destination, ".ccr-profile-ready"))
	if err != nil || !strings.Contains(string(marker), firstSource) || strings.Contains(string(marker), secondSource) {
		t.Fatalf("persistent profile marker = %q, %v; want original source identity", marker, err)
	}
}

func TestPersistentClaudeLaunchProfileRefreshesCopiedCustomizationTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires elevated privileges")
	}
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	assetTarget := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "skills", "research.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "skills", "stale.md"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(assetTarget, filepath.Join(source, "skills", "external")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	first, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("first prepareClaudeLaunchProfileAt() error = %v", err)
	}
	if cleanupErr := first.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if info, lstatErr := os.Lstat(filepath.Join(destination, "skills")); lstatErr != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("initial nested-link asset was not copied: %v, %v", info, lstatErr)
	}
	if writeErr := os.WriteFile(filepath.Join(source, "skills", "research.md"), []byte("new"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(source, "skills", "new.md"), []byte("added"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(filepath.Join(source, "skills", "stale.md")); removeErr != nil {
		t.Fatal(removeErr)
	}
	second, err := prepareClaudeLaunchProfileAt(destination)
	if err != nil {
		t.Fatalf("refreshing persistent profile error = %v", err)
	}
	if cleanupErr := second.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	for relative, want := range map[string]string{"skills/research.md": "new", "skills/new.md": "added"} {
		got, readErr := os.ReadFile(filepath.Join(destination, relative))
		if readErr != nil || string(got) != want {
			t.Fatalf("refreshed copied asset %s = %q, %v; want %q", relative, got, readErr, want)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "skills", "stale.md")); !os.IsNotExist(err) {
		t.Fatalf("removed native customization remained in copied profile: %v", err)
	}
}

func TestPersistentClaudeLaunchProfileRejectsForeignReadinessMarker(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "session-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, ".ccr-profile-ready"), []byte("foreign profile marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const foreignHistory = `{"session":"foreign-history"}`
	if err := os.WriteFile(filepath.Join(destination, ".claude.json"), []byte(foreignHistory), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := prepareClaudeLaunchProfileAt(destination)
	if err == nil || !strings.Contains(err.Error(), "not CCR-owned") {
		t.Fatalf("foreign persistent profile marker was reused: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(destination, ".claude.json"))
	if readErr != nil || string(got) != foreignHistory {
		t.Fatalf("foreign persistent profile state changed: %q, %v", got, readErr)
	}
}

func TestPersistentClaudeLaunchProfileCopiesExplicitNativeResumeSession(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	sessionID := "11111111-1111-4111-8111-111111111111"
	sessionPath := filepath.Join(source, "projects", "-repo", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte(`{"type":"user","message":"native session"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(source, "projects", "-repo", sessionID, "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte(`{"type":"assistant","message":"child history"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "foreground-profile")
	profile, err := prepareClaudeLaunchProfileAtForSession(destination, sessionID)
	if err != nil {
		t.Fatalf("prepareClaudeLaunchProfileAtForSession() error = %v", err)
	}
	if cleanupErr := profile.cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	got, err := os.ReadFile(filepath.Join(destination, "projects", "-repo", sessionID+".jsonl"))
	if err != nil || string(got) != `{"type":"user","message":"native session"}` {
		t.Fatalf("private native resume session = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destination, "projects", "-repo", sessionID, "subagents", "agent-child.jsonl"))
	if err != nil || string(got) != `{"type":"assistant","message":"child history"}` {
		t.Fatalf("private native subagent session sidecar = %q, %v", got, err)
	}
}

func TestPersistentClaudeLaunchProfileRemovesIncompleteResumeProfile(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.WriteFile(filepath.Join(source, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "incomplete-profile")
	_, err := prepareClaudeLaunchProfileAtForSession(destination, "11111111-1111-4111-8111-111111111111")
	if err == nil || !strings.Contains(err.Error(), "session not found in native Claude Code profile") {
		t.Fatalf("prepareClaudeLaunchProfileAtForSession() error = %v, want missing-session failure", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("incomplete persistent profile remains after failed initialization: %v", statErr)
	}
}

func TestPersistentClaudeLaunchProfileRejectsResumeWithoutProjectsDirectory(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	destination := filepath.Join(t.TempDir(), "missing-projects-profile")
	_, err := prepareClaudeLaunchProfileAtForSession(destination, "11111111-1111-4111-8111-111111111111")
	if err == nil || !strings.Contains(err.Error(), "session not found in native Claude Code profile") {
		t.Fatalf("prepareClaudeLaunchProfileAtForSession() error = %v, want missing-session failure", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("profile without a resumable transcript remains after failed initialization: %v", statErr)
	}
}
