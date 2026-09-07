package gateway

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// providerReceipt is optional diagnostic evidence, never routing policy. Only
// documented LiteLLM opaque IDs and nonnegative counters are admitted; bodies,
// URLs, model names and arbitrary response headers are deliberately excluded.
type providerReceipt struct {
	seen      bool
	callID    string
	modelID   string
	retries   string
	fallbacks string
}

func receiptFromHeaders(headers http.Header) providerReceipt {
	return providerReceipt{
		seen:      true,
		callID:    safeReceiptID(headers.Get("X-Litellm-Call-Id")),
		modelID:   safeReceiptID(headers.Get("X-Litellm-Model-Id")),
		retries:   safeReceiptCount(headers.Get("X-Litellm-Attempted-Retries")),
		fallbacks: safeReceiptCount(headers.Get("X-Litellm-Attempted-Fallbacks")),
	}
}

func safeReceiptID(value string) string {
	if len(value) != 32 && len(value) != 36 && len(value) != 64 {
		return ""
	}
	normalized := strings.ReplaceAll(value, "-", "")
	if len(normalized) != 32 && len(normalized) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return ""
	}
	return value
}

func safeReceiptCount(value string) string {
	if value == "" || len(value) > 7 {
		return ""
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 0 || number > 1000000 {
		return ""
	}
	return strconv.Itoa(number)
}

func mergeReceiptValue(previous, current string) string {
	if previous == current {
		return current
	}
	return "mixed"
}

func (r *providerReceipt) merge(other providerReceipt) {
	if !r.seen {
		*r = other
		return
	}
	r.callID = mergeReceiptValue(r.callID, other.callID)
	r.modelID = mergeReceiptValue(r.modelID, other.modelID)
	r.retries = mergeReceiptValue(r.retries, other.retries)
	r.fallbacks = mergeReceiptValue(r.fallbacks, other.fallbacks)
}

func (r providerReceipt) reason() string {
	result := ""
	for _, field := range []struct {
		name, value string
	}{
		{"provider_call_id", r.callID},
		{"provider_model_id", r.modelID},
		{"provider_retries", r.retries},
		{"provider_fallbacks", r.fallbacks},
	} {
		if field.value != "" {
			result += "; " + field.name + "=" + field.value
		}
	}
	return result
}
