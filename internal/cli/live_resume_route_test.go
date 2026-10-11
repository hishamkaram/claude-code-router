//go:build live && (linux || darwin)

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	liveResumeRouteProviderSecretEnv = "CCR_LIVE_RESUME_ROUTE_PROVIDER_SECRET"
	liveResumeRouteProviderSecret    = "ccr-live-resume-route-provider-secret"
	liveResumeRouteProviderName      = "resume-route-provider"
	liveResumeRouteModelAlias        = "resume-route"
	liveResumeRouteProviderModel     = "resume-route-model"
	liveResumeRouteInitialResponse   = "CCR_LIVE_RESUME_ROUTE_INITIAL_OK"
	liveResumeRouteResumedResponse   = "CCR_LIVE_RESUME_ROUTE_RESUMED_OK"
)

func TestLiveResumeRouteRestoresRecordedProviderRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	isolateLiveResumeRouteState(t)
	t.Setenv(liveResumeRouteProviderSecretEnv, liveResumeRouteProviderSecret)
	provider := newLiveResumeRouteProvider(t)
	t.Cleanup(provider.Close)
	database := filepath.Join(t.TempDir(), "ccr.db")
	configureLiveResumeRouteModel(t, ctx, database, provider.URL())

	initialOut, initialErrOut, initialErr := runLiveCommand(
		ctx,
		Dependencies{In: strings.NewReader("Remember the route continuity marker.\n")},
		"--db", database, "launch", "--model", liveResumeRouteModelAlias,
		"--print", "--auth-mode", "provider-only", "--no-statusline", "--",
		"--output-format", "stream-json", "--verbose", "--max-turns", "1", "--tools", "",
	)
	if initialErr != nil {
		t.Fatalf("initial live launch error\n%s", liveResumeRouteDiagnostics(liveResumeRouteProviderSecret, liveResumeRouteErrorText(initialErr), initialOut, initialErrOut))
	}
	assertLiveResumeRouteOutput(t, initialOut, initialErrOut, liveResumeRouteInitialResponse)
	assertLiveResumeRouteNoSecret(t, database, initialOut, initialErrOut)

	initialLaunch, initialSession, recorded := readLiveResumeRouteEvidence(t, ctx, database)
	if initialLaunch.AuthMode != launchAuthModeProviderOnly || initialLaunch.State != "completed" {
		t.Fatalf("initial launch did not complete as provider-only: state=%q auth=%q", initialLaunch.State, initialLaunch.AuthMode)
	}
	if initialSession.ClaudeSessionID == "" || initialSession.ActiveRouteKind != "registered" ||
		initialSession.ActiveModelAlias != liveResumeRouteModelAlias ||
		initialSession.ActiveProviderName != liveResumeRouteProviderName ||
		initialSession.ActiveProviderModel != liveResumeRouteProviderModel {
		t.Fatalf("initial live session route evidence incomplete: %#v", initialSession)
	}
	if recorded.SourceLaunchID != initialLaunch.ID || recorded.ModelAlias != liveResumeRouteModelAlias ||
		recorded.ProviderName != liveResumeRouteProviderName || recorded.ProviderModel != liveResumeRouteProviderModel {
		t.Fatalf("recorded resume route = %#v", recorded)
	}

	modelsBeforeResume := provider.modelCount()
	chatBeforeResume := provider.chatCount()
	resumeOut, resumeErrOut, resumeErr := runLiveCommand(
		ctx,
		Dependencies{In: strings.NewReader("Continue this same native session and report the route marker.\n")},
		"--db", database, "launch", "--resume-route", "--print", "--no-statusline", "--",
		"--resume", initialSession.ClaudeSessionID, "--output-format", "stream-json", "--verbose", "--max-turns", "1", "--tools", "",
	)
	if resumeErr != nil {
		t.Fatalf("resume-route live launch error\n%s", liveResumeRouteDiagnostics(liveResumeRouteProviderSecret, liveResumeRouteErrorText(resumeErr), resumeOut, resumeErrOut))
	}
	assertLiveResumeRouteOutput(t, resumeOut, resumeErrOut, liveResumeRouteResumedResponse)
	assertLiveResumeRouteNoSecret(t, database, resumeOut, resumeErrOut)
	if provider.chatCount() <= chatBeforeResume {
		t.Fatalf("resume-route did not send a second provider chat request: chats before=%d after=%d", chatBeforeResume, provider.chatCount())
	}
	if provider.modelCount() != modelsBeforeResume {
		t.Fatalf("resume-route re-ran provider model discovery: models before=%d after=%d", modelsBeforeResume, provider.modelCount())
	}
	payloads := provider.chatPayloads()
	if len(payloads) < 2 {
		t.Fatalf("provider chat requests = %d, want at least 2", len(payloads))
	}
	resumedPayload := payloads[len(payloads)-1]
	if !openAIMessagesContain(resumedPayload.Messages, liveResumeRouteInitialResponse) {
		t.Fatalf("resumed provider request did not contain the initial native session response")
	}
	assertLiveResumedRouteEvidence(t, ctx, database, initialSession.ClaudeSessionID, initialLaunch.ID)

	mutateLiveResumeRouteModel(t, ctx, database)
	requestsBeforeDrift := provider.requestCount()
	launchesBeforeDrift := countLiveResumeRouteLaunches(t, ctx, database)
	driftOut, driftErrOut, driftErr := runLiveCommand(
		ctx,
		Dependencies{},
		"--db", database, "launch", "--resume-route", "--print", "--no-statusline", "--",
		"--resume", initialSession.ClaudeSessionID, "--output-format", "stream-json", "--verbose", "--max-turns", "1", "--tools", "",
	)
	if driftErr == nil || !strings.Contains(driftErr.Error(), "no longer matches its current provider configuration") {
		t.Fatalf("configuration drift error\n%s", liveResumeRouteDiagnostics(liveResumeRouteProviderSecret, liveResumeRouteErrorText(driftErr), driftOut, driftErrOut))
	}
	assertLiveResumeRouteNoSecret(t, database, driftOut, driftErrOut, liveResumeRouteErrorText(driftErr))
	if provider.requestCount() != requestsBeforeDrift {
		t.Fatalf("configuration drift contacted provider: requests before=%d after=%d", requestsBeforeDrift, provider.requestCount())
	}
	if got := countLiveResumeRouteLaunches(t, ctx, database); got != launchesBeforeDrift {
		t.Fatalf("configuration drift created a launch: before=%d after=%d", launchesBeforeDrift, got)
	}
}

