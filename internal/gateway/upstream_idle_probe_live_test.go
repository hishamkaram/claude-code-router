//go:build live

package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"
)

// A transparent downstream tap verifies events actually sent to the real CLI.
// It retains only counters and timing, never event bodies or headers.
type idleDownstreamProbe struct {
	mu            sync.Mutex
	started       time.Time
	firstStart    time.Duration
	starts, pings int
}

func newIdleDownstreamProbe(t *testing.T, target string) (*httptest.Server, *idleDownstreamProbe) {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	probe := &idleDownstreamProbe{started: time.Now()}
	proxy := httputil.NewSingleHostReverseProxy(parsed)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(response *http.Response) error {
		if isOpenAIEventStream(response.Header.Get("Content-Type")) {
			response.Body = &idleProbeBody{ReadCloser: response.Body, probe: probe}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(func() { server.Close(); transport.CloseIdleConnections() })
	return server, probe
}

type idleProbeBody struct {
	io.ReadCloser
	probe   *idleDownstreamProbe
	pending []byte
}

func (b *idleProbeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.pending = append(b.pending, p[:n]...)
	for {
		index := bytes.IndexByte(b.pending, '\n')
		if index < 0 {
			break
		}
		line := bytes.TrimSpace(b.pending[:index])
		b.probe.mu.Lock()
		if bytes.Equal(line, []byte("event: message_start")) {
			if b.probe.starts == 0 {
				b.probe.firstStart = time.Since(b.probe.started)
			}
			b.probe.starts++
		}
		if bytes.Equal(line, []byte("event: ping")) {
			b.probe.pings++
		}
		b.probe.mu.Unlock()
		b.pending = b.pending[index+1:]
	}
	return n, err
}

func (p *idleDownstreamProbe) assertLiveness(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.starts != 2 || p.pings < 2 || p.firstStart > 10*time.Second {
		t.Fatalf("downstream liveness: starts=%d pings=%d first_start=%s", p.starts, p.pings, p.firstStart)
	}
	t.Logf("downstream starts=%d heartbeats=%d first_start=%s", p.starts, p.pings, p.firstStart.Round(time.Millisecond))
}
