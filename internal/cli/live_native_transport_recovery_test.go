//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	nativeRecoveryClean int32 = iota
	nativeRecoveryTransport
	nativeRecoveryProtocol
	nativeRecoveryCleanMarker = "CCR_NATIVE_CLEAN_STREAM_OK"
	nativeRecoveryMarker      = "CCR_NATIVE_RECOVERY_OK"
	nativeRecoveryPrompt      = "Return the armed synthetic recovery fixture marker."
)

func TestLiveFixtureNativeTransportRecovery(t *testing.T) {
	proveNativeRecovery(t, nativeRecoveryTransport)
}

func TestLiveFixtureNativeProtocolRecoveryCannotProveStreaming(t *testing.T) {
	proveNativeRecovery(t, nativeRecoveryProtocol)
}

func proveNativeRecovery(t *testing.T, failureMode int32) {
	t.Helper()
	ctx := liveCompactionContext(t)
	t.Chdir(t.TempDir())
	fixture := newNativeRecoveryFixture(t)
	database := fixture.configure(t, ctx)
	s, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal("open synthetic recovery route evidence")
	}
	t.Cleanup(func() { _ = s.Close() })
	expected, err := s.GetModel(ctx, "native-transport")
	if err != nil {
		t.Fatal("read synthetic recovery target")
	}
	var launchID atomic.Int64
	deps := Dependencies{StartGateway: func(startCtx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		launchID.Store(cfg.Tracker.Snapshot().LaunchID)
		cfg.AnthropicBaseURL = fixture.firstParty.URL
		// Keep the normal gateway-owned upstream transport. Fail only the
		// synthetic provider's real HTTP connection, never replace the client.
		return gateway.Start(startCtx, cfg)
	}}
	run := startLiveCompactionCommand(t, ctx, deps, []string{
		"--db", database, "launch", "--model", "native-transport-seed", "--print", "--auth-mode", "provider-only", "--no-statusline",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
	})
	correlated := liveNativeVersionAtLeast(t, ctx, [3]int{2, 1, 310})
	selection := nativeRecoveryTurn(t, ctx, run, correlated, "/model anthropic.ccr.native-transport")
	if !strings.HasPrefix(selection, "Set model to ") {
		t.Fatal("native recovery fixture did not acknowledge its canonical model")
	}
	wantSelectionCalls := int64(1)
	if livePickerCatalogSupported(t, ctx) {
		wantSelectionCalls = 0
	}
	if fixture.inferenceCalls.Load() != wantSelectionCalls {
		t.Fatal("native recovery fixture changed catalog/legacy selection inference behavior")
	}
	boundary := liveRealRouteBoundary(t, ctx, s, launchID.Load())
	clean := nativeRecoveryTurn(t, ctx, run, correlated, "Return the clean synthetic streaming fixture marker.")
	if strings.TrimSpace(clean) != nativeRecoveryCleanMarker {
		t.Fatal("native recovery fixture did not return its exact clean-stream marker")
	}
	cleanProof := requireLiveRealMarkerRouteProof(t, ctx, s, launchID.Load(), boundary, &expected)
	if cleanProof.Mode != "streaming" || cleanProof.TransportFailures != 0 {
		t.Fatal("native recovery fixture lacked its independent clean streaming baseline")
	}
	boundary = liveRealRouteBoundary(t, ctx, s, launchID.Load())
	// Capture only the next turn's output; the clean turn's message_start must
	// never release a new upstream request before this gateway's early commit.
	offset := len(run.out.String())
	fixture.failureMode.Store(failureMode)
	var result string
	if failureMode == nativeRecoveryTransport {
		result = nativeRecoveryAfterCommit(t, ctx, run, fixture, correlated, offset)
	} else {
		result = nativeRecoveryTurn(t, ctx, run, correlated, nativeRecoveryPrompt)
	}
	if strings.TrimSpace(result) != nativeRecoveryMarker {
		t.Fatal("native recovery did not return the exact same-target marker")
	}
	if fixture.failedStreams.Load() == 0 || fixture.recoveredNonstream.Load() == 0 {
		t.Fatal("native recovery did not exercise both the failed stream and real nonstream request")
	}
	requireNativeRecoveryProof(t, ctx, s, launchID.Load(), boundary, expected, failureMode, correlated)
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatalf("finish native recovery fixture: %v", err)
	}
	if _, err := liveRealCorrelatedDrain(run); err != nil {
		t.Fatalf("drain native recovery fixture: %v", err)
	}
	if fixture.firstPartyCalls.Load() != 0 || fixture.wrongModel.Load() {
		t.Fatal("native recovery changed provider target or contacted a first-party fallback")
	}
}

