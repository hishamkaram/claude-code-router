package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/routeidentity"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestResumeRouteInvocationPreservesNativeResume(t *testing.T) {
	t.Parallel()
	invocation, err := parseLaunchInvocation([]string{
		"--resume-route=true", "--", "--resume", "native-session", "--verbose",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invocation.resumeRoute || !strings.EqualFold(invocation.authMode, launchAuthModeAuto) ||
		!slices.Equal(invocation.claudeArgs, []string{"--resume", "native-session", "--verbose"}) {
		t.Fatalf("parseLaunchInvocation() = %#v", invocation)
	}
	if err := validateResumeRouteInvocation(invocation); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRouteInvocationRejectsAmbiguousOrConflictingInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing_native_resume", args: []string{"--resume-route"}, want: "requires a native --resume"},
		{name: "missing_native_resume_value", args: []string{"--resume-route", "--", "--resume", "-p"}, want: "requires a nonempty native --resume"},
		{name: "duplicate_native_resume", args: []string{"--resume-route", "--", "--resume=a", "--resume=b"}, want: "exactly one native --resume"},
		{name: "session_id", args: []string{"--resume-route", "--", "--resume=a", "--session-id=b"}, want: "native session option --session-id"},
		{name: "continue", args: []string{"--resume-route", "--", "--resume=a", "--continue"}, want: "native session option --continue"},
		{name: "short_continue", args: []string{"--resume-route", "--", "--resume=a", "-c"}, want: "native session option -c"},
		{name: "short_resume", args: []string{"--resume-route", "--", "--resume=a", "-r", "b"}, want: "native session option -r"},
		{name: "fork_session", args: []string{"--resume-route", "--", "--resume=a", "--fork-session"}, want: "native session option --fork-session"},
		{name: "no_persistence", args: []string{"--resume-route", "--", "--resume=a", "--no-session-persistence"}, want: "native session option --no-session-persistence"},
		{name: "model", args: []string{"--resume-route", "--model", "coder", "--", "--resume=a"}, want: "cannot be combined with --model"},
		{name: "auth", args: []string{"--resume-route", "--auth-mode", "provider-only", "--", "--resume=a"}, want: "cannot be combined with --auth-mode"},
		{name: "detached", args: []string{"--resume-route", "--detach", "--", "--resume=a"}, want: "requires a foreground"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invocation, err := parseLaunchInvocation(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateResumeRouteInvocation(invocation); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateResumeRouteInvocation() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResumeRouteValidationPrecedesLaunchSideEffects(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	database := filepath.Join(t.TempDir(), "ccr.db")
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("PONG"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "subscription_pool",
			args: []string{"launch", "--db", database, "--resume-route", "--auth-mode", "subscription-pool", "--", "--resume", "native-session"},
			want: "cannot be combined with --auth-mode",
		},
		{
			name: "detached",
			args: []string{"launch", "--db", database, "--resume-route", "--detach", "-p", "--prompt-file", prompt, "--", "--resume", "native-session"},
			want: "requires a foreground",
		},
		{
			name: "invalid_session_id",
			args: []string{"launch", "--db", database, "--", "--session-id", "bad/session"},
			want: "session_id has an invalid format",
		},
		{
			name: "invalid_resume_id",
			args: []string{"launch", "--db", database, "--", "--resume", "bad/session"},
			want: "session_id has an invalid format",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewRootCommand(context.Background(), Dependencies{})
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatalf("invalid resume route created database: %v", err)
	}
	entries, err := os.ReadDir(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid resume route created state under XDG_DATA_HOME: %v", entries)
	}
}

func TestNativeKnownSessionUsesOneForwardedSessionSelector(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantID     string
		wantSource string
		wantFound  bool
	}{
		{name: "session_id", args: []string{"--session-id", "source-session"}, wantID: "source-session", wantSource: "startup", wantFound: true},
		{name: "resume", args: []string{"--resume=prior-session"}, wantID: "prior-session", wantSource: "resume", wantFound: true},
		{name: "prompt_boundary", args: []string{"--session-id", "source-session", "--", "--resume", "prompt-text"}, wantID: "source-session", wantSource: "startup", wantFound: true},
		{name: "ambiguous", args: []string{"--session-id", "source-session", "--resume", "prior-session"}},
		{name: "missing_value", args: []string{"--session-id", "--verbose"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotSource, gotFound := nativeKnownSession(tt.args)
			if gotID != tt.wantID || gotSource != tt.wantSource || gotFound != tt.wantFound {
				t.Fatalf("nativeKnownSession(%q) = (%q, %q, %t)", tt.args, gotID, gotSource, gotFound)
			}
		})
	}
}

func TestLaunchResumeRouteRestoresObservedProviderRouteWithoutDiscovery(t *testing.T) {
	t.Parallel()
	var providerRequests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"}]}`))
			return
		}
		if r.URL.Path == "/model/info" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer provider.Close()

	database := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", database, "provider", "add", "litellm", "--base-url", provider.URL, "--no-api-key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, "--db", database, "model", "add", "coder", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatal(err)
	}
	seedRecordedCoderResumeRoute(t, database)
	providerRequests.Store(0)

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
		launchPrepared: func(resolved resolvedLaunch) error {
			if resolved.pinnedRoute == nil {
				t.Fatal("resume launch did not pin its admitted route")
			}
			ctx := context.Background()
			s, openErr := store.Open(ctx, database)
			if openErr != nil {
				return openErr
			}
			defer func() { _ = s.Close() }()
			model, modelErr := s.GetModel(ctx, "coder")
			if modelErr != nil {
				return modelErr
			}
			model.Status = "chat-only"
			return s.UpdateModel(ctx, model)
		},
	}, "--db", database, "launch", "--resume-route", "--", "--resume", "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if providerRequests.Load() != 0 {
		t.Fatalf("provider requests after resume admission = %d, want 0", providerRequests.Load())
	}
	if launcher.starts != 1 || !launcher.hasArg("--resume") || !launcher.hasArg("native-session") {
		t.Fatalf("launcher starts=%d args=%#v", launcher.starts, launcher.args)
	}
	if !launcher.hasEnvPrefix("ANTHROPIC_AUTH_TOKEN=") || !launcher.unsetsEnv("CLAUDE_CODE_OAUTH_TOKEN") ||
		launcher.hasEnvPrefix("ANTHROPIC_CUSTOM_HEADERS=") {
		t.Fatalf("resume route did not retain provider-only auth: %s", launcher.environmentSummary())
	}
	if launcher.hasArg("--tools") {
		t.Fatalf("post-admission model mutation changed pinned tool support: args=%#v", launcher.args)
	}
	settings := launchSettingsPayload(t, launcher)
	if !slices.Equal(settings.AvailableModels, []string{"anthropic.ccr.coder"}) {
		t.Fatalf("resume availableModels = %#v, want only pinned alias", settings.AvailableModels)
	}
}

