//go:build live && (linux || darwin)

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	liveEnvironmentGenericName  = "CCR_LIVE_PLUGIN_GENERIC_TOKEN"
	liveEnvironmentGenericValue = "ccr-synthetic-plugin-token"
	liveEnvironmentSettingsName = "CCR_LIVE_PLUGIN_SETTINGS_VALUE"
)

type liveEnvironmentFixture struct {
	plugin  string
	reports string
	project string
}

func newLiveEnvironmentFixture(t *testing.T) liveEnvironmentFixture {
	t.Helper()
	return newLiveEnvironmentFixtureForProject(t, t.TempDir())
}

func newLiveEnvironmentFixtureForProject(t *testing.T, project string) liveEnvironmentFixture {
	t.Helper()
	// Claude keys project trust by its physical working directory. Keep one
	// canonical path for both that key and the workload launch directory.
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatalf("resolve live environment project: %v", err)
	}
	isolateLiveResumeRouteState(t)
	fixture := liveEnvironmentFixture{plugin: t.TempDir(), reports: t.TempDir(), project: canonical}
	t.Chdir(fixture.project)
	t.Setenv(liveResumeRouteProviderSecretEnv, liveResumeRouteProviderSecret)
	t.Setenv(liveEnvironmentGenericName, liveEnvironmentGenericValue)
	t.Setenv(liveEnvironmentSettingsName, "shell-fixture")
	t.Setenv("CCR_LIVE_ENV_EMPTY", "")
	t.Setenv("CCR_LIVE_ENV_EXPECT_HOME", os.Getenv("HOME"))
	t.Setenv("CCR_LIVE_ENV_EXPECT_CONFIG", os.Getenv("CLAUDE_CONFIG_DIR"))
	t.Setenv("CCR_LIVE_ENV_REPORT_DIR", fixture.reports)
	t.Setenv("CLAUDE_CODE_SIMPLE", "")
	t.Setenv("CLAUDE_CODE_SUBPROCESS_ENV_SCRUB", "")
	fixture.writePlugin(t)
	writeLiveEnvironmentJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), struct {
		Env map[string]string `json:"env"`
	}{Env: map[string]string{liveEnvironmentSettingsName: "settings-fixture"}})
	type projectTrust struct {
		Accepted bool `json:"hasTrustDialogAccepted"`
		Seen     int  `json:"projectOnboardingSeenCount"`
	}
	writeLiveEnvironmentJSON(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), struct {
		Onboarded bool                    `json:"hasCompletedOnboarding"`
		Projects  map[string]projectTrust `json:"projects"`
	}{Onboarded: true, Projects: map[string]projectTrust{fixture.project: {Accepted: true, Seen: 1}}})
	return fixture
}

func TestLiveFixtureEnvironmentCanonicalProjectTrust(t *testing.T) {
	project := t.TempDir()
	alias := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatalf("create project path alias: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatalf("resolve fixture project: %v", err)
	}
	fixture := newLiveEnvironmentFixtureForProject(t, alias)
	data, err := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"))
	if err != nil {
		t.Fatalf("read synthetic project trust: %v", err)
	}
	var state struct {
		Projects map[string]struct {
			Accepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode synthetic project trust: %v", err)
	}
	if fixture.project != canonical || !state.Projects[canonical].Accepted || len(state.Projects) != 1 {
		t.Fatal("fixture launch directory and its sole trust key must use the canonical project path")
	}
}

func (fixture liveEnvironmentFixture) writePlugin(t *testing.T) {
	t.Helper()
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve live environment test binary")
	}
	writeLiveEnvironmentJSON(t, filepath.Join(fixture.plugin, ".claude-plugin", "plugin.json"), struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}{Name: "ccr-env-fixture", Version: "1.0.0"})
	for _, role := range []string{"Hook", "MCP"} {
		path := filepath.Join(fixture.plugin, "env-"+role)
		script := "#!/bin/sh\nCCR_LIVE_ENV_HELPER=1 exec " + quoteLiveShellArg(binary) + " -test.run='^TestLiveEnvironment" + role + "Process$'\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal("write live environment helper")
		}
	}
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []hook `json:"hooks"`
	}
	writeLiveEnvironmentJSON(t, filepath.Join(fixture.plugin, "hooks", "hooks.json"), struct {
		Hooks map[string][]matcher `json:"hooks"`
	}{Hooks: map[string][]matcher{"SessionStart": {{Hooks: []hook{{Type: "command", Command: quoteLiveShellArg(filepath.Join(fixture.plugin, "env-Hook"))}}}}}})
	type server struct {
		Command string `json:"command"`
	}
	writeLiveEnvironmentJSON(t, filepath.Join(fixture.plugin, ".mcp.json"), map[string]server{
		"environment": {Command: filepath.Join(fixture.plugin, "env-MCP")},
	})
}

