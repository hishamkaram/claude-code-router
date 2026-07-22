package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func (h *handler) handleAnthropicPassThrough(w http.ResponseWriter, r *http.Request, body []byte, providerOverride *store.Provider, authMode anthropicAuthMode, responseModel string) observability.TokenUsage {
	if body == nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxGatewayRequestBytes+1))
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic request")
			return observability.TokenUsage{}
		}
		if len(body) > maxGatewayRequestBytes {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "Anthropic request exceeds the 32 MiB gateway limit")
			return observability.TokenUsage{}
		}
	}
	if providerOverride == nil {
		writeAnthropicError(w, http.StatusBadGateway, "Anthropic route missing upstream provider")
		return observability.TokenUsage{}
	}
	provider := *providerOverride
	endpoint, err := anthropicEndpoint(provider.BaseURL, anthropicResourceFromPath(r.URL.Path))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return observability.TokenUsage{}
	}
	if r.URL.RawQuery != "" {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			writeAnthropicError(w, http.StatusBadGateway, parseErr.Error())
			return observability.TokenUsage{}
		}
		parsed.RawQuery = r.URL.RawQuery
		endpoint = parsed.String()
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("creating Anthropic pass-through request: %v", err))
		return observability.TokenUsage{}
	}
	copyAnthropicPassThroughHeaders(req.Header, r.Header, h.cfg.Token, authMode == anthropicAuthIncoming)
	if authMode == anthropicAuthProviderSecret {
		apiKey, secretErr := resolveProviderSecret(r.Context(), h.cfg.Secrets, provider.SecretRef)
		if secretErr != nil {
			writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(provider.SecretRef)))
			return observability.TokenUsage{}
		}
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("requesting Anthropic provider %q: %v", provider.Name, err))
		return observability.TokenUsage{}
	}
	defer func() { _ = resp.Body.Close() }()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return copyProviderResponseBody(w, resp, responseModel)
}

func rewriteAnthropicRequestModel(body []byte, model string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("rewriting Anthropic request model: %w", err)
	}
	payload["model"] = model
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("rewriting Anthropic request model: %w", err)
	}
	return rewritten, nil
}

func rewriteAnthropicMessageBody(body []byte, providerModel string, disableParallelTools bool) ([]byte, error) {
	rewritten := body
	var err error
	if disableParallelTools {
		rewritten, err = rewriteAnthropicDisableParallelTools(rewritten)
		if err != nil {
			return nil, err
		}
	}
	if providerModel != "" {
		rewritten, err = rewriteAnthropicRequestModel(rewritten, providerModel)
		if err != nil {
			return nil, err
		}
	}
	return rewritten, nil
}

func rewriteAnthropicDisableParallelTools(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("enforcing Anthropic serial tool use: %w", err)
	}
	toolChoice := make(map[string]json.RawMessage)
	if raw, ok := payload["tool_choice"]; ok && rawJSONPresent(raw) {
		if err := json.Unmarshal(raw, &toolChoice); err != nil || toolChoice == nil {
			return nil, fmt.Errorf("enforcing Anthropic serial tool use: tool_choice must be an object")
		}
	}
	if _, ok := toolChoice["type"]; !ok {
		toolChoice["type"] = json.RawMessage(`"auto"`)
	}
	toolChoice["disable_parallel_tool_use"] = json.RawMessage("true")
	encodedChoice, err := json.Marshal(toolChoice)
	if err != nil {
		return nil, fmt.Errorf("enforcing Anthropic serial tool use: %w", err)
	}
	payload["tool_choice"] = encodedChoice
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("enforcing Anthropic serial tool use: %w", err)
	}
	return rewritten, nil
}

func anthropicResourceFromPath(path string) string {
	path = strings.Trim(path, "/")
	path = strings.TrimPrefix(path, "v1/")
	switch path {
	case "messages/count_tokens":
		return "messages/count_tokens"
	default:
		return "messages"
	}
}

func anthropicEndpoint(baseURL, resource string) (string, error) {
	cleanBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.ParseRequestURI(cleanBase)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Anthropic base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid Anthropic base URL %q: scheme must be http or https", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = path + "/" + resource
	} else {
		parsed.Path = path + "/v1/" + resource
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func copyAnthropicPassThroughHeaders(dst, src http.Header, localToken string, forwardAuth bool) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		lower := strings.ToLower(canonical)
		copyAnthropicPassThroughHeader(dst, canonical, lower, values, localToken, forwardAuth)
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
	if dst.Get("Accept") == "" {
		dst.Set("Accept", "application/json")
	}
}

func copyAnthropicPassThroughHeader(dst http.Header, canonical, lower string, values []string, localToken string, forwardAuth bool) {
	if isSkippedAnthropicPassThroughHeader(lower) {
		return
	}
	if lower == "authorization" || lower == "x-api-key" {
		if forwardAuth {
			copyIncomingAnthropicAuthHeader(dst, canonical, lower, values, localToken)
		}
		return
	}
	if !isAllowedAnthropicPassThroughHeader(lower) {
		return
	}
	for _, value := range values {
		dst.Add(canonical, value)
	}
}

func isSkippedAnthropicPassThroughHeader(lower string) bool {
	return lower == "host" || lower == "content-length" || lower == ccrSessionTokenLower
}

func isAllowedAnthropicPassThroughHeader(lower string) bool {
	return lower == "content-type" || lower == "accept" || lower == "user-agent" ||
		strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-claude-code-")
}

func copyIncomingAnthropicAuthHeader(dst http.Header, canonical, lower string, values []string, localToken string) {
	for _, value := range values {
		if isLocalGatewayAuthValue(lower, value, localToken) {
			continue
		}
		dst.Add(canonical, value)
	}
}

