package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func lifecycleGateway(t *testing.T, client *http.Client) *Server {
	t.Helper()
	return lifecycleGatewayTo(t, client, "http://127.0.0.1:1")
}

func lifecycleGatewayTo(t *testing.T, client *http.Client, url string) *Server {
	t.Helper()
	db := newGatewayStore(t, store.Provider{Name: "fixture", Type: "litellm", BaseURL: url}, store.Model{Alias: "model", ProviderName: "fixture", ProviderModel: "model", Status: "degraded"})
	server := startGatewayWithConfig(t, context.Background(), Config{Store: db, Token: "token", HTTPClient: client})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return server
}

func TestUpstreamGatewayOwnsSeparatePools(t *testing.T) {
	first := lifecycleGateway(t, nil)
	second := lifecycleGateway(t, nil)
	if first.upstream == nil || second.upstream == nil || first.upstream == second.upstream || first.upstream.base == second.upstream.base {
		t.Fatal("gateway pools not independently owned")
	}
	handler := first.httpServer.Handler.(*handler)
	if handler.httpClient() != handler.cfg.HTTPClient || handler.httpClient().Transport != first.upstream {
		t.Fatal("upstream client not initialized")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := first.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	response, err := http.Get(second.URL())
	if err != nil {
		t.Fatalf("shutdown affected another gateway: %v", err)
	}
	_ = response.Body.Close()
}

type callerTransport struct{ closed atomic.Bool }

func (t *callerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fixture")
}
func (t *callerTransport) CloseIdleConnections() { t.closed.Store(true) }

func TestUpstreamGatewayDoesNotOwnInjectedClient(t *testing.T) {
	transport := &callerTransport{}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	server := lifecycleGateway(t, client)
	if server.upstream != nil || server.httpServer.Handler.(*handler).httpClient() != client {
		t.Fatal("changed injected client")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.closed.Load() {
		t.Fatal("closed caller-owned transport")
	}
}

func TestUpstreamGatewayShutdownCancelsActiveRequest(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	server := lifecycleGatewayTo(t, nil, upstream.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, requestErr := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"wait"}]}`))
		if requestErr != nil {
			t.Error(requestErr)
			return
		}
		request.Header.Set("Authorization", "Bearer token")
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
	if err := server.Shutdown(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost shutdown error: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("handler context not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client not released")
	}
}

func TestUpstreamTransportReusesAndClosesConnections(t *testing.T) {
	closed := make(chan struct{}, 4)
	var accepted atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") }))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	transport := fixtureUpstreamTransport(t, upstream, 0)
	client := &http.Client{Transport: transport}
	for range 2 {
		response, err := client.Get(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if accepted.Load() != 1 {
		t.Fatalf("connections=%d; expected reuse", accepted.Load())
	}
	transport.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("owned idle connection not closed")
	}
}

func TestUpstreamGatewayGracefulShutdownWaitsForResponse(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			t.Error("graceful shutdown canceled active request")
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	server := lifecycleGatewayTo(t, nil, upstream.URL)
	responseDone := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"wait"}]}`))
		if err != nil {
			responseDone <- err
			return
		}
		request.Header.Set("Authorization", "Bearer token")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				err = fmt.Errorf("status %d", response.StatusCode)
			}
		}
		responseDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { done <- server.Shutdown(ctx) }()
	// Serve exits when Shutdown closes its listener, before draining active requests.
	select {
	case <-server.serveDone:
	case <-ctx.Done():
		t.Fatal("listener not closed")
	}
	close(release)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamGatewayForcedShutdownClosesHTTP2Connection(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{}, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	upstream.EnableHTTP2 = true
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	upstream.StartTLS()
	defer upstream.Close()
	server := lifecycleGatewayTo(t, nil, upstream.URL)
	server.upstream.base.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"wait"}]}`))
		if err != nil {
			t.Error(err)
			return
		}
		request.Header.Set("Authorization", "Bearer token")
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("request did not start")
	}
	expired, expire := context.WithCancel(ctx)
	expire()
	if err := server.Shutdown(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("HTTP/2 connection escaped shutdown")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("downstream request not released")
	}
	server.upstream.mu.Lock()
	remaining := len(server.upstream.connections)
	server.upstream.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("owned connections=%d", remaining)
	}
}
