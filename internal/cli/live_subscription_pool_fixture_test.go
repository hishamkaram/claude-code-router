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
	configDir := isolateLiveSubscriptionClaudeHome(t)
	settingsPath := filepath.Join(configDir, "settings.json")
	staleStatusline := "CCR_STALE_SHARED_PROFILE_LIMITS"
	writeLiveSubscriptionJSON(t, settingsPath, map[string]any{
		"theme": "dark",
		"statusLine": map[string]any{
			"type": "command", "command": "printf " + staleStatusline,
		},
	})

	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{account: "work", token: liveSubscriptionWorkToken, text: "CCR_LIVE_SUBSCRIPTION_POOL_OK"},
	})
	defer fixture.Close()
	dbPath, secrets := seedLiveSubscriptionCredentials(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	deps := Dependencies{
		In:           strings.NewReader("Reply exactly CCR_LIVE_SUBSCRIPTION_POOL_OK.\n"),
		Secrets:      secrets,
		StartGateway: fixture.StartGateway,
	}

	out, errOut, err := runLiveCommand(ctx, deps,
		"--db", dbPath, "launch",
		"--auth-mode", "subscription-pool", "--claude-account", "work",
		"--print", "--no-lifecycle",
	)
	if err != nil {
		t.Fatalf("subscription-pool live fixture error = %s\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(err.Error()),
			redactLiveSubscriptionOutput(out),
			redactLiveSubscriptionOutput(errOut))
	}
	assertLiveSubscriptionFirstPartyOutput(t, out, errOut, staleStatusline)
	settings, readErr := os.ReadFile(settingsPath)
	if readErr != nil || !strings.Contains(string(settings), staleStatusline) {
		t.Fatalf("subscription-pool launch changed user settings: %q, %v", settings, readErr)
	}
	fixture.AssertCalls(t, []string{"work"})
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{
		{account: "work", state: "completed"},
	})
	launches := loadSubscriptionLaunches(t, dbPath)
	if len(launches) != 1 || launches[0].StatuslineState != "isolated" {
		t.Fatalf("subscription-pool live status-line state = %#v, want isolated", launches)
	}
	statusOut, _, err := runLiveCommand(ctx, Dependencies{Secrets: secrets}, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("subscription-pool status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=subscription-pool account=work") {
		t.Fatalf("subscription-pool status did not expose launch auth metadata: %s", statusOut)
	}
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
	assertNoSubscriptionTokenLeak(t, out+errOut+statusOut, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}

func assertLiveSubscriptionFirstPartyOutput(t *testing.T, out, errOut, staleStatusline string) {
	t.Helper()
	if !strings.Contains(out, "CCR_LIVE_SUBSCRIPTION_POOL_OK") {
		t.Fatalf("subscription-pool live fixture output missing sentinel\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	if !strings.Contains(errOut, "Claude model-request account selected: work") {
		t.Fatalf("subscription-pool selection metadata not visible\nstdout:\n%s\nstderr:\n%s",
			redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	for _, want := range []string{
		"Subscription limits for account work: unknown",
		"Existing Claude statusLine preserved through a launch-only credential-isolation wrapper",
		"CCR_CLAUDE_ACCOUNT=work",
		"OAuth and gateway tokens are removed",
		"Automatic account rotation: disabled because --claude-account pins one account",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("subscription-pool status-line evidence missing %q\nstdout:\n%s\nstderr:\n%s",
				want, redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
		}
	}
	if strings.Contains(out+errOut, staleStatusline) {
		t.Fatalf("subscription-pool live output used the shared-profile status line: %s",
			redactLiveSubscriptionOutput(out+errOut))
	}
}

func TestLiveFixtureSubscriptionPoolRotatesOneProcessAndPreservesFinalLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account: "personal", token: liveSubscriptionPersonalToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			representativeClaim: "five_hour",
		},
		{
			account: "work", token: liveSubscriptionWorkToken,
			status: http.StatusTooManyRequests, unifiedStatus: "rejected",
			representativeClaim: "five_hour",
		},
	})
	defer fixture.Close()
	dbPath, secrets := seedLiveSubscriptionCredentials(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	launcher := &liveSubscriptionHTTPLauncher{}
	out, errOut, err := runLiveCommand(ctx, Dependencies{
		Secrets: secrets, Launcher: launcher, StartGateway: fixture.StartGateway,
	}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool", "--no-lifecycle", "--no-statusline")
	if err != nil {
		t.Fatalf("subscription-pool continuity error = %v\nstdout:\n%s\nstderr:\n%s",
			err, redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
	}
	if launcher.StartCount() != 1 {
		t.Fatalf("Claude process starts = %d, want 1", launcher.StartCount())
	}
	start := launcher.StartAt(0)
	if start.authToken == "" ||
		start.authToken == liveSubscriptionPersonalToken ||
		start.authToken == liveSubscriptionWorkToken {
		t.Fatal("Claude process did not use an isolated local gateway credential")
	}
	combined := out + errOut
	for _, want := range []string{
		"switched from account personal to work inside the existing gateway",
		"no replacement account is usable",
		"kept Claude Code running",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("subscription-pool continuity output missing %q\nstdout:\n%s\nstderr:\n%s",
				want, redactLiveSubscriptionOutput(out), redactLiveSubscriptionOutput(errOut))
		}
	}
	fixture.AssertCalls(t, []string{"personal", "work"})
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{
		{account: "work", state: "completed"},
	})
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
	assertNoSubscriptionTokenLeak(t, combined, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
}

func TestLiveFixtureSubscriptionPoolRotatesRealClaudeWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	isolateLiveSubscriptionClaudeHome(t)
	fixture := newLiveSubscriptionFixture(t, []liveSubscriptionResponse{
		{
			account:             "personal",
			token:               liveSubscriptionPersonalToken,
			status:              http.StatusTooManyRequests,
			unifiedStatus:       "rejected",
			representativeClaim: "five_hour",
		},
		{account: "work", token: liveSubscriptionWorkToken, text: "CCR_LIVE_SAME_PROCESS_ROTATION_OK"},
	})
	fixture.repeatLast = true
	dbPath, secrets := seedLiveSubscriptionCredentials(t, []subscriptionAccountFixture{
		{name: "personal", token: liveSubscriptionPersonalToken},
		{name: "work", token: liveSubscriptionWorkToken},
	})
	launcher := newLiveSubscriptionPTYLauncher("")
	commandOut, commandErr := &bytes.Buffer{}, &bytes.Buffer{}
	commandDone := make(chan error, 1)
	t.Cleanup(func() {
		launcher.Close()
		fixture.Close()
	})
	go runLiveSubscriptionContinuityCommand(
		ctx, dbPath, secrets, launcher, fixture, commandOut, commandErr, commandDone,
	)

	start := launcher.WaitStart(t, ctx, commandDone, commandOut, commandErr)
	waitForLivePickerText(t, ctx, start.Transcript, commandDone, "Welcome back!")
	start.Submit(t, "Trigger the configured rate-limit response.")
	if err := fixture.WaitCallCount(ctx, 2); err != nil {
		t.Fatalf("waiting for real Claude rate-limit request: %v", err)
	}
	waitForLivePickerText(
		t, ctx, start.Transcript, commandDone, "LIVE_SAME_PROCESS_ROTATION_OK",
	)
	if start.PID == 0 || start.Process.Stopped() || start.Process.DoneObserved() {
		t.Fatalf("real Claude process did not survive rotation: pid=%d", start.PID)
	}
	if start.OAuthTokenExposed || !start.LocalGatewayAuth {
		t.Fatal("real Claude process received account OAuth instead of only local gateway auth")
	}
	select {
	case unexpected := <-launcher.starts:
		t.Fatalf("CCR started a second Claude process with pid %d", unexpected.PID)
	default:
	}
	if stopErr := start.Process.Stop(); stopErr != nil {
		t.Fatalf("stopping test-owned real Claude process: %v", stopErr)
	}
	var launchErr error
	select {
	case launchErr = <-commandDone:
	case <-ctx.Done():
		t.Fatalf("waiting for test-owned Claude shutdown: %v", ctx.Err())
	}
	combined := commandOut.String() + commandErr.String() + launcher.Transcript() + fmt.Sprint(launchErr)
	if !strings.Contains(combined, "switched from account personal to work inside the existing gateway") {
		t.Fatalf("real Claude output missing same-process rotation metadata:\n%s",
			redactLiveSubscriptionOutput(combined))
	}
	fixture.AssertRotationCalls(t, "personal", "work")
	assertSubscriptionLaunchMetadata(t, dbPath, []subscriptionLaunchWant{{account: "work", state: "failed"}})
	statusOut, _, err := runLiveCommand(ctx, Dependencies{Secrets: secrets}, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("subscription-pool status error = %v", err)
	}
	if !strings.Contains(statusOut, "Launch auth: mode=subscription-pool account=work") {
		t.Fatalf("subscription-pool status did not expose rotated account metadata: %s", statusOut)
	}
	assertSubscriptionDatabaseRedaction(t, dbPath, liveSubscriptionPersonalToken, liveSubscriptionWorkToken)
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

func seedLiveSubscriptionCredentials(
	t *testing.T,
	accounts []subscriptionAccountFixture,
) (string, *accountTestSecrets) {
	t.Helper()
	values := make(map[string]string, len(accounts))
	for _, account := range accounts {
		values[secret.ClaudeAccountAccessTokenRef(account.name)] = account.token
	}
	return seedSubscriptionAccounts(t, accounts), &accountTestSecrets{values: values}
}

type liveSubscriptionResponse struct {
	account             string
	token               string
	status              int
	text                string
	unifiedStatus       string
	representativeClaim string
	fallbackAvailable   bool
	retryAfterSeconds   int
	hold429Until        <-chan struct{}
}

type liveSubscriptionFixture struct {
	server     *httptest.Server
	responses  []liveSubscriptionResponse
	repeatLast bool

	mu    sync.Mutex
	calls []string

	nextResponseIndex int
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
	response, _, ok := f.nextResponse(r.Header.Get("Authorization"))
	if !ok {
		t.Errorf(
			"subscription-pool fixture received unexpected account auth: call=%d account=%s",
			f.CallCount()+1,
			f.accountForAuthorization(r.Header.Get("Authorization")),
		)
		http.Error(w, "unexpected auth", http.StatusUnauthorized)
		return
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Content-Type", "application/json")
		retryAfterSeconds := response.retryAfterSeconds
		if retryAfterSeconds <= 0 {
			retryAfterSeconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
		if response.unifiedStatus != "" {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", response.unifiedStatus)
		}
		if response.representativeClaim != "" {
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", response.representativeClaim)
		}
		if response.fallbackAvailable {
			w.Header().Set("Anthropic-Ratelimit-Unified-Fallback", "available")
		}
		if response.unifiedStatus == "rejected" {
			w.Header().Set(
				"Anthropic-Ratelimit-Unified-Reset",
				fmt.Sprintf("%d", time.Now().UTC().Add(30*time.Second).Unix()),
			)
		}
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
	defer f.mu.Unlock()
	normalizedAuth := strings.TrimSpace(auth)
	index := f.nextResponseIndex
	if index < len(f.responses) {
		response := f.responses[index]
		if normalizedAuth == "Bearer "+response.token {
			f.calls = append(f.calls, response.account)
			f.nextResponseIndex++
			return response, response.account, true
		}
		if index > 0 {
			previous := f.responses[index-1]
			if previous.status == http.StatusTooManyRequests && normalizedAuth == "Bearer "+previous.token {
				f.calls = append(f.calls, previous.account)
				return previous, previous.account, true
			}
		}
		return liveSubscriptionResponse{}, "", false
	}
	if !f.repeatLast || len(f.responses) == 0 {
		return liveSubscriptionResponse{}, "", false
	}
	response := f.responses[len(f.responses)-1]
	if normalizedAuth != "Bearer "+response.token {
		return liveSubscriptionResponse{}, "", false
	}
	f.calls = append(f.calls, response.account)
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

func (f *liveSubscriptionFixture) AssertRotationCalls(t *testing.T, first, active string) {
	t.Helper()
	f.mu.Lock()
	got := append([]string(nil), f.calls...)
	f.mu.Unlock()
	if len(got) < 2 || got[0] != first {
		t.Fatalf("subscription-pool upstream accounts = %v, want %s then %s", got, first, active)
	}
	activeIndex := -1
	for index, account := range got[1:] {
		if account == active {
			activeIndex = index + 1
			break
		}
		if account != first {
			t.Fatalf(
				"subscription-pool upstream accounts before rotation = %v, want only %s until %s",
				got, first, active,
			)
		}
	}
	if activeIndex == -1 {
		t.Fatalf("subscription-pool upstream accounts = %v, want %s then %s", got, first, active)
	}
	for _, account := range got[activeIndex+1:] {
		if account != active {
			t.Fatalf("subscription-pool upstream accounts after rotation = %v, want only %s", got, active)
		}
	}
}

func (f *liveSubscriptionFixture) accountForAuthorization(auth string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, response := range f.responses {
		if strings.TrimSpace(auth) == "Bearer "+response.token {
			return response.account
		}
	}
	return "unknown"
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

func isolateLiveSubscriptionClaudeHome(t *testing.T) string {
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
	return configDir
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
