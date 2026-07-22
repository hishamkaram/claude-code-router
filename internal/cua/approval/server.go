// Package approval provides human approval gates for managed computer use.
package approval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	DefaultTimeout         = 2 * time.Minute
	defaultShutdownTimeout = 5 * time.Second
	requestIDBytes         = 16
	tokenBytes             = 32
	maxDecisionBodyBytes   = 4 << 10
)

var (
	ErrClosed          = errors.New("approval server closed")
	ErrInvalidDecision = errors.New("invalid approval decision")
)

type NotifyFunc func(context.Context, Prompt) error

type Config struct {
	Timeout  time.Duration
	Notify   NotifyFunc
	Fallback cua.Approver

	Now func() time.Time
}

type Prompt struct {
	URL       string
	ExpiresAt time.Time
	Request   cua.ApprovalRequest
}

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	url        string
	host       string
	origin     string

	timeout  time.Duration
	notify   NotifyFunc
	fallback cua.Approver
	now      func() time.Time

	closedCtx    context.Context
	cancelClosed context.CancelFunc
	serveDone    chan error
	closeDone    chan struct{}
	closeOnce    sync.Once
	closeErr     error

	mu      sync.Mutex
	closed  bool
	pending map[string]*pendingApproval
}

type pendingApproval struct {
	id        string
	token     string
	request   cua.ApprovalRequest
	expiresAt time.Time
	done      chan decisionResult
	once      sync.Once
}

type decisionResult struct {
	decision cua.Decision
	err      error
}

func Start(ctx context.Context, config Config) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("approval.Start: context canceled before listening: %w", err)
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("approval.Start: listening on loopback: %w", err)
	}
	closedCtx, cancelClosed := context.WithCancel(context.Background())
	server := &Server{
		listener: listener, url: "http://" + listener.Addr().String(),
		host: listener.Addr().String(), origin: "http://" + listener.Addr().String(),
		timeout: timeout, notify: config.Notify, fallback: config.Fallback,
		now: config.Now, closedCtx: closedCtx, cancelClosed: cancelClosed,
		serveDone: make(chan error, 1), closeDone: make(chan struct{}),
		pending: make(map[string]*pendingApproval),
	}
	if server.now == nil {
		server.now = time.Now
	}
	server.httpServer = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go server.serve()

	select {
	case err := <-server.serveDone:
		if err != nil {
			cancelClosed()
			return nil, fmt.Errorf("approval.Start: serving: %w", err)
		}
		cancelClosed()
		return nil, fmt.Errorf("approval.Start: server stopped during startup")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil, fmt.Errorf("approval.Start: context canceled while starting: %w", ctx.Err())
	default:
		return server, nil
	}
}

func (s *Server) URL() string {
	if s == nil {
		return ""
	}
	return s.url
}

func (s *Server) Approve(ctx context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	if err := validateApprovalRequest(request); err != nil {
		return cua.DecisionDeny, err
	}
	approvalCtx, cancel := s.contextForApproval(ctx)
	defer cancel()

	if s.notify == nil && s.fallback != nil {
		return s.approveWithFallback(approvalCtx, request)
	}

	pending, prompt, err := s.newPending(request)
	if err != nil {
		return cua.DecisionDeny, err
	}
	defer s.removePending(pending.id)

	if s.notify != nil {
		if err := s.notify(approvalCtx, prompt); err != nil {
			s.removePending(pending.id)
			if s.fallback != nil {
				return s.approveWithFallback(approvalCtx, request)
			}
			return cua.DecisionDeny, fmt.Errorf("approval.Server.Approve: notify approval prompt: %w", err)
		}
	}

	select {
	case result := <-pending.done:
		if result.err != nil {
			return cua.DecisionDeny, result.err
		}
		return normalizeDecision(result.decision)
	case <-approvalCtx.Done():
		if errors.Is(s.closedCtx.Err(), context.Canceled) {
			return cua.DecisionDeny, fmt.Errorf("approval.Server.Approve: approval denied during shutdown: %w", ErrClosed)
		}
		return cua.DecisionDeny, fmt.Errorf("approval.Server.Approve: approval denied: %w", approvalCtx.Err())
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.shutdown(ctx)
		close(s.closeDone)
	})
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		return fmt.Errorf("approval.Server.Shutdown: waiting for shutdown: %w", ctx.Err())
	}
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	return s.Shutdown(ctx)
}

func (s *Server) serve() {
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		s.serveDone <- nil
		return
	}
	s.serveDone <- err
}

