package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/providers"
)

const conformanceAnthropicVersion = "2023-06-01"

type checkRunner struct {
	config     Config
	target     target
	gatewayURL string
	token      string
	client     *http.Client
}

type probeResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r checkRunner) run(ctx context.Context) []Check {
	checks := make([]Check, 0, 8)
	checks = append(
		checks,
		r.execute(ctx, "discovery", r.checkDiscovery),
		r.execute(ctx, "text", r.checkText),
	)
	if r.config.SmokeOnly {
		return checks
	}
	streaming := modelCapabilityEnabled(r.target.capabilities.SupportsStreaming, r.target.modelCapabilities.SupportsStreaming)
	checks = append(checks, r.capabilityCheck(ctx, "stream", streaming, r.checkStream))
	tools := modelCapabilityEnabled(r.target.capabilities.SupportsTools, r.target.modelCapabilities.SupportsTools) && r.target.model.Status != "chat-only"
	forcedTools := tools && modelCapabilityEnabled(true, r.target.modelCapabilities.SupportsToolChoice)
	thinking := modelCapabilityEnabled(r.target.capabilities.SupportsThinking, r.target.modelCapabilities.SupportsThinking)
	if r.target.modelCapabilities.MaxOutputTokens != nil && *r.target.modelCapabilities.MaxOutputTokens <= 1024 {
		thinking = false
	}
	checks = append(
		checks,
		r.capabilityCheck(ctx, "forced_tool", forcedTools, r.checkForcedTool),
		r.capabilityCheck(ctx, "thinking", thinking, r.checkThinking),
		r.capabilityCheck(ctx, "count_tokens", r.target.capabilities.SupportsCountTokens, r.checkCountTokens),
		r.execute(ctx, "cancellation", r.checkCancellation),
		r.execute(ctx, "sanitized_error", r.checkSanitizedError),
	)
	return checks
}

func (r checkRunner) execute(ctx context.Context, name string, check func(context.Context) (string, error)) Check {
	started := time.Now()
	evidence, err := check(ctx)
	result := Check{Name: name, Status: StatusPassed, Latency: time.Since(started), Evidence: evidence}
	if err != nil {
		result.Status = StatusFailed
		result.FailureKind, result.HTTPStatus, result.ProviderHTTPStatus, result.Evidence = classifyCheckFailure(name, err)
	}
	return result
}

func modelCapabilityEnabled(providerEnabled bool, modelValue *bool) bool {
	return providerEnabled && (modelValue == nil || *modelValue)
}

func (r checkRunner) capabilityCheck(ctx context.Context, name string, enabled bool, check func(context.Context) (string, error)) Check {
	if !enabled {
		return Check{Name: name, Status: StatusNotApplicable, Evidence: "capability is not declared"}
	}
	return r.execute(ctx, name, check)
}

func (r checkRunner) checkDiscovery(ctx context.Context) (string, error) {
	response, requestErr := r.request(ctx, http.MethodGet, "/v1/models", nil)
	if requestErr != nil {
		return "", requestErr
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return "", fmt.Errorf("invalid gateway discovery response")
	}
	want, err := gateway.DiscoveryIDForModel(r.target.model)
	if err != nil {
		return "", err
	}
	found := false
	for _, item := range payload.Data {
		found = found || item.ID == want
	}
	if !found {
		return "", fmt.Errorf("configured alias is absent from gateway discovery")
	}
	if r.target.capabilities.Protocol != providers.ProtocolOpenAICompatible ||
		!r.target.capabilities.SupportsModelDiscovery {
		return "configured alias is advertised by the production gateway", nil
	}
	apiKey, err := r.providerSecret(ctx)
	if err != nil {
		return "", err
	}
	models, err := (providers.Discoverer{HTTPClient: r.config.HTTPClient, Timeout: r.config.Timeout}).DiscoverOpenAICompatibleModels(ctx, providers.DiscoveryConfig{
		Type: r.target.provider.Type, BaseURL: r.target.provider.BaseURL, APIKey: apiKey,
	})
	if err != nil {
		return "", fmt.Errorf("provider discovery failed: %w", err)
	}
	if !models.HasRoutableID(r.target.model.ProviderModel) {
		return "", fmt.Errorf("provider model is absent from discovery")
	}
	return "gateway and provider discovery include the configured model", nil
}

