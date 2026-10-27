//go:build live

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

// The store owns route evidence. Capture its latest event ID after /model ACK
// and before sending the marker: an earlier validation request cannot later
// become marker proof merely because its completion arrives after that ACK.
func liveRealRouteBoundary(t *testing.T, ctx context.Context, s *store.Store, launchID int64) int64 {
	t.Helper()
	if launchID <= 0 {
		t.Fatal("marker route boundary requires the current launch")
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launchID, Limit: 1})
	if err != nil {
		t.Fatal("reading marker route boundary failed; details withheld")
	}
	if len(events) == 0 {
		return 0
	}
	return events[0].ID
}

func assertLiveRealMarkerRoute(t *testing.T, ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model) {
	t.Helper()
	if err := waitLiveRealMarkerRoute(ctx, s, launchID, afterID, expected); err != nil {
		t.Fatal(err)
	}
}

type liveRealMarkerRouteProof struct {
	Mode              string
	TransportFailures int
}

func requireLiveRealMarkerRouteProof(t *testing.T, ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model) liveRealMarkerRouteProof {
	t.Helper()
	proof, err := waitLiveRealMarkerRouteProof(ctx, s, launchID, afterID, expected)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func waitLiveRealMarkerRouteProof(ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model) (liveRealMarkerRouteProof, error) {
	return waitLiveRealMarkerRouteEvidence(ctx, s, launchID, afterID, expected, true)
}

// message_stop can reach Claude before the gateway's deferred route completion
// transaction commits. Wait only for that local persistence, under the caller's
// existing deadline; every scan starts at the immutable pre-marker boundary.
func waitLiveRealMarkerRoute(ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model) error {
	_, err := waitLiveRealMarkerRouteEvidence(ctx, s, launchID, afterID, expected, false)
	return err
}

func waitLiveRealMarkerRouteEvidence(ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model, allowRecovery bool) (liveRealMarkerRouteProof, error) {
	if launchID <= 0 || afterID < 0 {
		return liveRealMarkerRouteProof{}, errors.New("marker route assertion requires a current launch and valid boundary")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		proof, err := scanLiveRealMarkerRouteEvidence(waitCtx, s, launchID, afterID, expected, allowRecovery)
		if err != nil {
			if waitCtx.Err() != nil {
				return liveRealMarkerRouteProof{}, fmt.Errorf("waiting for completed marker route evidence: %w", waitCtx.Err())
			}
			return liveRealMarkerRouteProof{}, err
		}
		if proof.Mode != "" {
			return proof, nil
		}
		select {
		case <-waitCtx.Done():
			return liveRealMarkerRouteProof{}, fmt.Errorf("waiting for completed marker route evidence: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func scanLiveRealMarkerRoutes(ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model) (bool, error) {
	proof, err := scanLiveRealMarkerRouteEvidence(ctx, s, launchID, afterID, expected, false)
	return proof.Mode != "", err
}

func scanLiveRealMarkerRouteEvidence(ctx context.Context, s *store.Store, launchID, afterID int64, expected *store.Model, allowRecovery bool) (liveRealMarkerRouteProof, error) {
	const pageSize = 1000
	cursor := afterID
	// This scan alone owns derived retry evidence. Recreate it from the immutable
	// boundary on every readiness poll, including failures committed after retry.
	scan := liveRealMarkerProofScan{transportFailures: make(map[liveRealMarkerRouteIdentity]int)}
	for {
		events, err := s.ListTraceEvents(ctx, store.TraceFilter{
			LaunchID: launchID, AfterID: cursor, OldestFirst: true,
			Kind: "route", Name: "messages", Limit: pageSize,
		})
		if err != nil {
			return liveRealMarkerRouteProof{}, errors.New("reading completed marker route evidence failed; details withheld")
		}
		for _, event := range events {
			proof, err := scan.observe(event, launchID, afterID, expected, allowRecovery)
			if err != nil || !allowRecovery && proof.Mode != "" {
				return proof, err
			}
			cursor = event.ID
		}
		if len(events) < pageSize {
			return scan.proof(), nil
		}
	}
}

type liveRealMarkerRouteIdentity struct {
	requestedID, routeKind, alias, provider, backend string
}

type liveRealMarkerProofScan struct {
	lastID            int64
	pendingStream     bool
	candidateMode     string
	transportFailures map[liveRealMarkerRouteIdentity]int
}

// Recovery-aware evidence is provisional until every matching event/page has
// been inspected. Later protocol failures or unresolved streams invalidate it.
func (scan *liveRealMarkerProofScan) proof() liveRealMarkerRouteProof {
	if scan.pendingStream || scan.candidateMode == "" {
		return liveRealMarkerRouteProof{}
	}
	failures := 0
	for _, count := range scan.transportFailures {
		failures += count
	}
	return liveRealMarkerRouteProof{Mode: scan.candidateMode, TransportFailures: failures}
}

func (scan *liveRealMarkerProofScan) observe(event store.TraceEvent, launchID, afterID int64, expected *store.Model, allowRecovery bool) (liveRealMarkerRouteProof, error) {
	if event.ID <= scan.lastID {
		return liveRealMarkerRouteProof{}, errors.New("marker route evidence is not ordered by event ID")
	}
	scan.lastID = event.ID
	if !liveRealMarkerRouteTargetMatches(event, launchID, afterID, expected) {
		return liveRealMarkerRouteProof{}, nil
	}
	if !allowRecovery {
		if liveRealMarkerRouteMatches(event, launchID, afterID, expected) {
			return liveRealMarkerRouteProof{Mode: "streaming"}, nil
		}
		return liveRealMarkerRouteProof{}, nil
	}
	route := event.Route
	identity := liveRealMarkerRouteIdentity{
		requestedID: route.RequestedModel, routeKind: route.RouteKind, alias: route.ModelAlias,
		provider: route.ProviderName, backend: route.ProviderModel,
	}
	if route.Streaming {
		switch event.Status {
		case "failed":
			if route.ErrorClass != "provider_transport" {
				return liveRealMarkerRouteProof{}, errors.New("main marker stream failed outside provider transport; recovery is not route proof")
			}
			scan.transportFailures[identity]++
		case "canceled":
			return liveRealMarkerRouteProof{}, errors.New("main marker stream canceled; not route proof")
		case "started":
			// Wait for classification; deferred completion may follow the retry.
			scan.pendingStream = true
		case "succeeded":
		default:
			return liveRealMarkerRouteProof{}, errors.New("main marker stream has an unknown trace status")
		}
	}
	if event.Status != "succeeded" || route.HTTPStatus < http.StatusOK || route.HTTPStatus >= http.StatusMultipleChoices || route.ErrorClass != "" || scan.pendingStream {
		return liveRealMarkerRouteProof{}, nil
	}
	failures := scan.transportFailures[identity]
	if route.Streaming {
		scan.candidateMode = "streaming"
	} else if failures > 0 && scan.candidateMode == "" {
		scan.candidateMode = "recovered"
	}
	return liveRealMarkerRouteProof{}, nil
}

// A nil expected model denotes this matrix's /model sonnet checks, not any
// first-party side call that happens to complete during the marker window.
func liveRealMarkerRouteMatches(event store.TraceEvent, launchID, afterID int64, expected *store.Model) bool {
	return liveRealMarkerRouteTargetMatches(event, launchID, afterID, expected) && event.Status == "succeeded" &&
		event.Route.Streaming && event.Route.HTTPStatus >= http.StatusOK && event.Route.HTTPStatus < http.StatusMultipleChoices
}

func liveRealMarkerRouteTargetMatches(event store.TraceEvent, launchID, afterID int64, expected *store.Model) bool {
	if event.ID <= afterID || event.LaunchID != launchID || event.Kind != "route" ||
		event.Name != "messages" || event.Route.ClassifierSelectionSource != "" {
		return false
	}
	route := event.Route
	if expected == nil {
		return route.RouteKind == "first-party-anthropic" && strings.Contains(strings.ToLower(route.RequestedModel), "sonnet")
	}
	if expected.Alias == "" || expected.ProviderName == "" || expected.ProviderModel == "" ||
		route.RouteKind != "registered" || route.ModelAlias != expected.Alias ||
		route.ProviderName != expected.ProviderName || route.ProviderModel != expected.ProviderModel {
		return false
	}
	return route.RequestedModel == expected.Alias || gateway.DiscoveryIDMatchesAlias(route.RequestedModel, expected.Alias)
}

func TestLiveRealMarkerRouteMatchesRequiresFreshMainRoute(t *testing.T) {
	t.Parallel()
	const launchID, boundary = int64(7), int64(50)
	expected := store.Model{Alias: "fixture-sonnet", ProviderName: "fixture-provider", ProviderModel: "fixture-backend"}
	for _, test := range []struct {
		name       string
		change     func(*store.TraceEvent)
		firstParty bool
		want       bool
	}{
		{name: "registered canonical", want: true},
		{name: "registered direct alias", change: func(event *store.TraceEvent) { event.Route.RequestedModel = expected.Alias }, want: true},
		{name: "registered one-million canonical", change: func(event *store.TraceEvent) { event.Route.RequestedModel += "[1m]" }, want: true},
		{name: "pre-boundary validation", change: func(event *store.TraceEvent) { event.ID = boundary - 1 }},
		{name: "boundary validation completes later", change: func(event *store.TraceEvent) { event.ID = boundary; event.CompletedAt = "later" }},
		{name: "different launch", change: func(event *store.TraceEvent) { event.LaunchID++ }},
		{name: "lifecycle event", change: func(event *store.TraceEvent) { event.Kind = "lifecycle" }},
		{name: "count tokens", change: func(event *store.TraceEvent) { event.Name = "count_tokens" }},
		{name: "failed", change: func(event *store.TraceEvent) { event.Status = "failed" }},
		{name: "pending", change: func(event *store.TraceEvent) { event.Status = "started" }},
		{name: "bad HTTP status", change: func(event *store.TraceEvent) { event.Route.HTTPStatus = http.StatusBadGateway }},
		{name: "non-streaming validation", change: func(event *store.TraceEvent) { event.Route.Streaming = false }},
		{name: "classifier operation", change: func(event *store.TraceEvent) { event.Name = "auto_mode_classifier" }},
		{name: "classifier selection source", change: func(event *store.TraceEvent) {
			event.Route.ClassifierSelectionSource = store.ClassifierSelectionSourceActive
		}},
		{name: "wrong alias", change: func(event *store.TraceEvent) { event.Route.ModelAlias = "other" }},
		{name: "wrong provider", change: func(event *store.TraceEvent) { event.Route.ProviderName = "other" }},
		{name: "wrong provider model", change: func(event *store.TraceEvent) { event.Route.ProviderModel = "other" }},
		{name: "wrong requested ID", change: func(event *store.TraceEvent) { event.Route.RequestedModel = gateway.DiscoveryIDForAlias("other") }},
		{name: "requested backend is not alias", change: func(event *store.TraceEvent) { event.Route.RequestedModel = expected.ProviderModel }},
		{name: "malformed requested ID", change: func(event *store.TraceEvent) { event.Route.RequestedModel += "[1m][1m]" }},
		{name: "first-party cannot prove registered", change: func(event *store.TraceEvent) { event.Route.RouteKind = "first-party-anthropic" }},
		{name: "registered cannot prove first-party", firstParty: true},
		{name: "first-party Sonnet", firstParty: true, change: func(event *store.TraceEvent) {
			event.Route.RouteKind = "first-party-anthropic"
			event.Route.RequestedModel = "claude-sonnet-4-6"
		}, want: true},
		{name: "first-party Sonnet alias", firstParty: true, change: func(event *store.TraceEvent) {
			event.Route.RouteKind = "first-party-anthropic"
			event.Route.RequestedModel = "SONNET"
		}, want: true},
		{name: "first-party Haiku cannot prove Sonnet", firstParty: true, change: func(event *store.TraceEvent) {
			event.Route.RouteKind = "first-party-anthropic"
			event.Route.RequestedModel = "claude-haiku-4-5"
		}},
		{name: "first-party classifier", firstParty: true, change: func(event *store.TraceEvent) {
			event.Route.RouteKind = "first-party-anthropic"
			event.Route.RequestedModel = "sonnet"
			event.Route.ClassifierSelectionSource = store.ClassifierSelectionSourceExplicit
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := store.TraceEvent{
				ID: boundary + 1, LaunchID: launchID, Kind: "route", Name: "messages", Status: "succeeded",
				Route: store.RouteEvent{
					RouteKind: "registered", ModelAlias: expected.Alias, ProviderName: expected.ProviderName,
					ProviderModel: expected.ProviderModel, RequestedModel: gateway.DiscoveryIDForAlias(expected.Alias),
					Streaming: true, HTTPStatus: http.StatusOK,
				},
			}
			if test.change != nil {
				test.change(&event)
			}
			model := &expected
			if test.firstParty {
				model = nil
			}
			if got := liveRealMarkerRouteMatches(event, launchID, boundary, model); got != test.want {
				t.Fatalf("route proof matched=%t, want=%t", got, test.want)
			}
		})
	}
}

func TestLiveRealMarkerRouteProofAcceptsExactOrderedEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		failures  int
		streaming bool
		mode      string
	}{
		{"clean streaming", 0, true, "streaming"},
		{"single transport failure then recovery", 1, false, "recovered"},
		{"multiple transport failures then recovery", 2, false, "recovered"},
		{"transport failure then streaming success", 1, true, "streaming"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure, recovery, model := liveRealMarkerProofTestPair()
			events := make([]store.TraceEvent, 0, test.failures+1)
			for index := range test.failures {
				attempt := failure
				attempt.ID += int64(index)
				events = append(events, attempt)
			}
			recovery.ID = failure.ID + int64(test.failures)
			recovery.Route.Streaming = test.streaming
			events = append(events, recovery)
			proof, err := liveRealMarkerProofTestEvents(events, &model)
			if err != nil || proof.Mode != test.mode || proof.TransportFailures != test.failures {
				t.Fatalf("proof mode=%q transport_failures=%d error=%t", proof.Mode, proof.TransportFailures, err != nil)
			}
		})
	}
}

func TestLiveRealMarkerRouteProofRejectsInvalidRecovery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                      string
		change                    func(*store.TraceEvent, *store.TraceEvent)
		omitFailure, omitRecovery bool
		wantError                 bool
	}{
		{name: "standalone nonstream validation", omitFailure: true},
		{name: "failed stream alone", omitRecovery: true},
		{name: "stale failure", change: func(f, _ *store.TraceEvent) { f.ID = 50 }},
		{name: "reversed event IDs", change: func(f, _ *store.TraceEvent) { f.ID = 53 }, wantError: true},
		{name: "wrong failure alias", change: func(f, _ *store.TraceEvent) { f.Route.ModelAlias = "other" }},
		{name: "wrong failure provider", change: func(f, _ *store.TraceEvent) { f.Route.ProviderName = "other" }},
		{name: "wrong failure backend", change: func(f, _ *store.TraceEvent) { f.Route.ProviderModel = "other" }},
		{name: "different valid failure requested ID", change: func(f, _ *store.TraceEvent) { f.Route.RequestedModel = "marker-fixture" }},
		{name: "wrong recovery alias", change: func(_, r *store.TraceEvent) { r.Route.ModelAlias = "other" }},
		{name: "wrong recovery provider", change: func(_, r *store.TraceEvent) { r.Route.ProviderName = "other" }},
		{name: "wrong recovery backend", change: func(_, r *store.TraceEvent) { r.Route.ProviderModel = "other" }},
		{name: "different valid recovery requested ID", change: func(_, r *store.TraceEvent) { r.Route.RequestedModel = "marker-fixture" }},
		{name: "wrong requested ID", change: func(_, r *store.TraceEvent) { r.Route.RequestedModel = "unknown" }},
		{name: "failure from another launch", change: func(f, _ *store.TraceEvent) { f.LaunchID++ }},
		{name: "recovery from another launch", change: func(_, r *store.TraceEvent) { r.LaunchID++ }},
		{name: "firstparty failure for registered recovery", change: func(f, _ *store.TraceEvent) { f.Route.RouteKind = "first-party-anthropic" }},
		{name: "firstparty recovery for registered failure", change: func(_, r *store.TraceEvent) { r.Route.RouteKind = "first-party-anthropic" }},
		{name: "classifier failure", change: func(f, _ *store.TraceEvent) {
			f.Route.ClassifierSelectionSource = store.ClassifierSelectionSourceActive
		}},
		{name: "classifier recovery", change: func(_, r *store.TraceEvent) {
			r.Route.ClassifierSelectionSource = store.ClassifierSelectionSourceExplicit
		}},
		{name: "classifier operation", change: func(f, _ *store.TraceEvent) { f.Name = "auto_mode_classifier" }},
		{name: "nonstream failure", change: func(f, _ *store.TraceEvent) { f.Route.Streaming = false }},
		{name: "failed recovery", change: func(_, r *store.TraceEvent) { r.Status = "failed" }},
		{name: "non2xx recovery", change: func(_, r *store.TraceEvent) { r.Route.HTTPStatus = http.StatusBadGateway }},
		{name: "pending recovery", change: func(_, r *store.TraceEvent) { r.Status = "started" }},
		{name: "recovery with error class", change: func(_, r *store.TraceEvent) { r.Route.ErrorClass = "provider_protocol" }},
		{name: "stream success with error class", change: func(_, r *store.TraceEvent) { r.Route.Streaming = true; r.Route.ErrorClass = "provider_protocol" }},
		{name: "protocol failure", change: func(f, _ *store.TraceEvent) { f.Route.ErrorClass = "provider_protocol" }, wantError: true},
		{name: "translation failure", change: func(f, _ *store.TraceEvent) { f.Route.ErrorClass = "ccr_translation" }, wantError: true},
		{name: "unclassified failure", change: func(f, _ *store.TraceEvent) { f.Route.ErrorClass = "" }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure, recovery, model := liveRealMarkerProofTestPair()
			if test.change != nil {
				test.change(&failure, &recovery)
			}
			var events []store.TraceEvent
			if !test.omitFailure {
				events = append(events, failure)
			}
			if !test.omitRecovery {
				events = append(events, recovery)
			}
			proof, err := liveRealMarkerProofTestEvents(events, &model)
			if proof.Mode != "" || (err != nil) != test.wantError {
				t.Fatalf("invalid recovery accepted or misclassified: mode=%q error=%t", proof.Mode, err != nil)
			}
		})
	}
}

