//go:build live

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
)

const livePickerCatalogResponse = "CCR_LIVE_MODEL_PICKER_TURN_OK"

func TestLiveFixtureModelPickerSelectionUsesCatalog(t *testing.T) {
	for _, target := range []struct {
		name, discoveryID, providerModel string
	}{
		{"ordinary", "anthropic.ccr.picker-ordinary", "picker-ordinary-model"},
		{"million", "anthropic.ccr.picker-million[1m]", "picker-million-model"},
	} {
		t.Run(target.name, func(t *testing.T) {
			ctx := liveCompactionContext(t)
			t.Chdir(t.TempDir())
			modern := livePickerCatalogSupported(t, ctx)
			fixture := newLivePickerCatalogFixture(t, target.providerModel)
			database := fixture.configure(t, ctx)
			run := startLivePickerCatalogRun(t, ctx, database, fixture)
			selection, err := liveRealTurn(ctx, run, liveStreamInput(t, "/model "+target.discoveryID))
			if err != nil || !strings.HasPrefix(selection, "Set model to ") {
				t.Fatalf("native catalog selection was not acknowledged: error=%v; %s", err, liveRealSelectionSummary(selection, target.discoveryID))
			}
			fixture.assertSelectionCalls(t, modern)
			fixture.turnStarted.Store(true)
			result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with the synthetic catalog fixture response."))
			if err != nil || strings.TrimSpace(result) != livePickerCatalogResponse {
				t.Fatalf("selected native model did not complete its real turn: result_matches=%t error=%v", strings.TrimSpace(result) == livePickerCatalogResponse, err)
			}
			fixture.assertRealRoute(t)
			if _, _, err := run.finish(ctx); err != nil {
				t.Fatalf("finish catalog fixture launch: %v", err)
			}
		})
	}
}

func TestLiveFixtureModelPickerSlowProviderSelection(t *testing.T) {
	ctx := liveCompactionContext(t)
	t.Chdir(t.TempDir())
	modern := livePickerCatalogSupported(t, ctx)
	fixture := newLivePickerCatalogFixture(t, "picker-ordinary-model")
	fixture.holdSelection.Store(true)
	database := fixture.configure(t, ctx)
	run := startLivePickerCatalogRun(t, ctx, database, fixture)
	const model = "anthropic.ccr.picker-ordinary"
	var selection string
	var err error
	if modern {
		selection, err = liveRealTurn(ctx, run, liveStreamInput(t, "/model "+model))
	} else {
		selection, err = completeLegacyPickerValidation(t, ctx, run, fixture, model)
	}
	if err != nil {
		t.Fatalf("slow synthetic provider model selection did not produce a native result: %v", err)
	}
	if !strings.HasPrefix(selection, "Set model to ") {
		t.Fatalf("catalog selection incorrectly waited for provider inference: %s", liveRealSelectionSummary(selection, model))
	}
	fixture.assertSelectionCalls(t, modern)
	fixture.turnStarted.Store(true)
	result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with the synthetic catalog fixture response."))
	if err != nil || strings.TrimSpace(result) != livePickerCatalogResponse {
		t.Fatalf("slow-selection fixture did not route its first real turn: result_matches=%t error=%v", strings.TrimSpace(result) == livePickerCatalogResponse, err)
	}
	fixture.assertRealRoute(t)
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatalf("finish slow catalog fixture launch: %v", err)
	}
}