func (s *Server) shutdown(ctx context.Context) error {
	s.cancelClosed()
	s.denyPending()
	var shutdownErr error
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("approval.Server.Shutdown: shutting down http server: %w", err))
		}
	}
	select {
	case err := <-s.serveDone:
		if err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("approval.Server.Shutdown: serving: %w", err))
		}
	case <-ctx.Done():
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("approval.Server.Shutdown: waiting for server: %w", ctx.Err()))
	}
	return shutdownErr
}

func (s *Server) contextForApproval(ctx context.Context) (context.Context, context.CancelFunc) {
	baseCtx, cancelBase := context.WithCancel(ctx)
	stopClosed := context.AfterFunc(s.closedCtx, cancelBase)
	timeoutCtx, cancelTimeout := context.WithTimeout(baseCtx, s.timeout)
	return timeoutCtx, func() {
		cancelTimeout()
		if stopClosed() {
			cancelBase()
		}
	}
}

func (s *Server) approveWithFallback(ctx context.Context, request cua.ApprovalRequest) (cua.Decision, error) {
	if s.fallback == nil {
		return cua.DecisionDeny, fmt.Errorf("approval.Server.Approve: fallback approver is not configured")
	}
	decision, err := s.fallback.Approve(ctx, request)
	if err != nil {
		return cua.DecisionDeny, fmt.Errorf("approval.Server.Approve: fallback approval: %w", err)
	}
	return normalizeDecision(decision)
}

func (s *Server) newPending(request cua.ApprovalRequest) (*pendingApproval, Prompt, error) {
	id, err := newRandomHex(requestIDBytes)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("approval.Server.Approve: generating request id: %w", err)
	}
	token, err := newRandomHex(tokenBytes)
	if err != nil {
		return nil, Prompt{}, fmt.Errorf("approval.Server.Approve: generating approval token: %w", err)
	}
	expiresAt := s.now().Add(s.timeout)
	pending := &pendingApproval{
		id: id, token: token, request: request, expiresAt: expiresAt,
		done: make(chan decisionResult, 1),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, Prompt{}, fmt.Errorf("approval.Server.Approve: %w", ErrClosed)
	}
	s.pending[id] = pending
	return pending, Prompt{URL: s.approvalURL(id, token), ExpiresAt: expiresAt, Request: request}, nil
}

func (s *Server) approvalURL(id, token string) string {
	query := url.Values{}
	query.Set("request_id", id)
	query.Set("token", token)
	return s.url + "/approve?" + query.Encode()
}

func (s *Server) removePending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *Server) denyPending() {
	s.mu.Lock()
	s.closed = true
	pending := make([]*pendingApproval, 0, len(s.pending))
	for id, request := range s.pending {
		delete(s.pending, id)
		pending = append(pending, request)
	}
	s.mu.Unlock()
	for _, request := range pending {
		request.complete(decisionResult{
			decision: cua.DecisionDeny,
			err:      fmt.Errorf("approval.Server.Approve: approval denied during shutdown: %w", ErrClosed),
		})
	}
}

func (p *pendingApproval) complete(result decisionResult) {
	p.once.Do(func() {
		p.done <- result
		close(p.done)
	})
}

func validateApprovalRequest(request cua.ApprovalRequest) error {
	if strings.TrimSpace(request.ActionID) == "" {
		return fmt.Errorf("approval.Server.Approve: action id is required")
	}
	switch request.Kind {
	case cua.ActionScreenshot, cua.ActionClick, cua.ActionDoubleClick, cua.ActionDrag,
		cua.ActionMove, cua.ActionType, cua.ActionKeypress, cua.ActionScroll, cua.ActionWait:
	default:
		return fmt.Errorf("approval.Server.Approve: unsupported action kind %q", request.Kind)
	}
	if err := validateRisk(request.Risk); err != nil {
		return err
	}
	return nil
}

func validateRisk(risk cua.Risk) error {
	switch risk {
	case cua.RiskLow, cua.RiskHigh:
		return nil
	default:
		return fmt.Errorf("approval.Server.Approve: unsupported action risk %q", risk)
	}
}

func normalizeDecision(decision cua.Decision) (cua.Decision, error) {
	switch decision {
	case cua.DecisionApprove, cua.DecisionDeny:
		return decision, nil
	default:
		return cua.DecisionDeny, fmt.Errorf("%w %q", ErrInvalidDecision, decision)
	}
}

func sameSecret(expected, provided string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func newRandomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