func (r checkRunner) checkText(ctx context.Context) (string, error) {
	response, err := r.message(ctx, map[string]any{
		"model": r.config.Alias, "max_tokens": r.probeMaxTokens(32),
		"messages": []map[string]string{{"role": "user", "content": "Reply with the word OK."}},
	})
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	var payload struct {
		Type    string `json:"type"`
		Content []any  `json:"content"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil || len(payload.Content) == 0 {
		return "", fmt.Errorf("invalid Anthropic message response")
	}
	return "production gateway returned an Anthropic message", nil
}

func (r checkRunner) checkStream(ctx context.Context) (string, error) {
	response, err := r.message(ctx, map[string]any{
		"model": r.config.Alias, "max_tokens": r.probeMaxTokens(32), "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "Reply with the word OK."}},
	})
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") ||
		!bytes.Contains(response.Body, []byte("message_start")) ||
		!bytes.Contains(response.Body, []byte("message_stop")) {
		return "", fmt.Errorf("invalid Anthropic event stream")
	}
	return "production gateway returned a complete Anthropic event stream", nil
}

func (r checkRunner) checkForcedTool(ctx context.Context) (string, error) {
	response, err := r.message(ctx, map[string]any{
		"model": r.config.Alias, "max_tokens": r.probeMaxTokens(128),
		"messages": []map[string]string{{"role": "user", "content": "Call the required probe tool."}},
		"tools": []map[string]any{{
			"name": "ccr_probe", "description": "Conformance probe",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		"tool_choice": map[string]any{
			"type": "tool", "name": "ccr_probe", "disable_parallel_tool_use": true,
		},
	})
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	if !bytes.Contains(response.Body, []byte(`"tool_use"`)) {
		return "", fmt.Errorf("forced tool response omitted tool_use")
	}
	return "forced tool choice produced an Anthropic tool_use block", nil
}

func (r checkRunner) checkThinking(ctx context.Context) (string, error) {
	response, err := r.message(ctx, map[string]any{
		"model": r.config.Alias, "max_tokens": r.probeMaxTokens(1200),
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"messages": []map[string]string{{"role": "user", "content": "Reply with the word OK."}},
	})
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	return "declared thinking capability completed through the production gateway", nil
}

func (r checkRunner) checkCountTokens(ctx context.Context) (string, error) {
	response, err := r.request(ctx, http.MethodPost, "/v1/messages/count_tokens", map[string]any{
		"model":    r.config.Alias,
		"messages": []map[string]string{{"role": "user", "content": "Count these tokens."}},
	})
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	if response.Header.Get("X-CCR-Token-Count-Mode") != "provider" {
		return "", fmt.Errorf("declared token count capability used estimation")
	}
	var payload struct {
		InputTokens *int `json:"input_tokens"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil || payload.InputTokens == nil || *payload.InputTokens < 0 {
		return "", fmt.Errorf("invalid token count response")
	}
	return "provider token counting returned a non-negative count", nil
}

func (r checkRunner) checkCancellation(ctx context.Context) (string, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	response, err := r.message(requestCtx, map[string]any{
		"model": r.config.Alias, "max_tokens": r.probeMaxTokens(32),
		"messages": []map[string]string{{"role": "user", "content": "CCR_CONFORMANCE_CANCEL"}},
	})
	if errors.Is(err, context.Canceled) || errors.Is(requestCtx.Err(), context.Canceled) {
		return "request cancellation propagated through the production gateway", nil
	}
	if err != nil {
		return "", err
	}
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	return "request completed safely before the cancellation deadline", nil
}

func (r checkRunner) checkSanitizedError(ctx context.Context) (string, error) {
	model, err := r.absentProbeModel(ctx)
	if err != nil {
		return "", err
	}
	response, err := r.message(ctx, map[string]any{
		"model": model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "error probe"}},
	})
	if err != nil {
		return "", err
	}
	if response.Status < http.StatusBadRequest {
		return "", fmt.Errorf("unknown model did not fail")
	}
	apiKey, _ := r.providerSecret(ctx)
	if apiKey != "" && bytes.Contains(response.Body, []byte(apiKey)) {
		return "", fmt.Errorf("error response disclosed provider credential")
	}
	return "unknown model failed visibly without provider credential disclosure", nil
}

