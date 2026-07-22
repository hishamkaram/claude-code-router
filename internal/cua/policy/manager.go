// Package policy enforces managed computer-use approval policy.
package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const DefaultApprovalTimeout = 2 * time.Minute

var ErrClosed = errors.New("approval policy closed")

type Config struct {
	Approver        cua.Approver
	Executor        string
	ApprovalTimeout time.Duration
}

type Manager struct {
	approver cua.Approver
	executor string
	timeout  time.Duration

	closedCtx    context.Context
	cancelClosed context.CancelFunc

	mu         sync.Mutex
	closed     bool
	remembered map[approvalKey]struct{}
	inflight   map[approvalKey]*approvalCall
}

type approvalKey struct {
	project string
	kind    cua.ActionKind
}

type approvalCall struct {
	done     chan struct{}
	decision cua.Decision
	err      error
}

func NewManager(config Config) (*Manager, error) {
	if config.Approver == nil {
		return nil, fmt.Errorf("policy.NewManager: approver is required")
	}
	timeout := config.ApprovalTimeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	closedCtx, cancelClosed := context.WithCancel(context.Background())
	return &Manager{
		approver: config.Approver, executor: strings.TrimSpace(config.Executor),
		timeout: timeout, closedCtx: closedCtx, cancelClosed: cancelClosed,
		remembered: make(map[approvalKey]struct{}),
		inflight:   make(map[approvalKey]*approvalCall),
	}, nil
}

func (m *Manager) Authorize(ctx context.Context, project string, action cua.Action) (cua.Decision, error) {
	if err := action.Validate(); err != nil {
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: validate action: %w", err)
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: project is required")
	}
	request := cua.ApprovalRequest{
		ActionID: action.CallID,
		Kind:     action.Kind,
		Risk:     action.Risk(),
		Executor: m.executor,
	}
	if request.Risk == cua.RiskHigh {
		return m.requestFreshApproval(ctx, request)
	}
	return m.requestLowRiskApproval(ctx, project, request)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.cancelClosed()
	return nil
}

func (m *Manager) requestFreshApproval(ctx context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	decision, err := m.callApprover(ctx, request)
	if err != nil {
		return cua.DecisionDeny, err
	}
	return normalizeDecision(decision)
}

func (m *Manager) requestLowRiskApproval(ctx context.Context, project string, request cua.ApprovalRequest) (cua.Decision, error) {
	key := approvalKey{project: project, kind: request.Kind}
	call, owner, err := m.lowRiskCall(key)
	if err != nil {
		return cua.DecisionDeny, err
	}
	if !owner {
		return m.waitForLowRiskCall(ctx, call)
	}

	decision, approveErr := m.callApprover(ctx, request)
	decision, approveErr = normalizeDecisionWithError(decision, approveErr)
	m.completeLowRiskCall(key, call, decision, approveErr)
	if approveErr != nil {
		return cua.DecisionDeny, approveErr
	}
	return decision, nil
}

func (m *Manager) lowRiskCall(key approvalKey) (*approvalCall, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, fmt.Errorf("policy.Manager.Authorize: %w", ErrClosed)
	}
	if _, ok := m.remembered[key]; ok {
		return &approvalCall{done: closedDone(), decision: cua.DecisionApprove}, false, nil
	}
	if call, ok := m.inflight[key]; ok {
		return call, false, nil
	}
	call := &approvalCall{done: make(chan struct{})}
	m.inflight[key] = call
	return call, true, nil
}

func (m *Manager) waitForLowRiskCall(ctx context.Context, call *approvalCall) (cua.Decision, error) {
	select {
	case <-call.done:
		if call.err != nil {
			return cua.DecisionDeny, call.err
		}
		return call.decision, nil
	case <-ctx.Done():
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: approval denied: %w", ctx.Err())
	case <-m.closedCtx.Done():
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: approval denied during shutdown: %w", ErrClosed)
	}
}

func (m *Manager) completeLowRiskCall(key approvalKey, call *approvalCall, decision cua.Decision, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if decision == cua.DecisionApprove && err == nil {
		m.remembered[key] = struct{}{}
	}
	delete(m.inflight, key)
	call.decision = decision
	call.err = err
	close(call.done)
}

func (m *Manager) callApprover(ctx context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	approvalCtx, cancel := m.contextForApproval(ctx)
	defer cancel()
	decision, err := m.approver.Approve(approvalCtx, request)
	if err != nil {
		if errors.Is(m.closedCtx.Err(), context.Canceled) {
			return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: approval denied during shutdown: %w", ErrClosed)
		}
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: request approval: %w", err)
	}
	if err := approvalCtx.Err(); err != nil {
		if errors.Is(m.closedCtx.Err(), context.Canceled) {
			return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: approval denied during shutdown: %w", ErrClosed)
		}
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: approval denied: %w", err)
	}
	return decision, nil
}

func (m *Manager) contextForApproval(ctx context.Context) (context.Context, context.CancelFunc) {
	baseCtx, cancelBase := context.WithCancel(ctx)
	stopClosed := context.AfterFunc(m.closedCtx, cancelBase)
	timeoutCtx, cancelTimeout := context.WithTimeout(baseCtx, m.timeout)
	return timeoutCtx, func() {
		cancelTimeout()
		if stopClosed() {
			cancelBase()
		}
	}
}

func normalizeDecisionWithError(decision cua.Decision, err error) (cua.Decision, error) {
	if err != nil {
		return cua.DecisionDeny, err
	}
	return normalizeDecision(decision)
}

func normalizeDecision(decision cua.Decision) (cua.Decision, error) {
	switch decision {
	case cua.DecisionApprove, cua.DecisionDeny:
		return decision, nil
	default:
		return cua.DecisionDeny, fmt.Errorf("policy.Manager.Authorize: invalid approval decision %q", decision)
	}
}

func closedDone() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
