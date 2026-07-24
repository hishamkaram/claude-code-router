//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/secret"
)

const (
	liveSubscriptionPersonalToken = "ccr-live-subscription-personal-oauth-token"
	liveSubscriptionWorkToken     = "ccr-live-subscription-work-oauth-token"
)

func TestLiveFixtureSubscriptionPoolFirstParty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	isolateLiveSubscriptionClaudeHome(t)

	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{account: "personal", token: liveSubscriptionPersonalToken, text: "CCR_LIVE_SUBSCRIPTION_POOL_OK"},
	})
	defer fixture.Close()
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
	}}
	deps := Dependencies{
		In:           strings.NewReader("Reply exactly CCR_LIVE_SUBSCRIPTION_POOL_OK.\n"),
		Secrets:      secrets,
		StartGateway: fixture.StartGateway,
	}

	out, errOut, err := runLiveCommand(ctx, deps,
		"--db", dbPath, "launch",
		"--auth-mode", "subscription-pool", "--claude-account", "personal",
		"--print", "--no-lifecycle", "--no-statusline",
	)
	if err != nil {
		t.Fatalf("subscription-pool live fixture error = %s\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(err.Error()),
			redactLiveSubscriptionOutput(out),
			redactLiveSubscriptionOutput(errOut))
	}
	if !strings.Contains(out, "CCR_LIVE_SUBSCRIPTION_POOL_OK") {
		t.Fatalf("subscription-pool live fixture output missing sentinel\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	if !strings.Contains(errOut, "Claude account selected: personal") {
		t.Fatalf("subscription-pool selection metadata not visible\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	fixture.AssertCalls(t, []string{"personal"})
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{
		{account: "personal", state: "completed"},
	})
	statusOut, _, err := runLiveCommand(ctx, Dependencies{Secrets: secrets}, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("subscription-pool status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=subscription-pool account=personal") {
		t.Fatalf("subscription-pool status did not expose launch auth metadata: %s", statusOut)
	}
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken)
	assertNoSubscriptionTokenLeak(t, out+errOut+statusOut, liveSubscriptionPersonalToken)
}

func TestLiveFixtureSubscriptionPoolRelaunchesOnFirstParty429(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{account: "personal", token: liveSubscriptionPersonalToken, status: http.StatusTooManyRequests},
		{account: "work", token: liveSubscriptionWorkToken, status: http.StatusTooManyRequests},
	})
	defer fixture.Close()
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
		secret.ClaudeAccountAccessTokenRef("work"):     liveSubscriptionWorkToken,
	}}
	launcher := &liveSubscriptionHTTPLauncher{}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err == nil {
		t.Fatalf("subscription-pool exhaustion succeeded unexpectedly\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	if launcher.StartCount() != 2 {
		t.Fatalf("Claude process starts = %d, want 2", launcher.StartCount())
	}
	if !launcher.StartAt(0).UsesToken(liveSubscriptionPersonalToken) ||
		!launcher.StartAt(1).UsesToken(liveSubscriptionWorkToken) ||
		!containsString(launcher.StartAt(1).args, "--continue") {
		t.Fatal("subscription-pool did not rotate by relaunching with the next process-bound account")
	}
	combined := out + errOut + err.Error()
	for _, want := range []string{
		"relaunching with the next available account",
		"claude subscription pool has no usable accounts",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("subscription-pool exhaustion output missing %q\nstdout:\n%s\nstderr:\n%s\nerror:\n%s",
				want, redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut),
				redactLiveSubscriptionOutput(err.Error()))
		}
	}
	fixture.AssertCalls(t, []string{"personal", "work"})
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{
		{account: "work", state: "failed", endReason: "subscription_exhausted"},
		{account: "personal", state: "failed", endReason: "subscription_exhausted"},
	})
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
	assertNoSubscriptionTokenLeak(t, combined, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}

func TestLiveFixtureSubscriptionPoolRelaunchesRealClaudeOnFirstParty429(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	isolateLiveSubscriptionClaudeHome(t)
	run := newLiveSubscriptionRealRelaunchRun(t, ctx)
	first, second, launchErr := run.rotate(t, ctx)
	run.assert(t, ctx, first, second, launchErr)
}

