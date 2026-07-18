package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLaunchInjectsLifecycleHooksAndStatuslineWithoutWritingSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	settingsJSON, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch args missing --settings: %#v", launcher.args)
	}
	var settings struct {
		Hooks      map[string][]claudeHookMatcher `json:"hooks"`
		StatusLine map[string]any                 `json:"statusLine"`
	}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		t.Fatalf("settings decode error = %v", err)
	}
	for _, event := range []string{
		"SessionStart", "SessionEnd", "SubagentStart", "SubagentStop",
		"TaskCreated", "TaskCompleted", "TeammateIdle", "StopFailure",
	} {
		matchers := settings.Hooks[event]
		if len(matchers) != 1 || len(matchers[0].Hooks) != 1 {
			t.Fatalf("hook %s = %#v", event, matchers)
		}
		hook := matchers[0].Hooks[0]
		if hook.Type != "http" || !strings.HasSuffix(hook.URL, "/internal/v1/hooks") ||
			hook.Headers[observerTokenHeader] != "${CCR_OBSERVER_TOKEN}" ||
			len(hook.AllowedEnvVars) != 1 || hook.AllowedEnvVars[0] != statuslineTokenEnv {
			t.Fatalf("hook %s = %#v", event, hook)
		}
	}
	if settings.StatusLine["type"] != "command" || !strings.Contains(settings.StatusLine["command"].(string), "__statusline") {
		t.Fatalf("statusLine = %#v", settings.StatusLine)
	}
	observerToken, ok := launcher.envValue(statuslineTokenEnv)
	if !ok || observerToken == "" {
		t.Fatal("launch environment missing observer token")
	}
	if gatewayURL, ok := launcher.envValue(statuslineGatewayURLEnv); !ok || !strings.HasPrefix(gatewayURL, "http://127.0.0.1:") {
		t.Fatalf("launch gateway environment = %q, %v", gatewayURL, ok)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("launch wrote a user settings file: %v", err)
	}

	launches := readLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].State != "completed" ||
		launches[0].LifecycleState != "unobserved" || launches[0].StatuslineState != "injected" {
		t.Fatalf("launches = %#v", launches)
	}
	databaseBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile(database) error = %v", err)
	}
	if strings.Contains(string(databaseBytes), observerToken) {
		t.Fatal("database contains ephemeral observer token")
	}
}

func TestLaunchPreservesExistingStatusline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	existing := `{"statusLine":{"type":"command","command":"existing-status"},"hooks":{"SessionStart":[]}}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	settingsJSON, ok := launcher.settingsArgValue()
	if !ok {
		t.Fatalf("launch args missing --settings: %#v", launcher.args)
	}
	var generated map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsJSON), &generated); err != nil {
		t.Fatalf("generated settings decode error = %v", err)
	}
	if _, exists := generated["statusLine"]; exists {
		t.Fatalf("generated settings replaced existing statusLine: %s", settingsJSON)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil || string(data) != existing {
		t.Fatalf("existing settings changed: %q, %v", data, err)
	}
	launches := readLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].StatuslineState != "preserved" {
		t.Fatalf("launches = %#v", launches)
	}
}

func TestLaunchVisibilityOptOutsAreIndependent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath,
		"launch", "--no-history", "--no-lifecycle", "--no-statusline"); err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if settings, ok := launcher.settingsArgValue(); ok {
		t.Fatalf("opted-out launch generated settings %q", settings)
	}
	launches := readLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].LifecycleState != "disabled" ||
		launches[0].StatuslineState != "disabled" {
		t.Fatalf("launches = %#v", launches)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launches[0].ID})
	if err != nil || len(events) != 0 {
		t.Fatalf("ListTraceEvents() = %#v, %v", events, err)
	}
}

func TestLaunchStartupFailureIsFinalized(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err == nil {
		t.Fatal("launch error = nil, want malformed settings failure")
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	launches := readLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].State != "failed" || launches[0].EndReason != "startup_failed" || launches[0].EndedAt == "" {
		t.Fatalf("launches = %#v", launches)
	}
}

func TestLaunchRejectedSettingsOverrideHasNoRuntimeSideEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid()}
	gatewayStarts := 0
	deps := Dependencies{
		Launcher: launcher,
		StartGateway: func(context.Context, gateway.Config) (*gateway.Server, error) {
			gatewayStarts++
			return nil, errors.New("gateway should not start")
		},
	}
	_, _, err := runCommandWithDeps(t, deps, "--db", dbPath, "launch", "--settings", "./claude.json")
	if err == nil || !strings.Contains(err.Error(), "--settings cannot override") {
		t.Fatalf("launch error = %v, want settings override error", err)
	}
	if launcher.starts != 0 || gatewayStarts != 0 {
		t.Fatalf("launcher starts = %d, gateway starts = %d; want zero", launcher.starts, gatewayStarts)
	}
	if launches := readLaunches(t, dbPath); len(launches) != 0 {
		t.Fatalf("launches = %#v, want none", launches)
	}
}

func TestLaunchProcessFailureIsFinalized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	launcher := &fakeLauncher{pid: os.Getpid(), waitErr: errors.New("process failed")}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch"); err == nil {
		t.Fatal("launch error = nil, want process failure")
	}
	launches := readLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].State != "failed" || launches[0].EndReason != "process_error" {
		t.Fatalf("launches = %#v", launches)
	}
}

func TestFetchStatuslineUsesObserverEndpoint(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(observerTokenHeader) != "observer-token" {
			t.Fatalf("observer token header = %q", r.Header.Get(observerTokenHeader))
		}
		_ = json.NewEncoder(w).Encode(session.Snapshot{
			SchemaVersion: 1,
			Route:         session.Route{ModelAlias: "coder", ProviderName: "fixture", ProviderModel: "model-v1"},
			ActiveAgents:  2, ActiveTasks: 1,
			Observability: observability.Snapshot{Healthy: true},
		})
	}))
	defer server.Close()
	line, err := fetchStatusline(context.Background(), server.URL, "observer-token")
	if err != nil {
		t.Fatalf("fetchStatusline() error = %v", err)
	}
	if line != "CCR | coder | fixture/model-v1 | agents 2 | tasks 1" {
		t.Fatalf("fetchStatusline() = %q", line)
	}
	if _, err := statuslineEndpoint("https://example.com"); err == nil {
		t.Fatal("statuslineEndpoint(non-loopback) error = nil")
	}
}

func readLaunches(t *testing.T, dbPath string) []store.Launch {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		t.Fatalf("ListLaunches() error = %v", err)
	}
	return launches
}
