package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestUpstreamPartialToolFailureHasOneTerminalErrorAndTrace(t *testing.T) {
	var calls atomic.Int32
	abort := make(chan struct{})
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Litellm-Model-Id", strings.Repeat("a", 64))
		w.Header().Set("X-Litellm-Call-Id", "credential-must-not-be-recorded")
		_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"partial-text","tool_calls":[{"index":0,"id":"call_fixture","type":"function","function":{"name":"Read","arguments":"{\"file_path\":"}}]}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// Wait until the downstream observer has received the accompanying text.
		select {
		case <-abort:
			panic(http.ErrAbortHandler)
		case <-r.Context().Done():
			return
		}
	}))
	provider.EnableHTTP2 = true
	provider.StartTLS()
	defer provider.Close()
	transport := fixtureUpstreamTransport(t, provider, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := newGatewayStore(t, store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: provider.URL, SupportsStreaming: true, SupportsTools: true}, store.Model{Alias: "model", ProviderName: "fixture", ProviderModel: "model", Status: "degraded"})
	launchID, err := db.CreateLaunch(ctx, "model", "pending", "pending")
	if err != nil {
		t.Fatal(err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{Store: db, LaunchID: launchID, Enabled: true})
	server := startGatewayWithConfig(t, ctx, Config{Store: db, Token: "token", HTTPClient: &http.Client{Transport: transport}, Recorder: recorder})
	defer server.Shutdown(ctx)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(`{"model":"model","stream":true,"messages":[{"role":"user","content":"read"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	var prefix strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		prefix.WriteString(line)
		if strings.Contains(line, "partial-text") {
			close(abort)
			break
		}
		if readErr != nil {
			t.Fatalf("partial stream not delivered: %v", readErr)
		}
	}
	body, err := io.ReadAll(reader)
	body = append([]byte(prefix.String()), body...)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || strings.Count(string(body), "event: error") != 1 || strings.Contains(string(body), "event: message_stop") || !strings.Contains(string(body), "partial-text") || calls.Load() != 1 {
		t.Fatalf("invalid terminal contract: status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
	}
	events, err := db.ListTraceEvents(ctx, store.TraceFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	for _, event := range events {
		if event.Kind == "lifecycle" && event.Lifecycle.Name == "upstream_transport" {
			observations++
			if event.Lifecycle.ExternalID != response.Header.Get(ccrRequestIDHeader) {
				t.Fatal("transport correlation missing")
			}
			if !strings.Contains(event.Lifecycle.Reason, "owner=caller; protocol=unknown; idle_ping=unverified") {
				t.Fatal("injected protection claimed")
			}
			if strings.Contains(event.Lifecycle.Reason, "credential") {
				t.Fatal("unsafe response header recorded")
			}
		}
	}
	if observations != 1 {
		t.Fatalf("observations=%d", observations)
	}
}