func isolateLiveResumeRouteState(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude-config"))
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
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
}

type liveResumeRouteProvider struct {
	t        *testing.T
	server   *httptest.Server
	requests atomic.Int64
	models   atomic.Int64
	chats    atomic.Int64
	mu       sync.Mutex
	payloads []liveOpenAIChatPayload
}

func newLiveResumeRouteProvider(t *testing.T) *liveResumeRouteProvider {
	t.Helper()
	provider := &liveResumeRouteProvider{t: t}
	provider.server = httptest.NewServer(provider)
	return provider
}

func (p *liveResumeRouteProvider) URL() string {
	return p.server.URL
}

func (p *liveResumeRouteProvider) Close() {
	p.server.Close()
}

func (p *liveResumeRouteProvider) requestCount() int64 {
	return p.requests.Load()
}

func (p *liveResumeRouteProvider) chatCount() int64 {
	return p.chats.Load()
}

func (p *liveResumeRouteProvider) modelCount() int64 {
	return p.models.Load()
}

func (p *liveResumeRouteProvider) chatPayloads() []liveOpenAIChatPayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]liveOpenAIChatPayload(nil), p.payloads...)
}

func (p *liveResumeRouteProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.requests.Add(1)
	if r.Header.Get("Authorization") != "Bearer "+liveResumeRouteProviderSecret {
		http.Error(w, "provider authentication rejected", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		p.models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": liveResumeRouteProviderModel}},
		})
	case "/v1/chat/completions":
		var payload liveOpenAIChatPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			p.t.Errorf("resume-route provider request decode error = %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if payload.Model != liveResumeRouteProviderModel {
			p.t.Errorf("resume-route provider model was not restored")
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.payloads = append(p.payloads, payload)
		p.mu.Unlock()
		call := p.chats.Add(1)
		response := liveResumeRouteInitialResponse
		if call > 1 {
			response = liveResumeRouteResumedResponse
		}
		writeLiveOpenAITextFixture(w, payload, "chatcmpl-live-resume-route", response, 4, 2)
	default:
		http.NotFound(w, r)
	}
}

func configureLiveResumeRouteModel(t *testing.T, ctx context.Context, database, baseURL string) {
	t.Helper()
	commands := [][]string{
		{"--db", database, "provider", "add", liveResumeRouteProviderName, "--type", "litellm", "--base-url", baseURL, "--api-key-env", liveResumeRouteProviderSecretEnv},
		{"--db", database, "model", "add", liveResumeRouteModelAlias, "--provider", liveResumeRouteProviderName, "--model", liveResumeRouteProviderModel},
	}
	for _, args := range commands {
		out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...)
		if err != nil {
			t.Fatalf("run %v error\n%s", args, liveResumeRouteDiagnostics(liveResumeRouteProviderSecret, liveResumeRouteErrorText(err), out, errOut))
		}
		assertLiveResumeRouteNoSecret(t, database, out, errOut)
	}
}

