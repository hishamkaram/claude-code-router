package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type idleReadConn struct {
	net.Conn
	idle time.Duration
}

func (c idleReadConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func fixtureUpstreamTransport(t *testing.T, server *httptest.Server, idle time.Duration) *upstreamTransport {
	t.Helper()
	transport, err := newUpstreamTransport(server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	transport.base.ForceAttemptHTTP2 = true
	transport.base.HTTP2.SendPingTimeout = 50 * time.Millisecond
	transport.base.HTTP2.PingTimeout = 200 * time.Millisecond
	if idle > 0 {
		transport.base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return idleReadConn{Conn: conn, idle: idle}, nil
		}
	}
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func TestUpstreamTransportIdleRegression(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: first\n\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-time.After(900 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				_, _ = fmt.Fprint(w, "complete")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			for _, enabled := range []bool{false, true} {
				transport := fixtureUpstreamTransport(t, server, 350*time.Millisecond)
				if !enabled {
					transport.base.HTTP2.SendPingTimeout = 0
				}
				observation := &transportObservation{}
				ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), transportObservationKey{}, observation), 4*time.Second)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("same request"))
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				response, callErr := (&http.Client{Transport: transport}).Do(request)
				var body []byte
				if response != nil {
					body, err = io.ReadAll(response.Body)
					_ = response.Body.Close()
					callErr = errors.Join(callErr, err)
				}
				cancel()
				transport.CloseIdleConnections()
				if enabled && (callErr != nil || !strings.HasSuffix(string(body), "complete")) {
					t.Fatalf("ping enabled: body=%q error=%v", body, callErr)
				}
				if !enabled && callErr == nil {
					t.Fatal("control did not reproduce idle failure")
				}
				if got := observation.snapshot(); got != "h2" {
					t.Fatalf("protocol=%q", got)
				}
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("ambiguous requests replayed: calls=%d", got)
			}
		})
	}
}

func TestUpstreamTransportDeadlineDespiteHealthyPings(t *testing.T) {
	stopped := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(stopped) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := fixtureUpstreamTransport(t, server, 350*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("wait"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("provider was not canceled")
	}
}

func TestUpstreamTransportClonePreservesConfiguration(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.HTTP2 = &http.HTTP2Config{SendPingTimeout: time.Hour, PingTimeout: time.Minute}
	base.MaxIdleConns = 37
	transport, err := newUpstreamTransport(base)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if base.HTTP2.SendPingTimeout != time.Hour || base.HTTP2.PingTimeout != time.Minute {
		t.Fatal("mutated caller HTTP2 config")
	}
	if transport.base.HTTP2 == base.HTTP2 || transport.base == base {
		t.Fatal("shared mutable configuration")
	}
	if transport.base.MaxIdleConns != 37 || transport.base.Proxy == nil || transport.base.DialContext == nil {
		t.Fatal("lost standard transport configuration")
	}
	if transport.base.HTTP2.SendPingTimeout != 20*time.Second || transport.base.HTTP2.PingTimeout != 15*time.Second {
		t.Fatal("incorrect production liveness policy")
	}
	if _, err := newUpstreamTransport(nil); err == nil {
		t.Fatal("accepted incompatible default transport")
	}
}

func TestUpstreamTransportHTTP1Visibility(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") }))
	defer server.Close()
	transport := fixtureUpstreamTransport(t, server, 0)
	observation := &transportObservation{}
	ctx := context.WithValue(context.Background(), transportObservationKey{}, observation)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if got := observation.snapshot(); got != "http/1.1" {
		t.Fatalf("protocol=%q", got)
	}
	observation.observe("h2")
	observation.observe("HTTP/1.1")
	if observation.snapshot() != "mixed" {
		t.Fatal("lost mixed protocol observation")
	}
}

type blackholeReadConn struct {
	net.Conn
	blocked *atomic.Bool
}

func (c blackholeReadConn) Read(p []byte) (int, error) {
	for {
		n, err := c.Conn.Read(p)
		if err != nil || !c.blocked.Load() {
			return n, err
		}
	}
}

func TestUpstreamTransportMissingPingACKFailsWithoutReplay(t *testing.T) {
	var blocked atomic.Bool
	var calls atomic.Int32
	stopped := make(chan struct{})
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		blocked.Store(true)
		<-r.Context().Done()
		close(stopped)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	transport := fixtureUpstreamTransport(t, upstream, 0)
	transport.base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return blackholeReadConn{Conn: conn, blocked: &blocked}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader("do not repeat"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ping failure before total deadline: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("peer not disconnected")
	}
	if calls.Load() != 1 {
		t.Fatalf("replayed accepted request: %d", calls.Load())
	}
}

func TestUpstreamTransportMultiplexedCancellation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var connections atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		_, _ = fmt.Fprint(w, "fast")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	transport := fixtureUpstreamTransport(t, upstream, 0)
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/slow", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, requestErr := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow stream not started")
	}
	response, err := client.Get(upstream.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "fast" {
		t.Fatalf("fast stream: %q %v", body, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("slow stream error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow stream leaked")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream cancellation not propagated")
	}
	if connections.Load() != 1 {
		t.Fatalf("not multiplexed: connections=%d", connections.Load())
	}
}

func TestUpstreamTransportPreservesTLSVerificationAndProxy(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	transport, err := newUpstreamTransport(http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "fixture.invalid" {
			t.Error("proxy request changed")
		}
		_, _ = fmt.Fprint(w, "proxied")
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(proxyURL)
	proxied, err := newUpstreamTransport(base)
	if err != nil {
		t.Fatal(err)
	}
	defer proxied.CloseIdleConnections()
	response, err = (&http.Client{Transport: proxied}).Get("http://fixture.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if calls.Load() != 1 {
		t.Fatal("explicit proxy not used")
	}
}

func TestUpstreamTransportClosesLateDial(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	connection, peer := net.Pipe()
	defer peer.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release
		return connection, nil
	}
	transport, err := newUpstreamTransport(base)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, dialErr := transport.base.DialContext(context.Background(), "tcp", "fixture")
		done <- dialErr
	}()
	<-entered
	transport.close()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("late dial error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late dial not released")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	var buffer [1]byte
	if _, err := peer.Read(buffer[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("late connection not closed: %v", err)
	}
}