type liveSubscriptionRealRelaunchRun struct {
	dbPath      string
	secrets     *accountTestSecrets
	fixture     *liveSubscriptionFixture
	launcher    *liveSubscriptionPTYLauncher
	commandOut  *bytes.Buffer
	commandErr  *bytes.Buffer
	commandDone chan error
}

func newLiveSubscriptionRealRelaunchRun(
	t *testing.T,
	ctx context.Context,
) *liveSubscriptionRealRelaunchRun {
	t.Helper()
	first429Released := make(chan struct{})
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{account: "personal", token: liveSubscriptionPersonalToken},
		{
			account:      "personal",
			token:        liveSubscriptionPersonalToken,
			status:       http.StatusTooManyRequests,
			hold429Until: first429Released,
		},
	})
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): liveSubscriptionPersonalToken,
		secret.ClaudeAccountAccessTokenRef("work"):     liveSubscriptionWorkToken,
	}}
	launcher := newLiveSubscriptionPTYLauncher()
	run := &liveSubscriptionRealRelaunchRun{
		dbPath: dbPath, secrets: secrets, fixture: fixture, launcher: launcher,
		commandOut: &bytes.Buffer{}, commandErr: &bytes.Buffer{}, commandDone: make(chan error, 1),
	}
	t.Cleanup(func() {
		close(first429Released)
		launcher.Close()
		fixture.Close()
	})
	go func() {
		cmd := NewRootCommand(ctx, Dependencies{
			In:           strings.NewReader(""),
			Out:          run.commandOut,
			Err:          run.commandErr,
			Secrets:      secrets,
			Launcher:     launcher,
			StartGateway: fixture.StartGateway,
		})
		cmd.SetArgs([]string{
			"--db", dbPath, "launch",
			"--auth-mode", "subscription-pool",
			"--no-lifecycle", "--no-statusline",
		})
		run.commandDone <- cmd.Execute()
	}()
	return run
}

func (r *liveSubscriptionRealRelaunchRun) rotate(
	t *testing.T,
	ctx context.Context,
) (*liveSubscriptionPTYStart, *liveSubscriptionPTYStart, error) {
	t.Helper()
	first := r.launcher.WaitStart(t, ctx, r.commandDone, r.commandOut, r.commandErr)
	waitForLivePickerText(t, ctx, first.Transcript, r.commandDone, "Welcome back!")
	first.Submit(t, "Confirm that this session is ready.")
	if err := r.fixture.WaitCallCount(ctx, 1); err != nil {
		t.Fatalf("waiting for first real Claude request: %v", err)
	}
	first.Submit(t, "Trigger the configured rate-limit response.")
	if err := r.fixture.WaitCallCount(ctx, 2); err != nil {
		t.Fatalf("waiting for real Claude rate-limit request: %v", err)
	}
	second := r.launcher.WaitStart(t, ctx, r.commandDone, r.commandOut, r.commandErr)
	if !first.Process.Stopped() || !first.Process.DoneObserved() {
		t.Fatalf("first real Claude process was not stopped before relaunch")
	}
	second.WaitReady(t, ctx, r.commandDone)
	if stopErr := second.Process.Stop(); stopErr != nil {
		t.Fatalf("stopping relaunched real Claude process: %v", stopErr)
	}
	var launchErr error
	select {
	case launchErr = <-r.commandDone:
	case <-ctx.Done():
		t.Fatalf("waiting for stopped real Claude relaunch: %v", ctx.Err())
	}
	return first, second, launchErr
}