func TestLiveRealMarkerRouteProofRequiresMatchingSonnetRecovery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, failureID, recoveryID string
		changeRecovery              func(*store.RouteEvent)
		want                        bool
	}{
		{name: "Sonnet", failureID: "claude-sonnet-4-6", recoveryID: "claude-sonnet-4-6", want: true},
		{name: "Sonnet alias", failureID: "sonnet", recoveryID: "sonnet", want: true},
		{name: "different Sonnet IDs", failureID: "sonnet", recoveryID: "claude-sonnet-4-6"},
		{name: "Haiku", failureID: "claude-haiku-4-5", recoveryID: "claude-haiku-4-5"},
		{name: "Haiku failure Sonnet recovery", failureID: "claude-haiku-4-5", recoveryID: "sonnet"},
		{name: "Sonnet failure Haiku recovery", failureID: "sonnet", recoveryID: "claude-haiku-4-5"},
		{name: "registered recovery", failureID: "sonnet", recoveryID: "sonnet", changeRecovery: func(r *store.RouteEvent) { r.RouteKind = "registered" }},
		{name: "different alias", failureID: "sonnet", recoveryID: "sonnet", changeRecovery: func(r *store.RouteEvent) { r.ModelAlias = "other" }},
		{name: "different provider", failureID: "sonnet", recoveryID: "sonnet", changeRecovery: func(r *store.RouteEvent) { r.ProviderName = "other" }},
		{name: "different backend", failureID: "sonnet", recoveryID: "sonnet", changeRecovery: func(r *store.RouteEvent) { r.ProviderModel = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure, recovery, _ := liveRealMarkerProofTestPair()
			failure.Route.RouteKind, recovery.Route.RouteKind = "first-party-anthropic", "first-party-anthropic"
			failure.Route.ModelAlias, recovery.Route.ModelAlias = "", ""
			failure.Route.ProviderName, recovery.Route.ProviderName = "", ""
			failure.Route.ProviderModel, recovery.Route.ProviderModel = "", ""
			failure.Route.RequestedModel, recovery.Route.RequestedModel = test.failureID, test.recoveryID
			if test.changeRecovery != nil {
				test.changeRecovery(&recovery.Route)
			}
			proof, err := liveRealMarkerProofTestEvents([]store.TraceEvent{failure, recovery}, nil)
			if err != nil || (proof.Mode == "recovered") != test.want {
				t.Fatalf("firstparty recovery matched=%t error=%t", proof.Mode == "recovered", err != nil)
			}
		})
	}
}

