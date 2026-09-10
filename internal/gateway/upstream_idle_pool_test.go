package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamTransportBoundsIdlePoolWithoutChangingCaller(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		configured time.Duration
		want       time.Duration
	}{
		{0, 15 * time.Second},
		{-time.Second, 15 * time.Second},
		{5 * time.Second, 5 * time.Second},
		{15 * time.Second, 15 * time.Second},
		{90 * time.Second, 15 * time.Second},
	} {
		t.Run(test.configured.String(), func(t *testing.T) {
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.IdleConnTimeout = test.configured
			transport, err := newUpstreamTransport(base)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.close()
			if base.IdleConnTimeout != test.configured || transport.base.IdleConnTimeout != test.want {
				t.Fatalf("idle policy: caller=%v owned=%v want=%v", base.IdleConnTimeout, transport.base.IdleConnTimeout, test.want)
			}
		})
	}
}

func TestUpstreamTransportIdlePoolExpiry(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("eviction_disabled=%t", disabled), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprint(w, "ok")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			transport, firstConnection := idlePoolTransport(t, server, disabled)
			client := &http.Client{Transport: transport}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := idlePoolRequest(ctx, client, server.URL); err != nil {
				t.Fatal(err)
			}
			first := <-firstConnection
			first.blocked.Store(true)
			// This window ends after idle eviction but before a failed health
			// probe can close the connection. Arbitrary reads are not a barrier.
			window := time.NewTimer(transport.base.HTTP2.SendPingTimeout + transport.base.HTTP2.PingTimeout/2)
			defer window.Stop()
			select {
			case <-first.closed:
				if disabled {
					t.Fatal("negative control connection retired with idle eviction disabled")
				}
			case <-window.C:
				if !disabled {
					t.Fatal("idle connection was not retired before the health-probe failure window")
				}
			case <-ctx.Done():
				t.Fatal("idle connection neither retired nor probed")
			}
			var reused atomic.Bool
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }}
			err := idlePoolRequest(httptrace.WithClientTrace(ctx, trace), client, server.URL)
			if disabled {
				if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connection lost") || !reused.Load() {
					t.Fatalf("negative control did not reuse and fail the unhealthy connection: reused=%t error=%v", reused.Load(), err)
				}
			} else if err != nil || reused.Load() {
				t.Fatalf("request reused unhealthy idle connection: reused=%t error=%v", reused.Load(), err)
			}
			if calls.Load() != 2 {
				t.Fatalf("accepted requests were lost or replayed: calls=%d, want 2", calls.Load())
			}
		})
	}
}

func idlePoolTransport(t *testing.T, server *httptest.Server, disabled bool) (*upstreamTransport, <-chan *idlePoolConn) {
	t.Helper()
	base := server.Client().Transport.(*http.Transport).Clone()
	base.IdleConnTimeout = 90 * time.Second
	transport, err := newUpstreamTransport(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.close)
	transport.base.ForceAttemptHTTP2 = true
	// Scale the constructor's actual idle policy, not a replacement test policy.
	transport.base.IdleConnTimeout /= 100
	transport.base.HTTP2.SendPingTimeout = 200 * time.Millisecond
	transport.base.HTTP2.PingTimeout = time.Second
	if disabled {
		transport.base.IdleConnTimeout = 0
	}
	first := make(chan *idlePoolConn, 1)
	var dials atomic.Int32
	dial := transport.base.DialContext
	transport.base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil || dials.Add(1) != 1 {
			return conn, err
		}
		observed := &idlePoolConn{Conn: conn, closed: make(chan struct{})}
		first <- observed
		return observed, nil
	}
	return transport, first
}

func idlePoolRequest(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("do not replay"))
	if err != nil {
		return fmt.Errorf("creating idle-pool request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending idle-pool request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading idle-pool response: %w", err)
	}
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK || string(body) != "ok" {
		return fmt.Errorf("unexpected idle-pool response: protocol=%s status=%d", resp.Proto, resp.StatusCode)
	}
	return nil
}

type idlePoolConn struct {
	net.Conn
	blocked   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *idlePoolConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if err != nil || !c.blocked.Load() {
			return n, err
		}
	}
}

func (c *idlePoolConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}
