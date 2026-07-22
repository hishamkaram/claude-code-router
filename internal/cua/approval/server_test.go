package approval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestServerApprovalAPIUsesScopedTokens(t *testing.T) {
	t.Parallel()

	prompts := make(chan Prompt, 2)
	server := startTestServer(t, Config{
		Timeout: time.Minute,
		Notify: func(ctx context.Context, prompt Prompt) error {
			select {
			case prompts <- prompt:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})

	first := startApproval(t, server, "call_1", cua.ActionScreenshot)
	firstPrompt := awaitPrompt(t, prompts)
	second := startApproval(t, server, "call_2", cua.ActionScroll)
	secondPrompt := awaitPrompt(t, prompts)
	if firstPrompt.URL == secondPrompt.URL {
		t.Fatal("approval prompts reused the same scoped URL")
	}

	firstQuery := promptQuery(t, firstPrompt)
	secondQuery := promptQuery(t, secondPrompt)
	if firstQuery.Get("request_id") == "" || firstQuery.Get("token") == "" {
		t.Fatalf("approval prompt did not include request_id and token: %q", firstPrompt.URL)
	}
	if firstQuery.Get("token") == secondQuery.Get("token") {
		t.Fatal("approval prompts reused the same token")
	}

	if status := getStatus(t, server.URL()+"/api/request?"+firstQuery.Encode(), nil); status != http.StatusOK {
		t.Fatalf("GET /api/request status = %d, want %d", status, http.StatusOK)
	}
	wrongToken := url.Values{}
	wrongToken.Set("request_id", secondQuery.Get("request_id"))
	wrongToken.Set("token", firstQuery.Get("token"))
	if status := postDecision(t, server, wrongToken, cua.DecisionApprove, server.URL()); status != http.StatusUnauthorized {
		t.Fatalf("cross-scoped token status = %d, want %d", status, http.StatusUnauthorized)
	}

	if status := postDecision(t, server, firstQuery, cua.DecisionApprove, server.URL()); status != http.StatusOK {
		t.Fatalf("first decision status = %d, want %d", status, http.StatusOK)
	}
	if status := postDecision(t, server, secondQuery, cua.DecisionDeny, server.URL()); status != http.StatusOK {
		t.Fatalf("second decision status = %d, want %d", status, http.StatusOK)
	}
	if result := awaitResult(t, first); result.decision != cua.DecisionApprove || result.err != nil {
		t.Fatalf("first approval result = %#v", result)
	}
	if result := awaitResult(t, second); result.decision != cua.DecisionDeny || result.err != nil {
		t.Fatalf("second approval result = %#v", result)
	}
}

func TestServerRejectsInvalidHostOriginAndObserverToken(t *testing.T) {
	t.Parallel()

	prompts := make(chan Prompt, 1)
	server := startTestServer(t, Config{
		Timeout: time.Minute,
		Notify: func(ctx context.Context, prompt Prompt) error {
			select {
			case prompts <- prompt:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	result := startApproval(t, server, "call_1", cua.ActionScreenshot)
	query := promptQuery(t, awaitPrompt(t, prompts))

	if status := getStatus(t, server.URL()+"/api/request?"+query.Encode(), func(req *http.Request) {
		req.Host = "evil.example"
	}); status != http.StatusForbidden {
		t.Fatalf("bad host status = %d, want %d", status, http.StatusForbidden)
	}
	if status := getStatus(t, server.URL()+"/api/request", func(req *http.Request) {
		req.Header.Set("X-CCR-Observer-Token", "gateway-observer-token")
	}); status != http.StatusUnauthorized {
		t.Fatalf("observer token status = %d, want %d", status, http.StatusUnauthorized)
	}
	if status := postDecision(t, server, query, cua.DecisionApprove, ""); status != http.StatusForbidden {
		t.Fatalf("missing origin status = %d, want %d", status, http.StatusForbidden)
	}
	if status := postDecision(t, server, query, cua.DecisionApprove, "http://evil.example"); status != http.StatusForbidden {
		t.Fatalf("bad origin status = %d, want %d", status, http.StatusForbidden)
	}

	if status := postDecision(t, server, query, cua.DecisionDeny, server.URL()); status != http.StatusOK {
		t.Fatalf("valid deny status = %d, want %d", status, http.StatusOK)
	}
	if got := awaitResult(t, result); got.decision != cua.DecisionDeny || got.err != nil {
		t.Fatalf("approval result = %#v", got)
	}
}

func TestServerRejectsExpiredBrowserToken(t *testing.T) {
	t.Parallel()

	var nowMu sync.Mutex
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	prompts := make(chan Prompt, 1)
	server := startTestServer(t, Config{
		Timeout: time.Hour,
		Now: func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			return now
		},
		Notify: func(ctx context.Context, prompt Prompt) error {
			select {
			case prompts <- prompt:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	result := startApproval(t, server, "call_expired", cua.ActionScreenshot)
	query := promptQuery(t, awaitPrompt(t, prompts))

	nowMu.Lock()
	now = now.Add(time.Hour + time.Second)
	nowMu.Unlock()

	if status := getStatus(t, server.URL()+"/api/request?"+query.Encode(), nil); status != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want %d", status, http.StatusUnauthorized)
	}
	got := awaitResult(t, result)
	if got.decision != cua.DecisionDeny || !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("expired token result = %#v, want deny with deadline exceeded", got)
	}
}

func TestServerDeniesOnTimeoutAndShutdown(t *testing.T) {
	t.Parallel()

	timeoutPrompts := make(chan Prompt, 1)
	timeoutServer := startTestServer(t, Config{
		Timeout: 500 * time.Millisecond,
		Notify: func(ctx context.Context, prompt Prompt) error {
			select {
			case timeoutPrompts <- prompt:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	timeoutResult := startApproval(t, timeoutServer, "call_timeout", cua.ActionScreenshot)
	_ = awaitPrompt(t, timeoutPrompts)
	gotTimeout := awaitResult(t, timeoutResult)
	if gotTimeout.decision != cua.DecisionDeny || !errors.Is(gotTimeout.err, context.DeadlineExceeded) {
		t.Fatalf("timeout result = %#v, want deny with deadline exceeded", gotTimeout)
	}

	shutdownPrompts := make(chan Prompt, 1)
	shutdownServer := startTestServer(t, Config{
		Timeout: time.Second,
		Notify: func(ctx context.Context, prompt Prompt) error {
			select {
			case shutdownPrompts <- prompt:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	shutdownResult := startApproval(t, shutdownServer, "call_shutdown", cua.ActionScreenshot)
	_ = awaitPrompt(t, shutdownPrompts)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdownServer.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	gotShutdown := awaitResult(t, shutdownResult)
	if gotShutdown.decision != cua.DecisionDeny || !errors.Is(gotShutdown.err, ErrClosed) {
		t.Fatalf("shutdown result = %#v, want deny with ErrClosed", gotShutdown)
	}
}

func TestServerUsesFallbackWhenPromptCannotBeDelivered(t *testing.T) {
	t.Parallel()

	fallback := &fakeApprover{decision: cua.DecisionApprove}
	server := startTestServer(t, Config{
		Timeout:  time.Second,
		Fallback: fallback,
		Notify: func(context.Context, Prompt) error {
			return errors.New("terminal prompt unavailable")
		},
	})
	decision, err := server.Approve(context.Background(), cua.ApprovalRequest{
		ActionID: "call_1",
		Kind:     cua.ActionScreenshot,
		Risk:     cua.RiskLow,
		Executor: "docker",
	})
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if decision != cua.DecisionApprove {
		t.Fatalf("Approve() decision = %q, want %q", decision, cua.DecisionApprove)
	}
	if count := fallback.count(); count != 1 {
		t.Fatalf("fallback call count = %d, want 1", count)
	}
}

func TestValidateApprovalRequestAllowsMove(t *testing.T) {
	t.Parallel()

	err := validateApprovalRequest(cua.ApprovalRequest{
		ActionID: "call_move",
		Kind:     cua.ActionMove,
		Risk:     cua.RiskLow,
		Executor: "docker",
	})
	if err != nil {
		t.Fatalf("validateApprovalRequest() error = %v", err)
	}
}

type approvalResult struct {
	decision cua.Decision
	err      error
}

func startTestServer(t *testing.T, config Config) *Server {
	t.Helper()
	server, err := Start(context.Background(), config)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return server
}

func startApproval(
	t *testing.T,
	server *Server,
	actionID string,
	kind cua.ActionKind,
) <-chan approvalResult {
	t.Helper()
	results := make(chan approvalResult, 1)
	go func() {
		decision, err := server.Approve(context.Background(), cua.ApprovalRequest{
			ActionID: actionID,
			Kind:     kind,
			Risk:     cua.RiskLow,
			Executor: "docker",
		})
		results <- approvalResult{decision: decision, err: err}
	}()
	return results
}

func awaitPrompt(t *testing.T, prompts <-chan Prompt) Prompt {
	t.Helper()
	select {
	case prompt := <-prompts:
		return prompt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval prompt")
		return Prompt{}
	}
}

func awaitResult(t *testing.T, results <-chan approvalResult) approvalResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval result")
		return approvalResult{}
	}
}

func promptQuery(t *testing.T, prompt Prompt) url.Values {
	t.Helper()
	parsed, err := url.Parse(prompt.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", prompt.URL, err)
	}
	if parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "/approve" {
		t.Fatalf("prompt URL = %q, want loopback approval URL", prompt.URL)
	}
	return parsed.Query()
}

func getStatus(t *testing.T, target string, mutate func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("Body.Close() error = %v", err)
		}
	}()
	return resp.StatusCode
}

func postDecision(
	t *testing.T,
	server *Server,
	query url.Values,
	decision cua.Decision,
	origin string,
) int {
	t.Helper()
	body := fmt.Sprintf(
		`{"request_id":%q,"token":%q,"decision":%q}`,
		query.Get("request_id"),
		query.Get("token"),
		decision,
	)
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		server.URL()+"/api/decision",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("Body.Close() error = %v", err)
		}
	}()
	return resp.StatusCode
}

type fakeApprover struct {
	mu       sync.Mutex
	decision cua.Decision
	requests []cua.ApprovalRequest
}

func (a *fakeApprover) Approve(_ context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, request)
	return a.decision, nil
}

func (a *fakeApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}