func isLocalGatewayAuthValue(lowerHeader, value, localToken string) bool {
	token := strings.TrimSpace(localToken)
	if token == "" {
		return false
	}
	switch lowerHeader {
	case "x-api-key":
		return strings.TrimSpace(value) == token
	case "authorization":
		return strings.EqualFold(strings.TrimSpace(value), "Bearer "+token)
	default:
		return false
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if strings.EqualFold(canonical, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func copyProviderResponseBody(w http.ResponseWriter, resp *http.Response, responseModel string) observability.TokenUsage {
	if responseModel == "" || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(w, resp.Body)
		return observability.TokenUsage{}
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") || strings.Contains(contentType, "+json") {
		return copyJSONProviderResponseBody(w, resp.Body, responseModel)
	}
	if strings.Contains(contentType, "text/event-stream") {
		if flusher, ok := w.(http.Flusher); ok {
			return copyAndRewriteSSE(w, resp.Body, flusher, responseModel)
		}
	}
	_, _ = io.Copy(w, resp.Body)
	return observability.TokenUsage{}
}

func copyJSONProviderResponseBody(dst io.Writer, src io.Reader, responseModel string) observability.TokenUsage {
	raw, err := io.ReadAll(src)
	if err != nil {
		return observability.TokenUsage{}
	}
	usage := anthropicUsageFromJSON(raw)
	if rewritten, ok := rewriteAnthropicResponseModel(raw, responseModel); ok {
		raw = rewritten
	}
	_, _ = dst.Write(raw)
	return usage
}

func copyAndRewriteSSE(dst io.Writer, src io.Reader, flusher http.Flusher, responseModel string) observability.TokenUsage {
	reader := bufio.NewReader(src)
	var usage observability.TokenUsage
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			mergeTokenUsage(&usage, anthropicUsageFromSSELine(line))
			if _, writeErr := dst.Write(rewriteSSEDataLine(line, responseModel)); writeErr != nil {
				return usage
			}
			flusher.Flush()
		}
		if err != nil {
			return usage
		}
	}
}

type anthropicUsageFields struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_input_tokens"`
	CacheWriteTokens *int64 `json:"cache_creation_input_tokens"`
}

func anthropicUsageFromJSON(raw []byte) observability.TokenUsage {
	var payload struct {
		InputTokens *int64                `json:"input_tokens"`
		Usage       *anthropicUsageFields `json:"usage"`
		Message     *struct {
			Usage *anthropicUsageFields `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return observability.TokenUsage{}
	}
	var usage observability.TokenUsage
	if payload.InputTokens != nil {
		usage.Observed = true
		usage.InputTokens = *payload.InputTokens
	}
	mergeUsageFields(&usage, payload.Message)
	if payload.Usage != nil {
		applyUsageFields(&usage, payload.Usage)
	}
	return usage
}

func mergeUsageFields(usage *observability.TokenUsage, message *struct {
	Usage *anthropicUsageFields `json:"usage"`
},
) {
	if message != nil && message.Usage != nil {
		applyUsageFields(usage, message.Usage)
	}
}

func applyUsageFields(usage *observability.TokenUsage, fields *anthropicUsageFields) {
	if fields.InputTokens != nil {
		usage.Observed, usage.InputTokens = true, *fields.InputTokens
	}
	if fields.OutputTokens != nil {
		usage.Observed, usage.OutputTokens = true, *fields.OutputTokens
	}
	if fields.CacheReadTokens != nil {
		usage.Observed, usage.CacheReadTokens = true, *fields.CacheReadTokens
	}
	if fields.CacheWriteTokens != nil {
		usage.Observed, usage.CacheWriteTokens = true, *fields.CacheWriteTokens
	}
}

func anthropicUsageFromSSELine(line []byte) observability.TokenUsage {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return observability.TokenUsage{}
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return observability.TokenUsage{}
	}
	return anthropicUsageFromJSON(payload)
}

func mergeTokenUsage(current *observability.TokenUsage, next observability.TokenUsage) {
	if !next.Observed {
		return
	}
	current.Observed = true
	if next.InputTokens != 0 {
		current.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		current.OutputTokens = next.OutputTokens
	}
	if next.CacheReadTokens != 0 {
		current.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheWriteTokens != 0 {
		current.CacheWriteTokens = next.CacheWriteTokens
	}
}

func rewriteSSEDataLine(line []byte, responseModel string) []byte {
	trimmedLine := bytes.TrimRight(line, "\r\n")
	lineEnding := line[len(trimmedLine):]
	field := bytes.TrimLeft(trimmedLine, " \t")
	leadingLen := len(trimmedLine) - len(field)
	if !bytes.HasPrefix(field, []byte("data:")) {
		return line
	}
	dataStart := leadingLen + len("data:")
	data := bytes.TrimSpace(trimmedLine[dataStart:])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return line
	}
	rewritten, ok := rewriteAnthropicResponseModel(data, responseModel)
	if !ok {
		return line
	}
	out := make([]byte, 0, dataStart+1+len(rewritten)+len(lineEnding))
	out = append(out, trimmedLine[:dataStart]...)
	out = append(out, ' ')
	out = append(out, rewritten...)
	out = append(out, lineEnding...)
	return out
}

func rewriteAnthropicResponseModel(raw []byte, responseModel string) ([]byte, bool) {
	if strings.TrimSpace(responseModel) == "" {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	changed := false
	if _, ok := payload["model"]; ok {
		payload["model"] = responseModel
		changed = true
	}
	if message, ok := payload["message"].(map[string]any); ok {
		if _, hasModel := message["model"]; hasModel {
			message["model"] = responseModel
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}
