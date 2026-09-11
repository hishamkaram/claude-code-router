package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// RequestAccounting observes message-request entry independently of optional
// history and lifecycle hooks. Sealing rejects new entries and joins all prior
// requests before a detached owner publishes its final classification.
type RequestAccounting struct {
	mu      sync.Mutex
	entered uint64
	active  int
	sealed  bool
	idle    chan struct{}
}

func NewRequestAccounting() *RequestAccounting {
	idle := make(chan struct{})
	close(idle)
	return &RequestAccounting{idle: idle}
}

func (a *RequestAccounting) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealed {
		return false
	}
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++
	a.entered++
	return true
}

func (a *RequestAccounting) end() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if a.active == 0 {
		close(a.idle)
	}
}

func (a *RequestAccounting) Entered() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entered
}

func (a *RequestAccounting) SealAndWait(ctx context.Context) error {
	a.mu.Lock()
	a.sealed = true
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for gateway request accounting: %w", ctx.Err())
	}
}

func (h *handler) handleAccountedMessages(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestAccounting != nil {
		if !h.cfg.RequestAccounting.begin() {
			writeAnthropicError(w, http.StatusServiceUnavailable, "job is finalizing")
			return
		}
		defer h.cfg.RequestAccounting.end()
	}
	h.handleMessages(w, r)
}
