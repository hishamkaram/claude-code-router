//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const liveFixtureAPIKey = "ccr-live-fixture-api-key"

func TestLiveFixtureMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")

	protocols, err := selectedLiveFixtureProtocols(os.Getenv("CCR_LIVE_FIXTURE_PROTOCOL"))
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range protocols {
		protocol := protocol
		t.Run(protocol, func(t *testing.T) {
			isolatedHome := t.TempDir()
			t.Setenv("HOME", isolatedHome)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(isolatedHome, ".claude"))
			runLiveFixtureProtocol(t, ctx, protocol)
		})
	}
}

func selectedLiveFixtureProtocols(selected string) ([]string, error) {
	switch selected {
	case "":
		return []string{"openai-chat", "anthropic-native", "openai-responses"}, nil
	case "openai":
		return []string{"openai-chat"}, nil
	case "anthropic":
		return []string{"anthropic-native"}, nil
	case "openai-chat", "anthropic-native", "openai-responses":
		return []string{selected}, nil
	default:
		return nil, fmt.Errorf("invalid CCR_LIVE_FIXTURE_PROTOCOL %q; expected openai-chat, anthropic-native, or openai-responses", selected)
	}
}

func runLiveFixtureProtocol(t *testing.T, ctx context.Context, protocol string) {
	t.Helper()
	existingHook := installLiveExistingHook(t)
	fixture := newLiveMatrixFixture(t, protocol)
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	deps := liveMatrixDependencies(fixture)
	configureLiveMatrixModels(t, ctx, deps, dbPath, fixture.URL(), protocol)

	input := liveStreamInput(
		t,
		"/model anthropic.ccr.fixture-full[1m]",
		"Reply with the configured alias fixture response.",
		"/model sonnet",
		"Reply exactly CCR_LIVE_ANTHROPIC.",
		"/model anthropic.ccr.fixture-full[1m]",
		"Reply with the configured alias fixture response again.",
	)
	launchDeps := deps
	launchDeps.In = strings.NewReader(input)
	out, errOut, err := runLiveCommand(
		ctx, launchDeps,
		"--db", dbPath, "launch", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	)
	if err != nil {
		t.Fatalf("streaming model-switch launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{"CCR_LIVE_ALIAS", "CCR_LIVE_ANTHROPIC"} {
		if !strings.Contains(out, want) {
			t.Fatalf("streaming launch output missing %q\nstdout:\n%s\nstderr:\n%s", want, out, errOut)
		}
	}
	fixture.assertSwitching(t, out, errOut)
	existingHook.assertExecutedAndPreserved(t)
	assertLiveMatrixVisibility(t, ctx, dbPath)
	assertLiveDatabaseRedaction(t, dbPath, []string{
		liveFixtureAPIKey, "Reply with the configured alias fixture response", "CCR_LIVE_ALIAS",
	})

	runLiveMatrixMode(t, ctx, deps, fixture, dbPath, "fixture-degraded", "CCR_LIVE_DEGRADED", true)
	runLiveMatrixMode(t, ctx, deps, fixture, dbPath, "fixture-chat", "CCR_LIVE_CHAT", false)
}

type liveExistingHook struct {
	markerPath   string
	settingsPath string
	original     []byte
}

func installLiveExistingHook(t *testing.T) liveExistingHook {
	t.Helper()
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		t.Fatal("CLAUDE_CONFIG_DIR is required for the live hook fixture")
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", configDir, err)
	}
	markerPath := filepath.Join(t.TempDir(), "existing-session-start-hook")
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "printf preserved > " + quoteLiveShellArg(markerPath),
						},
					},
				},
			},
		},
	}
	original, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("Marshal(existing hook settings) error = %v", err)
	}
	original = append(original, '\n')
	settingsPath := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(settingsPath, original, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", settingsPath, err)
	}
	return liveExistingHook{markerPath: markerPath, settingsPath: settingsPath, original: original}
}

func (hook liveExistingHook) assertExecutedAndPreserved(t *testing.T) {
	t.Helper()
	marker, err := os.ReadFile(hook.markerPath)
	if err != nil || string(marker) != "preserved" {
		t.Fatalf("existing SessionStart hook marker = %q, %v", marker, err)
	}
	settings, err := os.ReadFile(hook.settingsPath)
	if err != nil || !bytes.Equal(settings, hook.original) {
		t.Fatalf("existing Claude settings changed: %q, %v", settings, err)
	}
}

func quoteLiveShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func liveMatrixDependencies(fixture *liveMatrixFixture) Dependencies {
	return Dependencies{
		StartGateway: func(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			cfg.AnthropicBaseURL = fixture.URL()
			return gateway.Start(ctx, cfg)
		},
	}
}