func TestLaunchResumeRouteUsesAdmittedProviderSecretEnvironmentAfterMutation(t *testing.T) {
	const admittedSecretEnv = "CCR_RESUME_ADMITTED_PROVIDER_TOKEN"
	const mutatedSecretEnv = "CCR_RESUME_MUTATED_PROVIDER_TOKEN"
	const admittedSecret = "admitted-provider-secret"
	const mutatedSecret = "mutated-provider-secret"
	t.Setenv(admittedSecretEnv, admittedSecret)
	t.Setenv(mutatedSecretEnv, mutatedSecret)

	provider := newModelsServer(t, []string{"gpt-5"})
	database := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", database, "provider", "add", "litellm", "--base-url", provider.URL, "--api-key-env", admittedSecretEnv); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, "--db", database, "model", "add", "coder", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatal(err)
	}
	seedRecordedCoderResumeRoute(t, database)

	launcher := &fakeLauncher{pid: os.Getpid()}
	secrets := &fakeSecrets{
		values: map[string]string{"env:" + admittedSecretEnv: admittedSecret},
		onResolve: func(ref string) error {
			if ref != "env:"+admittedSecretEnv {
				return fmt.Errorf("resolved secret reference %q, want admitted provider reference", ref)
			}
			return updateResumeRouteProviderConfiguration(database, mutatedSecretEnv)
		},
	}
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		Secrets:          secrets,
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
		launchPrepared: func(resolved resolvedLaunch) error {
			if resolved.pinnedRoute == nil {
				t.Fatal("resume launch did not pin its admitted route")
			}
			return nil
		},
	}, "--db", database, "launch", "--resume-route", "--", "--resume", "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if secrets.resolveCount != 1 {
		t.Fatalf("resume route resolved provider secret %d times, want exactly once at admission", secrets.resolveCount)
	}
	if !strings.Contains(out, "Provider protocol=openai-compatible mode=degraded") {
		t.Fatalf("resume launch summary did not retain admitted provider compatibility:\n%s", out)
	}
	if strings.Contains(out, "Provider protocol=anthropic-compatible mode=full") {
		t.Fatalf("resume launch summary used provider configuration changed after admission:\n%s", out)
	}
	for _, output := range []string{out, errOut} {
		if strings.Contains(output, admittedSecret) || strings.Contains(output, mutatedSecret) {
			t.Fatalf("resume launch output leaked a provider secret: %q", output)
		}
	}
	for _, path := range mustGlobResumeRouteDatabaseArtifacts(t, database) {
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, secretValue := range []string{admittedSecret, mutatedSecret} {
			if strings.Contains(string(contents), secretValue) {
				t.Fatalf("resume route database artifact %q stored provider secret %q", path, secretValue)
			}
		}
	}
	childEnvironment := environmentEntries(applyClaudeEnvironment(os.Environ(), launcher.env))
	for name, expected := range map[string]string{admittedSecretEnv: admittedSecret, mutatedSecretEnv: mutatedSecret} {
		if childEnvironment[name] != expected {
			t.Fatalf("resumed launch did not inherit caller variable %s", name)
		}
	}
}

