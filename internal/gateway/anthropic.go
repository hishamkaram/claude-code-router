package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func (h *handler) handleAnthropicPassThrough(w http.ResponseWriter, r *http.Request, body []byte, providerOverride *store.Provider, responseModel string) {
	if body == nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic request")
			return
		}
	}
	provider, err := h.anthropicProviderForRequest(r.Context(), providerOverride)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	endpoint, err := anthropicEndpoint(provider.BaseURL, anthropicResourceFromPath(r.URL.Path))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	if r.URL.RawQuery != "" {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			writeAnthropicError(w, http.StatusBadGateway, parseErr.Error())
			return
		}
		parsed.RawQuery = r.URL.RawQuery
		endpoint = parsed.String()
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("creating Anthropic pass-through request: %v", err))
		return
	}
	copyAnthropicPassThroughHeaders(req.Header, r.Header)
	apiKey, err := resolveProviderSecret(r.Context(), h.cfg.Secrets, provider.SecretRef)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("provider secret %s could not be resolved", secret.RedactRef(provider.SecretRef)))
		return
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("requesting Anthropic provider %q: %v", provider.Name, err))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	copyProviderResponseBody(w, resp, responseModel)
}

func (h *handler) anthropicProviderForRequest(ctx context.Context, override *store.Provider) (store.Provider, error) {
	if override != nil {
		return *override, nil
	}
	return h.defaultAnthropicProvider(ctx)
}

func (h *handler) defaultAnthropicProvider(ctx context.Context) (store.Provider, error) {
	providersList, err := h.cfg.Store.ListProviders(ctx)
	if err != nil {
		return store.Provider{}, fmt.Errorf("listing providers for Anthropic pass-through: %w", err)
	}
	var first store.Provider
	for i := range providersList {
		provider := providersList[i]
		if provider.Type != "anthropic" {
			continue
		}
		if provider.Name == "anthropic" {
			return provider, nil
		}
		if first.Name == "" {
			first = provider
		}
	}
	if first.Name != "" {
		return first, nil
	}
	return store.Provider{}, errNoAnthropicPassThroughProvider
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

func copyAnthropicPassThroughHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		lower := strings.ToLower(canonical)
		if lower == "authorization" || lower == "x-api-key" || lower == "host" || lower == "content-length" {
			continue
		}
		if lower == "content-type" || lower == "accept" || lower == "user-agent" ||
			strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-claude-code-") {
			for _, value := range values {
				dst.Add(canonical, value)
			}
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
	if dst.Get("Accept") == "" {
		dst.Set("Accept", "application/json")
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

func copyProviderResponseBody(w http.ResponseWriter, resp *http.Response, responseModel string) {
	if responseModel == "" || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") || strings.Contains(contentType, "+json") {
		copyJSONProviderResponseBody(w, resp.Body, responseModel)
		return
	}
	if strings.Contains(contentType, "text/event-stream") {
		if flusher, ok := w.(http.Flusher); ok {
			copyAndRewriteSSE(w, resp.Body, flusher, responseModel)
			return
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

func copyJSONProviderResponseBody(dst io.Writer, src io.Reader, responseModel string) {
	raw, err := io.ReadAll(src)
	if err != nil {
		return
	}
	if rewritten, ok := rewriteAnthropicResponseModel(raw, responseModel); ok {
		raw = rewritten
	}
	_, _ = dst.Write(raw)
}

func copyAndRewriteSSE(dst io.Writer, src io.Reader, flusher http.Flusher, responseModel string) {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := dst.Write(rewriteSSEDataLine(line, responseModel)); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
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
