package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestGatewayInjectsOpenAIRouteIdentityForModelQuestion(t *testing.T) {
	ctx := context.Background()
	var gotMessages []openAIMessage
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []openAIMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("provider decode error = %v", err)
		}
		gotMessages = payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"glm"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer provider.Close()

	s := newGatewayStore(t, store.Provider{Name: "litellm", Type: "litellm", BaseURL: provider.URL, SecretRef: ""}, store.Model{Alias: "glm", ProviderName: "litellm", ProviderModel: "glm-5.2", Status: "degraded"})
	server := startGateway(t, ctx, s, fakeGatewaySecrets{})
	defer func() {
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	}()

	body := `{
		"model":"claude-ccr-glm",
		"messages":[
			{"role":"user","content":"which model were you earlier?"},
			{"role":"assistant","content":"I am Sonnet."},
			{"role":"user","content":"which model are you now?"}
		]
	}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gateway request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200", resp.StatusCode)
	}
	if len(gotMessages) != 4 {
		t.Fatalf("provider messages = %#v, want route identity plus transcript", gotMessages)
	}
	identity := gotMessages[0]
	if identity.Role != "system" ||
		!strings.Contains(identity.Content, `CCR alias "glm"`) ||
		!strings.Contains(identity.Content, `provider "litellm"`) ||
		!strings.Contains(identity.Content, `provider model "glm-5.2"`) ||
		!strings.Contains(identity.Content, `Claude Code requested model ID "claude-ccr-glm"`) {
		t.Fatalf("route identity message = %#v", identity)
	}
	if gotMessages[2].Role != "assistant" || gotMessages[2].Content != "I am Sonnet." {
		t.Fatalf("provider transcript = %#v", gotMessages)
	}
}
