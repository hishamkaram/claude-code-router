//go:build live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/responses"
)

func TestLiveFixtureResponsesImageToolResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "1")
	var imageSeen atomic.Bool
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveLiveResponsesImage(t, w, r, &imageSeen)
	}))
	defer fixture.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"--db", dbPath, "provider", "add", "image-provider", "--type", "openai-compatible", "--base-url", fixture.URL, "--no-api-key", "--responses"},
		{"--db", dbPath, "model", "add", "image-responses", "--provider", "image-provider", "--model", "image-model"},
		{"--db", dbPath, "model", "update", "image-responses", "--model-kind", "responses", "--responses", "true", "--vision", "true", "--input-modalities", "text,image"},
	} {
		if _, _, err := runLiveCommand(ctx, Dependencies{}, args...); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader("Use the fixture image tool and report its result.\n")},
		"--db", dbPath, "launch", "--model", "image-responses", "-p", "--auth-mode", "provider-only",
		"--permission-mode", "bypassPermissions", "--mcp-config", writeLiveImageMCPConfig(t), "--max-turns", "4")
	if err != nil || !imageSeen.Load() || !strings.Contains(out, liveImageToolResult) {
		t.Fatalf("image result seen=%t err=%v\nstdout: %s\nstderr: %s", imageSeen.Load(), err, out, errOut)
	}
}

func serveLiveResponsesImage(t *testing.T, w http.ResponseWriter, r *http.Request, seen *atomic.Bool) {
	t.Helper()
	if r.URL.Path == "/v1/models" {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"image-model"}]}`)
		return
	}
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	var payload responses.Request
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	for _, item := range payload.Input {
		if item.Type == "function_call_output" && item.CallID == "call_image" {
			encoded, err := json.Marshal(item.Output)
			if err != nil {
				t.Errorf("encode function output: %v", err)
				return
			}
			var content []responses.Content
			if err := json.Unmarshal(encoded, &content); err != nil || len(content) != 1 || content[0].Type != "input_image" || content[0].ImageURL != "data:image/png;base64,"+liveImagePNGData {
				t.Errorf("image function output not preserved: %s", encoded)
				http.Error(w, "invalid image output", http.StatusBadRequest)
				return
			}
			seen.Store(true)
			writeLiveResponsesTextFixture(w, payload, "resp_image_done", liveImageToolResult, 9, 3)
			return
		}
	}
	for _, tool := range payload.Tools {
		if strings.HasSuffix(tool.Name, "__"+liveImageToolName) {
			writeLiveResponsesToolFixture(w, payload, "resp_image_call", "call_image", tool.Name, map[string]any{})
			return
		}
	}
	for _, tool := range payload.Tools {
		if tool.Name == "ToolSearch" {
			writeLiveResponsesToolFixture(w, payload, "resp_search", "call_search", tool.Name, map[string]string{"query": "select:mcp__fixture__image"})
			return
		}
	}
	http.Error(w, "image tool missing", http.StatusBadRequest)
}