func readLiveResumeRouteEvidence(t *testing.T, ctx context.Context, database string) (store.Launch, store.ClaudeSession, store.ResumeRoute) {
	t.Helper()
	s := openLiveResumeRouteStore(t, ctx, database)
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 1 {
		t.Fatalf("initial launches = %#v, error = %v", launches, err)
	}
	sessions, err := s.ListClaudeSessions(ctx, launches[0].ID, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("initial Claude sessions = %#v, error = %v", sessions, err)
	}
	route, err := s.FindResumeRoute(ctx, sessions[0].ClaudeSessionID)
	if err != nil {
		t.Fatalf("FindResumeRoute() error = %v", err)
	}
	return launches[0], sessions[0], route
}

func assertLiveResumedRouteEvidence(t *testing.T, ctx context.Context, database, nativeSessionID string, initialLaunchID int64) {
	t.Helper()
	s := openLiveResumeRouteStore(t, ctx, database)
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 2 {
		t.Fatalf("launches after resume = %#v, error = %v", launches, err)
	}
	resumedLaunch := launches[0]
	if resumedLaunch.ID == initialLaunchID || resumedLaunch.AuthMode != launchAuthModeProviderOnly || resumedLaunch.ModelAlias != liveResumeRouteModelAlias || resumedLaunch.State != "completed" {
		t.Fatalf("resumed launch = %#v", resumedLaunch)
	}
	sessions, err := s.ListClaudeSessions(ctx, resumedLaunch.ID, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("resumed Claude sessions = %#v, error = %v", sessions, err)
	}
	resumedSession := sessions[0]
	if resumedSession.ClaudeSessionID != nativeSessionID || resumedSession.ActiveRouteKind != "registered" ||
		resumedSession.ActiveModelAlias != liveResumeRouteModelAlias ||
		resumedSession.ActiveProviderName != liveResumeRouteProviderName ||
		resumedSession.ActiveProviderModel != liveResumeRouteProviderModel {
		t.Fatalf("resumed live session route evidence incomplete: %#v", resumedSession)
	}
	route, err := s.FindResumeRoute(ctx, nativeSessionID)
	if err != nil || route.SourceLaunchID != resumedLaunch.ID || route.ProviderModel != liveResumeRouteProviderModel {
		t.Fatalf("latest recorded resume route = %#v, error = %v", route, err)
	}
}

func mutateLiveResumeRouteModel(t *testing.T, ctx context.Context, database string) {
	t.Helper()
	s := openLiveResumeRouteStore(t, ctx, database)
	defer func() { _ = s.Close() }()
	model, err := s.GetModel(ctx, liveResumeRouteModelAlias)
	if err != nil {
		t.Fatalf("GetModel() error = %v", err)
	}
	model.ProviderModel = "non-routable-drifted-model"
	if err := s.UpdateModel(ctx, model); err != nil {
		t.Fatalf("UpdateModel() error = %v", err)
	}
}

func countLiveResumeRouteLaunches(t *testing.T, ctx context.Context, database string) int {
	t.Helper()
	s := openLiveResumeRouteStore(t, ctx, database)
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		t.Fatalf("ListLaunches() error = %v", err)
	}
	return len(launches)
}

func openLiveResumeRouteStore(t *testing.T, ctx context.Context, database string) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, database)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	return s
}

func assertLiveResumeRouteOutput(t *testing.T, stdout, stderr, response string) {
	t.Helper()
	if !strings.Contains(stdout, response) {
		t.Fatalf("live resume-route output missing %q\n%s", response, liveResumeRouteDiagnostics(liveResumeRouteProviderSecret, stdout, stderr))
	}
}

func assertLiveResumeRouteNoSecret(t *testing.T, database string, output ...string) {
	t.Helper()
	for _, value := range output {
		if strings.Contains(value, liveResumeRouteProviderSecret) {
			t.Fatal("live resume-route output exposed the provider secret")
		}
	}
	paths, err := filepath.Glob(database + "*")
	if err != nil {
		t.Fatalf("database artifact scan error = %v", err)
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("database artifact read error = %v", err)
		}
		if strings.Contains(string(contents), liveResumeRouteProviderSecret) {
			t.Fatal("runtime database artifact stored the provider secret")
		}
	}
}

func liveResumeRouteDiagnostics(secret string, output ...string) string {
	var builder strings.Builder
	for index, value := range output {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(strings.ReplaceAll(value, secret, "[provider-secret]"))
	}
	return builder.String()
}

func liveResumeRouteErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
