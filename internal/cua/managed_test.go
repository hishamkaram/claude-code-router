package cua

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestManagedRuntimeApprovesExecutesAndAuditsRedactedMetadata(t *testing.T) {
	t.Parallel()

	executor := &managedTestExecutor{name: "external:fixture", observation: Observation{Screenshot: []byte("png"), ContentType: "image/png"}}
	authorizer := &managedTestAuthorizer{decision: DecisionApprove}
	auditor := &managedTestAuditor{}
	runtime, err := NewManagedRuntime(context.Background(), Config{
		Mode: ModeManaged, Executor: "external:fixture", MaxTurns: 2, MaxActions: 3, Timeout: time.Minute,
	}, executor, authorizer, auditor)
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if beginErr := runtime.BeginTurn(context.Background()); beginErr != nil {
		t.Fatalf("BeginTurn() error = %v", beginErr)
	}
	observation, err := runtime.Execute(context.Background(), "project", Action{
		CallID: "call_1", Kind: ActionType, Text: "sensitive text", Raw: []byte(`{"text":"sensitive text"}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(observation.Screenshot) != "png" || executor.calls != 1 {
		t.Fatalf("Execute() observation=%#v calls=%d", observation, executor.calls)
	}
	events := auditor.Events()
	if len(events) != 1 || events[0].Action != ActionType || events[0].Executor != "external:fixture" || events[0].Status != "approved" {
		t.Fatalf("audit events = %#v", events)
	}
}

func TestManagedRuntimeDeniesWithoutExecuting(t *testing.T) {
	t.Parallel()

	executor := &managedTestExecutor{name: "docker"}
	runtime, err := NewManagedRuntime(context.Background(), Config{Mode: ModeManaged, Executor: "docker"}, executor,
		&managedTestAuthorizer{decision: DecisionDeny}, &managedTestAuditor{})
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	defer func() { _ = runtime.Close() }()

	_, err = runtime.Execute(context.Background(), "project", Action{CallID: "call_1", Kind: ActionClick})
	if !errors.Is(err, ErrApprovalDeny) {
		t.Fatalf("Execute() error = %v, want ErrApprovalDeny", err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
}

func TestManagedRuntimeEnforcesTurnAndActionLimits(t *testing.T) {
	t.Parallel()

	executor := &managedTestExecutor{name: "docker"}
	runtime, err := NewManagedRuntime(context.Background(), Config{
		Mode: ModeManaged, Executor: "docker", MaxTurns: 1, MaxActions: 1, Timeout: time.Minute,
	}, executor, &managedTestAuthorizer{decision: DecisionApprove}, &managedTestAuditor{})
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if err := runtime.BeginTurn(context.Background()); err != nil {
		t.Fatalf("first BeginTurn() error = %v", err)
	}
	if err := runtime.BeginTurn(context.Background()); !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("second BeginTurn() error = %v, want ErrTurnLimit", err)
	}
	if _, err := runtime.Execute(context.Background(), "project", Action{CallID: "call_1", Kind: ActionWait}); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := runtime.Execute(context.Background(), "project", Action{CallID: "call_2", Kind: ActionWait}); !errors.Is(err, ErrActionLimit) {
		t.Fatalf("second Execute() error = %v, want ErrActionLimit", err)
	}
}

func TestManagedRuntimeCloseCancelsAndWaitsForActiveExecution(t *testing.T) {
	t.Parallel()

	executor := &managedTestExecutor{name: "docker", waitForContext: true, started: make(chan struct{})}
	runtime, err := NewManagedRuntime(context.Background(), Config{Mode: ModeManaged, Executor: "docker", Timeout: time.Minute}, executor,
		&managedTestAuthorizer{decision: DecisionApprove}, &managedTestAuditor{})
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, executeErr := runtime.Execute(context.Background(), "project", Action{CallID: "call_1", Kind: ActionWait})
		done <- executeErr
	}()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case executeErr := <-done:
		if !errors.Is(executeErr, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", executeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("active execution did not stop")
	}
	if !executor.closed {
		t.Fatal("executor was not closed")
	}
}

func TestManagedRuntimeSerializesActionBatches(t *testing.T) {
	started := make(chan string, 3)
	releaseFirst := make(chan struct{}, 1)
	executor := &serialManagedTestExecutor{started: started, releaseFirst: releaseFirst}
	runtime, err := NewManagedRuntime(context.Background(), Config{
		Mode: ModeManaged, Executor: "docker", MaxActions: 3, Timeout: time.Minute,
	}, executor, &managedTestAuthorizer{decision: DecisionApprove}, &managedTestAuditor{})
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	defer func() { _ = runtime.Close() }()
	defer func() {
		select {
		case releaseFirst <- struct{}{}:
		default:
		}
	}()

	batchDone := make(chan error, 1)
	go func() {
		_, executeErr := runtime.ExecuteActions(context.Background(), "project", []Action{
			{CallID: "batch-first", Kind: ActionWait},
			{CallID: "batch-second", Kind: ActionWait},
		})
		batchDone <- executeErr
	}()
	if got := waitForManagedAction(t, started); got != "batch-first" {
		t.Fatalf("first executor action = %q, want batch-first", got)
	}

	secondLaunched := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondLaunched)
		_, executeErr := runtime.Execute(context.Background(), "project", Action{CallID: "other", Kind: ActionWait})
		secondDone <- executeErr
	}()
	select {
	case <-secondLaunched:
	case <-time.After(time.Second):
		t.Fatal("second batch did not launch")
	}
	select {
	case action := <-started:
		t.Fatalf("action %q started while the first batch held the executor", action)
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst <- struct{}{}
	if got := waitForManagedAction(t, started); got != "batch-second" {
		t.Fatalf("second batch action = %q, want batch-second", got)
	}
	if got := waitForManagedAction(t, started); got != "other" {
		t.Fatalf("third executor action = %q, want other", got)
	}
	for name, done := range map[string]chan error{"batch": batchDone, "other": secondDone} {
		select {
		case executeErr := <-done:
			if executeErr != nil {
				t.Fatalf("%s execution error = %v", name, executeErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s execution did not finish", name)
		}
	}
	if maxInFlight := executor.MaxInFlight(); maxInFlight != 1 {
		t.Fatalf("maximum concurrent executor calls = %d, want 1", maxInFlight)
	}
}

func waitForManagedAction(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case action := <-started:
		return action
	case <-time.After(time.Second):
		t.Fatal("executor did not start an action")
		return ""
	}
}

type managedTestAuthorizer struct {
	decision Decision
	err      error
}

func (a *managedTestAuthorizer) Authorize(context.Context, string, Action) (Decision, error) {
	return a.decision, a.err
}

type managedTestExecutor struct {
	name           string
	observation    Observation
	waitForContext bool
	started        chan struct{}

	mu     sync.Mutex
	calls  int
	closed bool
	start  sync.Once
}

type serialManagedTestExecutor struct {
	started      chan<- string
	releaseFirst <-chan struct{}

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
}

func (e *serialManagedTestExecutor) Name() string { return "docker" }

func (e *serialManagedTestExecutor) Check(context.Context) error { return nil }

func (e *serialManagedTestExecutor) Execute(ctx context.Context, action Action) (Observation, error) {
	e.mu.Lock()
	e.inFlight++
	if e.inFlight > e.maxInFlight {
		e.maxInFlight = e.inFlight
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inFlight--
		e.mu.Unlock()
	}()

	select {
	case e.started <- action.CallID:
	case <-ctx.Done():
		return Observation{}, ctx.Err()
	}
	if action.CallID == "batch-first" {
		select {
		case <-e.releaseFirst:
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		}
	}
	return Observation{}, nil
}

func (e *serialManagedTestExecutor) Close() error { return nil }

func (e *serialManagedTestExecutor) MaxInFlight() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxInFlight
}

func (e *managedTestExecutor) Name() string { return e.name }

func (e *managedTestExecutor) Check(context.Context) error { return nil }

func (e *managedTestExecutor) Execute(ctx context.Context, _ Action) (Observation, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.started != nil {
		e.start.Do(func() { close(e.started) })
	}
	if e.waitForContext {
		<-ctx.Done()
		return Observation{}, ctx.Err()
	}
	return e.observation, nil
}

func (e *managedTestExecutor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

type managedTestAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (a *managedTestAuditor) Record(_ context.Context, event AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := append([]AuditEvent(nil), a.events...)
	a.events = append(events, event)
	return nil
}

func (a *managedTestAuditor) Events() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}
