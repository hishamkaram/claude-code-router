//go:build live

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveGatewayTokenLaunchDiscoversConfiguredAlias(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	chatCalled := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-discovery","choices":[{"message":{"content":"gateway-discovery-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("hello\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled || !strings.Contains(out, "gateway-discovery-ok") {
		t.Fatalf("launch did not complete through fake provider\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}

	cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
	cache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("reading Claude gateway discovery cache: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !bytes.Contains(cache, []byte("anthropic.ccr.gpt")) {
		t.Fatalf("gateway discovery cache does not include configured alias: %s", cache)
	}
}
