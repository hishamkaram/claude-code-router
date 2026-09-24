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

func TestParseLaunchInvocationDefaultsAuthModeAuto(t *testing.T) {
	t.Parallel()

	invocation, err := parseLaunchInvocation(nil)
	if err != nil {
		t.Fatalf("parseLaunchInvocation() error = %v", err)
	}
	if invocation.authMode != launchAuthModeAuto || invocation.authModeSet {
		t.Fatalf("auth mode = %q set=%t, want auto false", invocation.authMode, invocation.authModeSet)
	}
	invocation, err = parseLaunchInvocation([]string{"--auth-mode", "auto"})
	if err != nil {
		t.Fatalf("parseLaunchInvocation(--auth-mode auto) error = %v", err)
	}
	if invocation.authMode != launchAuthModeAuto || !invocation.authModeSet {
		t.Fatalf("explicit auth mode = %q set=%t, want auto true", invocation.authMode, invocation.authModeSet)
	}
}

func TestNativeClaudeResumeSessionRejectsDuplicateOptions(t *testing.T) {
	t.Parallel()
	first := "550e8400-e29b-41d4-a716-446655440000"
	second := "6ba7b810-9dad-41d1-80b4-00c04fd430c8"
	for _, args := range [][]string{
		{"--resume", first, "--resume", second},
		{"--resume=" + first, "-r", second},
		{"-r" + first, "--resume=" + second},
	} {
		if _, err := nativeClaudeResumeSession(args); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("nativeClaudeResumeSession(%q) error = %v, want duplicate-option refusal", args, err)
		}
	}
}

func TestClaudeSessionScannersSkipValueTakingPassthroughOptions(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	args := []string{"--system-prompt", "--session-id", sessionID}
	if got, err := nativeClaudeResumeSession(args); err != nil || got != "" {
		t.Fatalf("nativeClaudeResumeSession(%#v) = %q, %v; want no session control", args, got, err)
	}
	if got, err := nativeClaudeSessionID(args); err != nil || got != "" {
		t.Fatalf("nativeClaudeSessionID(%#v) = %q, %v; want no session control", args, got, err)
	}
	if option := detachedNativeSessionOption(args); option != "" {
		t.Fatalf("detachedNativeSessionOption(%#v) = %q, want no session control", args, option)
	}
}

func TestParseLaunchInvocationPreservesClaudeOptionValuesThatLookLikeCCRFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantClaude []string
		wantDetach bool
		wantModel  string
		wantPrint  bool
	}{
		{
			name:       "system prompt consumes model and detach",
			args:       []string{"--system-prompt", "--model", "--detach"},
			wantClaude: []string{"--system-prompt", "--model"},
			wantDetach: true,
		},
		{
			name:       "system prompt consumes help",
			args:       []string{"--system-prompt", "--help", "--no-history"},
			wantClaude: []string{"--system-prompt", "--help"},
		},
		{
			name:       "owned model still parses before Claude values",
			args:       []string{"--model", "gpt", "--system-prompt", "--detach", "--print"},
			wantClaude: []string{"--system-prompt", "--detach"},
			wantModel:  "gpt",
			wantPrint:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invocation, err := parseLaunchInvocation(tt.args)
			if err != nil {
				t.Fatalf("parseLaunchInvocation() error = %v", err)
			}
			if !slices.Equal(invocation.claudeArgs, tt.wantClaude) {
				t.Fatalf("claude args = %#v, want %#v", invocation.claudeArgs, tt.wantClaude)
			}
			if invocation.detach != tt.wantDetach {
				t.Fatalf("detach = %t, want %t", invocation.detach, tt.wantDetach)
			}
			if invocation.modelAlias != tt.wantModel {
				t.Fatalf("model alias = %q, want %q", invocation.modelAlias, tt.wantModel)
			}
			if invocation.printMode != tt.wantPrint {
				t.Fatalf("print mode = %t, want %t", invocation.printMode, tt.wantPrint)
			}
		})
	}
}

