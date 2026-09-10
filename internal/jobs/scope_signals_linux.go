package jobs

import (
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
)

// Filter before enqueueing: godbus's default handler can spawn a blocked
// delivery goroutine for every signal received while a job is running.
type scopeSignals struct {
	unit     string
	owner    atomic.Pointer[string]
	events   chan *dbus.Signal
	overflow chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func newScopeSignals(unit string) *scopeSignals {
	return &scopeSignals{unit: unit, events: make(chan *dbus.Signal, 16), overflow: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (s *scopeSignals) DeliverSignal(iface, member string, signal *dbus.Signal) {
	owner := s.owner.Load()
	if owner == nil || signal == nil || signal.Sender != *owner || iface != "org.freedesktop.systemd1.Manager" || member != "JobRemoved" || len(signal.Body) != 4 || signal.Body[2] != s.unit {
		return
	}
	select {
	case <-s.closed:
	case s.events <- signal:
	default:
		select {
		case s.overflow <- struct{}{}:
		default:
		}
	}
}

func (s *scopeSignals) Terminate() { s.once.Do(func() { close(s.closed) }) }
