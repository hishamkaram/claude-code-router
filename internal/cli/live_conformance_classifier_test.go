//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func (f *liveClaudeConformanceFixture) handleCountTokens(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding conformance count_tokens request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if isLiveAnthropicAutoClassifierRequest(payload) {
		if strings.HasPrefix(payload.Model, "fixture-") {
			f.recordSelectedClassifier(true)
		} else {
			f.recordFirstPartyClassifier(true)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
}

func (f *liveClaudeConformanceFixture) recordSelectedClassifier(countTokens bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if countTokens {
		f.selectedClassifierCountTokens++
		return
	}
	f.selectedClassifierMessages++
}

func (f *liveClaudeConformanceFixture) recordFirstPartyClassifier(countTokens bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if countTokens {
		f.firstPartyClassifierCountTokens++
		return
	}
	f.firstPartyClassifierMessages++
}

func (f *liveClaudeConformanceFixture) summary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf(
		"aliases=%v firstParty=%d selectedClassifier=%d/%d firstPartyClassifier=%d/%d agent=%t workflow=%t steps=%v",
		f.aliasModels,
		f.firstParty,
		f.selectedClassifierMessages,
		f.selectedClassifierCountTokens,
		f.firstPartyClassifierMessages,
		f.firstPartyClassifierCountTokens,
		f.agentToolSeen,
		f.workflowSeen,
		f.requestSteps,
	)
}

func (f *liveClaudeConformanceFixture) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	// Classifier traces are validated against the ordered active route by the
	// conformance verifier. This transport fixture only requires that Claude
	// exercised classifier routing through a selected CCR alias.
	if f.aliasModels["fixture-full-model"] == 0 || f.aliasModels["fixture-chat-model"] == 0 ||
		f.firstParty == 0 || f.selectedClassifierMessages == 0 ||
		!f.agentToolSeen || !f.workflowSeen {
		t.Fatalf(
			"live conformance fixture incomplete: aliases=%v firstParty=%d selectedClassifier=%d/%d firstPartyClassifier=%d/%d agent=%v workflow=%v\nstdout:\n%s\nstderr:\n%s",
			f.aliasModels,
			f.firstParty,
			f.selectedClassifierMessages,
			f.selectedClassifierCountTokens,
			f.firstPartyClassifierMessages,
			f.firstPartyClassifierCountTokens,
			f.agentToolSeen,
			f.workflowSeen,
			out,
			errOut,
		)
	}
}

func (f *liveClaudeConformanceFixture) assertClassifierTraceCoverage(t *testing.T, dbPath string) {
	t.Helper()
	f.mu.Lock()
	wantMessages := f.selectedClassifierMessages + f.firstPartyClassifierMessages
	wantCountTokens := f.selectedClassifierCountTokens + f.firstPartyClassifierCountTokens
	f.mu.Unlock()

	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening conformance trace store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{Limit: 10_000, OldestFirst: true})
	if err != nil {
		t.Fatalf("reading conformance route trace: %v", err)
	}
	gotMessages, gotCountTokens := 0, 0
	for index := range events {
		switch events[index].Route.Operation {
		case "auto_mode_classifier":
			gotMessages++
		case "auto_mode_classifier_count_tokens":
			gotCountTokens++
		}
	}
	if gotMessages != wantMessages || gotCountTokens != wantCountTokens {
		t.Fatalf(
			"classifier trace coverage = %d/%d, want provider-observed %d/%d",
			gotMessages, gotCountTokens, wantMessages, wantCountTokens,
		)
	}
}

func liveConformanceRouteSummary(t *testing.T, dbPath string) string {
	t.Helper()
	s, err := store.OpenReadOnly(context.Background(), dbPath)
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	defer func() { _ = s.Close() }()
	events, err := s.ListTraceEvents(context.Background(), store.TraceFilter{Limit: 1000, OldestFirst: true})
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	routes := make([]string, 0, len(events))
	for index := range events {
		if events[index].Kind != "route" {
			continue
		}
		route := events[index].Route
		routes = append(routes, fmt.Sprintf("session=%d operation=%s kind=%s alias=%s status=%s",
			route.SessionID, route.Operation, route.RouteKind, route.ModelAlias, route.Status))
	}
	return strings.Join(routes, "; ")
}