func liveRealMarkerProofTestPair() (store.TraceEvent, store.TraceEvent, store.Model) {
	model := store.Model{Alias: "marker-fixture", ProviderName: "marker-provider", ProviderModel: "marker-backend"}
	failure := store.TraceEvent{ID: 51, LaunchID: 7, Kind: "route", Name: "messages", Status: "failed", Route: store.RouteEvent{
		RouteKind: "registered", RequestedModel: gateway.DiscoveryIDForAlias(model.Alias), ModelAlias: model.Alias,
		ProviderName: model.ProviderName, ProviderModel: model.ProviderModel, Streaming: true,
		HTTPStatus: http.StatusBadGateway, ErrorClass: "provider_transport",
	}}
	recovery := failure
	recovery.ID, recovery.Status = 52, "succeeded"
	recovery.Route.Streaming, recovery.Route.HTTPStatus, recovery.Route.ErrorClass = false, http.StatusOK, ""
	return failure, recovery, model
}

func TestLiveRealMarkerRouteProofRejectsMixedStreamFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, status, errorClass   string
		streamingRecovery          bool
		alternateRequestedID       bool
		afterRecovery, omitFailure bool
		wantError                  bool
	}{
		{name: "protocol failure", status: "failed", errorClass: "provider_protocol", wantError: true},
		{name: "canceled stream before recovery", status: "canceled", errorClass: "canceled", wantError: true},
		{name: "canceled stream after clean stream", status: "canceled", errorClass: "canceled", streamingRecovery: true, afterRecovery: true, omitFailure: true, wantError: true},
		{name: "unknown stream status", status: "unknown", wantError: true},
		{name: "uncommitted failure before nonstream", status: "started"},
		{name: "uncommitted failure before stream", status: "started", streamingRecovery: true},
		{name: "uncommitted alternate selected ID", status: "started", alternateRequestedID: true},
		{name: "protocol failure after recovery", status: "failed", errorClass: "provider_protocol", afterRecovery: true, wantError: true},
		{name: "protocol failure after clean stream", status: "failed", errorClass: "provider_protocol", streamingRecovery: true, afterRecovery: true, omitFailure: true, wantError: true},
		{name: "pending stream after recovery", status: "started", afterRecovery: true},
		{name: "pending stream after clean stream", status: "started", streamingRecovery: true, afterRecovery: true, omitFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure, recovery, model := liveRealMarkerProofTestPair()
			middle := failure
			middle.ID, middle.Status, middle.Route.ErrorClass = 52, test.status, test.errorClass
			if test.alternateRequestedID {
				middle.Route.RequestedModel = model.Alias
			}
			recovery.ID, recovery.Route.Streaming = 53, test.streamingRecovery
			events := []store.TraceEvent{failure, middle, recovery}
			if test.afterRecovery {
				recovery.ID, middle.ID = 52, 53
				events = []store.TraceEvent{failure, recovery, middle}
			}
			if test.omitFailure {
				events = events[1:]
			}
			proof, err := liveRealMarkerProofTestEvents(events, &model)
			if (err != nil) != test.wantError || proof.Mode != "" {
				t.Fatal("transport retry evidence masked an unresolved or nontransport main stream failure")
			}
		})
	}
}