func TestResolveClaudeLaunchProfilePlanRejectsResumeWithoutPersistence(t *testing.T) {
	_, err := resolveClaudeLaunchProfilePlan(launchInvocation{
		claudeArgs: []string{
			"--resume", "11111111-1111-4111-8111-111111111111",
			"--no-session-persistence",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "--resume cannot be combined with --no-session-persistence") {
		t.Fatalf("resolveClaudeLaunchProfilePlan() error = %v, want explicit persistence conflict", err)
	}
}

func TestResolveClaudeLaunchProfilePlanRejectsConflictingResumeAndSessionID(t *testing.T) {
	_, err := resolveClaudeLaunchProfilePlan(launchInvocation{
		claudeArgs: []string{
			"--resume", "11111111-1111-4111-8111-111111111111",
			"--session-id", "22222222-2222-4222-8222-222222222222",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "--resume cannot be combined with --session-id") {
		t.Fatalf("resolveClaudeLaunchProfilePlan() error = %v, want conflicting session identity refusal", err)
	}
}

func TestParseLaunchInvocationRejectsConflictingResumeAndSessionID(t *testing.T) {
	_, err := parseLaunchInvocation([]string{
		"--resume", "11111111-1111-4111-8111-111111111111",
		"--session-id", "22222222-2222-4222-8222-222222222222",
	})
	if err == nil || !strings.Contains(err.Error(), "--resume cannot be combined with --session-id") {
		t.Fatalf("parseLaunchInvocation() error = %v, want conflicting session identity refusal", err)
	}
}

func TestParseLaunchInvocationRejectsResumeWithoutPersistenceBeforeStartup(t *testing.T) {
	_, err := parseLaunchInvocation([]string{
		"--resume", "11111111-1111-4111-8111-111111111111",
		"--no-session-persistence",
	})
	if err == nil || !strings.Contains(err.Error(), "--resume cannot be combined with --no-session-persistence") {
		t.Fatalf("parseLaunchInvocation() error = %v, want pre-startup persistence conflict", err)
	}
}

func TestResolveClaudeLaunchProfilePlanRejectsForkedSession(t *testing.T) {
	_, err := resolveClaudeLaunchProfilePlan(launchInvocation{
		claudeArgs: []string{
			"--resume", "11111111-1111-4111-8111-111111111111",
			"--fork-session",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "--fork-session is not supported through ccr") {
		t.Fatalf("resolveClaudeLaunchProfilePlan() error = %v, want explicit fork-session refusal", err)
	}
}

func TestParseLaunchInvocationRejectsPRLinkedSessionBeforeStartup(t *testing.T) {
	_, err := parseLaunchInvocation([]string{"--from-pr", "123"})
	if err == nil || !strings.Contains(err.Error(), "--from-pr is not supported through ccr") {
		t.Fatalf("parseLaunchInvocation() error = %v, want explicit PR-session refusal", err)
	}
}

func TestResolveClaudeLaunchProfilePlanRejectsPRLinkedSession(t *testing.T) {
	_, err := resolveClaudeLaunchProfilePlan(launchInvocation{claudeArgs: []string{"--from-pr", "123"}})
	if err == nil || !strings.Contains(err.Error(), "--from-pr is not supported through ccr") {
		t.Fatalf("resolveClaudeLaunchProfilePlan() error = %v, want explicit PR-session refusal", err)
	}
}

func TestParseLaunchInvocationRejectsForkedSessionBeforeStartup(t *testing.T) {
	_, err := parseLaunchInvocation([]string{"--fork-session"})
	if err == nil || !strings.Contains(err.Error(), "--fork-session is not supported through ccr") {
		t.Fatalf("parseLaunchInvocation() error = %v, want preflight fork-session refusal", err)
	}
}

func TestParseLaunchInvocationRejectsTeleportBeforeProfilePlanning(t *testing.T) {
	for _, args := range [][]string{{"--teleport"}, {"--teleport", "cloud-session"}, {"--teleport=cloud-session"}} {
		_, err := parseLaunchInvocation(args)
		if err == nil || !strings.Contains(err.Error(), "--teleport is not supported through ccr") {
			t.Fatalf("parseLaunchInvocation(%#v) error = %v, want explicit teleport refusal", args, err)
		}
	}
}

func TestResolveClaudeLaunchProfilePlanRejectsTeleportBeforeGeneratingSessionID(t *testing.T) {
	_, err := resolveClaudeLaunchProfilePlan(launchInvocation{claudeArgs: []string{"--teleport", "cloud-session"}})
	if err == nil || !strings.Contains(err.Error(), "--teleport is not supported through ccr") {
		t.Fatalf("resolveClaudeLaunchProfilePlan() error = %v, want explicit teleport refusal", err)
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
	want := []string{"--ccr-cua-mode", "managed", "--ccr-cua-timeout=5s"}
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
			name:    "external token environment rejects Claude OAuth access token",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=CLAUDE_CODE_OAUTH_TOKEN"},
			wantErr: "reserved by CCR or Claude Code",
		},
		{
			name:    "external token environment rejects Claude OAuth refresh token",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=CLAUDE_CODE_OAUTH_REFRESH_TOKEN"},
			wantErr: "reserved by CCR or Claude Code",
		},
		{
			name:    "external token environment rejects Claude OAuth scopes",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=CLAUDE_CODE_OAUTH_SCOPES"},
			wantErr: "reserved by CCR or Claude Code",
		},
		{
			name:    "external token environment rejects Claude config directory",
			args:    []string{"--ccr-cua-mode=managed", "--ccr-cua-executor=external:browser", "--ccr-cua-external-url=https://executor.example/cua", "--ccr-cua-external-token-env=CLAUDE_CONFIG_DIR"},
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

func TestNativeClaudeResumeSessionRequiresExplicitSessionID(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "long", args: []string{"--resume", sessionID}, want: sessionID},
		{name: "long inline", args: []string{"--resume=" + sessionID}, want: sessionID},
		{name: "short inline", args: []string{"-r" + sessionID}, want: sessionID},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := nativeClaudeResumeSession(test.args)
			if err != nil || got != test.want {
				t.Fatalf("nativeClaudeResumeSession(%#v) = %q, %v; want %q", test.args, got, err, test.want)
			}
		})
	}
	for _, args := range [][]string{{"--resume"}, {"--resume="}, {"--resume", "--output-format"}, {"-r="}, {"-r", "--output-format"}, {"--continue"}, {"-c"}} {
		if _, err := nativeClaudeResumeSession(args); err == nil {
			t.Fatalf("nativeClaudeResumeSession(%#v) succeeded, want explicit isolation error", args)
		}
	}
}

func TestNativeClaudeResumeSessionStopsAtForwardedClaudeSeparator(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--", "--resume", "session-id"}, {"--", "--continue"}} {
		if got, err := nativeClaudeResumeSession(args); err != nil || got != "" {
			t.Fatalf("nativeClaudeResumeSession(%#v) = %q, %v; forwarded prompt was interpreted as CCR launch options", args, got, err)
		}
	}
}

func TestNativeClaudeSessionIDValidatesExplicitIdentity(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-1111-4111-8111-111111111111"
	for _, args := range [][]string{
		{"--session-id", sessionID},
		{"--session-id=" + sessionID},
	} {
		got, err := nativeClaudeSessionID(args)
		if err != nil || got != sessionID {
			t.Fatalf("nativeClaudeSessionID(%#v) = %q, %v; want %q", args, got, err, sessionID)
		}
	}
	for _, args := range [][]string{
		{"--session-id"},
		{"--session-id="},
		{"--session-id", "not-a-uuid"},
		{"--session-id", sessionID, "--session-id", sessionID},
	} {
		if _, err := nativeClaudeSessionID(args); err == nil {
			t.Fatalf("nativeClaudeSessionID(%#v) succeeded, want validation error", args)
		}
	}
}

func TestClaudeLaunchDisablesSessionPersistenceOnlyForExplicitFlag(t *testing.T) {
	t.Parallel()
	if !claudeLaunchDisablesSessionPersistence([]string{"--no-session-persistence"}) {
		t.Fatal("--no-session-persistence was not recognized")
	}
	if !claudeLaunchDisablesSessionPersistence([]string{"--no-session-persistence=true"}) {
		t.Fatal("--no-session-persistence=true was not recognized")
	}
	if claudeLaunchDisablesSessionPersistence([]string{"--no-session-persistence=false"}) {
		t.Fatal("--no-session-persistence=false disabled persistence")
	}
}
