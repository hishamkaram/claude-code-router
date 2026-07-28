package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type continuityCommandResult struct {
	out    string
	errOut string
	err    error
}

func TestSubscriptionPoolKeepsClaudeOpenWhenNoReplacementIsUsable(t *testing.T) {
	t.Parallel()

	dbPath := seedSubscriptionAccounts(t, []subscriptionAccountFixture{
		{name: "personal", token: "personal-oauth-token"},
	})
	secrets := &accountTestSecrets{values: map[string]string{
		secret.ClaudeAccountAccessTokenRef("personal"): "personal-oauth-token",
	}}
	process := newContinuityTestProcess()
	launcher := &continuityTestLauncher{
		process: process, started: make(chan struct{}),
	}
	exhaustionSinks := make(chan (chan<- gateway.AnthropicSubscriptionExhaustionEvent), 1)
	startGateway := func(ctx context.Context, config gateway.Config) (*gateway.Server, error) {
		server, err := gateway.Start(ctx, config)
		if err == nil {
			exhaustionSinks <- config.AnthropicSubscriptionExhaustion
		}
		return server, err
	}

	result := make(chan continuityCommandResult, 1)
	go func() {
		out, errOut, err := runCommandWithDeps(t, Dependencies{
			Secrets: secrets, Launcher: launcher, StartGateway: startGateway,
		}, "--db", dbPath, "launch", "--auth-mode", "subscription-pool",
			"--no-lifecycle", "--no-statusline")
		result <- continuityCommandResult{out: out, errOut: errOut, err: err}
	}()

	var exhaustion chan<- gateway.AnthropicSubscriptionExhaustionEvent
	select {
	case exhaustion = <-exhaustionSinks:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway did not expose the subscription exhaustion sink")
	}
	select {
	case <-launcher.started:
	case <-time.After(10 * time.Second):
		t.Fatal("Claude process did not start")
	}
	exhaustion <- gateway.AnthropicSubscriptionExhaustionEvent{
		StatusCode: 429, RetryAfterDuration: time.Hour,
		RepresentativeClaim: gateway.AnthropicRateLimitClaimSevenDay,
	}
	waitForClaudeAccountFailure(t, dbPath, "personal", "subscription_limit_seven_day")
	if process.StopCalls() != 0 {
		t.Fatalf("Claude Stop calls = %d, want 0", process.StopCalls())
	}
	select {
	case got := <-result:
		t.Fatalf("CCR exited while Claude should remain open: err=%v stderr=%q", got.err, got.errOut)
	default:
	}

	process.Complete(nil)
	var got continuityCommandResult
	select {
	case got = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("CCR did not return after Claude exited naturally")
	}
	assertContinuityCommandResult(t, got, launcher)
}

func assertContinuityCommandResult(
	t *testing.T,
	got continuityCommandResult,
	launcher *continuityTestLauncher,
) {
	t.Helper()
	if got.err != nil {
		t.Fatalf("subscription-pool continuity error = %v, stderr=%q", got.err, got.errOut)
	}
	if launcher.StartCount() != 1 {
		t.Fatalf("Claude starts = %d, want 1", launcher.StartCount())
	}
	for _, want := range []string{
		"No replacement account is currently usable",
		"keeping the current Claude Code process open",
		"retrying pool selection after the next rejected quota response",
	} {
		if !strings.Contains(got.errOut, want) {
			t.Fatalf("continuity notice missing %q: %s", want, got.errOut)
		}
	}
	if strings.Contains(got.out+got.errOut, "personal-oauth-token") {
		t.Fatal("continuity output leaked an OAuth token")
	}
}