func liveRealMarkerProofTestEvents(events []store.TraceEvent, expected *store.Model) (liveRealMarkerRouteProof, error) {
	scan := liveRealMarkerProofScan{transportFailures: make(map[liveRealMarkerRouteIdentity]int)}
	for _, event := range events {
		if _, err := scan.observe(event, 7, 50, expected, true); err != nil {
			return liveRealMarkerRouteProof{}, err
		}
	}
	return scan.proof(), nil
}

func TestLiveRealMarkerRouteProofPrefersStreamingAcrossWindow(t *testing.T) {
	t.Parallel()
	for _, streamFirst := range []bool{false, true} {
		failure, recovery, model := liveRealMarkerProofTestPair()
		stream := recovery
		stream.ID, stream.Route.Streaming = 53, true
		events := []store.TraceEvent{failure, recovery, stream}
		if streamFirst {
			stream.ID, failure.ID, recovery.ID = 51, 52, 53
			events = []store.TraceEvent{stream, failure, recovery}
		}
		proof, err := liveRealMarkerProofTestEvents(events, &model)
		if err != nil || proof.Mode != "streaming" || proof.TransportFailures != 1 {
			t.Fatal("whole-window proof lost streaming preference or transport failure count")
		}
	}
}

func TestLiveRealMarkerRouteWaitsForSQLiteCompletion(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	oldID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "validation")
	if err := s.CompleteRouteEvent(t.Context(), oldID, "succeeded", http.StatusOK, "", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete synthetic pre-marker route")
	}
	boundary := liveRealRouteBoundary(t, t.Context(), s, launchID)
	eventID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "marker")
	if found, err := scanLiveRealMarkerRoutes(t.Context(), s, launchID, boundary, &model); err != nil || found {
		t.Fatal("pending marker or pre-boundary validation became successful route proof")
	}
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	writerDone := make(chan struct{})
	completed := make(chan error, 1)
	// This test owns the only writer goroutine. Cancellation interrupts its timer
	// or SQLite work, and cleanup always waits before the store can be closed.
	go func() {
		defer close(writerDone)
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-writerCtx.Done():
			completed <- writerCtx.Err()
		case <-timer.C:
			completed <- s.CompleteRouteEvent(writerCtx, eventID, "succeeded", http.StatusOK, "", time.Millisecond, store.TokenUsage{}, nil)
		}
	}()
	t.Cleanup(func() { cancelWriter(); <-writerDone })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitLiveRealMarkerRoute(ctx, s, launchID, boundary, &model); err != nil {
		t.Fatal("did not observe delayed SQLite marker completion")
	}
	if err := <-completed; err != nil {
		t.Fatal("delayed synthetic SQLite completion failed")
	}
	events, err := s.ListTraceEvents(t.Context(), store.TraceFilter{LaunchID: launchID, AfterID: boundary, Kind: "route", Limit: 10})
	if err != nil || len(events) != 1 || events[0].ID != eventID {
		t.Fatal("readiness proof did not retain the original post-boundary event")
	}
}

