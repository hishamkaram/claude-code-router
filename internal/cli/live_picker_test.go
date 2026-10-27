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

	ccrExecutable := buildLiveCCRExecutable(t, ctx)
	configureIsolatedLivePickerClaude(t)

	routedModel, provider := newLivePickerProvider(t)
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLivePickerModels(t, ctx, dbPath, provider.URL)

	run := startLivePickerRun(t, ctx, dbPath, ccrExecutable)
	defer run.close()

	run.waitForText(t, ctx, "Detected a custom API key")
	run.write(t, "\x1b[A\r", "accepting isolated placeholder API key")
	run.waitForText(t, ctx, "Claude Code v")
	run.submit(t, ctx, "/model", "opening /model picker")
	run.waitForText(t, ctx,
		"Select model",
		"Opus",
		"Sonnet",
		"Haiku",
		"anthropic.ccr.gpt",
		"anthropic.ccr.s%6fnnet",
		"anthropic.ccr.%6fpus",
		"anthropic.ccr.h%61iku",
	)

	run.write(t, "\x1b", "closing /model picker")
	run.waitForText(t, ctx, "Kept model as")
	run.submit(t, ctx, "/model anthropic.ccr.gpt", "selecting registered model by ID")
	run.waitForText(t, ctx, "Set model to anthropic.ccr.gpt")
	run.submit(t, ctx, "Reply with the routed test response.", "sending routed prompt")
	run.assertRoutedModel(t, ctx, routedModel, "gpt-5")
	run.waitForText(t, ctx, "picker-route-ok")
	run.waitForText(t, ctx, "CCR | gpt | litellm/gpt-5")

	run.submit(t, ctx, "/exit", "exiting Claude Code")
	run.waitForExit(t, ctx)
}

type livePickerRun struct {
	session     livePickerSession
	commandDone chan error
	commandOut  *bytes.Buffer
	commandErr  *bytes.Buffer
	transcript  *livePickerTerminal
	readDone    chan error
}

func newLivePickerProvider(t *testing.T) (chan string, *httptest.Server) {
	t.Helper()

	routedModel := make(chan string, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"},{"id":"third-party-sonnet"},{"id":"third-party-opus"},{"id":"third-party-haiku"}]}`)
		case "/v1/chat/completions":
			var payload liveOpenAIChatPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("provider decode error = %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			select {
			case routedModel <- payload.Model:
			default:
			}
			writeLiveOpenAITextFixture(w, payload, "chatcmpl-picker", "picker-route-ok", 4, 2)
		default:
			http.NotFound(w, r)
		}
	}))
	return routedModel, provider
}

func startLivePickerRun(t *testing.T, ctx context.Context, dbPath, ccrExecutable string, startupArgs ...string) *livePickerRun {
	t.Helper()

	launcher := &livePickerLauncher{started: make(chan livePickerSession, 1), statuslineExecutable: ccrExecutable}
	commandOut := &bytes.Buffer{}
	commandErr := &bytes.Buffer{}
	commandDone := make(chan error, 1)
	go func() {
		cmd := NewRootCommand(ctx, Dependencies{
			In:             strings.NewReader(""),
			Out:            commandOut,
			Err:            commandErr,
			Launcher:       launcher,
			ExecutablePath: ccrExecutable,
		})
		args := append([]string{"--db", dbPath, "launch"}, startupArgs...)
		cmd.SetArgs(append(args, "--bare"))
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

	transcript := newLivePickerTerminal()
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(transcript, session.pty)
		readDone <- err
	}()
	return &livePickerRun{
		session:     session,
		commandDone: commandDone,
		commandOut:  commandOut,
		commandErr:  commandErr,
		transcript:  transcript,
		readDone:    readDone,
	}
}

func (r *livePickerRun) close() {
	_ = r.session.process.Stop()
	_ = r.session.pty.Close()
}

func (r *livePickerRun) waitForText(t *testing.T, ctx context.Context, wants ...string) {
	t.Helper()
	waitForLivePickerText(t, ctx, r.transcript.Snapshot, r.commandDone, wants...)
}