func requireNativeRecoveryProof(t *testing.T, ctx context.Context, s *store.Store, launchID, boundary int64, expected store.Model, failureMode int32, correlated bool) {
	t.Helper()
	summary := waitNativeRecoveryTrace(t, ctx, s, launchID, boundary, expected)
	if failureMode == nativeRecoveryTransport {
		proof := requireLiveRealMarkerRouteProof(t, ctx, s, launchID, boundary, &expected)
		if proof.Mode != "recovered" || proof.TransportFailures < 1 || summary.transportFailures < 1 || summary.otherFailures != 0 {
			t.Fatal("native pre-header transport failure did not produce explicit nonstream recovery proof")
		}
		t.Logf("native recovery proof: mode=%s transport_failures=%d correlated=%t", proof.Mode, proof.TransportFailures, correlated)
	} else {
		if summary.protocolFailures < 1 || summary.otherFailures != summary.protocolFailures || summary.transportFailures != 0 {
			t.Fatal("synthetic protocol failure did not retain its nontransport error classification")
		}
		if _, err := waitLiveRealMarkerRouteProof(ctx, s, launchID, boundary, &expected); err == nil {
			t.Fatal("nontransport protocol failure incorrectly qualified as a safe recovery pair")
		}
		t.Logf("native protocol recovery rejected: protocol_failures=%d nonstream_successes=%d correlated=%t", summary.protocolFailures, summary.nonstreamSuccesses, correlated)
	}
}

func nativeRecoveryTurn(t *testing.T, ctx context.Context, run *liveCompactionRun, correlated bool, prompt string) string {
	t.Helper()
	input := liveStreamInput(t, prompt)
	result, metadata, err := nativeRecoveryResult(ctx, run, correlated, input)
	if err != nil {
		t.Fatalf("native recovery turn failed (correlated=%t): %v", correlated, err)
	}
	if correlated && (!metadata.InputMatched || !metadata.OutputIDDistinct || strings.HasPrefix(prompt, "/model ") && !metadata.ModelCommand) {
		t.Fatal("native recovery turn lacked its verified input UUID attribution")
	}
	return result
}

func nativeRecoveryResult(ctx context.Context, run *liveCompactionRun, correlated bool, input string) (string, liveRealTurnMetadata, error) {
	if correlated {
		return liveRealCorrelatedTurn(ctx, run, input)
	}
	result, err := liveRealTurn(ctx, run, input)
	return result, liveRealTurnMetadata{}, err
}

type nativeRecoveryCompletion struct {
	text     string
	metadata liveRealTurnMetadata
	err      error
}

type nativeRecoveryStreamGate struct {
	release chan struct{}
}

func nativeRecoveryAfterCommit(t *testing.T, ctx context.Context, run *liveCompactionRun, fixture *nativeRecoveryFixture, correlated bool, offset int) string {
	t.Helper()
	turnCtx, cancel := context.WithCancel(ctx)
	completed := make(chan nativeRecoveryCompletion, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Error("native recovery turn waiter did not shut down")
		}
	})
	input := liveStreamInput(t, nativeRecoveryPrompt)
	go func() {
		defer close(completed)
		text, metadata, err := nativeRecoveryResult(turnCtx, run, correlated, input)
		completed <- nativeRecoveryCompletion{text: text, metadata: metadata, err: err}
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var pending []nativeRecoveryStreamGate
	released := 0
	for {
		starts := nativeRecoveryMessageStarts(run.out.String()[offset:])
		for len(pending) > 0 && starts > released {
			close(pending[0].release)
			pending = pending[1:]
			released++
		}
		select {
		case gate := <-fixture.streamGates:
			pending = append(pending, gate)
		case result := <-completed:
			if result.err != nil {
				t.Fatalf("native turn did not recover after early-commit transport failure: %v", result.err)
			}
			if correlated && (!result.metadata.InputMatched || !result.metadata.OutputIDDistinct) {
				t.Fatal("native transport recovery lacked input UUID attribution")
			}
			return result.text
		case <-ctx.Done():
			t.Fatalf("waiting for observed native early commit and recovery: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func nativeRecoveryMessageStarts(output string) int {
	count := 0
	for line := range strings.SplitSeq(output, "\n") {
		var message struct {
			Type  string `json:"type"`
			Event struct {
				Type string `json:"type"`
			} `json:"event"`
		}
		if json.Unmarshal([]byte(line), &message) == nil && message.Type == "stream_event" && message.Event.Type == "message_start" {
			count++
		}
	}
	return count
}

func TestLiveFixtureNativeRecoveryMessageStartProjection(t *testing.T) {
	output := "{\"type\":\"system\",\"event\":{\"type\":\"message_start\"}}\n" +
		"{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"text\":\"message_start\"}}\n" +
		"{\"type\":\"stream_event\",\"event\":{\"type\":\"message_start\"}}\n" +
		`{"type":"stream_event","event":{"type":"message_`
	if nativeRecoveryMessageStarts(output) != 1 {
		t.Fatal("recovery gate accepted an unrelated event, event body or partial JSON frame")
	}
	if nativeRecoveryMessageStarts(output+`start"}}`+"\n") != 2 {
		t.Fatal("recovery gate lost a completed typed message_start event")
	}
}

type nativeRecoveryFixture struct {
	t                    *testing.T
	provider, firstParty *httptest.Server
	failureMode          atomic.Int32
	inferenceCalls       atomic.Int64
	failedStreams        atomic.Int64
	recoveredNonstream   atomic.Int64
	firstPartyCalls      atomic.Int64
	wrongModel           atomic.Bool
	streamGates          chan nativeRecoveryStreamGate
}

func newNativeRecoveryFixture(t *testing.T) *nativeRecoveryFixture {
	t.Helper()
	fixture := &nativeRecoveryFixture{t: t, streamGates: make(chan nativeRecoveryStreamGate, 16)}
	fixture.provider = httptest.NewServer(http.HandlerFunc(fixture.handle))
	fixture.firstParty = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.firstPartyCalls.Add(1)
		http.Error(w, "unexpected synthetic first-party fallback", http.StatusBadRequest)
	}))
	t.Cleanup(fixture.provider.Close)
	t.Cleanup(fixture.firstParty.Close)
	return fixture
}