func completeLegacyPickerValidation(t *testing.T, ctx context.Context, run *liveCompactionRun, fixture *livePickerCatalogFixture, model string) (string, error) {
	t.Helper()
	selectionCtx, cancel := context.WithCancel(ctx)
	type selectionResult struct {
		text string
		err  error
	}
	completed := make(chan selectionResult, 1)
	input := liveStreamInput(t, "/model "+model)
	go func() {
		defer close(completed)
		text, err := liveRealTurn(selectionCtx, run, input)
		completed <- selectionResult{text: text, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Error("legacy selection waiter did not shut down")
		}
	})
	select {
	case <-fixture.selectionEntered:
	case <-completed:
		t.Fatal("legacy selection completed before contacting its validation provider")
	case <-ctx.Done():
		t.Fatal("legacy selection never reached its validation provider")
	}
	fixture.assertSelectionCalls(t, false)
	select {
	case <-completed:
		t.Fatal("legacy selection completed before the held validation response was released")
	default:
	}
	close(fixture.selectionRelease)
	select {
	case result := <-completed:
		return result.text, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestLiveFixtureModelPickerRejectsUnregisteredID(t *testing.T) {
	ctx := liveCompactionContext(t)
	t.Chdir(t.TempDir())
	fixture := newLivePickerCatalogFixture(t, "picker-seed-model")
	database := fixture.configure(t, ctx)
	run := startLivePickerCatalogRun(t, ctx, database, fixture)
	const unknown = "anthropic.ccr.picker-unregistered"
	const policyRejection = "Model '" + unknown + "' is not available. Your organization restricts model selection."
	selection, err := liveRealTurn(ctx, run, liveStreamInput(t, "/model "+unknown))
	if err != nil || strings.TrimSpace(selection) != policyRejection {
		t.Fatalf("native model policy did not reject the unregistered ID: error=%v; %s", err, liveRealSelectionSummary(selection, unknown))
	}
	if fixture.inferenceCalls.Load() != 0 || fixture.fallbackCalls.Load() != 0 {
		t.Fatal("unregistered-ID policy rejection contacted a provider")
	}
	fixture.turnStarted.Store(true)
	result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply with the synthetic catalog fixture response."))
	if err != nil || strings.TrimSpace(result) != livePickerCatalogResponse {
		t.Fatalf("previous model did not remain usable after rejecting an unknown ID: result_matches=%t error=%v", strings.TrimSpace(result) == livePickerCatalogResponse, err)
	}
	fixture.assertRealRoute(t)
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatalf("finish model-policy fixture launch: %v", err)
	}
}

func TestLiveFixtureModelPickerRealTurnFailureDoesNotFallback(t *testing.T) {
	ctx := liveCompactionContext(t)
	t.Chdir(t.TempDir())
	modern := livePickerCatalogSupported(t, ctx)
	fixture := newLivePickerCatalogFixture(t, "picker-ordinary-model")
	database := fixture.configure(t, ctx)
	run := startLivePickerCatalogRun(t, ctx, database, fixture)
	const model = "anthropic.ccr.picker-ordinary"
	selection, err := liveRealTurn(ctx, run, liveStreamInput(t, "/model "+model))
	if err != nil || !strings.HasPrefix(selection, "Set model to ") {
		t.Fatalf("synthetic model was not selectable before its real request: error=%v; %s", err, liveRealSelectionSummary(selection, model))
	}
	fixture.assertSelectionCalls(t, modern)
	fixture.failActual.Store(true)
	fixture.turnStarted.Store(true)
	offset := len(run.out.String())
	_, err = liveRealTurn(ctx, run, liveStreamInput(t, "Send the real request to the synthetic failing provider."))
	_, failed, complete := liveRealTurnResult(run.out.String()[offset:])
	if err == nil || !complete || !failed {
		t.Fatalf("real provider failure was not a visible native error result: error=%v complete=%t failed=%t", err, complete, failed)
	}
	fixture.assertRealRoute(t)
}

func livePickerCatalogSupported(t *testing.T, ctx context.Context) bool {
	t.Helper()
	// modelPicker was added in Claude Code 2.1.242. Earlier supported releases
	// honestly exercise their legacy one-request inference validation instead.
	return liveNativeVersionAtLeast(t, ctx, [3]int{2, 1, 242})
}

type livePickerCatalogFixture struct {
	provider, firstParty *httptest.Server
	expectedModel        string
	inferenceCalls       atomic.Int64
	actualCalls          atomic.Int64
	wrongRoute           atomic.Bool
	fallbackCalls        atomic.Int64
	turnStarted          atomic.Bool
	holdSelection        atomic.Bool
	failActual           atomic.Bool
	selectionEntered     chan struct{}
	selectionRelease     chan struct{}
}

