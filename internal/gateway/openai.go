package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const providerErrorDetailLimit = 4096

func validateThinking(raw json.RawMessage) error {
	thinkingType, err := openAIThinkingType(raw)
	if err != nil {
		return err
	}
	switch thinkingType {
	case "", "adaptive", "disabled", "enabled":
		return nil
	default:
		return fmt.Errorf("thinking mode %q is not supported by the OpenAI-compatible gateway path", thinkingType)
	}
}

func (h *handler) callOpenAICompatible(ctx context.Context, provider store.Provider, apiKey string, payload openAIChatRequest) (openAIChatResponse, error) {
	endpoint, err := providers.ChatCompletionsEndpoint(provider.BaseURL)
	if err != nil {
		return openAIChatResponse{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("encoding OpenAI-compatible request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("creating OpenAI-compatible request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		return openAIChatResponse{}, fmt.Errorf("requesting OpenAI-compatible provider %q: %w", provider.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, providerErrorDetailReadLimit(apiKey)))
		return openAIChatResponse{}, newOpenAIProviderStatusError(provider.Name, resp.StatusCode, sanitizeProviderErrorDetail(raw, apiKey))
	}
	var decoded openAIChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return openAIChatResponse{}, fmt.Errorf("decoding OpenAI-compatible provider response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return openAIChatResponse{}, fmt.Errorf("OpenAI-compatible provider %q returned no choices", provider.Name)
	}
	return decoded, nil
}

type openAIProviderStatusError struct {
	provider   string
	statusCode int
	detail     string
}

func newOpenAIProviderStatusError(provider string, statusCode int, detail string) *openAIProviderStatusError {
	return &openAIProviderStatusError{provider: provider, statusCode: statusCode, detail: detail}
}

func (e *openAIProviderStatusError) Error() string {
	statusText := http.StatusText(e.statusCode)
	if statusText == "" {
		statusText = "upstream error"
	}
	message := fmt.Sprintf("OpenAI-compatible provider %q returned HTTP %d %s", e.provider, e.statusCode, statusText)
	if e.detail != "" {
		message += ": " + e.detail
	}
	return message
}

func (e *openAIProviderStatusError) SafeStatusCode() int {
	if e.statusCode >= http.StatusBadRequest && e.statusCode <= 599 {
		return e.statusCode
	}
	return http.StatusBadGateway
}

func providerErrorDetailReadLimit(secrets ...string) int64 {
	extra := 1
	for _, value := range secrets {
		if value = strings.TrimSpace(value); len(value) > extra {
			extra = len(value)
		}
	}
	return int64(providerErrorDetailLimit + extra + 1)
}

func sanitizeProviderErrorDetail(raw []byte, secrets ...string) string {
	truncated := len(raw) > providerErrorDetailLimit
	detail := strings.ToValidUTF8(string(raw), "?")
	detail = strings.Join(strings.Fields(detail), " ")
	for _, value := range secrets {
		if value = strings.TrimSpace(value); value != "" {
			detail = strings.ReplaceAll(detail, value, "[redacted]")
		}
	}
	detail = redactCommonSecretPatterns(detail)
	if len(detail) > providerErrorDetailLimit {
		detail = detail[:providerErrorDetailLimit]
		truncated = true
	}
	if truncated && detail != "" {
		detail += "..."
	}
	return detail
}

func redactCommonSecretPatterns(detail string) string {
	bearer := regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]+`)
	detail = bearer.ReplaceAllString(detail, "${1}[redacted]")
	keyValue := regexp.MustCompile(`(?i)((?:authorization|x-api-key|api[_-]?key|access[_-]?token|secret|token)["']?\s*[:=]\s*["']?)[^"',}\s]+`)
	return keyValue.ReplaceAllString(detail, "${1}[redacted]")
}

func anthropicStopReasonFromOpenAI(resp openAIChatResponse) (string, error) {
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("OpenAI-compatible provider returned no choices")
	}
	switch finishReason := resp.Choices[0].FinishReason; finishReason {
	case "", "stop":
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	case "tool_calls":
		return "tool_use", nil
	case "function_call":
		if resp.Choices[0].Message.FunctionCall == nil {
			return "", fmt.Errorf("OpenAI-compatible provider returned function_call finish_reason without function_call")
		}
		return "tool_use", nil
	default:
		return "", fmt.Errorf("OpenAI-compatible provider returned unsupported finish_reason %q", finishReason)
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
