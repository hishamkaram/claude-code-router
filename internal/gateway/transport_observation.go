package gateway

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
)

type transportObservationKey struct{}

type transportObservation struct {
	mu        sync.Mutex
	protocol  string
	receipt   providerReceipt
	sessionID int64
}

func (o *transportObservation) observeReceipt(receipt providerReceipt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.receipt.merge(receipt)
}

func (o *transportObservation) receiptReason() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.receipt.reason()
}

func (o *transportObservation) gotConn(info httptrace.GotConnInfo) {
	if conn, ok := info.Conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
		protocol := conn.ConnectionState().NegotiatedProtocol
		if protocol == "" {
			protocol = "http/1.1"
		}
		o.observe(protocol)
	}
}

func (o *transportObservation) observe(protocol string) {
	switch protocol {
	case "HTTP/2.0", "h2":
		protocol = "h2"
	case "HTTP/1.1", "http/1.1":
		protocol = "http/1.1"
	default:
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.protocol == "" {
		o.protocol = protocol
	} else if o.protocol != protocol {
		o.protocol = "mixed"
	}
}

func (o *transportObservation) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.protocol == "" {
		return "unknown"
	}
	return o.protocol
}

func recordTransportPolicy(ctx context.Context, recorder *observability.Recorder, owned bool) {
	if recorder == nil {
		return
	}
	reason := "owner=caller; idle_ping=unverified"
	if owned {
		reason = "owner=ccr; http2_ping_after_ms=20000; http2_ping_timeout_ms=15000; max_idle_pool_ms=15000"
	}
	recorder.RecordLifecycle(ctx, observability.LifecycleEvent{
		Name: "upstream_transport_policy", Status: "observed", Reason: reason,
	})
}

func (h *handler) completeTransportObservation(ctx context.Context, span *observability.RouteSpan) {
	if h.cfg.Recorder == nil || span == nil || span.RequestID() == "" {
		return
	}
	observation, _ := ctx.Value(transportObservationKey{}).(*transportObservation)
	if observation == nil {
		return
	}
	protocol := observation.snapshot()
	status, reason := "unverified", "owner=caller; protocol=unknown; idle_ping=unverified"
	if h.upstreamOwned {
		status, reason = "degraded", "owner=ccr; protocol="+protocol+"; idle_ping=unavailable"
		switch protocol {
		case "h2":
			status, reason = "observed", "owner=ccr; protocol=h2; idle_ping=enabled"
		case "unknown":
			status, reason = "unverified", "owner=ccr; protocol=unknown; idle_ping=unverified"
		case "mixed":
			reason = "owner=ccr; protocol=mixed; idle_ping=partial"
		}
	}
	reason += observation.receiptReason()
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	h.cfg.Recorder.RecordLifecycle(recordCtx, observability.LifecycleEvent{
		Name: "upstream_transport", Status: status, ExternalID: span.RequestID(), Reason: reason, SessionID: observation.sessionID,
	})
}