func newLivePickerCatalogFixture(t *testing.T, expectedModel string) *livePickerCatalogFixture {
	t.Helper()
	fixture := &livePickerCatalogFixture{
		expectedModel: expectedModel, selectionEntered: make(chan struct{}, 1), selectionRelease: make(chan struct{}),
	}
	fixture.provider = httptest.NewServer(http.HandlerFunc(fixture.handleProvider))
	fixture.firstParty = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.fallbackCalls.Add(1)
		http.Error(w, "unexpected first-party fallback", http.StatusBadRequest)
	}))
	t.Cleanup(fixture.provider.Close)
	t.Cleanup(fixture.firstParty.Close)
	return fixture
}

func (fixture *livePickerCatalogFixture) configure(t *testing.T, ctx context.Context) string {
	t.Helper()
	database := filepath.Join(t.TempDir(), "ccr.db")
	commands := [][]string{{"--db", database, "provider", "add", "picker-fixture", "--type", "litellm", "--base-url", fixture.provider.URL, "--no-api-key", "--mode", "full"}}
	for _, alias := range []string{"seed", "ordinary", "million"} {
		commands = append(commands, []string{"--db", database, "model", "add", "picker-" + alias, "--provider", "picker-fixture", "--model", "picker-" + alias + "-model", "--compat", "full"})
	}
	commands = append(commands, []string{"--db", database, "model", "update", "picker-million", "--context-window", "1000000"})
	for _, args := range commands {
		if _, _, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("configure synthetic catalog registry: %v", err)
		}
	}
	return database
}

func startLivePickerCatalogRun(t *testing.T, ctx context.Context, database string, fixture *livePickerCatalogFixture) *liveCompactionRun {
	t.Helper()
	deps := Dependencies{StartGateway: func(startCtx context.Context, cfg gateway.Config) (*gateway.Server, error) {
		cfg.AnthropicBaseURL = fixture.firstParty.URL
		return gateway.Start(startCtx, cfg)
	}}
	return startLiveCompactionCommand(t, ctx, deps, []string{
		"--db", database, "launch", "--model", "picker-seed", "--print", "--auth-mode", "provider-only", "--no-statusline",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	})
}

func (fixture *livePickerCatalogFixture) handleProvider(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"picker-seed-model"},{"id":"picker-ordinary-model"},{"id":"picker-million-model"}]}`))
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var payload liveOpenAIChatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid synthetic catalog request", http.StatusBadRequest)
		return
	}
	fixture.inferenceCalls.Add(1)
	actual := fixture.turnStarted.Load()
	if !actual && fixture.holdSelection.Load() {
		// Modern catalog selection must issue no validation request. Legacy
		// selection depends on this response and is released only by its test.
		select {
		case fixture.selectionEntered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			return
		case <-fixture.selectionRelease:
		}
	}
	if actual {
		fixture.actualCalls.Add(1)
		if payload.Model != fixture.expectedModel {
			fixture.wrongRoute.Store(true)
		}
		if fixture.failActual.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"synthetic real-turn rejection"}}`))
			return
		}
	}
	writeLiveOpenAITextFixture(w, payload, "chatcmpl-picker-catalog", livePickerCatalogResponse, 4, 2)
}

func (fixture *livePickerCatalogFixture) assertSelectionCalls(t *testing.T, modern bool) {
	t.Helper()
	want := int64(1)
	if modern {
		want = 0
	}
	if got := fixture.inferenceCalls.Load(); got != want {
		t.Fatalf("native model selection inference requests=%d, want=%d (catalog_supported=%t)", got, want, modern)
	}
	if fixture.fallbackCalls.Load() != 0 {
		t.Fatal("native model selection contacted the first-party fallback")
	}
}

func (fixture *livePickerCatalogFixture) assertRealRoute(t *testing.T) {
	t.Helper()
	if fixture.actualCalls.Load() == 0 || fixture.wrongRoute.Load() {
		t.Fatal("native real turn did not use exactly the selected provider model")
	}
	if fixture.fallbackCalls.Load() != 0 {
		t.Fatal("native real turn contacted the first-party fallback")
	}
}