func TestLiveRealMarkerRouteReadinessFailsSafelyOnClosedStore(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal("close synthetic trace store")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := waitLiveRealMarkerRoute(ctx, s, launchID, 0, &model)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatal("closed-store failure was retried, ignored or changed into a deadline")
	}
	if strings.Contains(err.Error(), "sql:") || strings.Contains(err.Error(), "fixture-traces.db") {
		t.Fatal("trace readiness error exposed underlying store details")
	}
}

func TestLiveRealMarkerRouteReadinessHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := waitLiveRealMarkerRoute(ctx, s, launchID, 0, &model); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("missing marker evidence bypassed the existing caller deadline")
	}
}

func TestLiveRealMarkerRouteProofWaitsForDeferredFailureCommit(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	failureID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "failed-stream")
	recoveryID := beginLiveRealMarkerProofTestEvent(t, s, launchID, model, "nonstream-retry", false)
	if err := s.CompleteRouteEvent(t.Context(), recoveryID, "succeeded", http.StatusOK, "", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete synthetic nonstream retry")
	}
	if proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, 0, &model, true); err != nil || proof.Mode != "" {
		t.Fatal("nonstream retry became proof before its failed stream committed")
	}
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	writerDone := make(chan struct{})
	completed := make(chan error, 1)
	// This test owns and waits for its delayed writer, including failed assertions.
	go func() {
		defer close(writerDone)
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-writerCtx.Done():
			completed <- writerCtx.Err()
		case <-timer.C:
			completed <- s.CompleteRouteEvent(writerCtx, failureID, "failed", http.StatusBadGateway, "provider_transport", time.Millisecond, store.TokenUsage{}, nil)
		}
	}()
	t.Cleanup(func() { cancelWriter(); <-writerDone })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	proof, err := waitLiveRealMarkerRouteProof(ctx, s, launchID, 0, &model)
	if err != nil || proof.Mode != "recovered" || proof.TransportFailures != 1 {
		t.Fatal("deferred stream-failure commit did not establish ordered recovery proof")
	}
	if err := <-completed; err != nil {
		t.Fatal("complete delayed synthetic stream failure")
	}
	if found, err := scanLiveRealMarkerRoutes(t.Context(), s, launchID, 0, &model); err != nil || found {
		t.Fatal("strict streaming fixture accepted nonstream recovery")
	}
}

