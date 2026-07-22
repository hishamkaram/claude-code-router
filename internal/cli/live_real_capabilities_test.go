//go:build live

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/cua/executor"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLiveLocalRealConfiguredVision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_VISION") != "1" {
		t.Skip("set CCR_LIVE_REAL_VISION=1 to run configured-provider vision verification")
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("configured vision requires an installed Claude Code CLI: %v", err)
	}
	model := requireLiveConfiguredModel(t, ctx, "CCR_LIVE_REAL_VISION_MODEL_ALIAS")
	if !modelSupportsImage(model) {
		t.Skipf("configured model alias %q is not marked vision-capable; set CCR_LIVE_REAL_VISION_MODEL_ALIAS to a vision-capable alias", model.Alias)
	}
	gatewayURL, token := startLiveConfiguredGateway(t, ctx, nil, "")
	status, response := postLiveConfiguredGatewayMessage(t, ctx, gatewayURL, token, liveVisionRequest(t, model.Alias), nil)
	if status != http.StatusOK {
		t.Fatalf("configured vision gateway request returned HTTP %d; inspect ccr trace and provider diagnostics", status)
	}
	assertLiveGatewayTextResponse(t, response, "configured vision")
}

func TestLiveLocalRealAnthropicCUA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_ANTHROPIC_CUA") != "1" {
		t.Skip("set CCR_LIVE_REAL_ANTHROPIC_CUA=1 to run real Anthropic CUA verification")
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("Anthropic CUA requires an installed Claude Code CLI: %v", err)
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is required for direct first-party client-managed CUA verification")
	}
	model := requiredLiveEnvironment(t, "CCR_LIVE_REAL_ANTHROPIC_CUA_MODEL")
	toolType := requiredLiveEnvironment(t, "CCR_LIVE_REAL_ANTHROPIC_CUA_TOOL_TYPE")
	beta := requiredLiveEnvironment(t, "CCR_LIVE_REAL_ANTHROPIC_CUA_BETA")
	gatewayURL, token := startLiveConfiguredGateway(t, ctx, nil, "")
	status, response := postLiveConfiguredGatewayMessage(t, ctx, gatewayURL, token,
		liveAnthropicComputerUseRequest(t, model, toolType), http.Header{
			"x-api-key":         []string{apiKey},
			"anthropic-version": []string{"2023-06-01"},
			"anthropic-beta":    []string{beta},
		})
	if status != http.StatusOK {
		t.Fatalf("first-party client-managed CUA gateway request returned HTTP %d; inspect ccr trace and Anthropic diagnostics", status)
	}
	assertLiveGatewayComputerToolUse(t, response, "first-party client-managed CUA")
}

