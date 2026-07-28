package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const observerTokenHeader = "X-CCR-Observer-Token"

type launchSettingsOptions struct {
	IncludeToolDisabled          bool
	LifecycleEnabled             bool
	StatuslineEnabled            bool
	IsolateStatuslineCredentials bool
	GatewayURL                   string
	StatuslineExecutable         string
}

type launchSettingsResult struct {
	JSON            string
	StatuslineState string
}

type claudeHookHandler struct {
	Type           string            `json:"type"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	AllowedEnvVars []string          `json:"allowedEnvVars"`
	Timeout        int               `json:"timeout"`
}

type claudeHookMatcher struct {
	Hooks []claudeHookHandler `json:"hooks"`
}

func launchClaudeSettingsArg(ctx context.Context, s *store.Store, options launchSettingsOptions) (launchSettingsResult, error) {
	settings := make(map[string]any, 3)
	result := launchSettingsResult{}
	if err := addLaunchAvailableModels(ctx, s, options.IncludeToolDisabled, settings); err != nil {
		return launchSettingsResult{}, err
	}
	if options.LifecycleEnabled {
		if strings.TrimSpace(options.GatewayURL) == "" {
			return launchSettingsResult{}, fmt.Errorf("building Claude Code lifecycle hooks: gateway URL is required")
		}
		settings["hooks"] = launchHookSettings(options.GatewayURL)
	}
	statuslineState, err := addLaunchStatusline(settings, options)
	if err != nil {
		return launchSettingsResult{}, err
	}
	result.StatuslineState = statuslineState
	if len(settings) == 0 {
		return result, nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return launchSettingsResult{}, fmt.Errorf("building Claude Code settings override: %w", err)
	}
	result.JSON = string(encoded)
	return result, nil
}

func addLaunchStatusline(settings map[string]any, options launchSettingsOptions) (string, error) {
	if !options.StatuslineEnabled {
		return "disabled", nil
	}
	statusline, statuslineState, err := claudeStatuslineSetting()
	if err != nil {
		return "", err
	}
	if statuslineState == claudeStatuslineDisabled {
		return "disabled", nil
	}
	if statuslineState == claudeStatuslineAbsent {
		return addCCRStatusline(settings, "injected", options.StatuslineExecutable)
	}
	if !options.IsolateStatuslineCredentials {
		return "preserved", nil
	}
	if !statuslineCredentialIsolationSupported(runtime.GOOS) {
		return addCCRStatusline(settings, "replaced", options.StatuslineExecutable)
	}
	isolated, err := isolateClaudeStatuslineCredentials(statusline, options.StatuslineExecutable)
	if err != nil {
		return "", err
	}
	settings["statusLine"] = isolated
	return "isolated", nil
}

func addCCRStatusline(settings map[string]any, state, executable string) (string, error) {
	command, err := launchStatuslineCommand(executable)
	if err != nil {
		return "", err
	}
	settings["statusLine"] = map[string]any{
		"type": "command", "command": command, "padding": 0,
	}
	return state, nil
}

func addLaunchAvailableModels(ctx context.Context, s *store.Store, includeToolDisabled bool, settings map[string]any) error {
	existing, configured, err := claudeAvailableModels()
	if err != nil {
		return err
	}
	models, hasRoutable, err := routableModels(ctx, s, includeToolDisabled)
	if err != nil {
		return fmt.Errorf("building Claude Code model allowlist extension: %w", err)
	}
	if !hasRoutable {
		return nil
	}
	baseIDs := existing
	if !configured {
		baseIDs = gateway.FirstPartyAnthropicModelIDs()
	}
	ids, err := mergedClaudeModelIDs(baseIDs, models)
	if err != nil {
		return fmt.Errorf("building Claude Code model IDs: %w", err)
	}
	settings["availableModels"] = ids
	return nil
}

func mergedClaudeModelIDs(baseIDs []string, models []store.Model) ([]string, error) {
	ids := make([]string, 0, len(baseIDs)+len(models))
	seen := make(map[string]struct{}, len(baseIDs)+len(models))
	for _, id := range baseIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for index := range models {
		id, err := gateway.DiscoveryIDForModel(models[index])
		if err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func launchHookSettings(gatewayURL string) map[string][]claudeHookMatcher {
	handler := claudeHookHandler{
		Type: "http", URL: strings.TrimRight(gatewayURL, "/") + "/internal/v1/hooks",
		Headers:        map[string]string{observerTokenHeader: "${CCR_OBSERVER_TOKEN}"},
		AllowedEnvVars: []string{statuslineTokenEnv},
		Timeout:        5,
	}
	events := [...]string{
		"SessionStart", "SessionEnd", "SubagentStart", "SubagentStop",
		"TaskCreated", "TaskCompleted", "TeammateIdle", "StopFailure",
	}
	hooks := make(map[string][]claudeHookMatcher, len(events))
	for _, event := range events {
		hooks[event] = []claudeHookMatcher{{Hooks: []claudeHookHandler{handler}}}
	}
	return hooks
}

func launchStatuslineCommand(executable string) (string, error) {
	return launchHiddenStatuslineCommand(executable, "__statusline")
}

func launchStatuslineAccountCommand(executable string) (string, error) {
	return launchHiddenStatuslineCommand(executable, "__statusline-account")
}

func launchHiddenStatuslineCommand(executable, command string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("building CCR status line command: %w", err)
		}
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(executable, `"`, `""`) + `" ` + command, nil
	}
	return quotePOSIXShellArg(executable) + " " + command, nil
}