func configureLiveMatrixModels(t *testing.T, ctx context.Context, deps Dependencies, dbPath, baseURL, protocol string) {
	t.Helper()
	providerType := "litellm"
	if protocol == "anthropic-native" {
		providerType = "anthropic"
	}
	for _, entry := range []struct {
		name string
		mode string
	}{
		{name: "fixture-full", mode: "full"},
		{name: "fixture-degraded", mode: "degraded"},
		{name: "fixture-chat", mode: "chat-only"},
	} {
		commands := [][]string{
			{"--db", dbPath, "provider", "add", entry.name, "--type", providerType, "--base-url", baseURL, "--no-api-key", "--mode", entry.mode},
			{"--db", dbPath, "model", "add", entry.name, "--provider", entry.name, "--model", entry.name + "-model", "--compat", entry.mode},
		}
		if protocol == "openai-responses" {
			commands[0] = append(commands[0], "--responses")
			commands = append(commands, []string{"--db", dbPath, "model", "update", entry.name, "--model-kind", "responses", "--responses", "true"})
		}
		if entry.name == "fixture-full" {
			commands = append(commands,
				[]string{"--db", dbPath, "model", "update", entry.name, "--context-window", "1000000"})
		}
		for _, args := range commands {
			out, errOut, err := runLiveCommand(ctx, deps, args...)
			if err != nil {
				t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
			}
		}
	}
}

func runLiveMatrixMode(t *testing.T, ctx context.Context, deps Dependencies, fixture *liveMatrixFixture, dbPath, alias, response string, toolsExpected bool) {
	t.Helper()
	launchDeps := deps
	launchDeps.In = strings.NewReader("Return the fixture response.\n")
	out, errOut, err := runLiveCommand(
		ctx, launchDeps,
		"--db", dbPath, "launch", "--model", alias, "--print", "--auth-mode", "gateway-token",
	)
	if err != nil {
		t.Fatalf("%s launch error = %v\nstdout:\n%s\nstderr:\n%s", alias, err, out, errOut)
	}
	expectedMode := strings.TrimPrefix(alias, "fixture-")
	if expectedMode == "chat" {
		expectedMode = "chat-only"
	}
	if !strings.Contains(out, response) || !strings.Contains(errOut, "mode="+expectedMode) {
		t.Fatalf("%s launch output mismatch\nstdout:\n%s\nstderr:\n%s", alias, out, errOut)
	}
	if got := fixture.toolsSeen(alias + "-model"); got != toolsExpected {
		t.Fatalf("%s tools seen = %v, want %v", alias, got, toolsExpected)
	}
}

func liveStreamInput(t *testing.T, messages ...string) string {
	t.Helper()
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, message := range messages {
		payload := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": message,
			},
			"parent_tool_use_id": nil,
		}
		if err := encoder.Encode(payload); err != nil {
			t.Fatalf("encoding stream input: %v", err)
		}
	}
	return input.String()
}

func assertLiveMatrixVisibility(t *testing.T, ctx context.Context, dbPath string) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 1 {
		t.Fatalf("ListLaunches() = %#v, %v", launches, err)
	}
	launch := launches[0]
	if launch.State != "completed" || launch.LifecycleState != "observed" || launch.StatuslineState != "injected" {
		t.Fatalf("launch visibility state = %#v", launch)
	}
	sessions, err := s.ListClaudeSessions(ctx, launch.ID, false)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("ListClaudeSessions() = %#v, %v", sessions, err)
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 500})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	var aliasRoutes, anthropicRoutes int
	var sessionEnd bool
	for _, event := range events {
		switch {
		case event.Kind == "route" && event.Route.RouteKind == "registered" && event.Route.ModelAlias == "fixture-full" && event.Status == "succeeded":
			aliasRoutes++
		case event.Kind == "route" && event.Route.RouteKind == "first-party-anthropic" && event.Status == "succeeded":
			anthropicRoutes++
		case event.Kind == "lifecycle" && event.Name == "SessionEnd":
			sessionEnd = true
		}
	}
	if aliasRoutes < 2 || anthropicRoutes < 1 || !sessionEnd {
		t.Fatalf("visibility evidence incomplete: aliasRoutes=%d anthropicRoutes=%d sessionEnd=%v events=%#v", aliasRoutes, anthropicRoutes, sessionEnd, events)
	}
}

func assertLiveDatabaseRedaction(t *testing.T, dbPath string, forbidden []string) {
	t.Helper()
	paths, err := filepath.Glob(dbPath + "*")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		for _, value := range forbidden {
			if bytes.Contains(contents, []byte(value)) {
				t.Fatalf("runtime database artifact %s contains forbidden content %q", path, value)
			}
		}
	}
}

func assertLiveAgentVisibility(t *testing.T, ctx context.Context, dbPath string) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) == 0 {
		t.Fatalf("ListLaunches() = %#v, %v", launches, err)
	}
	launch := launches[0]
	agents, err := s.ListRuntimeAgents(ctx, launch.ID, 0, false)
	if err != nil || len(agents) == 0 {
		t.Fatalf("ListRuntimeAgents() = %#v, %v", agents, err)
	}
	completedAgent := false
	for _, agent := range agents {
		completedAgent = completedAgent || agent.Status == "completed"
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 500})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	var subagentStart, subagentStop bool
	for _, event := range events {
		subagentStart = subagentStart || event.Kind == "lifecycle" && event.Name == "SubagentStart"
		subagentStop = subagentStop || event.Kind == "lifecycle" && event.Name == "SubagentStop"
	}
	if !completedAgent || !subagentStart || !subagentStop {
		t.Fatalf("subagent lifecycle evidence incomplete: completed=%v start=%v stop=%v agents=%#v events=%#v", completedAgent, subagentStart, subagentStop, agents, events)
	}
}