func TestLiveLocalRealOpenAIResponsesCUA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_OPENAI_RESPONSES_CUA") != "1" {
		t.Skip("set CCR_LIVE_REAL_OPENAI_RESPONSES_CUA=1 to run real OpenAI Responses CUA verification")
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("OpenAI Responses CUA requires an installed Claude Code CLI: %v", err)
	}
	model := requireLiveConfiguredModel(t, ctx, "CCR_LIVE_REAL_OPENAI_RESPONSES_MODEL_ALIAS")
	if !modelSupportsResponsesCUA(model) {
		t.Skipf("configured model alias %q is not marked Responses and computer-use capable", model.Alias)
	}
	acquireLiveDesktopLock(t)
	externalName := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_NAME")
	externalURL := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_URL")
	externalTokenEnv := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_TOKEN_ENV")
	externalToken := requiredLiveSecretFromEnvironment(t, externalTokenEnv)
	managedExecutor, err := executor.NewExternalHTTP(ctx, externalName, executor.ExternalOptions{BaseURL: externalURL, BearerToken: externalToken})
	if err != nil {
		t.Fatalf("constructing external CUA executor: %v", err)
	}
	executorOwnedByRuntime := false
	defer func() {
		if !executorOwnedByRuntime {
			if closeErr := managedExecutor.Close(); closeErr != nil {
				t.Errorf("closing external CUA executor: %v", closeErr)
			}
		}
	}()
	if err := managedExecutor.Check(ctx); err != nil {
		t.Fatalf("checking external CUA executor: %v", err)
	}

	// This is the actual Claude Code CLI startup path. The direct request below
	// then verifies a real provider computer-call loop without auto-approving
	// anything except an explicitly requested screenshot.
	out, errOut, modelAlias := runConfiguredProviderProbe(t, ctx,
		"Reply exactly CCR_LIVE_MANAGED_CUA_STARTUP_OK. Do not use tools.",
		"--ccr-cua-mode", "managed",
		"--ccr-cua-executor", "external:"+externalName,
		"--ccr-cua-external-url", externalURL,
		"--ccr-cua-external-token-env", externalTokenEnv,
		"--ccr-cua-max-turns", "2",
		"--ccr-cua-max-actions", "2",
		"--ccr-cua-timeout", "2m",
	)
	assertConfiguredProviderProbe(t, out, errOut, modelAlias, "CCR_LIVE_MANAGED_CUA_STARTUP_OK")
	if !strings.Contains(errOut, "CCR: managed CUA executor external:"+externalName+" is ready") {
		t.Fatalf("managed CUA launch did not report executor readiness")
	}

	auditor := &liveCUAAuditor{}
	managedRuntime, err := cua.NewManagedRuntime(ctx, cua.Config{
		Mode: cua.ModeManaged, Executor: "external:" + externalName,
		MaxTurns: 2, MaxActions: 2, Timeout: 2 * time.Minute,
	}, managedExecutor, liveScreenshotOnlyAuthorizer{}, auditor)
	if err != nil {
		t.Fatalf("creating managed CUA runtime: %v", err)
	}
	executorOwnedByRuntime = true
	defer func() {
		if closeErr := managedRuntime.Close(); closeErr != nil {
			t.Errorf("closing managed CUA runtime: %v", closeErr)
		}
	}()
	gatewayURL, token := startLiveConfiguredGateway(t, ctx, managedRuntime, t.TempDir())
	status, response := postLiveConfiguredGatewayMessage(t, ctx, gatewayURL, token, liveResponsesComputerUseRequest(t, model.Alias), nil)
	if status != http.StatusOK {
		t.Fatalf("managed Responses CUA gateway request returned HTTP %d; inspect ccr trace and provider diagnostics", status)
	}
	assertLiveGatewayTextResponse(t, response, "managed Responses CUA")
	if !auditor.hasApprovedScreenshot() {
		t.Fatal("managed Responses CUA did not execute and audit an approved screenshot action")
	}
}

func TestLiveLocalRealCUAExecutors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_CUA_EXECUTORS") != "1" {
		t.Skip("set CCR_LIVE_REAL_CUA_EXECUTORS=1 to run real CUA executor matrix")
	}
	acquireLiveDesktopLock(t)
	for _, target := range []string{"docker", "local-browser", "external", "macos-preview"} {
		target := target
		t.Run(target, func(t *testing.T) {
			targetCtx, targetCancel := context.WithTimeout(ctx, liveCUAExecutorTimeout(target))
			defer targetCancel()
			managedExecutor := startLiveCUAExecutor(t, targetCtx, target)
			defer func() {
				if closeErr := managedExecutor.Close(); closeErr != nil {
					t.Errorf("closing %s CUA executor: %v", target, closeErr)
				}
			}()
			waitForLiveCUAExecutorReady(t, targetCtx, managedExecutor, target)
			observation, err := managedExecutor.Execute(targetCtx, cua.Action{CallID: "live-screenshot", Kind: cua.ActionScreenshot})
			if err != nil {
				t.Fatalf("executing %s screenshot: %v", target, err)
			}
			if len(observation.Screenshot) == 0 || !strings.HasPrefix(strings.ToLower(observation.ContentType), "image/") {
				t.Fatalf("%s screenshot observation was empty or not an image", target)
			}
		})
	}
}

func liveCUAExecutorTimeout(target string) time.Duration {
	if target == "docker" {
		return 3 * time.Minute
	}
	return time.Minute
}

func waitForLiveCUAExecutorReady(t *testing.T, ctx context.Context, managedExecutor cua.Executor, target string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	progress := time.NewTicker(10 * time.Second)
	defer progress.Stop()
	started := time.Now()
	var lastErr error
	for {
		if err := managedExecutor.Check(ctx); err == nil {
			return
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("checking %s CUA executor: %v after %s: %v", target, ctx.Err(), time.Since(started).Round(time.Second), lastErr)
		case <-progress.C:
			t.Logf("waiting for %s CUA executor to become ready (%s elapsed): %v",
				target, time.Since(started).Round(time.Second), lastErr)
		case <-ticker.C:
		}
	}
}

