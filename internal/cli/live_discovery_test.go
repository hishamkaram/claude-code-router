//go:build live

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveGatewayTokenLaunchDiscoversConfiguredAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	chatCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled = true
			payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
			if !ok {
				return
			}
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-discovery", "gateway-discovery-ok", 4, 2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled || !strings.Contains(out, "gateway-discovery-ok") {
		t.Fatalf("launch did not complete through fake provider\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}

	cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("native Claude gateway discovery cache was modified: %v", err)
	}
}

func TestLiveAutoProviderAliasIgnoresDetectedClaudeAuth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	isolateLiveClaudeAuth(t)
	t.Setenv("CCR_TEST_PROVIDER_TOKEN", "live-provider-token")
	var chatCalled atomic.Bool
	var providerCredentialSeen atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") == "Bearer live-provider-token" {
				providerCredentialSeen.Store(true)
			} else {
				t.Errorf("provider model discovery did not use the configured provider credential")
				http.Error(w, "missing provider credential", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") == "Bearer live-provider-token" {
				providerCredentialSeen.Store(true)
			} else {
				t.Errorf("provider chat request did not use the configured provider credential")
				http.Error(w, "missing provider credential", http.StatusUnauthorized)
				return
			}
			chatCalled.Store(true)
			payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
			if !ok {
				return
			}
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-auto-provider-only", "auto-provider-only-ok", 4, 2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", provider.URL, "--api-key-env", "CCR_TEST_PROVIDER_TOKEN"},
		{"--db", dbPath, "model", "add", "litellm-glm-5-3-1m", "--provider", "litellm", "--model", "gpt-5"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		In:               strings.NewReader("hello\n"),
		DetectClaudeAuth: func(context.Context) (bool, error) { return true, nil },
	}, "--db", dbPath, "launch", "--model", "litellm-glm-5-3-1m", "--print")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled.Load() || !strings.Contains(out, "auto-provider-only-ok") {
		t.Fatalf("launch did not complete through fake provider\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !providerCredentialSeen.Load() {
		t.Fatalf("provider did not receive its configured credential")
	}
	if strings.Contains(out+errOut, "claude /login") || strings.Contains(out+errOut, "Please run /login") {
		t.Fatalf("provider-backed launch prompted for Claude subscription login\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "Provider-only auth is active for this launch") {
		t.Fatalf("launch summary missing provider-only auth mode:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	statusOut, statusErrOut, statusErr := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "status")
	if statusErr != nil {
		t.Fatalf("status error = %v\nstdout:\n%s\nstderr:\n%s", statusErr, statusOut, statusErrOut)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=provider-only") {
		t.Fatalf("status output missing resolved provider-only auth mode:\n%s", statusOut)
	}
	doctorOut, doctorErrOut, doctorErr := runLiveCommand(ctx, Dependencies{}, "--db", dbPath, "doctor")
	if doctorErr != nil {
		t.Fatalf("doctor error = %v\nstdout:\n%s\nstderr:\n%s", doctorErr, doctorOut, doctorErrOut)
	}
	if !strings.Contains(doctorOut, "Launch auth: mode=provider-only") {
		t.Fatalf("doctor output missing resolved provider-only auth mode:\n%s", doctorOut)
	}
}

func TestLiveAutoNoClaudeAuthWithoutModelFailsBeforeClaudeStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	isolateLiveClaudeAuth(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	launcher := &fakeLauncher{pid: 12345}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch")
	if err == nil {
		t.Fatalf("launch succeeded, want no-auth/no-model preflight failure\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if launcher.starts != 0 {
		t.Fatalf("Claude launcher started %d times, want preflight failure before start", launcher.starts)
	}
	for _, want := range []string{
		"claude subscription authentication was not found",
		"CCR will not choose a provider automatically",
		"ccr launch --model gpt",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("launch error missing %q:\n%v", want, err)
		}
	}
}

func isolateLiveClaudeAuth(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	for _, name := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
	} {
		t.Setenv(name, "")
	}
}
