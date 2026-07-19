package conformance

import (
	"context"
	"errors"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
)

const maxCheckEvidenceLength = 240

func classifyCheckFailure(check string, err error) (failureKind string, gatewayStatus, providerStatus int, evidence string) {
	if errors.Is(err, context.Canceled) {
		return "canceled", 0, 0, check + " check was canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", 0, 0, check + " check timed out"
	}
	var httpErr *probeHTTPStatusError
	if errors.As(err, &httpErr) {
		evidence := check + " check received HTTP " + httpStatusNumber(httpErr.status) + " from the gateway"
		if httpErr.providerStatus != 0 {
			evidence += " after provider HTTP " + httpStatusNumber(httpErr.providerStatus)
		}
		kind := "http_status"
		if httpErr.providerStatus == 401 || httpErr.providerStatus == 403 {
			kind = "credential"
		}
		return kind, httpErr.status, httpErr.providerStatus, evidence
	}
	var discoveryErr *providers.DiscoveryHTTPError
	if errors.As(err, &discoveryErr) {
		if discoveryErr.Authentication {
			return "credential", 0, discoveryErr.StatusCode, "provider rejected model discovery credentials"
		}
		return "provider_http_status", 0, discoveryErr.StatusCode,
			"provider model discovery returned HTTP " + httpStatusNumber(discoveryErr.StatusCode)
	}
	message := boundedCheckEvidence(err.Error())
	switch {
	case strings.Contains(message, "provider model is absent"):
		return "model_absent", 0, 0, "provider model is absent or non-routable in discovery"
	case strings.Contains(message, "configured alias is absent"):
		return "alias_absent", 0, 0, "configured alias is absent from gateway discovery"
	case strings.Contains(message, "credential"):
		return "credential", 0, 0, message
	case strings.Contains(message, "invalid"), strings.Contains(message, "omitted"):
		return "invalid_response", 0, 0, message
	case strings.Contains(message, "connection refused"), strings.Contains(message, "no such host"), strings.Contains(message, "requesting"):
		return "network", 0, 0, message
	default:
		return "check_failed", 0, 0, message
	}
}

func boundedCheckEvidence(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "check failed without provider response content"
	}
	if len(value) > maxCheckEvidenceLength {
		value = value[:maxCheckEvidenceLength]
	}
	return value
}

func httpStatusNumber(status int) string {
	if status == 0 {
		return "0"
	}
	digits := [3]byte{byte('0' + status/100%10), byte('0' + status/10%10), byte('0' + status%10)}
	return string(digits[:])
}