func TestWaitForClaudeProcessRetriesExhaustionDecisionBeforeStopping(t *testing.T) {
	t.Parallel()

	process := newContinuityTestProcess()
	events := make(chan gateway.AnthropicSubscriptionExhaustionEvent, 2)
	events <- gateway.AnthropicSubscriptionExhaustionEvent{StatusCode: 429}
	events <- gateway.AnthropicSubscriptionExhaustionEvent{StatusCode: 429}
	decisions := 0
	decisionsAtStop := 0
	process.onStop = func() {
		decisionsAtStop = decisions
	}
	control := subscriptionExhaustionControl{
		events: events,
		handle: func(io.Writer, gateway.AnthropicSubscriptionExhaustionEvent) bool {
			decisions++
			return decisions == 2
		},
	}

	waitErr, event, stopErr := waitForClaudeProcess(context.Background(), process, control, io.Discard)
	if waitErr != nil || event == nil || stopErr != nil {
		t.Fatalf("waitForClaudeProcess() = wait=%v event=%v stop=%v", waitErr, event, stopErr)
	}
	if decisions != 2 {
		t.Fatalf("exhaustion decisions = %d, want 2", decisions)
	}
	if decisionsAtStop != 2 {
		t.Fatalf("Claude stopped after %d decisions, want replacement decision first", decisionsAtStop)
	}
	if process.StopCalls() != 1 {
		t.Fatalf("Claude Stop calls = %d, want 1", process.StopCalls())
	}
}

func TestWaitForClaudeProcessUsesProvidedNoticeWriter(t *testing.T) {
	t.Parallel()

	process := newContinuityTestProcess()
	events := make(chan gateway.AnthropicSubscriptionExhaustionEvent, 1)
	events <- gateway.AnthropicSubscriptionExhaustionEvent{StatusCode: 429}
	noticeOut := &strings.Builder{}
	var received io.Writer
	control := subscriptionExhaustionControl{
		events: events,
		handle: func(out io.Writer, _ gateway.AnthropicSubscriptionExhaustionEvent) bool {
			received = out
			process.Complete(nil)
			return false
		},
	}

	waitErr, event, stopErr := waitForClaudeProcess(
		context.Background(), process, control, noticeOut,
	)
	if waitErr != nil || event != nil || stopErr != nil {
		t.Fatalf("waitForClaudeProcess() = wait=%v event=%v stop=%v", waitErr, event, stopErr)
	}
	if received != noticeOut {
		t.Fatal("exhaustion handler did not receive the process-synchronized notice writer")
	}
}

func waitForClaudeAccountFailure(t *testing.T, dbPath, name, failureClass string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	deadline := time.Now().Add(10 * time.Second)
	for {
		account, getErr := s.GetClaudeAccount(ctx, name)
		if getErr == nil && account.LastError == failureClass {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Claude account %q failure = %q, error=%v, want %q",
				name, account.LastError, getErr, failureClass)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type continuityTestLauncher struct {
	mu      sync.Mutex
	starts  int
	process *continuityTestProcess
	started chan struct{}
	once    sync.Once
}

func (l *continuityTestLauncher) Start(
	ctx context.Context,
	_ []string,
	_ ClaudeEnvironment,
	_ io.Reader,
	_, _ io.Writer,
) (ClaudeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.starts++
	l.mu.Unlock()
	l.once.Do(func() {
		close(l.started)
	})
	return l.process, nil
}

func (l *continuityTestLauncher) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts
}

type continuityTestProcess struct {
	done      chan error
	finish    sync.Once
	mu        sync.Mutex
	stopCalls int
	onStop    func()
}

func newContinuityTestProcess() *continuityTestProcess {
	return &continuityTestProcess{done: make(chan error, 1)}
}

func (p *continuityTestProcess) PID() int {
	return 9100
}

func (p *continuityTestProcess) Done() <-chan error {
	return p.done
}

func (p *continuityTestProcess) Stop() error {
	p.mu.Lock()
	p.stopCalls++
	onStop := p.onStop
	p.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	p.Complete(nil)
	return nil
}

func (p *continuityTestProcess) Complete(err error) {
	p.finish.Do(func() {
		p.done <- err
		close(p.done)
	})
}

func (p *continuityTestProcess) StopCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCalls
}
