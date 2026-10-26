//go:build live && (linux || darwin)

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveFixtureEnvironmentInheritance(t *testing.T) {
	for _, mode := range []string{launchAuthModeProviderOnly, launchAuthModePreserve} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			if _, err := liveclaude.Check(ctx); err != nil {
				t.Skipf("live Claude Code unavailable: %v", err)
			}
			fixture := newLiveEnvironmentFixture(t)
			if mode == launchAuthModePreserve {
				t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
			}
			provider := newLiveResumeRouteProvider(t)
			t.Cleanup(provider.Close)
			database := filepath.Join(t.TempDir(), "ccr.db")
			configureLiveResumeRouteModel(t, ctx, database, provider.URL())
			out, errOut, err := runLiveEnvironmentLaunch(ctx, database, fixture.plugin, mode)
			assertLiveEnvironmentLaunch(t, database, out, errOut, err)
			fixture.assertReports(t, ctx)
			if mode == launchAuthModeProviderOnly {
				proveLiveEnvironmentResume(t, ctx, database, fixture)
			}
		})
	}
}

func runLiveEnvironmentLaunch(ctx context.Context, database, plugin, mode string) (string, string, error) {
	return runLiveCommand(
		ctx, Dependencies{In: strings.NewReader("Reply with the synthetic route fixture response.\n")},
		"--db", database, "launch", "--model", liveResumeRouteModelAlias,
		"--print", "--auth-mode", mode, "--no-statusline", "--",
		"--plugin-dir", plugin, "--output-format", "stream-json", "--verbose", "--max-turns", "1",
	)
}

func assertLiveEnvironmentLaunch(t *testing.T, database, out, errOut string, err error) {
	t.Helper()
	assertLiveResumeRouteNoSecret(t, database, out, errOut)
	paths, globErr := filepath.Glob(database + "*")
	if globErr != nil {
		t.Fatal("find synthetic database artifacts")
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal("read synthetic database artifact")
		}
		if bytes.Contains(data, []byte(liveEnvironmentGenericValue)) {
			t.Fatal("database artifact stored synthetic plugin credential")
		}
	}
	if strings.Contains(out+errOut, liveEnvironmentGenericValue) {
		t.Fatal("synthetic plugin credential appeared in launch output")
	}
	if err != nil {
		t.Fatalf("synthetic environment launch failed: %s", liveResumeRouteDiagnostics(liveEnvironmentGenericValue, liveResumeRouteErrorText(err), out, errOut))
	}
	if !strings.Contains(out, liveResumeRouteInitialResponse) && !strings.Contains(out, liveResumeRouteResumedResponse) {
		t.Fatal("synthetic environment launch did not use the configured provider")
	}
}

func proveLiveEnvironmentResume(t *testing.T, ctx context.Context, database string, fixture liveEnvironmentFixture) {
	t.Helper()
	_, session, _ := readLiveResumeRouteEvidence(t, ctx, database)
	fixture.reports = t.TempDir()
	t.Setenv("CCR_LIVE_ENV_REPORT_DIR", fixture.reports)
	out, errOut, err := runLiveCommand(
		ctx, Dependencies{In: strings.NewReader("Continue the synthetic route fixture.\n")},
		"--db", database, "launch", "--resume-route", "--print", "--no-statusline", "--",
		"--resume", session.ClaudeSessionID, "--plugin-dir", fixture.plugin,
		"--output-format", "stream-json", "--verbose", "--max-turns", "1",
	)
	assertLiveEnvironmentLaunch(t, database, out, errOut, err)
	fixture.assertReports(t, ctx)
}

func TestLiveFixtureEnvironmentManagedJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	binary := buildLiveCCRExecutable(t, ctx)
	fixture := newLiveEnvironmentFixture(t)
	provider := newLiveResumeRouteProvider(t)
	t.Cleanup(provider.Close)
	database := filepath.Join(t.TempDir(), "ccr.db")
	configureLiveResumeRouteModel(t, ctx, database, provider.URL())
	prompt := filepath.Join(fixture.project, "prompt.txt")
	if err := os.WriteFile(prompt, []byte("Reply with the synthetic route fixture response.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(
		ctx, binary,
		"--db", database, "launch", "--model", liveResumeRouteModelAlias,
		"--auth-mode", "provider-only", "--detach", "--print", "--prompt-file", prompt,
		"--submission-id", uuid.NewString(), "--no-statusline", "--",
		"--plugin-dir", fixture.plugin, "--output-format", "stream-json", "--verbose", "--max-turns", "1",
	)
	command.Dir = fixture.project
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("admit synthetic environment job: %v", err)
	}
	var receipt jobReceipt
	if err := json.Unmarshal(output, &receipt); err != nil {
		t.Fatalf("decode synthetic job receipt: %v", err)
	}
	registerJobCleanup(t, ctx, binary, receipt.JobID)
	for {
		record := readBuiltJobStatus(t, ctx, binary, receipt.JobID)
		if record.Terminal() {
			if record.Status != "completed" {
				t.Fatalf("synthetic environment job failed: status=%s reason=%s", record.Status, record.ReasonCode)
			}
			log, logErr := os.ReadFile(record.Log)
			errLog, errLogErr := os.ReadFile(record.ErrorLog)
			if logErr != nil || errLogErr != nil {
				t.Fatal("read synthetic job logs")
			}
			assertLiveEnvironmentLaunch(t, database, string(log), string(errLog), nil)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for synthetic environment job: %v", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	fixture.assertReports(t, ctx)
}

func TestLiveFixtureEnvironmentForeground(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	fixture := newLiveEnvironmentFixture(t)
	provider := newLiveResumeRouteProvider(t)
	t.Cleanup(provider.Close)
	database := filepath.Join(t.TempDir(), "ccr.db")
	configureLiveResumeRouteModel(t, ctx, database, provider.URL())
	launcher := newLiveSubscriptionPTYLauncher(fixture.project)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		launcher.Close()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("foreground environment test did not shut down")
		}
	})
	go func() {
		defer close(done)
		cmd := NewRootCommand(ctx, Dependencies{In: strings.NewReader(""), Out: out, Err: errOut, Launcher: launcher})
		cmd.SetArgs([]string{
			"--db", database, "launch", "--model", liveResumeRouteModelAlias,
			"--auth-mode", "provider-only", "--no-statusline", "--", "--plugin-dir", fixture.plugin,
		})
		done <- cmd.Execute()
	}()
	start := launcher.WaitStart(t, ctx, done, out, errOut)
	start.WaitReady(t, ctx, done)
	fixture.assertReports(t, ctx)
	if err := start.Process.Stop(); err != nil {
		t.Fatalf("stop foreground environment fixture: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("wait for foreground environment fixture: %v", ctx.Err())
	}
	combined := out.String() + errOut.String() + launcher.Transcript()
	assertLiveResumeRouteNoSecret(t, database, combined)
	if strings.Contains(combined, liveEnvironmentGenericValue) {
		t.Fatal("foreground output exposed synthetic plugin credential")
	}
}

func TestLiveFixtureEnvironmentSettingsRouting(t *testing.T) {
	for _, scenario := range []string{"base-url", "provider-selector", "auth-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if _, err := liveclaude.Check(ctx); err != nil {
				t.Skipf("live Claude Code unavailable: %v", err)
			}
			fixture := newLiveEnvironmentFixture(t)
			provider := newLiveResumeRouteProvider(t)
			t.Cleanup(provider.Close)
			database := filepath.Join(t.TempDir(), "ccr.db")
			configureLiveResumeRouteModel(t, ctx, database, provider.URL())
			env := map[string]string{}
			switch scenario {
			case "base-url":
				env["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:1"
			case "provider-selector":
				env["CLAUDE_CODE_USE_BEDROCK"] = "1"
				t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
			case "auth-conflict":
				env["ANTHROPIC_AUTH_TOKEN"] = "ccr-synthetic-invalid-launch-token"
			}
			writeLiveEnvironmentJSON(t, filepath.Join(fixture.project, ".claude", "settings.json"), struct {
				Env map[string]string `json:"env"`
			}{Env: env})
			if scenario == "auth-conflict" {
				proveLiveEnvironmentAuthRejection(t, ctx, database, fixture.plugin, provider)
				return
			}
			out, errOut, err := runLiveEnvironmentLaunch(ctx, database, fixture.plugin, launchAuthModeProviderOnly)
			assertLiveEnvironmentLaunch(t, database, out, errOut, err)
			fixture.assertReports(t, ctx)
		})
	}
}

