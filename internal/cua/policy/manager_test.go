package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestManagerRemembersLowRiskByProjectAndActionClass(t *testing.T) {
	t.Parallel()

	approver := newScriptedApprover(cua.DecisionApprove, cua.DecisionApprove, cua.DecisionApprove)
	manager := newTestManager(t, Config{Approver: approver, Executor: "docker"})

	if decision := authorizeAction(t, manager, "project-a", "call_1", cua.ActionScreenshot); decision != cua.DecisionApprove {
		t.Fatalf("first screenshot decision = %q", decision)
	}
	if decision := authorizeAction(t, manager, "project-a", "call_2", cua.ActionScreenshot); decision != cua.DecisionApprove {
		t.Fatalf("remembered screenshot decision = %q", decision)
	}
	if count := approver.count(); count != 1 {
		t.Fatalf("approval count after remembered request = %d, want 1", count)
	}

	_ = authorizeAction(t, manager, "project-b", "call_3", cua.ActionScreenshot)
	_ = authorizeAction(t, manager, "project-a", "call_4", cua.ActionScroll)
	if count := approver.count(); count != 3 {
		t.Fatalf("approval count after new project/action class = %d, want 3", count)
	}
}

func TestManagerRequiresFreshApprovalForHighRiskActions(t *testing.T) {
	t.Parallel()

	approver := newScriptedApprover(cua.DecisionApprove, cua.DecisionApprove)
	manager := newTestManager(t, Config{Approver: approver, Executor: "docker"})

	_ = authorizeAction(t, manager, "project-a", "call_1", cua.ActionClick)
	_ = authorizeAction(t, manager, "project-a", "call_2", cua.ActionClick)
	if count := approver.count(); count != 2 {
		t.Fatalf("high-risk approval count = %d, want 2", count)
	}
}

func TestManagerDeniesOnTimeoutAndShutdown(t *testing.T) {
	t.Parallel()

	timeoutApprover := newBlockingApprover()
	timeoutManager := newTestManager(t, Config{
		Approver:        timeoutApprover,
		ApprovalTimeout: 20 * time.Millisecond,
	})
	decision, err := timeoutManager.Authorize(context.Background(), "project-a", cua.Action{
		CallID: "call_timeout",
		Kind:   cua.ActionScreenshot,
	})
	if decision != cua.DecisionDeny || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout decision = %q, err = %v; want deny with deadline exceeded", decision, err)
	}

	shutdownApprover := newBlockingApprover()
	shutdownManager := newTestManager(t, Config{
		Approver:        shutdownApprover,
		ApprovalTimeout: time.Second,
	})
	results := make(chan managerResult, 1)
	go func() {
		decision, err := shutdownManager.Authorize(context.Background(), "project-a", cua.Action{
			CallID: "call_shutdown",
			Kind:   cua.ActionScreenshot,
		})
		results <- managerResult{decision: decision, err: err}
	}()
	shutdownApprover.awaitCall(t)
	if err := shutdownManager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result := awaitManagerResult(t, results)
	if result.decision != cua.DecisionDeny || !errors.Is(result.err, ErrClosed) {
		t.Fatalf("shutdown result = %#v, want deny with ErrClosed", result)
	}
}

func TestManagerCoalescesConcurrentLowRiskFirstApproval(t *testing.T) {
	t.Parallel()

	approver := newBlockingApprover()
	manager := newTestManager(t, Config{Approver: approver, ApprovalTimeout: time.Second})

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan managerResult, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := manager.Authorize(context.Background(), "project-a", cua.Action{
				CallID: "call_shared",
				Kind:   cua.ActionScreenshot,
			})
			results <- managerResult{decision: decision, err: err}
		}()
	}
	approver.awaitCall(t)
	if count := approver.count(); count != 1 {
		t.Fatalf("approval count before release = %d, want 1", count)
	}
	approver.release(cua.DecisionApprove)
	wg.Wait()
	close(results)

	for result := range results {
		if result.decision != cua.DecisionApprove || result.err != nil {
			t.Fatalf("concurrent result = %#v, want approve", result)
		}
	}
	_ = authorizeAction(t, manager, "project-a", "call_after", cua.ActionScreenshot)
	if count := approver.count(); count != 1 {
		t.Fatalf("approval count after remembered request = %d, want 1", count)
	}
}

type managerResult struct {
	decision cua.Decision
	err      error
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()
	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return manager
}

func authorizeAction(
	t *testing.T,
	manager *Manager,
	project string,
	callID string,
	kind cua.ActionKind,
) cua.Decision {
	t.Helper()
	decision, err := manager.Authorize(context.Background(), project, cua.Action{CallID: callID, Kind: kind})
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	return decision
}

func awaitManagerResult(t *testing.T, results <-chan managerResult) managerResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager result")
		return managerResult{}
	}
}

type scriptedApprover struct {
	mu        sync.Mutex
	requests  []cua.ApprovalRequest
	decisions []cua.Decision
}

func newScriptedApprover(decisions ...cua.Decision) *scriptedApprover {
	return &scriptedApprover{decisions: decisions}
}

func (a *scriptedApprover) Approve(_ context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, request)
	if len(a.decisions) == 0 {
		return cua.DecisionDeny, nil
	}
	decision := a.decisions[0]
	a.decisions = a.decisions[1:]
	return decision, nil
}

func (a *scriptedApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

type blockingApprover struct {
	mu       sync.Mutex
	called   chan struct{}
	releaseC chan cua.Decision
	requests []cua.ApprovalRequest
}

func newBlockingApprover() *blockingApprover {
	return &blockingApprover{
		called:   make(chan struct{}),
		releaseC: make(chan cua.Decision, 1),
	}
}

func (a *blockingApprover) Approve(ctx context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	if len(a.requests) == 1 {
		close(a.called)
	}
	a.mu.Unlock()

	select {
	case decision := <-a.releaseC:
		return decision, nil
	case <-ctx.Done():
		return cua.DecisionDeny, ctx.Err()
	}
}

func (a *blockingApprover) awaitCall(t *testing.T) {
	t.Helper()
	select {
	case <-a.called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approver call")
	}
}

func (a *blockingApprover) release(decision cua.Decision) {
	a.releaseC <- decision
}

func (a *blockingApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}