func updateResumeRouteProviderConfiguration(database string, secretEnv string) error {
	ctx := context.Background()
	s, err := store.Open(ctx, database)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	provider, err := s.GetProvider(ctx, "litellm")
	if err != nil {
		return err
	}
	provider.SecretRef = "env:" + secretEnv
	provider.Protocol = "anthropic-compatible"
	provider.Mode = "full"
	_, err = s.UpdateProvider(ctx, provider)
	return err
}

func mustGlobResumeRouteDatabaseArtifacts(t *testing.T, database string) []string {
	t.Helper()
	paths, err := filepath.Glob(database + "*")
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestLaunchResumeRouteRejectsNonRoutableModelKindBeforeClaudeStarts(t *testing.T) {
	t.Parallel()
	database := filepath.Join(t.TempDir(), "ccr.db")
	seedResumeRouteModel(t, database)
	seedRecordedCoderResumeRoute(t, database)

	ctx := context.Background()
	s, openErr := store.Open(ctx, database)
	if openErr != nil {
		t.Fatal(openErr)
	}
	model, modelErr := s.GetModel(ctx, "coder")
	if modelErr != nil {
		_ = s.Close()
		t.Fatal(modelErr)
	}
	model.CapabilityOverrides.Kind = modelcap.KindEmbedding
	if updateErr := s.UpdateModel(ctx, model); updateErr != nil {
		_ = s.Close()
		t.Fatal(updateErr)
	}
	if closeErr := s.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, launchErr := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", database, "launch", "--resume-route", "--", "--resume=native-session")
	if launchErr == nil || !strings.Contains(launchErr.Error(), `non-routable model kind "embedding"`) {
		t.Fatalf("launch error = %v", launchErr)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchResumeRouteRejectsMissingRouteBeforeClaudeStarts(t *testing.T) {
	t.Parallel()
	database := filepath.Join(t.TempDir(), "ccr.db")
	seedBlankResumeSession(t, database, "native-session")
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher:         launcher,
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
	}, "--db", database, "launch", "--resume-route", "--", "--resume=native-session")
	if err == nil || !strings.Contains(err.Error(), "no observed route") {
		t.Fatalf("launch error = %v", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchResumeRouteRejectsConfigurationDriftBeforeClaudeStarts(t *testing.T) {
	t.Parallel()
	database := filepath.Join(t.TempDir(), "ccr.db")
	seedResumeRouteModel(t, database)
	seedRecordedCoderResumeRoute(t, database)
	ctx := context.Background()
	s, openErr := store.Open(ctx, database)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = s.Close() }()
	model, modelErr := s.GetModel(ctx, "coder")
	if modelErr != nil {
		t.Fatal(modelErr)
	}
	model.ProviderModel = "changed-model"
	if updateErr := s.UpdateModel(ctx, model); updateErr != nil {
		t.Fatal(updateErr)
	}
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, launchErr := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", database, "launch", "--resume-route", "--", "--resume=native-session")
	if launchErr == nil || !strings.Contains(launchErr.Error(), "no longer matches its current provider configuration") {
		t.Fatalf("launch error = %v", launchErr)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchResumeRouteRejectsProtocolDriftBeforeClaudeStarts(t *testing.T) {
	t.Parallel()
	database := filepath.Join(t.TempDir(), "ccr.db")
	seedResumeRouteModel(t, database)
	seedRecordedCoderResumeRoute(t, database)
	ctx := context.Background()
	s, openErr := store.Open(ctx, database)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = s.Close() }()
	provider, providerErr := s.GetProvider(ctx, "litellm")
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	provider.Protocol = "anthropic-compatible"
	if _, updateErr := s.UpdateProvider(ctx, provider); updateErr != nil {
		t.Fatal(updateErr)
	}
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, launchErr := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", database, "launch", "--resume-route", "--", "--resume=native-session")
	if launchErr == nil || !strings.Contains(launchErr.Error(), "no longer matches its current protocol") {
		t.Fatalf("launch error = %v", launchErr)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
}

func TestLaunchResumeRouteRejectsConfigurationIdentityDriftBeforeClaudeStarts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *store.Store) error
	}{
		{
			name: "provider_endpoint",
			mutate: func(ctx context.Context, s *store.Store) error {
				return mutateResumeProvider(ctx, s, func(provider *store.Provider) {
					provider.BaseURL = "http://127.0.0.1:65535"
				})
			},
		},
		{
			name: "provider_type",
			mutate: func(ctx context.Context, s *store.Store) error {
				return mutateResumeProvider(ctx, s, func(provider *store.Provider) {
					provider.Type = "local"
				})
			},
		},
		{
			name: "provider_capability",
			mutate: func(ctx context.Context, s *store.Store) error {
				return mutateResumeProvider(ctx, s, func(provider *store.Provider) {
					provider.SupportsTools = !provider.SupportsTools
				})
			},
		},
		{
			name: "provider_secret_reference",
			mutate: func(ctx context.Context, s *store.Store) error {
				return mutateResumeProvider(ctx, s, func(provider *store.Provider) {
					provider.SecretRef = "env:CCR_CHANGED_PROVIDER_TOKEN"
				})
			},
		},
		{
			name: "effective_model_capability",
			mutate: func(ctx context.Context, s *store.Store) error {
				model, err := s.GetModel(ctx, "coder")
				if err != nil {
					return err
				}
				model.CapabilityOverrides.SupportsTools = modelcap.Bool(false)
				return s.UpdateModel(ctx, model)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := filepath.Join(t.TempDir(), "ccr.db")
			seedResumeRouteModel(t, database)
			seedRecordedCoderResumeRoute(t, database)

			ctx := context.Background()
			s, openErr := store.Open(ctx, database)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if mutateErr := test.mutate(ctx, s); mutateErr != nil {
				_ = s.Close()
				t.Fatal(mutateErr)
			}
			if closeErr := s.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}

			launcher := &fakeLauncher{pid: os.Getpid()}
			_, _, launchErr := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", database, "launch", "--resume-route", "--", "--resume=native-session")
			if launchErr == nil || !strings.Contains(launchErr.Error(), "no longer matches its current provider configuration") {
				t.Fatalf("launch error = %v", launchErr)
			}
			if launcher.starts != 0 {
				t.Fatalf("launcher starts = %d, want 0", launcher.starts)
			}
		})
	}
}

func mutateResumeProvider(ctx context.Context, s *store.Store, mutate func(*store.Provider)) error {
	provider, err := s.GetProvider(ctx, "litellm")
	if err != nil {
		return err
	}
	mutate(&provider)
	_, err = s.UpdateProvider(ctx, provider)
	return err
}

func seedResumeRouteModel(t *testing.T, database string) {
	t.Helper()
	server := newModelsServer(t, []string{"gpt-5"})
	if _, _, err := runCommand(t, "--db", database, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, "--db", database, "model", "add", "coder", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatal(err)
	}
}

func seedBlankResumeSession(t *testing.T, database, nativeSessionID string) {
	t.Helper()
	ctx := context.Background()
	s, openErr := store.Open(ctx, database)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = s.Close() }()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	launchID, createErr := s.CreateLaunchWithAuth(ctx, "", "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	if _, upsertErr := s.UpsertClaudeSession(ctx, store.ClaudeSession{LaunchID: launchID, ClaudeSessionID: nativeSessionID, Source: "hook-recovery"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
}

func seedRecordedCoderResumeRoute(t *testing.T, database string) {
	t.Helper()
	const (
		alias         = "coder"
		providerName  = "litellm"
		providerModel = "gpt-5"
	)
	ctx := context.Background()
	s, openErr := store.Open(ctx, database)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = s.Close() }()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	fingerprint := resumeRouteFingerprint(t, ctx, s, alias, providerName)
	launchID, createErr := s.CreateLaunchWithAuth(ctx, alias, "pending", "pending", "provider-only", "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	sessionID, upsertErr := s.UpsertClaudeSession(ctx, store.ClaudeSession{LaunchID: launchID, ClaudeSessionID: "native-session", Source: "hook-recovery"})
	if upsertErr != nil {
		t.Fatal(upsertErr)
	}
	if updateErr := s.UpdateClaudeSessionRoute(ctx, sessionID, "registered", alias, providerName, providerModel, "openai-compatible", fingerprint); updateErr != nil {
		t.Fatal(updateErr)
	}
}

func resumeRouteFingerprint(t *testing.T, ctx context.Context, s *store.Store, alias, providerName string) string {
	t.Helper()
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := s.GetProvider(ctx, providerName)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := routeidentity.Fingerprint(model, provider, effective.Values)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}