func TestLiveRealMarkerRouteProofCannotMaskDeferredProtocolFailure(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	transportID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "transport-failure")
	if err := s.CompleteRouteEvent(t.Context(), transportID, "failed", http.StatusBadGateway, "provider_transport", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete initial synthetic transport failure")
	}
	pendingID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "pending-protocol-failure")
	recoveryID := beginLiveRealMarkerProofTestEvent(t, s, launchID, model, "nonstream-retry", false)
	if err := s.CompleteRouteEvent(t.Context(), recoveryID, "succeeded", http.StatusOK, "", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete synthetic nonstream retry before deferred failure")
	}
	if proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, 0, &model, true); err != nil || proof.Mode != "" {
		t.Fatal("earlier transport failure masked an unresolved main stream")
	}
	if err := s.CompleteRouteEvent(t.Context(), pendingID, "failed", http.StatusBadGateway, "provider_protocol", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete deferred synthetic protocol failure")
	}
	if proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, 0, &model, true); err == nil || proof.Mode != "" {
		t.Fatal("nonstream success masked a deferred nontransport main stream failure")
	}
}

func TestLiveRealMarkerRouteProofRetainsFailureAcrossPages(t *testing.T) {
	t.Parallel()
	s, launchID, model := liveRealMarkerRouteTestStore(t)
	other := store.Model{Alias: "other-fixture", ProviderName: "other-provider", ProviderModel: "other-backend"}
	var firstUnrelatedID int64
	for index := range 999 {
		eventID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, other, fmt.Sprintf("uncompleted-route-%d", index))
		if index == 0 {
			firstUnrelatedID = eventID
		}
	}
	failureID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "last-first-page")
	if err := s.CompleteRouteEvent(t.Context(), failureID, "failed", http.StatusBadGateway, "provider_transport", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete last first-page stream failure")
	}
	recoveryID := beginLiveRealMarkerProofTestEvent(t, s, launchID, model, "first-second-page", false)
	if err := s.CompleteRouteEvent(t.Context(), recoveryID, "succeeded", http.StatusOK, "", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete second-page nonstream recovery")
	}
	proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, 0, &model, true)
	if err != nil || proof.Mode != "recovered" || proof.TransportFailures != 1 {
		t.Fatal("pagination lost matching stream-failure evidence")
	}
	lateID := beginLiveRealMarkerRouteTestEvent(t, s, launchID, model, "later-stream")
	// Excluding the first unrelated event puts the positive recovery at the end
	// of page one and this later stream on page two of the same immutable window.
	if proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, firstUnrelatedID, &model, true); err != nil || proof.Mode != "" {
		t.Fatal("positive recovery hid an unresolved stream on the following page")
	}
	if err := s.CompleteRouteEvent(t.Context(), lateID, "failed", http.StatusBadGateway, "provider_protocol", time.Millisecond, store.TokenUsage{}, nil); err != nil {
		t.Fatal("complete later synthetic protocol failure")
	}
	if proof, err := scanLiveRealMarkerRouteEvidence(t.Context(), s, launchID, firstUnrelatedID, &model, true); err == nil || proof.Mode != "" {
		t.Fatal("positive recovery hid a protocol failure on the following page")
	}
}

