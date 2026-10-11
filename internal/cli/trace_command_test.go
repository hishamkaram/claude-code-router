package cli

import (
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestTraceRouteViewIncludesClassifierDecisionEvidence(t *testing.T) {
	t.Parallel()
	view := newTraceRouteView(store.RouteEvent{
		ClassifierSelectionSource: store.ClassifierSelectionSourceActive,
		ClassifierActiveAlias:     "fixture",
	})
	if view.ClassifierSelectionSource != store.ClassifierSelectionSourceActive ||
		view.ClassifierActiveAlias != "fixture" {
		t.Fatalf("newTraceRouteView() = %#v", view)
	}
}
