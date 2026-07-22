package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestParseLaunchInvocationParsesCUAFlags(t *testing.T) {
	t.Parallel()

	invocation, err := parseLaunchInvocation([]string{
		"--model", "gpt",
		"--ccr-cua-mode=managed",
		"--ccr-cua-executor=external:browser_1.prod",
		"--ccr-cua-external-url=https://executor.example/cua",
		"--ccr-cua-external-token-env=CCR_CUA_TOKEN",
		"--ccr-cua-max-turns=12",
		"--ccr-cua-max-actions", "34",
		"--ccr-cua-timeout=2m",
		"--chrome",
		"review this page",
	})
	if err != nil {
		t.Fatalf("parseLaunchInvocation() error = %v", err)
	}
	if invocation.modelAlias != "gpt" {
		t.Fatalf("model alias = %q, want gpt", invocation.modelAlias)
	}
	if invocation.cuaConfig.Mode != cua.ModeManaged {
		t.Fatalf("CUA mode = %q, want %q", invocation.cuaConfig.Mode, cua.ModeManaged)
	}
	if invocation.cuaConfig.Executor != "external:browser_1.prod" {
		t.Fatalf("CUA executor = %q", invocation.cuaConfig.Executor)
	}
	if invocation.cuaExternalURL != "https://executor.example/cua" {
		t.Fatalf("CUA external URL = %q", invocation.cuaExternalURL)
	}
	if invocation.cuaTokenEnv != "CCR_CUA_TOKEN" {
		t.Fatalf("CUA external token environment = %q", invocation.cuaTokenEnv)
	}
	if invocation.cuaConfig.MaxTurns != 12 || invocation.cuaConfig.MaxActions != 34 ||
		invocation.cuaConfig.Timeout != 2*time.Minute {
		t.Fatalf("CUA config = %#v", invocation.cuaConfig)
	}
	if !slices.Equal(invocation.claudeArgs, []string{"--chrome", "review this page"}) {
		t.Fatalf("claude args = %#v", invocation.claudeArgs)
	}
}

func TestParseLaunchInvocationForwardsCUAFlagsAfterTerminator(t *testing.T) {
	t.Parallel()

	invocation, err := parseLaunchInvocation([]string{
		"--model", "gpt",
		"--",
		"--ccr-cua-mode",
		"managed",
		"--ccr-cua-timeout=5s",
	})
	if err != nil {
		t.Fatalf("parseLaunchInvocation() error = %v", err)
	}
	want := []string{"--", "--ccr-cua-mode", "managed", "--ccr-cua-timeout=5s"}
	if !slices.Equal(invocation.claudeArgs, want) {
		t.Fatalf("claude args = %#v, want %#v", invocation.claudeArgs, want)
	}
	if invocation.cuaOptionsConfigured() {
		t.Fatalf("CUA options were parsed after explicit terminator: %#v", invocation)
	}
}

type invalidLaunchInvocationCase struct {
	name    string
	args    []string
	wantErr string
}

func TestParseLaunchInvocationRejectsInvalidCUAModeAndExecutorFlags(t *testing.T) {
	t.Parallel()

	tests := []invalidLaunchInvocationCase{
		{
			name:    "missing mode value",
			args:    []string{"--ccr-cua-mode"},
			wantErr: "--ccr-cua-mode requires a value",
		},
		{
			name:    "invalid mode",
			args:    []string{"--ccr-cua-mode=auto"},
			wantErr: "invalid CUA mode",
		},
		{
			name:    "executor requires managed mode",
			args:    []string{"--ccr-cua-executor=docker"},
			wantErr: "requires --ccr-cua-mode managed",
		},
		{
			name:    "managed mode requires executor",
			args:    []string{"--ccr-cua-mode=managed"},
			wantErr: "requires --ccr-cua-executor",
		},
		{
			name:    "invalid executor",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:"},
			wantErr: "invalid CUA executor",
		},
		{
			name:    "external executor name rejects whitespace",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external: browser"},
			wantErr: "must not include whitespace",
		},
	}

	assertParseLaunchInvocationRejects(t, tests)
}

