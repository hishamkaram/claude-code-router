//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

const (
	liveDegradedProviderResponse = "CCR_LIVE_DEGRADED_PROVIDER_OK"
	livePrivateAuthBody          = "CCR_LIVE_PRIVATE_AUTH_BODY"
)

type liveDegradedAuthFixture struct {
	server *httptest.Server

	mu                   sync.Mutex
	firstPartyAuthCalls  int
	providerMessageCalls int
}

func newLiveDegradedAuthFixture(t *testing.T) *liveDegradedAuthFixture {
	t.Helper()
	fixture := &liveDegradedAuthFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveDegradedAuthFixture) Close() {
	f.server.Close()
}

func (f *liveDegradedAuthFixture) StartGateway(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
	cfg.AnthropicBaseURL = f.server.URL
	return gateway.Start(ctx, cfg)
}

func (f *liveDegradedAuthFixture) Counts() (firstPartyAuthCalls, providerMessageCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstPartyAuthCalls, f.providerMessageCalls
}

func (f *liveDegradedAuthFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-provider"}]}`)
	case "/v1/messages/count_tokens":
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding count-tokens fixture request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if payload.Model != "fixture-provider" {
			f.writeAuthFailure(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":3}`)
	case "/v1/messages":
		var payload liveAnthropicMessagePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding message fixture request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if payload.Model != "fixture-provider" {
			f.writeAuthFailure(w)
			return
		}
		f.mu.Lock()
		f.providerMessageCalls++
		f.mu.Unlock()
		if payload.Stream {
			writeLiveAnthropicStream(w, payload.Model, liveDegradedProviderResponse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_live_degraded","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":2}}`, payload.Model, liveDegradedProviderResponse)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveDegradedAuthFixture) writeAuthFailure(w http.ResponseWriter) {
	f.mu.Lock()
	f.firstPartyAuthCalls++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":%q}}`, livePrivateAuthBody)
}

func TestLiveFixtureClaudeAuthDegradedKeepsProviderAliasRoutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), ".claude"))
	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")

	fixture := newLiveDegradedAuthFixture(t)
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	deps := Dependencies{
		StartGateway: fixture.StartGateway,
	}
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "fixture-provider", "--type", "anthropic", "--base-url", fixture.server.URL, "--no-api-key"},
		{"--db", dbPath, "model", "add", "fixture-provider", "--provider", "fixture-provider", "--model", "fixture-provider"},
	} {
		out, errOut, err := runLiveCommand(ctx, deps, args...)
		if err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	input := liveStreamInput(
		t,
		"/model sonnet",
		"Reply exactly CCR_LIVE_FIRST_PARTY_FAILED.",
		"/model anthropic.ccr.fixture-provider",
		"Reply exactly CCR_LIVE_DEGRADED_PROVIDER_OK.",
	)
	launchDeps := deps
	launchDeps.In = strings.NewReader(input)
	out, errOut, err := runLiveCommand(
		ctx,
		launchDeps,
		"--db", dbPath, "launch", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	)
	if err != nil {
		t.Fatalf("degraded-auth launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	combined := out + "\n" + errOut
	if !strings.Contains(combined, "re-login") {
		t.Fatalf("degraded-auth launch did not expose re-login guidance\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(combined, liveDegradedProviderResponse) {
		t.Fatalf("registered provider alias did not complete after first-party auth failure\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if strings.Contains(combined, livePrivateAuthBody) {
		t.Fatalf("upstream authentication body leaked through Claude Code output\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	firstPartyAuthCalls, providerMessageCalls := fixture.Counts()
	if firstPartyAuthCalls == 0 {
		t.Fatalf("live Claude Code never reached the first-party fixture")
	}
	if providerMessageCalls == 0 {
		t.Fatalf("live Claude Code never reached the registered provider fixture")
	}

	statusOut, statusErr, err := runLiveCommand(ctx, deps, "--db", dbPath, "status", "--json")
	if err != nil {
		t.Fatalf("status error = %v\nstdout:\n%s\nstderr:\n%s", err, statusOut, statusErr)
	}
	var status statusDocument
	if err := json.Unmarshal([]byte(statusOut), &status); err != nil {
		t.Fatalf("decode status JSON: %v\nstdout:\n%s", err, statusOut)
	}
	if status.SchemaVersion != 2 {
		t.Fatalf("status schema version = %d, want 2", status.SchemaVersion)
	}
	if status.ClaudeAuth.State != "needs_relogin" {
		t.Fatalf("Claude auth state = %q, want needs_relogin; status=%s", status.ClaudeAuth.State, statusOut)
	}
	if status.ClaudeAuth.Action != "claude /login" {
		t.Fatalf("Claude auth action = %q, want claude /login; status=%s", status.ClaudeAuth.Action, statusOut)
	}
}