func writeLiveEnvironmentJSON[T any](t *testing.T, path string, value T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create synthetic fixture directory: %v", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode synthetic fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write synthetic fixture: %v", err)
	}
}

func writeLiveEnvironmentReport(t *testing.T, role string) {
	t.Helper()
	empty, present := os.LookupEnv("CCR_LIVE_ENV_EMPTY")
	writeLiveEnvironmentJSON(t, filepath.Join(os.Getenv("CCR_LIVE_ENV_REPORT_DIR"), role+".json"), map[string]bool{
		"generic":         os.Getenv(liveEnvironmentGenericName) == liveEnvironmentGenericValue,
		"shared_provider": os.Getenv(liveResumeRouteProviderSecretEnv) == liveResumeRouteProviderSecret,
		"home":            os.Getenv("HOME") == os.Getenv("CCR_LIVE_ENV_EXPECT_HOME"),
		"config":          os.Getenv("CLAUDE_CONFIG_DIR") == os.Getenv("CCR_LIVE_ENV_EXPECT_CONFIG"),
		"empty":           present && empty == "",
		"native_settings": os.Getenv(liveEnvironmentSettingsName) == "settings-fixture",
	})
}

func (fixture liveEnvironmentFixture) assertReports(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, role := range []string{"hook", "mcp", "grandchild"} {
		path := filepath.Join(fixture.reports, role+".json")
		for {
			data, err := os.ReadFile(path)
			if err == nil {
				assertLiveEnvironmentReport(t, path, role, data)
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read %s environment report: %v", role, err)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s environment report was not produced: %v", role, ctx.Err())
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
}

func assertLiveEnvironmentReport(t *testing.T, path, role string, data []byte) {
	t.Helper()
	var report map[string]bool
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode %s boolean report: %v", role, err)
	}
	if len(report) != 6 {
		t.Fatalf("%s environment report has unexpected field count", role)
	}
	for name, ok := range report {
		if !ok {
			t.Errorf("%s did not preserve %s environment behavior", role, name)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s report permissions are not private", role)
	}
}

func TestLiveEnvironmentHookProcess(t *testing.T) {
	if os.Getenv("CCR_LIVE_ENV_HELPER") != "1" {
		return
	}
	writeLiveEnvironmentReport(t, "hook")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("resolve synthetic grandchild executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, binary, "-test.run=^TestLiveEnvironmentGrandchildProcess$")
	if err := child.Run(); err != nil {
		t.Fatalf("run synthetic grandchild: %v", err)
	}
	os.Exit(0)
}

func TestLiveEnvironmentGrandchildProcess(t *testing.T) {
	if os.Getenv("CCR_LIVE_ENV_HELPER") != "1" {
		return
	}
	writeLiveEnvironmentReport(t, "grandchild")
	os.Exit(0)
}

func TestLiveEnvironmentMCPProcess(t *testing.T) {
	if os.Getenv("CCR_LIVE_ENV_HELPER") != "1" {
		return
	}
	writeLiveEnvironmentReport(t, "mcp")
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request liveMCPRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				os.Exit(0)
			}
			t.Fatalf("decode synthetic MCP request: %v", err)
		}
		result, err := liveEnvironmentMCPResult(request)
		if err != nil {
			t.Fatalf("build synthetic MCP response: %v", err)
		}
		if result == nil {
			continue
		}
		if err := writeLiveMCPResponse(encoder, request.ID, result); err != nil {
			t.Fatalf("write synthetic MCP response: %v", err)
		}
	}
}

func liveEnvironmentMCPResult(request liveMCPRequest) (json.RawMessage, error) {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, err
		}
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2024-11-05"
		}
		return json.Marshal(struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct{} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		}{ProtocolVersion: params.ProtocolVersion, ServerInfo: struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{Name: "ccr-env-fixture", Version: "1.0.0"}})
	case "tools/list":
		return json.RawMessage(`{"tools":[{"name":"environment_check","description":"Check synthetic fixture environment","inputSchema":{"type":"object","properties":{}}}]}`), nil
	case "tools/call":
		return json.RawMessage(`{"content":[{"type":"text","text":"synthetic environment checked"}]}`), nil
	case "ping":
		return json.RawMessage(`{}`), nil
	default:
		return nil, nil
	}
}