func startLiveCUAExecutor(t *testing.T, ctx context.Context, target string) cua.Executor {
	t.Helper()
	switch target {
	case "docker":
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skipf("docker executor requires docker on PATH: %v", err)
		}
		managedExecutor, err := executor.NewDockerBrowser(ctx, executor.DockerBrowserOptions{
			Image: strings.TrimSpace(os.Getenv("CCR_CUA_DOCKER_IMAGE")),
		})
		if err != nil {
			t.Fatalf("starting docker CUA executor: %v", err)
		}
		return managedExecutor
	case "local-browser":
		if _, err := exec.LookPath("chromium"); err != nil {
			t.Skipf("local-browser executor requires chromium on PATH: %v", err)
		}
		managedExecutor, err := executor.NewLocalBrowser(ctx, executor.LocalBrowserOptions{})
		if err != nil {
			t.Fatalf("starting local-browser CUA executor: %v", err)
		}
		return managedExecutor
	case "external":
		name := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_NAME")
		url := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_URL")
		tokenEnv := requiredLiveEnvironment(t, "CCR_LIVE_CUA_EXTERNAL_TOKEN_ENV")
		token := requiredLiveSecretFromEnvironment(t, tokenEnv)
		managedExecutor, err := executor.NewExternalHTTP(ctx, name, executor.ExternalOptions{BaseURL: url, BearerToken: token})
		if err != nil {
			t.Fatalf("starting external CUA executor: %v", err)
		}
		return managedExecutor
	case "macos-preview":
		if runtime.GOOS != "darwin" {
			t.Skip("macos-preview executor requires macOS")
		}
		if _, err := exec.LookPath("ccr-cua-macos"); err != nil {
			t.Skipf("macos-preview executor requires ccr-cua-macos on PATH: %v", err)
		}
		managedExecutor, err := executor.NewMacOSPreview(ctx, executor.MacOSPreviewOptions{})
		if err != nil {
			t.Fatalf("starting macos-preview CUA executor: %v", err)
		}
		return managedExecutor
	default:
		t.Fatalf("unknown live CUA executor %q", target)
		return nil
	}
}

const maxLiveGatewayResponseBytes = 1 << 20

func startLiveConfiguredGateway(t *testing.T, ctx context.Context, managed *cua.ManagedRuntime, project string) (string, string) {
	t.Helper()
	s, err := store.Open(ctx, configuredLiveDBPath(t))
	if err != nil {
		t.Fatalf("opening configured live database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating configured live database: %v", err)
	}
	token, err := gateway.NewToken()
	if err != nil {
		t.Fatalf("creating local gateway token: %v", err)
	}
	server, err := gateway.Start(ctx, gateway.Config{
		Store: s, Token: token, ManagedCUA: managed, ManagedCUAProject: project,
	})
	if err != nil {
		t.Fatalf("starting configured live gateway: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	return server.URL(), token
}

func postLiveConfiguredGatewayMessage(t *testing.T, ctx context.Context, gatewayURL, token string, body []byte, headers http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating configured gateway request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCR-Session-Token", token)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requesting configured gateway: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLiveGatewayResponseBytes+1))
	if err != nil {
		t.Fatalf("reading configured gateway response: %v", err)
	}
	if len(data) > maxLiveGatewayResponseBytes {
		t.Fatalf("configured gateway response exceeded %d byte test limit", maxLiveGatewayResponseBytes)
	}
	return response.StatusCode, data
}

func liveVisionRequest(t *testing.T, model string) []byte {
	t.Helper()
	return liveGatewayRequest(t, map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "Describe this image in one word."},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "image/png",
					"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
				}},
			},
		}},
	})
}

