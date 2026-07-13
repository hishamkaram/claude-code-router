//go:build live

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveLaunchOpenAIProviderReceivesCurrentRouteIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}

	var chatCalled atomic.Bool
	var identitySeen atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5"}]}`)
		case "/v1/chat/completions":
			chatCalled.Store(true)
			payload, ok := decodeLiveOpenAIChatPayload(t, w, r)
			if !ok {
				return
			}
			identitySeen.Store(
				openAIMessagesContain(payload.Messages, `CCR alias "gpt"`) &&
					openAIMessagesContain(payload.Messages, `provider "litellm"`) &&
					openAIMessagesContain(payload.Messages, `provider model "gpt-5"`),
			)
			if !identitySeen.Load() {
				t.Errorf("provider messages missing current CCR route identity: %#v", payload.Messages)
				http.Error(w, "missing route identity", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-live-identity","choices":[{"message":{"content":"CCR_LIVE_ROUTE_IDENTITY_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addLiveOpenAIModel(t, ctx, dbPath, provider.URL)
	prompt := "Which model are you now?"
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, "--db", dbPath, "launch", "--model", "gpt", "--print", "--auth-mode", "gateway-token")
	if err != nil {
		t.Fatalf("launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !chatCalled.Load() || !identitySeen.Load() {
		t.Fatalf("live route identity incomplete: chatCalled=%v identitySeen=%v\nstdout:\n%s\nstderr:\n%s", chatCalled.Load(), identitySeen.Load(), out, errOut)
	}
	if !strings.Contains(out, "CCR_LIVE_ROUTE_IDENTITY_OK") {
		t.Fatalf("launch output missing routed identity response:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}