func liveRealMarkerRouteTestStore(t *testing.T) (*store.Store, int64, store.Model) {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fixture-traces.db"))
	if err != nil {
		t.Fatal("open synthetic marker trace store")
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal("migrate synthetic marker trace store")
	}
	model := store.Model{Alias: "marker-fixture", ProviderName: "marker-provider", ProviderModel: "marker-backend"}
	launchID, err := s.CreateLaunch(t.Context(), model.Alias, "disabled", "disabled")
	if err != nil {
		t.Fatal("create synthetic marker launch")
	}
	return s, launchID, model
}

func beginLiveRealMarkerRouteTestEvent(t *testing.T, s *store.Store, launchID int64, model store.Model, requestID string) int64 {
	t.Helper()
	return beginLiveRealMarkerProofTestEvent(t, s, launchID, model, requestID, true)
}

func beginLiveRealMarkerProofTestEvent(t *testing.T, s *store.Store, launchID int64, model store.Model, requestID string, streaming bool) int64 {
	t.Helper()
	eventID, err := s.BeginRouteEvent(t.Context(), store.RouteEvent{
		LaunchID: launchID, RequestID: requestID, Operation: "messages", RequestedModel: gateway.DiscoveryIDForAlias(model.Alias), Streaming: streaming,
	})
	if err != nil {
		t.Fatal("begin synthetic marker route")
	}
	if err := s.ResolveRouteEvent(t.Context(), eventID, 0, store.RouteEvent{
		RouteKind: "registered", ModelAlias: model.Alias, ProviderName: model.ProviderName, ProviderModel: model.ProviderModel, Protocol: "openai-compatible",
	}); err != nil {
		t.Fatal("resolve synthetic marker route")
	}
	return eventID
}