func (fixture *nativeRecoveryFixture) configure(t *testing.T, ctx context.Context) string {
	t.Helper()
	database := filepath.Join(t.TempDir(), "ccr.db")
	commands := [][]string{
		{"--db", database, "provider", "add", "native-transport", "--type", "litellm", "--base-url", fixture.provider.URL, "--no-api-key", "--mode", "full"},
		{"--db", database, "model", "add", "native-transport-seed", "--provider", "native-transport", "--model", "gpt-5", "--compat", "full"},
		{"--db", database, "model", "add", "native-transport", "--provider", "native-transport", "--model", "gpt-5", "--compat", "full"},
	}
	for _, args := range commands {
		if _, _, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("configure synthetic native recovery provider: %v", err)
		}
	}
	return database
}

func (fixture *nativeRecoveryFixture) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"}]}`))
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		fixture.t.Error("decode synthetic native recovery request")
		http.Error(w, "invalid synthetic request", http.StatusBadRequest)
		return
	}
	fixture.inferenceCalls.Add(1)
	if payload.Model != "gpt-5" {
		fixture.wrongModel.Store(true)
		http.Error(w, "wrong synthetic provider model", http.StatusBadRequest)
		return
	}
	mode := fixture.failureMode.Load()
	if payload.Stream && mode != nativeRecoveryClean {
		fixture.failedStreams.Add(1)
		if mode == nativeRecoveryTransport {
			gate := nativeRecoveryStreamGate{release: make(chan struct{})}
			select {
			case fixture.streamGates <- gate:
			case <-r.Context().Done():
				return
			}
			select {
			case <-gate.release:
			case <-r.Context().Done():
				return
			}
			fixture.closeBeforeHeaders(w)
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"synthetic-protocol-failure\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = fmt.Fprint(w, "data: {invalid-synthetic-json}\n\n")
		}
		return
	}
	if !payload.Stream && mode != nativeRecoveryClean {
		fixture.recoveredNonstream.Add(1)
	}
	marker := nativeRecoveryCleanMarker
	if openAIMessagesContain(payload.Messages, nativeRecoveryPrompt) {
		marker = nativeRecoveryMarker
	}
	writeLiveOpenAITextFixture(w, payload, "chatcmpl-native-recovery", marker, 4, 2)
}

func (fixture *nativeRecoveryFixture) closeBeforeHeaders(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fixture.t.Error("synthetic HTTP fixture cannot hijack its connection")
		http.Error(w, "synthetic hijack unavailable", http.StatusInternalServerError)
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		fixture.t.Error("hijack synthetic provider connection before response headers")
		return
	}
	if err := connection.Close(); err != nil {
		fixture.t.Error("close synthetic provider connection before response headers")
	}
}

type nativeRecoveryTraceSummary struct {
	transportFailures, protocolFailures, otherFailures, nonstreamSuccesses int
}

func waitNativeRecoveryTrace(t *testing.T, ctx context.Context, s *store.Store, launchID, afterID int64, expected store.Model) nativeRecoveryTraceSummary {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, AfterID: afterID, Kind: "route", Name: "messages", OldestFirst: true, Limit: 1000})
		if err != nil {
			t.Fatal("read synthetic native recovery route evidence")
		}
		summary := nativeRecoveryTraceSummary{}
		for _, event := range events {
			route := event.Route
			if route.RouteKind != "registered" || route.ModelAlias != expected.Alias || route.ProviderName != expected.ProviderName || route.ProviderModel != expected.ProviderModel || route.ClassifierSelectionSource != "" || !gateway.DiscoveryIDMatchesAlias(route.RequestedModel, expected.Alias) {
				continue
			}
			if event.Status == "failed" && route.Streaming {
				if route.ErrorClass == "provider_transport" {
					summary.transportFailures++
				} else {
					summary.otherFailures++
					if route.ErrorClass == "provider_protocol" || route.ErrorClass == "provider_stream" {
						summary.protocolFailures++
					}
				}
			}
			if event.Status == "succeeded" && !route.Streaming && route.HTTPStatus >= http.StatusOK && route.HTTPStatus < http.StatusMultipleChoices {
				summary.nonstreamSuccesses++
			}
		}
		if summary.nonstreamSuccesses > 0 && summary.transportFailures+summary.otherFailures > 0 {
			return summary
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native did not persist its same-target failed-stream/nonstream recovery pair: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