func TestParseLaunchInvocationRejectsInvalidCUAExternalExecutorFlags(t *testing.T) {
	t.Parallel()

	tests := []invalidLaunchInvocationCase{
		{
			name:    "external executor requires URL",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser"},
			wantErr: "requires --ccr-cua-external-url",
		},
		{
			name:    "external executor requires token environment",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua"},
			wantErr: "requires --ccr-cua-external-token-env",
		},
		{
			name:    "external token environment requires external executor",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-external-token-env=CCR_CUA_TOKEN"},
			wantErr: "requires --ccr-cua-executor external:<name>",
		},
		{
			name:    "external token environment validates name",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=not-valid"},
			wantErr: "invalid environment variable name",
		},
		{
			name:    "external token environment rejects reserved launch env",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=ANTHROPIC_CUSTOM_HEADERS"},
			wantErr: "reserved by CCR or Claude Code",
		},
		{
			name:    "external URL requires external executor",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-external-url=https://executor.example/cua"},
			wantErr: "requires --ccr-cua-executor external:<name>",
		},
		{
			name:    "external URL rejects HTTP",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=http://executor.example/cua"},
			wantErr: "absolute HTTPS URL",
		},
		{
			name:    "external URL rejects credentials",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://user:pass@executor.example/cua"},
			wantErr: "must not include credentials",
		},
		{
			name:    "external URL rejects query",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua?token=value"},
			wantErr: "must not include query or fragment",
		},
	}

	assertParseLaunchInvocationRejects(t, tests)
}

func TestParseLaunchInvocationRejectsInvalidCUALimitFlags(t *testing.T) {
	t.Parallel()

	tests := []invalidLaunchInvocationCase{
		{
			name:    "max turns must be positive",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-max-turns=0"},
			wantErr: "must be a positive integer",
		},
		{
			name:    "max actions must be an integer",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-max-actions=many"},
			wantErr: "invalid value for --ccr-cua-max-actions",
		},
		{
			name:    "timeout must be a duration",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-timeout=soon"},
			wantErr: "invalid value for --ccr-cua-timeout",
		},
		{
			name:    "timeout must be positive",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-timeout=0s"},
			wantErr: "--ccr-cua-timeout must be greater than zero",
		},
		{
			name:    "timeout rejects negative duration",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-timeout=-1s"},
			wantErr: "--ccr-cua-timeout must be greater than zero",
		},
		{
			name:    "timeout uses config minimum",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=docker", "--ccr-cua-timeout=500ms"},
			wantErr: "CUA timeout must be at least 1s",
		},
		{
			name:    "limits require managed mode",
			args:    []string{"--ccr-cua-max-turns=5"},
			wantErr: "managed CUA options require --ccr-cua-mode managed",
		},
	}

	assertParseLaunchInvocationRejects(t, tests)
}

func assertParseLaunchInvocationRejects(t *testing.T, tests []invalidLaunchInvocationCase) {
	t.Helper()

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseLaunchInvocation(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseLaunchInvocation(%#v) error = %v, want %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestLaunchCUAFlagErrorsFailBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing value",
			args:    []string{"--ccr-cua-mode"},
			wantErr: "--ccr-cua-mode requires a value",
		},
		{
			name: "malformed value",
			args: []string{
				"--ccr-cua-mode=managed",
				"--ccr-cua-executor=external:browser",
				"--ccr-cua-external-url=http://executor.example/cua",
			},
			wantErr: "absolute HTTPS URL",
		},
		{
			name:    "invalid external executor name",
			args:    []string{"--ccr-cua-executor=external: browser"},
			wantErr: "must not include whitespace",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dbPath := filepath.Join(t.TempDir(), "ccr.db")
			launcher := &fakeLauncher{pid: os.Getpid()}
			args := append([]string{"--db", dbPath, "launch"}, tt.args...)
			_, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, args...)
			if err == nil {
				t.Fatalf("launch unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("launch error = %v, want %q", err, tt.wantErr)
			}
			if launcher.starts != 0 {
				t.Fatalf("launcher starts = %d, want 0", launcher.starts)
			}
			if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
				t.Fatalf("database exists after CUA parse error: stat err=%v", statErr)
			}
		})
	}
}
