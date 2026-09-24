//go:build live

package cli

import (
	"bytes"
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
	cache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("reading Claude gateway discovery cache: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !bytes.Contains(cache, []byte("anthropic.ccr.gpt")) {
		t.Fatalf("gateway discovery cache does not include configured alias: %s", cache)
	}
}

func TestLiveAutoNoClaudeAuthLaunchesProviderOnlyConfiguredAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	isolateLiveClaudeAuth(t)
	var chatCalled atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
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
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		In: strings.NewReader("hello\n"),
	}, "--db", dbPath, "launch", "--model", "gpt", "--print")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled.Load() || !strings.Contains(out, "auto-provider-only-ok") {
		t.Fatalf("launch did not complete through fake provider\nstdout:\n%s\nstderr:\n%s", out, errOut)
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