func proveLiveEnvironmentAuthRejection(t *testing.T, ctx context.Context, database, plugin string, provider *liveResumeRouteProvider) {
	t.Helper()
	launchCtx, cancel := context.WithCancel(ctx)
	out, errOut := &synchronizedBuffer{}, &synchronizedBuffer{}
	started, done := make(chan string, 1), make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("auth-conflict Claude process did not shut down")
		}
	})
	go func() {
		defer close(done)
		cmd := NewRootCommand(launchCtx, Dependencies{
			In: strings.NewReader("Reply with the synthetic route fixture response.\n"), Out: out, Err: errOut,
			StartGateway: func(startCtx context.Context, cfg gateway.Config) (*gateway.Server, error) {
				server, err := gateway.Start(startCtx, cfg)
				if err == nil {
					started <- server.URL()
				}
				return server, err
			},
		})
		cmd.SetArgs([]string{
			"--db", database, "launch", "--model", liveResumeRouteModelAlias,
			"--print", "--auth-mode", "provider-only", "--no-statusline", "--", "--plugin-dir", plugin,
			"--output-format", "stream-json", "--verbose", "--max-turns", "1",
		})
		done <- cmd.Execute()
	}()
	var gatewayURL string
	select {
	case gatewayURL = <-started:
	case <-done:
		t.Fatal("auth-conflict launch exited before starting its gateway")
	case <-ctx.Done():
		t.Fatal("auth-conflict gateway startup timed out")
	}
	waitLiveEnvironmentAuthFailure(t, ctx, out, done)
	assertLiveEnvironmentGatewayRejection(t, ctx, gatewayURL)
	if provider.chatCount() != 0 {
		t.Fatal("settings auth conflict reached the upstream provider")
	}
	assertLiveResumeRouteNoSecret(t, database, out.String(), errOut.String())
	if strings.Contains(out.String()+errOut.String(), liveEnvironmentGenericValue) {
		t.Fatal("auth-conflict output exposed synthetic plugin credential")
	}
	// The real CLI has emitted its visible 401 event. Stop its native retry loop
	// instead of waiting for ten exponential retries to exhaust.
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("auth-conflict launch did not stop after its rejection proof")
	}
}

func waitLiveEnvironmentAuthFailure(t *testing.T, ctx context.Context, out *synchronizedBuffer, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		for line := range strings.SplitSeq(out.String(), "\n") {
			var event struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				Status  int    `json:"error_status"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && event.Type == "system" && event.Subtype == "api_retry" && event.Status == http.StatusUnauthorized {
				return
			}
		}
		select {
		case <-done:
			t.Fatal("native Claude exited without its visible auth rejection")
		case <-ctx.Done():
			t.Fatal("native Claude did not visibly report the settings auth rejection")
		case <-ticker.C:
		}
	}
}

func assertLiveEnvironmentGatewayRejection(t *testing.T, ctx context.Context, gatewayURL string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/v1/models", nil)
	if err != nil {
		t.Fatal("create synthetic rejected-auth request")
	}
	request.Header.Set("Authorization", "Bearer ccr-synthetic-invalid-launch-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("query test-owned gateway authentication failure")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil || response.StatusCode != http.StatusUnauthorized ||
		!bytes.Contains(body, []byte("ANTHROPIC_AUTH_TOKEN")) || !bytes.Contains(body, []byte("ANTHROPIC_CUSTOM_HEADERS")) {
		t.Fatal("gateway auth rejection did not include actionable credential names")
	}
}
