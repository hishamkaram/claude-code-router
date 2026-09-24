package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchAutoWithClaudeAuthPreservesFirstPartyAuth(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
	}, "--db", dbPath, "launch", "--model", "gpt")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	assertPreserveAuthEnv(t, launcher)
	if !strings.Contains(out, "Anthropic subscription login and Anthropic API-key auth are preserved") {
		t.Fatalf("launch output missing preserve summary:\n%s", out)
	}
	statusOut, _, err := runCommand(t, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=preserve") {
		t.Fatalf("status output missing resolved auth mode:\n%s", statusOut)
	}
}

func TestLaunchAutoWithoutModelDetectsClaudeAuthOnce(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	detectionCalls := 0
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) {
			detectionCalls++
			return true, nil
		},
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if detectionCalls != 1 {
		t.Fatalf("Claude auth detection calls = %d, want 1", detectionCalls)
	}
	assertPreserveAuthEnv(t, launcher)
}

func TestLaunchAutoAuthDetectionErrorPreservesClaudeAuth(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) {
			return false, errors.New("auth store unavailable")
		},
	}, "--db", dbPath, "launch")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	assertPreserveAuthEnv(t, launcher)
}

func TestLaunchAutoProviderModelWithoutClaudeAuthUsesProviderOnly(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--api-key-env", "CCR_TEST_PROVIDER_TOKEN"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		Secrets:          &fakeSecrets{values: map[string]string{"env:CCR_TEST_PROVIDER_TOKEN": "test-provider-token"}},
		DetectClaudeAuth: func(context.Context) (bool, error) { return false, nil },
	}, "--db", dbPath, "launch", "--model", "gpt")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") {
		t.Fatalf("provider-only launch env missing local token: %s", launcher.environmentSummary())
	}
	if !launcher.unsetsEnv("ANTHROPIC_API_KEY") || !launcher.unsetsEnv("CLAUDE_CODE_OAUTH_TOKEN") ||
		!launcher.unsetsEnv("CLAUDE_CODE_OAUTH_REFRESH_TOKEN") || !launcher.unsetsEnv("CCR_TEST_PROVIDER_TOKEN") {
		t.Fatalf("provider-only launch env did not isolate credentials: %s", launcher.environmentSummary())
	}
	if launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_HEADERS=") {
		t.Fatalf("provider-only launch should not set custom headers: %s", launcher.environmentSummary())
	}
	if !strings.Contains(out, "Provider-only auth is active") {
		t.Fatalf("launch output missing provider-only summary:\n%s", out)
	}
	statusOut, _, err := runCommand(t, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=provider-only") {
		t.Fatalf("status output missing provider-only auth mode:\n%s", statusOut)
	}
	doctorOut, _, err := runCommand(t, "--db", dbPath, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(doctorOut, "Launch auth: mode=provider-only") {
		t.Fatalf("doctor output missing provider-only auth mode:\n%s", doctorOut)
	}
}

func TestLaunchAutoWithoutModelAndWithoutClaudeAuthFailsWithProviderGuidance(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return false, nil },
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	for _, want := range []string{"claude subscription authentication was not found", "CCR will not choose a provider automatically", "ccr launch --model gpt", "Available provider aliases: gpt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("launch error = %v, missing %q", err, want)
		}
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchAutoWithoutModelAndWithoutAnyAuthReportsLoginGuidance(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return false, nil },
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "run claude /login") || !strings.Contains(err.Error(), "configure a CCR provider") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after no-auth/no-model launch rejection: stat err=%v", statErr)
	}
}

func TestLaunchProviderOnlyAuthModeForcesLocalToken(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
	}, "--db", dbPath, "launch", "--model", "gpt", "--auth-mode", "provider-only")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") || !launcher.unsetsEnv("ANTHROPIC_API_KEY") {
		t.Fatalf("provider-only launch env = %s", launcher.environmentSummary())
	}
	if !strings.Contains(out, "Provider-only auth is active") ||
		strings.Contains(out, "Anthropic subscription login and Anthropic API-key auth are preserved") {
		t.Fatalf("launch output = %s", out)
	}
}

func TestLaunchProviderOnlyAuthModesRequireCCRStartupModel(t *testing.T) {
	t.Parallel()

	for _, authMode := range []string{launchAuthModeProviderOnly, launchAuthModeGatewayToken} {
		authMode := authMode
		t.Run(authMode, func(t *testing.T) {
			t.Parallel()

			dbPath := filepath.Join(t.TempDir(), "ccr.db")
			launcher := &fakeLauncher{pid: os.Getpid()}
			_, _, err := runCommandWithDeps(t, Dependencies{
				Launcher: launcher,
			}, "--db", dbPath, "launch", "--auth-mode", authMode)
			if err == nil {
				t.Fatalf("launch unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), "--auth-mode "+authMode+" requires --model") {
				t.Fatalf("launch error = %v", err)
			}
			if launcher.starts != 0 {
				t.Fatalf("launcher starts = %d, want 0", launcher.starts)
			}
			if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
				t.Fatalf("database exists after missing provider-only model: stat err=%v", statErr)
			}
		})
	}
}