func (r *liveSubscriptionRealRelaunchRun) assert(
	t *testing.T,
	ctx context.Context,
	first, second *liveSubscriptionPTYStart,
	launchErr error,
) {
	t.Helper()
	if !first.UsesToken(liveSubscriptionPersonalToken) || !second.UsesToken(liveSubscriptionWorkToken) {
		t.Fatalf("real Claude subscription-pool relaunch did not use the next account token")
	}
	if !containsString(second.Args, "--continue") {
		t.Fatalf("second real Claude args = %v, want --continue", second.Args)
	}
	if first.PID == 0 || second.PID == 0 || first.PID == second.PID {
		t.Fatalf("real Claude PIDs = first:%d second:%d, want distinct nonzero processes", first.PID, second.PID)
	}
	combined := r.commandOut.String() + r.commandErr.String() + r.launcher.Transcript() + fmt.Sprint(launchErr)
	if !strings.Contains(combined, "relaunching with the next available account") {
		t.Fatalf("real Claude relaunch output missing visible rotation metadata\nstdout:\n%s\nstderr:\n%s\ntranscript:\n%s",
			redactLiveSubscriptionOutput(r.commandOut.String()),
			redactLiveSubscriptionOutput(r.commandErr.String()),
			redactLiveSubscriptionOutput(r.launcher.Transcript()))
	}
	r.fixture.AssertCalls(t, []string{"personal", "personal"})
	assertSubscriptionLaunchMetadata(t, r.dbPath, []subscriptionLaunchWant{
		{account: "work", state: "failed"},
		{account: "personal", state: "failed", endReason: "subscription_exhausted"},
	})
	statusOut, _, err := runLiveCommand(ctx, Dependencies{Secrets: r.secrets}, "--db", r.dbPath, "status")
	if err != nil {
		t.Fatalf("subscription-pool status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=subscription-pool account=work") {
		t.Fatalf("subscription-pool status did not expose rotated account metadata: %s", statusOut)
	}
	assertSubscriptionDatabaseRedaction(t, r.dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
	assertNoSubscriptionTokenLeak(t, combined+statusOut, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}

func TestLiveLocalRealSubscriptionPoolAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_SUBSCRIPTION_POOL") != "1" {
		t.Skip("set CCR_LIVE_REAL_SUBSCRIPTION_POOL=1 to run the opt-in local-real subscription-pool account probe")
	}
	token := liveRealSubscriptionToken(t)
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatalf("local-real subscription-pool probe requires an installed Claude Code CLI: %v", err)
	}
	isolateLiveSubscriptionClaudeHome(t)

	accountName := strings.TrimSpace(os.Getenv("CCR_LIVE_REAL_SUBSCRIPTION_ACCOUNT"))
	if accountName == "" {
		accountName = "local-real"
	}
	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{{name: accountName, token: token}})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef(accountName): token,
	}}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		In: strings.NewReader("Reply exactly CCR_LIVE_REAL_SUBSCRIPTION_POOL_OK.\n"), Secrets: secrets,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--claude-account", accountName,
		"--print", "--no-lifecycle", "--no-statusline")
	if err != nil {
		if liveAnthropicAuthUnavailable(out + errOut) {
			t.Fatalf("local-real subscription-pool account authentication failed; refresh the supplied OAuth token and retry")
		}
		t.Fatalf("local-real subscription-pool probe error = %s\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(err.Error(), token),
			redactLiveSubscriptionOutput(out, token),
			redactLiveSubscriptionOutput(errOut, token))
	}
	if !strings.Contains(out, "CCR_LIVE_REAL_SUBSCRIPTION_POOL_OK") {
		t.Fatalf("local-real subscription-pool probe output missing sentinel\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out, token), redactLiveSubscriptionOutput(errOut, token))
	}
	assertSubscriptionDatabaseRedaction(t, dbPath, token)
	assertNoSubscriptionTokenLeak(t, out+errOut, token)
}

func liveRealSubscriptionToken(t *testing.T) string {
	t.Helper()
	if token := strings.TrimSpace(os.Getenv("CCR_LIVE_REAL_SUBSCRIPTION_OAUTH_TOKEN")); token != "" {
		return token
	}
	credentials, err := claudeaccount.ReadCurrentCredentials()
	if err != nil {
		t.Fatalf(
			"current Claude credentials unavailable: %v; set CCR_LIVE_REAL_SUBSCRIPTION_OAUTH_TOKEN to run the probe",
			err,
		)
	}
	return credentials.AccessToken
}

type liveSubscriptionResponse struct {
	account      string
	token        string
	status       int
	text         string
	hold429Until <-chan struct{}
}

type liveSubscriptionFixture struct {
	server    *httptest.Server
	responses []liveSubscriptionResponse

	mu    sync.Mutex
	calls []string
}

func newLiveSubscriptionFixture(t *testing.T, responses []liveSubscriptionResponse) *liveSubscriptionFixture {
	t.Helper()
	fixture := &liveSubscriptionFixture{responses: responses}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveSubscriptionFixture) Close() {
	f.server.Close()
}

func (f *liveSubscriptionFixture) StartGateway(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
	cfg.AnthropicBaseURL = f.server.URL
	return gateway.Start(ctx, cfg)
}

