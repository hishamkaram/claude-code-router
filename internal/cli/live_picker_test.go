//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/xpty"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchModelPickerShowsAnthropicAndRegisteredModels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	configureIsolatedLivePickerClaude(t)

	routedModel := make(chan string, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"},{"id":"third-party-sonnet"},{"id":"third-party-opus"},{"id":"third-party-haiku"}]}`)
		case "/v1/chat/completions":
			var payload struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("provider decode error = %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			select {
			case routedModel <- payload.Model:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-picker","choices":[{"message":{"content":"picker-route-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLivePickerModels(t, ctx, dbPath, provider.URL)

	launcher := &livePickerLauncher{started: make(chan livePickerSession, 1)}
	var commandOut, commandErr bytes.Buffer
	commandDone := make(chan error, 1)
	go func() {
		cmd := NewRootCommand(ctx, Dependencies{
			In:       strings.NewReader(""),
			Out:      &commandOut,
			Err:      &commandErr,
			Launcher: launcher,
		})
		cmd.SetArgs([]string{"--db", dbPath, "launch", "--bare"})
		commandDone <- cmd.Execute()
	}()

	var session livePickerSession
	select {
	case session = <-launcher.started:
	case err := <-commandDone:
		t.Fatalf("ccr launch stopped before Claude Code started: %v\nstdout:\n%s\nstderr:\n%s", err, commandOut.String(), commandErr.String())
	case <-ctx.Done():
		t.Fatalf("waiting for Claude Code PTY: %v", ctx.Err())
	}
	defer func() {
		_ = session.process.Stop()
		_ = session.pty.Close()
	}()

	transcript := &synchronizedBuffer{}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(transcript, session.pty)
		readDone <- err
	}()

	waitForLivePickerText(t, ctx, transcript, commandDone, "Detected a custom API key")
	if _, err := session.pty.Write([]byte("\x1b[A\r")); err != nil {
		t.Fatalf("accepting isolated placeholder API key: %v", err)
	}
	waitForLivePickerText(t, ctx, transcript, commandDone, "Welcome back!")
	if _, err := session.pty.Write([]byte("/model\r")); err != nil {
		t.Fatalf("opening /model picker: %v", err)
	}
	waitForLivePickerText(t, ctx, transcript, commandDone,
		"Select model",
		"Opus",
		"Sonnet",
		"Haiku",
		"anthropic.ccr.gpt",
		"anthropic.ccr.s%6fnnet",
		"anthropic.ccr.%6fpus",
		"anthropic.ccr.h%61iku",
	)

	if _, err := session.pty.Write([]byte("\x1b")); err != nil {
		t.Fatalf("closing /model picker: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := session.pty.Write([]byte("/model anthropic.ccr.gpt\r")); err != nil {
		t.Fatalf("selecting registered model by ID: %v", err)
	}
	waitForLivePickerText(t, ctx, transcript, commandDone, "Set model to anthropic.ccr.gpt")
	if _, err := session.pty.Write([]byte("Reply with the routed test response.\r")); err != nil {
		t.Fatalf("sending routed prompt: %v", err)
	}

	select {
	case got := <-routedModel:
		if got != "gpt-5" {
			t.Fatalf("picker routed provider model = %q, want gpt-5\ntranscript:\n%s", got, ansi.Strip(transcript.String()))
		}
	case err := <-commandDone:
		t.Fatalf("ccr launch stopped before routing picker selection: %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s", err, commandOut.String(), commandErr.String(), ansi.Strip(transcript.String()))
	case <-ctx.Done():
		t.Fatalf("waiting for picker-selected route: %v\ntranscript:\n%s", ctx.Err(), ansi.Strip(transcript.String()))
	}
	time.Sleep(500 * time.Millisecond)

	if _, err := session.pty.Write([]byte("/exit\r")); err != nil {
		t.Fatalf("exiting Claude Code: %v", err)
	}
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatalf("ccr launch error = %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s", err, commandOut.String(), commandErr.String(), ansi.Strip(transcript.String()))
		}
	case <-ctx.Done():
		t.Fatalf("waiting for Claude Code exit: %v\ntranscript:\n%s", ctx.Err(), ansi.Strip(transcript.String()))
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
	}
}

func configureIsolatedLivePickerClaude(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	configDir := filepath.Join(home, "claude-config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("creating isolated Claude config: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading working directory: %v", err)
	}
	state := map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			cwd: map[string]any{
				"hasTrustDialogAccepted":     true,
				"projectOnboardingSeenCount": 1,
			},
		},
	}
	writeLivePickerJSON(t, filepath.Join(configDir, ".claude.json"), state)
	writeLivePickerJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{"theme": "dark"})

	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("ANTHROPIC_API_KEY", "ccr-live-picker-placeholder")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	t.Setenv("DISABLE_TELEMETRY", "1")
}

func writeLivePickerJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func addLivePickerModels(t *testing.T, ctx context.Context, dbPath, baseURL string) {
	t.Helper()
	commands := [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", baseURL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "gpt", "--provider", "litellm", "--model", "gpt-5"},
		{"--db", dbPath, "model", "add", "sonnet", "--provider", "litellm", "--model", "third-party-sonnet"},
		{"--db", dbPath, "model", "add", "opus", "--provider", "litellm", "--model", "third-party-opus"},
		{"--db", dbPath, "model", "add", "haiku", "--provider", "litellm", "--model", "third-party-haiku"},
	}
	for _, args := range commands {
		out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...)
		if err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}
}

type livePickerLauncher struct {
	started chan livePickerSession
}

type livePickerSession struct {
	pty     xpty.Pty
	process *livePickerProcess
}

func (l *livePickerLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, _ io.Reader, _, _ io.Writer) (ClaudeProcess, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("finding Claude Code: %w", err)
	}
	pty, err := xpty.NewPty(140, 60)
	if err != nil {
		return nil, fmt.Errorf("creating Claude Code PTY: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = applyClaudeEnvironment(os.Environ(), env)
	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		return nil, fmt.Errorf("starting Claude Code in PTY: %w", err)
	}
	process := &livePickerProcess{ctx: ctx, cmd: cmd, pty: pty}
	l.started <- livePickerSession{pty: pty, process: process}
	return process, nil
}

type livePickerProcess struct {
	ctx      context.Context
	cmd      *exec.Cmd
	pty      xpty.Pty
	waitOnce sync.Once
	waitErr  error
}

func (p *livePickerProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *livePickerProcess) Wait() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		p.waitErr = xpty.WaitProcess(p.ctx, p.cmd)
		_ = p.pty.Close()
	})
	if p.waitErr != nil {
		return fmt.Errorf("waiting for Claude Code in PTY: %w", p.waitErr)
	}
	return nil
}

func (p *livePickerProcess) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stopping Claude Code in PTY: %w", err)
	}
	return nil
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLivePickerText(t *testing.T, ctx context.Context, transcript *synchronizedBuffer, commandDone <-chan error, wants ...string) {
	t.Helper()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		plain := ansi.Strip(transcript.String())
		compact := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, plain)
		foundAll := true
		for _, want := range wants {
			compactWant := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, want)
			if !strings.Contains(plain, want) && !strings.Contains(compact, compactWant) {
				foundAll = false
				break
			}
		}
		if foundAll {
			return
		}
		select {
		case err := <-commandDone:
			t.Fatalf("ccr launch stopped while waiting for %q: %v\ntranscript:\n%s", wants, err, plain)
		case <-ctx.Done():
			t.Fatalf("waiting for live picker text %q: %v\ntranscript:\n%s", wants, ctx.Err(), plain)
		case <-ticker.C:
		}
	}
}
