package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestHumanTraceShowsUpstreamTransportDetails(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"upstream_transport_policy", "owner=ccr; http2_ping_after_ms=20000; http2_ping_timeout_ms=15000; max_idle_pool_ms=15000"},
		{"upstream_transport", "owner=ccr; protocol=h2; idle_ping=enabled"},
		{"upstream_transport", "owner=ccr; protocol=http/1.1; idle_ping=unavailable"},
		{"upstream_transport", "owner=caller; protocol=unknown; idle_ping=unverified"},
		{"upstream_transport", "owner=ccr\nnot another event"},
	} {
		var output bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&output)
		writeHumanTraceEvent(cmd, traceEventView{Lifecycle: &traceLifecycleView{
			Name: tc.name, ExternalID: "request-id", Reason: tc.reason,
		}})
		if !strings.Contains(output.String(), fmt.Sprintf("reason=%q", tc.reason)) || !strings.Contains(output.String(), "external=request-id") {
			t.Fatalf("transport details or correlation missing: %s", output.String())
		}
		if strings.Count(output.String(), "\n") != 1 {
			t.Fatal("reason injected another trace line")
		}
	}
}
