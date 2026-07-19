package conformance

import (
	"strings"
	"testing"
)

func TestClassifyCheckFailureReportsCredentialStatusesWithoutBody(t *testing.T) {
	t.Parallel()
	const secret = "provider-body-secret"
	body := []byte(`{"error":{"message":"provider request failed with HTTP 401 Unauthorized: ` + secret + `"}}`)
	providerStatus := providerHTTPStatusFromGatewayError(body)
	kind, gatewayStatus, gotProviderStatus, evidence := classifyCheckFailure(
		"text", &probeHTTPStatusError{status: 502, providerStatus: providerStatus},
	)
	if kind != "credential" || gatewayStatus != 502 || gotProviderStatus != 401 {
		t.Fatalf("classification = kind=%q gateway=%d provider=%d", kind, gatewayStatus, gotProviderStatus)
	}
	if strings.Contains(evidence, secret) || !strings.Contains(evidence, "provider HTTP 401") {
		t.Fatalf("evidence = %q", evidence)
	}
}

func TestBoundedCheckEvidenceNormalizesAndBoundsErrors(t *testing.T) {
	t.Parallel()
	evidence := boundedCheckEvidence(strings.Repeat("word \n", 100))
	if len(evidence) > maxCheckEvidenceLength || strings.Contains(evidence, "\n") {
		t.Fatalf("bounded evidence length=%d value=%q", len(evidence), evidence)
	}
}