func (f *liveSubscriptionFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/messages":
		f.handleMessage(t, w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveSubscriptionFixture) handleMessage(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding subscription-pool fixture request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	response, account, ok := f.nextResponse(r.Header.Get("Authorization"))
	if !ok {
		t.Error("subscription-pool fixture received unexpected account auth")
		http.Error(w, "unexpected auth", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, account)
	f.mu.Unlock()
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		if response.hold429Until != nil {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-response.hold429Until:
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"subscription exhausted"}}`)
		return
	}
	text := response.text
	if text == "" {
		text = "CCR_LIVE_SUBSCRIPTION_POOL_OK"
	}
	if payload.Stream {
		writeLiveAnthropicStream(w, payload.Model, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_subscription_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":3}}`, payload.Model, text)
}

func (f *liveSubscriptionFixture) nextResponse(auth string) (liveSubscriptionResponse, string, bool) {
	f.mu.Lock()
	index := len(f.calls)
	f.mu.Unlock()
	if index >= len(f.responses) {
		return liveSubscriptionResponse{}, "", false
	}
	response := f.responses[index]
	if strings.TrimSpace(auth) != "Bearer "+response.token {
		return liveSubscriptionResponse{}, "", false
	}
	return response, response.account, true
}

func (f *liveSubscriptionFixture) AssertCalls(t *testing.T, want []string) {
	t.Helper()
	f.mu.Lock()
	got := append([]string(nil), f.calls...)
	f.mu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("subscription-pool upstream accounts = %v, want %v", got, want)
	}
}

func (f *liveSubscriptionFixture) WaitCallCount(ctx context.Context, want int) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if f.CallCount() >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for subscription-pool upstream call %d: %w", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (f *liveSubscriptionFixture) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type subscriptionLaunchWant struct {
	account   string
	state     string
	endReason string
}

func assertSubscriptionLaunchMetadata(t *testing.T, dbPath string, wants []subscriptionLaunchWant) {
	t.Helper()
	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != len(wants) {
		t.Fatalf("subscription launch count = %d, want %d", len(launches), len(wants))
	}
	for index, want := range wants {
		launch := launches[index]
		if launch.AuthMode != launchAuthModeSubscriptionPool ||
			launch.ClaudeAccountName != want.account ||
			launch.State != want.state ||
			(want.endReason != "" && launch.EndReason != want.endReason) {
			t.Fatalf("subscription launch[%d] metadata = %#v, want account=%s state=%s reason=%s", index, launch, want.account, want.state, want.endReason)
		}
	}
}

func isolateLiveSubscriptionClaudeHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", configDir, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	writeLiveSubscriptionJSON(t, filepath.Join(configDir, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			cwd: map[string]any{
				"hasTrustDialogAccepted":     true,
				"projectOnboardingSeenCount": 1,
			},
		},
	})
	writeLiveSubscriptionJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{"theme": "dark"})
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	t.Setenv("DISABLE_TELEMETRY", "1")
}

func writeLiveSubscriptionJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func assertNoSubscriptionTokenLeak(t *testing.T, output string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if strings.Contains(output, token) {
			t.Fatal("subscription-pool output leaked an OAuth token")
		}
	}
}

func assertSubscriptionDatabaseRedaction(t *testing.T, dbPath string, tokens ...string) {
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
		for _, token := range tokens {
			if strings.Contains(string(contents), token) {
				t.Fatalf("runtime database artifact %s contains an OAuth token", path)
			}
		}
	}
}

func redactLiveSubscriptionOutput(output string, additionalTokens ...string) string {
	tokens := append([]string{
		liveSubscriptionPersonalToken,
		liveSubscriptionWorkToken,
	}, additionalTokens...)
	if token := strings.TrimSpace(os.Getenv("CCR_LIVE_REAL_SUBSCRIPTION_OAUTH_TOKEN")); token != "" {
		tokens = append(tokens, token)
	}
	for _, token := range tokens {
		if token != "" {
			output = strings.ReplaceAll(output, token, "[redacted-oauth-token]")
		}
	}
	return output
}

func TestRedactLiveSubscriptionOutputIncludesSuppliedToken(t *testing.T) {
	const token = "current-login-token-not-from-env"
	got := redactLiveSubscriptionOutput("failure exposed "+token, token)
	if strings.Contains(got, token) || !strings.Contains(got, "[redacted-oauth-token]") {
		t.Fatalf("redacted live output = %q", got)
	}
}
