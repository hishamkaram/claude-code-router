package providers

import "testing"

func TestNormalizeCapabilitiesOverlaysResponsesOnDefaults(t *testing.T) {
	t.Parallel()

	caps := NormalizeCapabilities("litellm", Capabilities{SupportsResponses: true})
	if caps.Protocol != ProtocolOpenAICompatible || caps.Mode != ModeDegraded ||
		!caps.SupportsTools || !caps.SupportsStreaming || !caps.SupportsThinking ||
		!caps.SupportsModelDiscovery || !caps.SupportsCountTokens || !caps.SupportsResponses {
		t.Fatalf("NormalizeCapabilities() = %#v, want litellm defaults plus responses", caps)
	}
}