func (r checkRunner) absentProbeModel(ctx context.Context) (string, error) {
	models, err := r.config.Store.ListModels(ctx)
	if err != nil {
		return "", fmt.Errorf("listing models for error probe: %w", err)
	}
	configured := make(map[string]struct{}, len(models))
	for index := range models {
		configured[models[index].Alias] = struct{}{}
	}
	const base = "ccr-conformance-unconfigured"
	for suffix := 0; suffix <= len(models); suffix++ {
		candidate := base
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		if _, exists := configured[candidate]; !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("selecting an unconfigured model for error probe")
}

func (r checkRunner) message(ctx context.Context, payload map[string]any) (probeResponse, error) {
	return r.request(ctx, http.MethodPost, "/v1/messages", payload)
}

func (r checkRunner) request(ctx context.Context, method, path string, payload any) (probeResponse, error) {
	var body io.Reader = http.NoBody
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return probeResponse{}, fmt.Errorf("encoding probe request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.gatewayURL+path, body)
	if err != nil {
		return probeResponse{}, fmt.Errorf("creating probe request: %w", err)
	}
	req.Header.Set("X-CCR-Session-Token", r.token)
	req.Header.Set("Anthropic-Version", conformanceAnthropicVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return probeResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return probeResponse{}, fmt.Errorf("reading probe response: %w", err)
	}
	return probeResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw}, nil
}

func (r checkRunner) providerSecret(ctx context.Context) (string, error) {
	if strings.TrimSpace(r.target.provider.SecretRef) == "" {
		return "", nil
	}
	value, err := r.config.Secrets.Resolve(ctx, r.target.provider.SecretRef)
	if err != nil {
		return "", fmt.Errorf("provider credential is unavailable")
	}
	return value, nil
}

func requireSuccess(response probeResponse) error {
	if response.Status < http.StatusOK || response.Status >= http.StatusMultipleChoices {
		return &probeHTTPStatusError{status: response.Status, providerStatus: providerHTTPStatusFromGatewayError(response.Body)}
	}
	return nil
}

type probeHTTPStatusError struct {
	status         int
	providerStatus int
}

func providerHTTPStatusFromGatewayError(body []byte) int {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return 0
	}
	message := payload.Error.Message
	for offset := 0; offset+8 <= len(message); offset++ {
		if message[offset:offset+5] != "HTTP " {
			continue
		}
		if message[offset+5] < '0' || message[offset+5] > '9' ||
			message[offset+6] < '0' || message[offset+6] > '9' ||
			message[offset+7] < '0' || message[offset+7] > '9' {
			continue
		}
		status := int(message[offset+5]-'0')*100 + int(message[offset+6]-'0')*10 + int(message[offset+7]-'0')
		if status >= 100 && status <= 599 {
			return status
		}
	}
	return 0
}

func (err *probeHTTPStatusError) Error() string {
	return fmt.Sprintf("gateway returned HTTP %d", err.status)
}

func (r checkRunner) probeMaxTokens(preferred int) int {
	if r.target.modelCapabilities.MaxOutputTokens == nil || *r.target.modelCapabilities.MaxOutputTokens >= int64(preferred) {
		return preferred
	}
	if *r.target.modelCapabilities.MaxOutputTokens < 1 {
		return 1
	}
	return int(*r.target.modelCapabilities.MaxOutputTokens)
}
