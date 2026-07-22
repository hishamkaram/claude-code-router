package cua

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrManagedClosed = errors.New("managed computer-use runtime is closed")
	ErrTurnLimit     = errors.New("managed computer-use turn limit reached")
	ErrActionLimit   = errors.New("managed computer-use action limit reached")
	ErrApprovalDeny  = errors.New("managed computer-use action was denied")
)

// Authorizer is the narrow policy boundary used by a managed runtime.
type Authorizer interface {
	Authorize(context.Context, string, Action) (Decision, error)
}

// AuditRecorder stores only redacted computer-use metadata.
type AuditRecorder interface {
	Record(context.Context, AuditEvent) error
}

// ManagedRuntime owns one managed computer-use executor for one CCR launch.
// It is safe for concurrent gateway requests. The launch owns Close, which
// cancels in-flight approval/execution and waits for each request to return.
type ManagedRuntime struct {
	config     Config
	executor   Executor
	authorizer Authorizer
	auditor    AuditRecorder

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	turns   int
	actions int
	active  sync.WaitGroup

	// executorGate serializes complete provider action batches. A shared local
	// executor must never receive interleaved actions from concurrent requests.
	executorGate chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// NewManagedRuntime creates a launch-scoped managed execution runtime. The
// caller must Close it exactly once during launch cleanup.
func NewManagedRuntime(ctx context.Context, config Config, executor Executor, authorizer Authorizer, auditor AuditRecorder) (*ManagedRuntime, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, fmt.Errorf("normalizing managed computer-use configuration: %w", err)
	}
	if normalized.Mode != ModeManaged {
		return nil, fmt.Errorf("managed computer-use runtime requires mode managed")
	}
	if strings.TrimSpace(normalized.Executor) == "" {
		return nil, fmt.Errorf("managed computer-use runtime requires an executor")
	}
	if executor == nil {
		return nil, fmt.Errorf("managed computer-use runtime requires an executor implementation")
	}
	if authorizer == nil {
		return nil, fmt.Errorf("managed computer-use runtime requires an approval policy")
	}
	if ctx == nil {
		return nil, fmt.Errorf("managed computer-use runtime context is required")
	}
	runtimeCtx, cancel := context.WithTimeout(ctx, normalized.Timeout)
	return &ManagedRuntime{
		config: normalized, executor: executor, authorizer: authorizer, auditor: auditor,
		ctx: runtimeCtx, cancel: cancel, executorGate: make(chan struct{}, 1),
	}, nil
}

// BeginTurn reserves one model-directed computer-use turn. Call it only after
// a provider has returned a native computer call.
func (r *ManagedRuntime) BeginTurn(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("beginning managed computer-use turn: %w", ErrManagedClosed)
	}
	if err := r.executionContextError(ctx); err != nil {
		return fmt.Errorf("beginning managed computer-use turn: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("beginning managed computer-use turn: %w", ErrManagedClosed)
	}
	if r.turns >= r.config.MaxTurns {
		return fmt.Errorf("beginning managed computer-use turn: %w", ErrTurnLimit)
	}
	r.turns++
	return nil
}

// Execute authorizes and runs one action. It never persists raw action data or
// observations. The caller owns any in-memory observation required by a model.
func (r *ManagedRuntime) Execute(ctx context.Context, project string, action Action) (Observation, error) {
	observations, err := r.ExecuteActions(ctx, project, []Action{action})
	if err != nil {
		return Observation{}, err
	}
	return observations[0], nil
}

// ExecuteActions authorizes and executes one provider computer-call batch as
// an atomic sequence. It validates the complete batch before reserving an
// action budget or allowing the executor to run.
func (r *ManagedRuntime) ExecuteActions(ctx context.Context, project string, actions []Action) ([]Observation, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("executing managed computer-use actions: at least one action is required")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("executing managed computer-use actions: project is required")
	}
	for _, action := range actions {
		if err := action.Validate(); err != nil {
			return nil, fmt.Errorf("executing managed computer-use action: %w", err)
		}
	}
	if err := r.acquireExecution(ctx, len(actions)); err != nil {
		return nil, fmt.Errorf("executing managed computer-use actions: %w", err)
	}
	defer r.active.Done()

	executionCtx, cancel := r.requestContext(ctx)
	defer cancel()
	if err := r.acquireExecutor(executionCtx); err != nil {
		return nil, fmt.Errorf("executing managed computer-use actions: %w", err)
	}
	defer r.releaseExecutor()

	observations := make([]Observation, 0, len(actions))
	for _, action := range actions {
		observation, err := r.executeAction(executionCtx, project, action)
		if err != nil {
			return observations, err
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func (r *ManagedRuntime) executeAction(ctx context.Context, project string, action Action) (Observation, error) {
	decision, err := r.authorizer.Authorize(ctx, project, action)
	if err != nil {
		r.record(ctx, action, DecisionDeny, statusForExecutionError(err))
		return Observation{}, fmt.Errorf("authorizing managed computer-use action: %w", err)
	}
	if decision != DecisionApprove {
		r.record(ctx, action, DecisionDeny, "denied")
		return Observation{}, ErrApprovalDeny
	}
	r.record(ctx, action, decision, "approved")
	observation, err := r.executor.Execute(ctx, action)
	if err != nil {
		r.record(ctx, action, decision, statusForExecutionError(err))
		return Observation{}, fmt.Errorf("executing managed computer-use action with %s: %w", r.executor.Name(), err)
	}
	return observation, nil
}

// Close cancels new and in-flight work, then waits for every gateway request to
// return before closing the policy and executor owned by this launch.
func (r *ManagedRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancel()
		r.active.Wait()
		var closeErr error
		if closer, ok := r.authorizer.(interface{ Close() error }); ok {
			closeErr = errors.Join(closeErr, closer.Close())
		}
		closeErr = errors.Join(closeErr, r.executor.Close())
		r.closeErr = closeErr
	})
	return r.closeErr
}

func (r *ManagedRuntime) ExecutorName() string {
	if r == nil || r.executor == nil {
		return ""
	}
	return r.executor.Name()
}

func (r *ManagedRuntime) acquireExecution(ctx context.Context, actionCount int) error {
	if r == nil {
		return ErrManagedClosed
	}
	if actionCount < 1 {
		return fmt.Errorf("action count must be positive")
	}
	if err := r.executionContextError(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrManagedClosed
	}
	if actionCount > r.config.MaxActions-r.actions {
		return ErrActionLimit
	}
	r.actions += actionCount
	r.active.Add(1)
	return nil
}

func (r *ManagedRuntime) acquireExecutor(ctx context.Context) error {
	select {
	case r.executorGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *ManagedRuntime) releaseExecutor() {
	<-r.executorGate
}

func (r *ManagedRuntime) executionContextError(ctx context.Context) error {
	if r == nil {
		return ErrManagedClosed
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (r *ManagedRuntime) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithCancel(r.ctx)
	}
	requestCtx, cancel := context.WithCancel(r.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return requestCtx, func() {
		if stop() {
			cancel()
		}
	}
}

func (r *ManagedRuntime) record(ctx context.Context, action Action, decision Decision, status string) {
	if r == nil || r.auditor == nil {
		return
	}
	_ = r.auditor.Record(ctx, AuditEvent{
		Executor: r.ExecutorName(), Action: action.Kind, Risk: action.Risk(),
		Decision: decision, Status: status,
	})
}

func statusForExecutionError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, ErrManagedClosed):
		return "shutdown"
	default:
		return "error"
	}
}
