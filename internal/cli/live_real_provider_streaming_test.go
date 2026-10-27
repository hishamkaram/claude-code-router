//go:build live

package cli

import (
	"slices"
	"testing"
)

type liveRealProviderMarkerProof struct {
	label    string
	provider liveRealProviderKey
	route    liveRealMarkerRouteProof
}

// Configured names and the native first-party route are separate identities,
// even when a configured provider is named first-party-anthropic.
type liveRealProviderKey struct {
	firstParty bool
	name       string
}

func (key liveRealProviderKey) String() string {
	if key.firstParty {
		return "first-party Anthropic"
	}
	return "configured " + key.name
}

// The matrix owns the immutable marker receipts. Provider coverage is derived
// from them so recovered turns cannot silently replace all streaming evidence.
func liveRealMissingStreamingProviders(providers []liveRealProviderKey, proofs []liveRealProviderMarkerProof) []liveRealProviderKey {
	streamed := make(map[liveRealProviderKey]bool, len(providers))
	for _, proof := range proofs {
		if proof.route.Mode == "streaming" {
			streamed[proof.provider] = true
		}
	}
	var missing []liveRealProviderKey
	for _, provider := range providers {
		if !streamed[provider] && !slices.Contains(missing, provider) {
			missing = append(missing, provider)
		}
	}
	return missing
}

func liveRealRecoveredMarkers(proofs []liveRealProviderMarkerProof) []string {
	var recovered []string
	for _, proof := range proofs {
		if proof.route.Mode == "recovered" {
			recovered = append(recovered, proof.label)
		}
	}
	return recovered
}

func TestLiveRealProviderCoverageRequiresCleanStreamPerProvider(t *testing.T) {
	t.Parallel()
	proofs := []liveRealProviderMarkerProof{
		{label: "first-party marker", provider: liveRealProviderKey{firstParty: true}, route: liveRealMarkerRouteProof{Mode: "streaming"}},
		{label: "provider-a marker", provider: liveRealProviderKey{name: "provider-a"}, route: liveRealMarkerRouteProof{Mode: "streaming"}},
		{label: "provider-b recovered", provider: liveRealProviderKey{name: "provider-b"}, route: liveRealMarkerRouteProof{Mode: "recovered", TransportFailures: 1}},
	}
	providers := []liveRealProviderKey{{firstParty: true}, {name: "provider-a"}, {name: "provider-b"}, {name: "provider-b"}}
	if missing := liveRealMissingStreamingProviders(providers, proofs); !slices.Equal(missing, []liveRealProviderKey{{name: "provider-b"}}) {
		t.Fatalf("missing streaming providers=%v, want provider-b", missing)
	}
	proofs = append(proofs, liveRealProviderMarkerProof{
		label: "provider-b stream after transport retry", provider: liveRealProviderKey{name: "provider-b"}, route: liveRealMarkerRouteProof{Mode: "streaming", TransportFailures: 1},
	})
	if missing := liveRealMissingStreamingProviders(providers, proofs); len(missing) != 0 {
		t.Fatalf("provider clean stream evidence was lost: %v", missing)
	}
	if recovered := liveRealRecoveredMarkers(proofs); !slices.Equal(recovered, []string{"provider-b recovered"}) {
		t.Fatal("recovered marker was hidden or counted as streaming")
	}
}

func TestLiveRealProviderCoverageSeparatesConfiguredAndFirstParty(t *testing.T) {
	t.Parallel()
	configured := liveRealProviderKey{name: "first-party-anthropic"}
	firstParty := liveRealProviderKey{firstParty: true}
	proofs := []liveRealProviderMarkerProof{
		{label: "native", provider: firstParty, route: liveRealMarkerRouteProof{Mode: "streaming"}},
		{label: "configured recovered", provider: configured, route: liveRealMarkerRouteProof{Mode: "recovered", TransportFailures: 1}},
	}
	if missing := liveRealMissingStreamingProviders([]liveRealProviderKey{firstParty, configured}, proofs); !slices.Equal(missing, []liveRealProviderKey{configured}) {
		t.Fatal("configured provider inherited native first-party streaming evidence")
	}
}
