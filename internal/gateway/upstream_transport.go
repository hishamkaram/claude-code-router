package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

const (
	upstreamMaxIdleTime = 15 * time.Second
	upstreamPingAfter   = 20 * time.Second
	upstreamPingTimeout = 15 * time.Second
)

// upstreamTransport owns the connection pool. The standard transport owns the
// HTTP/2 ping machinery; it does not retry ambiguous application failures here.
type upstreamTransport struct {
	base        *http.Transport
	mu          sync.Mutex
	closed      bool
	connections map[*upstreamConn]struct{}
}

func newUpstreamTransport(base http.RoundTripper) (*upstreamTransport, error) {
	standard, ok := base.(*http.Transport)
	if !ok || standard == nil {
		return nil, fmt.Errorf("gateway: default transport must be *http.Transport; supply Config.HTTPClient for a custom transport")
	}
	//nolint:staticcheck // Detect legacy TLS hooks too: they bypass owned TCP connection tracking.
	if standard.DialTLS != nil || standard.DialTLSContext != nil {
		return nil, fmt.Errorf("gateway: custom default TLS dialers require caller-owned Config.HTTPClient for transport cleanup")
	}
	cloned := standard.Clone()
	// Retire unused connections before their read-idle health probe starts.
	// Active streams retain the independent ping policy below.
	if cloned.IdleConnTimeout <= 0 || cloned.IdleConnTimeout > upstreamMaxIdleTime {
		cloned.IdleConnTimeout = upstreamMaxIdleTime
	}
	config := http.HTTP2Config{}
	if cloned.HTTP2 != nil {
		config = *cloned.HTTP2
	}
	config.SendPingTimeout = upstreamPingAfter
	config.PingTimeout = upstreamPingTimeout
	cloned.HTTP2 = &config
	owned := &upstreamTransport{base: cloned, connections: make(map[*upstreamConn]struct{})}
	dial := cloned.DialContext
	if dial == nil {
		//nolint:staticcheck // Preserve an explicitly configured legacy dialer.
		if cloned.Dial != nil {
			dial = func(_ context.Context, network, address string) (net.Conn, error) {
				//nolint:staticcheck // Preserve the caller's legacy dial semantics.
				return cloned.Dial(network, address)
			}
		} else {
			dial = (&net.Dialer{}).DialContext
		}
	}
	cloned.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		owned.mu.Lock()
		closed := owned.closed
		owned.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		owned.mu.Lock()
		if owned.closed {
			owned.mu.Unlock()
			_ = connection.Close()
			return nil, net.ErrClosed
		}
		tracked := &upstreamConn{Conn: connection, owner: owned}
		owned.connections[tracked] = struct{}{}
		owned.mu.Unlock()
		return tracked, nil
	}
	return owned, nil
}

func (t *upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	observation, _ := req.Context().Value(transportObservationKey{}).(*transportObservation)
	if observation == nil {
		return t.base.RoundTrip(req)
	}
	trace := &httptrace.ClientTrace{GotConn: observation.gotConn}
	request := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	response, err := t.base.RoundTrip(request)
	if response != nil {
		observation.observe(response.Proto)
		observation.observeReceipt(receiptFromHeaders(response.Header))
	}
	return response, err
}

func (t *upstreamTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

// close prevents late dials from escaping cleanup and closes active connections
// after request cancellation. CloseIdleConnections alone can miss HTTP/2 streams
// whose cancellation is still being processed by the standard transport.
func (t *upstreamTransport) close() {
	t.mu.Lock()
	t.closed = true
	connections := make([]*upstreamConn, 0, len(t.connections))
	for connection := range t.connections {
		connections = append(connections, connection)
	}
	t.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
	t.base.CloseIdleConnections()
}

type upstreamConn struct {
	net.Conn
	owner    *upstreamTransport
	once     sync.Once
	closeErr error
}

func (c *upstreamConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
	})
	return c.closeErr
}
