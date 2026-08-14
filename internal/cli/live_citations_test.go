//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

const (
	liveCitationSeed    = "CCR_LIVE_CITATION_SEED"
	liveCitationSuccess = "CCR_LIVE_CITATIONS_OK"
)

func TestLiveFixtureAssistantCitationsSurviveModelSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", liveFixtureAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	isolatedHome := t.TempDir()
	t.Setenv("HOME", isolatedHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(isolatedHome, ".claude"))

	fixture := newLiveCitationFixture(t)
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "litellm", "--base-url", fixture.URL(), "--no-api-key", "--mode", "full"},
		{"--db", dbPath, "model", "add", "citation-chat", "--provider", "litellm", "--model", "citation-chat-model", "--compat", "full"},
		{"--db", dbPath, "model", "update", "citation-chat", "--context-window", "1000000"},
	} {
		if out, errOut, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatalf("run %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, out, errOut)
		}
	}

	deps := Dependencies{
		In: strings.NewReader(liveStreamInput(
			t,
			"/model sonnet",
			"Generate the citation seed response.",
			"/model anthropic.ccr.citation-chat[1m]",
			"Reply exactly CCR_LIVE_CITATIONS_OK.",
		)),
		StartGateway: fixture.StartGateway,
	}
	out, errOut, err := runLiveCommand(
		ctx, deps,
		"--db", dbPath, "launch", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	)
	if err != nil {
		t.Fatalf("citation model-switch launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	combined := out + "\n" + errOut
	for _, want := range []string{liveCitationSeed, liveCitationSuccess} {
		if !strings.Contains(combined, want) {
			t.Fatalf("live citation launch output missing %q\nstdout:\n%s\nstderr:\n%s", want, out, errOut)
		}
	}
	for _, unexpected := range []string{"unsupported assistant text block", "API Error: 501", "status 501"} {
		if strings.Contains(strings.ToLower(combined), strings.ToLower(unexpected)) {
			t.Fatalf("live citation launch output contains %q\nstdout:\n%s\nstderr:\n%s", unexpected, out, errOut)
		}
	}
	fixture.AssertCitationHistory(t, liveCitationSeed)
}

type liveCitationFixture struct {
	server *httptest.Server

	mu             sync.Mutex
	firstPartyCall int
	chatCall       int
	chatBodies     [][]byte
	chatError      string
}

func newLiveCitationFixture(t *testing.T) *liveCitationFixture {
	t.Helper()
	fixture := &liveCitationFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.handle(t, w, r)
	}))
	return fixture
}

func (f *liveCitationFixture) URL() string {
	return f.server.URL
}

func (f *liveCitationFixture) Close() {
	f.server.Close()
}

func (f *liveCitationFixture) StartGateway(ctx context.Context, cfg gateway.Config) (*gateway.Server, error) {
	cfg.AnthropicBaseURL = f.URL()
	return gateway.Start(ctx, cfg)
}

func (f *liveCitationFixture) handle(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"citation-chat-model"}]}`)
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
	case "/v1/messages":
		f.handleAnthropic(w, r)
	case "/v1/chat/completions":
		f.handleOpenAI(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *liveCitationFixture) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		f.setError(fmt.Sprintf("decode Anthropic request: %v", err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.firstPartyCall++
	f.mu.Unlock()
	if payload.Stream {
		writeLiveAnthropicCitationStream(w, payload.Model, liveCitationSeed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"msg_live_citations","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q,"citations":[{"type":"char_location","cited_text":"seed","document_index":0,"document_title":"CCR citation fixture","start_char_index":0,"end_char_index":4}]}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}`, payload.Model, liveCitationSeed)
}

func (f *liveCitationFixture) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.setError(fmt.Sprintf("read OpenAI request: %v", err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var payload liveOpenAIChatPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		f.setError(fmt.Sprintf("decode OpenAI request: %v", err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.chatCall++
	f.chatBodies = append(f.chatBodies, append([]byte(nil), body...))
	f.mu.Unlock()
	if payload.Model != "citation-chat-model" {
		f.setError(fmt.Sprintf("OpenAI model = %q, want citation-chat-model", payload.Model))
		http.Error(w, "unexpected model", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_live_citations","choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`, liveCitationSuccess)
}

func (f *liveCitationFixture) setError(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatError == "" {
		f.chatError = message
	}
}

func (f *liveCitationFixture) AssertCitationHistory(t *testing.T, seed string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatError != "" {
		t.Fatalf("citation fixture error: %s", f.chatError)
	}
	if f.firstPartyCall == 0 || f.chatCall == 0 {
		t.Fatalf("fixture calls = first-party %d, OpenAI chat %d; want first-party seed and OpenAI chat requests", f.firstPartyCall, f.chatCall)
	}
	seedSeen := false
	for _, body := range f.chatBodies {
		if strings.Contains(string(body), `"citations"`) {
			t.Fatalf("OpenAI provider received citation metadata: %s", body)
		}
		var payload liveOpenAIChatPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("stored OpenAI request decode error: %v", err)
		}
		seedSeen = seedSeen || openAIMessagesContain(payload.Messages, seed)
	}
	if !seedSeen {
		t.Fatalf("OpenAI request lost cited assistant text: %d requests", len(f.chatBodies))
	}
}

func writeLiveAnthropicCitationStream(w http.ResponseWriter, model, text string) {
	citation, _ := json.Marshal(map[string]any{
		"type":             "char_location",
		"cited_text":       "seed",
		"document_index":   0,
		"document_title":   "CCR citation fixture",
		"start_char_index": 0,
		"end_char_index":   4,
	})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_live_citations\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n", model)
	_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\",\"citations\":[]}}\n\n")
	_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":%s}}\n\n", citation)
	_, _ = fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n")
	_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