func liveAnthropicComputerUseRequest(t *testing.T, model, toolType string) []byte {
	t.Helper()
	return liveGatewayRequest(t, map[string]any{
		"model":      model,
		"max_tokens": 128,
		"tools": []any{map[string]any{
			"type": toolType, "name": "computer", "display_width_px": 1024,
			"display_height_px": 768, "display_number": 1,
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "computer"},
		"messages": []any{map[string]any{
			"role": "user", "content": "Use the computer tool to take one screenshot. Do not return text.",
		}},
	})
}

func liveResponsesComputerUseRequest(t *testing.T, model string) []byte {
	t.Helper()
	return liveGatewayRequest(t, map[string]any{
		"model":      model,
		"max_tokens": 128,
		"tools": []any{map[string]any{
			"type": "computer_20250124", "name": "computer", "display_width_px": 1024,
			"display_height_px": 768, "display_number": 1,
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "computer"},
		"messages": []any{map[string]any{
			"role": "user", "content": "Take exactly one screenshot, then reply with CCR_LIVE_MANAGED_CUA_ACTION_OK. Do not click, type, scroll, move, drag, or open anything.",
		}},
	})
}

func liveGatewayRequest(t *testing.T, value map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding live gateway request: %v", err)
	}
	return encoded
}

func assertLiveGatewayTextResponse(t *testing.T, raw []byte, name string) {
	t.Helper()
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("%s response was not valid Anthropic JSON: %v", name, err)
	}
	for _, block := range response.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return
		}
	}
	t.Fatalf("%s response contained no text block", name)
}

func assertLiveGatewayComputerToolUse(t *testing.T, raw []byte, name string) {
	t.Helper()
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("%s response was not valid Anthropic JSON: %v", name, err)
	}
	for _, block := range response.Content {
		if block.Type == "tool_use" && block.Name == "computer" {
			return
		}
	}
	t.Fatalf("%s response did not contain a native computer tool call", name)
}

func requiredLiveEnvironment(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Skipf("%s is required for this explicit live verification", key)
	}
	return value
}

func requiredLiveSecretFromEnvironment(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Skipf("external CUA token environment variable %q is empty or unset", key)
	}
	return value
}

type liveScreenshotOnlyAuthorizer struct{}

func (liveScreenshotOnlyAuthorizer) Authorize(_ context.Context, _ string, action cua.Action) (cua.Decision, error) {
	if action.Kind != cua.ActionScreenshot {
		return cua.DecisionDeny, fmt.Errorf("live CUA test permits only screenshot actions, got %q", action.Kind)
	}
	return cua.DecisionApprove, nil
}

type liveCUAAuditor struct {
	mu     sync.Mutex
	events []cua.AuditEvent
}

func (a *liveCUAAuditor) Record(_ context.Context, event cua.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return nil
}

func (a *liveCUAAuditor) hasApprovedScreenshot() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, event := range a.events {
		if event.Action == cua.ActionScreenshot && event.Decision == cua.DecisionApprove && event.Status == "approved" {
			return true
		}
	}
	return false
}

func requireLiveConfiguredModel(t *testing.T, ctx context.Context, aliasEnv string) store.Model {
	t.Helper()
	if os.Getenv("CCR_LIVE_CONFIGURED_PROVIDER") != "1" {
		t.Skip("set CCR_LIVE_CONFIGURED_PROVIDER=1 to use configured real-provider aliases")
	}
	alias := strings.TrimSpace(os.Getenv(aliasEnv))
	if alias == "" {
		alias = strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_MODEL_ALIAS"))
	}
	if alias == "" {
		t.Skipf("%s or CCR_LIVE_CONFIGURED_MODEL_ALIAS is required", aliasEnv)
	}
	dbPath := configuredLiveDBPath(t)
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Skipf("configured provider database is unavailable: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Skipf("configured provider database migration failed: %v", err)
	}
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		t.Skipf("configured model alias %q is unavailable: %v", alias, err)
	}
	if model.Status == "blocked" {
		t.Skipf("configured model alias %q is blocked", alias)
	}
	return model
}

func modelSupportsImage(model store.Model) bool {
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return false
	}
	if effective.Values.SupportsVision != nil && *effective.Values.SupportsVision {
		return true
	}
	for _, modality := range effective.Values.InputModalities {
		if modality == "image" {
			return true
		}
	}
	return false
}

func modelSupportsResponsesCUA(model store.Model) bool {
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return false
	}
	values := effective.Values
	return values.Kind == modelcap.KindResponses &&
		values.SupportsResponses != nil && *values.SupportsResponses &&
		values.SupportsComputerUse != nil && *values.SupportsComputerUse
}

func acquireLiveDesktopLock(t *testing.T) {
	t.Helper()
	lockPath := filepath.Join(os.TempDir(), "ccr-live-desktop.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Skipf("desktop lock %s unavailable: %v", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		t.Skipf("desktop lock %s is held by another live CUA test: %v", lockPath, err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	})
}
