package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestProviderReceiptAdmitsOnlyBoundedMetadata(t *testing.T) {
	id := strings.Repeat("a", 64)
	headers := http.Header{
		"X-Litellm-Model-Id":            {id},
		"X-Litellm-Call-Id":             {"secret-bearing-url"},
		"X-Litellm-Attempted-Retries":   {"2"},
		"X-Litellm-Attempted-Fallbacks": {"-1"},
		"Authorization":                 {"secret"},
	}
	receipt := receiptFromHeaders(headers)
	if got := receipt.reason(); got != "; provider_model_id="+id+"; provider_retries=2" {
		t.Fatalf("unexpected receipt: %q", got)
	}
	for _, value := range []string{"", "secret", strings.Repeat("x", 64), "https://example.com", strings.Repeat("a", 65)} {
		if safeReceiptID(value) != "" {
			t.Fatal("unsafe ID admitted")
		}
	}
	for _, value := range []string{"-1", "1000001", "NaN", "secret"} {
		if safeReceiptCount(value) != "" {
			t.Fatal("unsafe count admitted")
		}
	}
}

func TestProviderReceiptDoesNotHideMissingOrDifferentAttempts(t *testing.T) {
	var receipt providerReceipt
	receipt.merge(providerReceipt{seen: true, modelID: strings.Repeat("a", 64), retries: "0"})
	receipt.merge(providerReceipt{seen: true})
	if receipt.modelID != "mixed" || receipt.retries != "mixed" {
		t.Fatal("missing later metadata hidden")
	}
	receipt.merge(providerReceipt{seen: true, modelID: strings.Repeat("a", 64)})
	if receipt.modelID != "mixed" {
		t.Fatal("ambiguity erased")
	}
}