func (r *livePickerRun) write(t *testing.T, input, action string) {
	t.Helper()
	if _, err := r.session.pty.Write([]byte(input)); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func (r *livePickerRun) submit(t *testing.T, ctx context.Context, input, action string) {
	t.Helper()
	stepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := waitForPickerState(stepCtx, r.transcript.Snapshot, livePickerPromptReady); err != nil {
		t.Fatalf("%s: prompt not ready: %v\n%s", action, err, r.transcript.Snapshot().diagnostics())
	}
	r.write(t, input, action)
	echoed := func(view livePickerSnapshot) bool { return livePickerInputEchoed(view, input) }
	if err := waitForPickerState(stepCtx, r.transcript.Snapshot, echoed); err != nil {
		t.Fatalf("%s: typed input not echoed: %q: %v\n%s", action, input, err, r.transcript.Snapshot().diagnostics())
	}
	r.write(t, "\r", action)
}

func (r *livePickerRun) assertRoutedModel(t *testing.T, ctx context.Context, routedModel <-chan string, want string) {
	t.Helper()

	select {
	case got := <-routedModel:
		if got != want {
			t.Fatalf("picker routed provider model = %q, want %s\ntranscript:\n%s", got, want, r.plainTranscript())
		}
	case err := <-r.commandDone:
		t.Fatalf("ccr launch stopped before routing picker selection: %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s", err, r.commandOut.String(), r.commandErr.String(), r.plainTranscript())
	case <-ctx.Done():
		t.Fatalf("waiting for picker-selected route: %v\ntranscript:\n%s", ctx.Err(), r.plainTranscript())
	}
}

func (r *livePickerRun) waitForExit(t *testing.T, ctx context.Context) {
	t.Helper()

	select {
	case err := <-r.commandDone:
		if err != nil {
			t.Fatalf("ccr launch error = %v\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s", err, r.commandOut.String(), r.commandErr.String(), r.plainTranscript())
		}
	case <-ctx.Done():
		t.Fatalf("waiting for Claude Code exit: %v\ntranscript:\n%s", ctx.Err(), r.plainTranscript())
	}
	select {
	case <-r.readDone:
	case <-time.After(time.Second):
	}
}

func (r *livePickerRun) plainTranscript() string {
	return ansi.Strip(r.transcript.String())
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
	started              chan livePickerSession
	cwd                  string
	statuslineExecutable string
}

type livePickerSession struct {
	pty     xpty.Pty
	process *livePickerProcess
}

func (l *livePickerLauncher) Start(ctx context.Context, args []string, env ClaudeEnvironment, _ io.Reader, _, _ io.Writer) (ClaudeProcess, error) {
	if err := l.checkStatuslineExecutable(args); err != nil {
		return nil, err
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("finding Claude Code: %w", err)
	}
	pty, err := xpty.NewPty(livePickerCols, livePickerRows)
	if err != nil {
		return nil, fmt.Errorf("creating Claude Code PTY: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = l.cwd
	cmd.Env = applyClaudeEnvironment(os.Environ(), env)
	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		return nil, fmt.Errorf("starting Claude Code in PTY: %w", err)
	}
	done := make(chan error, 1)
	process := &livePickerProcess{ctx: ctx, cmd: cmd, pty: pty, done: done}
	go func() {
		done <- process.wait()
		close(done)
	}()
	l.started <- livePickerSession{pty: pty, process: process}
	return process, nil
}

func (l *livePickerLauncher) checkStatuslineExecutable(args []string) error {
	if l.statuslineExecutable == "" {
		return nil
	}
	for index, arg := range args {
		if arg != "--settings" || index+1 >= len(args) {
			continue
		}
		data, err := os.ReadFile(args[index+1])
		if err != nil {
			return fmt.Errorf("reading picker settings: %w", err)
		}
		var settings struct {
			StatusLine struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"statusLine"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("decoding picker settings: %w", err)
		}
		want, err := launchStatuslineCommand(l.statuslineExecutable)
		if err != nil {
			return err
		}
		if settings.StatusLine.Type != "command" || settings.StatusLine.Command != want {
			return fmt.Errorf("picker statusline must invoke built CCR %q; got %q", want, settings.StatusLine.Command)
		}
		return nil
	}
	return errors.New("picker launch is missing private settings")
}

func TestLivePickerRejectsTestDriverStatusline(t *testing.T) {
	t.Parallel()
	executable := filepath.Join(t.TempDir(), "built ccr")
	launcher := &livePickerLauncher{statuslineExecutable: executable}
	for _, test := range []struct {
		name       string
		executable string
		wantError  bool
	}{
		{name: "built CCR", executable: executable},
		{name: "test driver fallback", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, err := launchStatuslineCommand(test.executable)
			if err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(t.TempDir(), "settings.json")
			settings := struct {
				StatusLine struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"statusLine"`
			}{}
			settings.StatusLine.Type, settings.StatusLine.Command = "command", command
			writeLivePickerJSON(t, settingsPath, settings)
			err = launcher.checkStatuslineExecutable([]string{"--settings", settingsPath})
			if (err != nil) != test.wantError {
				t.Fatalf("statusline guard error = %v, want error %t", err, test.wantError)
			}
		})
	}
}

type livePickerProcess struct {
	ctx      context.Context
	cmd      *exec.Cmd
	pty      xpty.Pty
	done     <-chan error
	waitOnce sync.Once
	waitErr  error
}

func (p *livePickerProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *livePickerProcess) Done() <-chan error {
	return p.done
}

func (p *livePickerProcess) wait() error {
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

func (b *synchronizedBuffer) Snapshot() livePickerSnapshot {
	return livePickerSnapshot{stream: b.String()}
}

func waitForLivePickerText(t *testing.T, ctx context.Context, snapshot func() livePickerSnapshot, commandDone <-chan error, wants ...string) {
	t.Helper()

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		view := snapshot()
		missing := livePickerTextMissing(view, wants)
		if len(missing) == 0 {
			return
		}
		select {
		case err := <-commandDone:
			t.Fatalf("ccr launch stopped while waiting for %q: %v\n%s", missing, err, view.diagnostics())
		case <-ctx.Done():
			t.Fatalf("waiting for live picker text %q: %v\n%s", missing, ctx.Err(), view.diagnostics())
		case <-ticker.C:
		}
	}
}
