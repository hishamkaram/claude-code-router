package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const observerTokenHeader = "X-CCR-Observer-Token"

type launchSettingsOptions struct {
	IncludeToolDisabled bool
	LifecycleEnabled    bool
	StatuslineEnabled   bool
	GatewayURL          string
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
	if err := addLaunchAvailableModels(ctx, s, options.IncludeToolDisabled, settings); err != nil {
		return launchSettingsResult{}, err
	}
	if options.LifecycleEnabled {
		if strings.TrimSpace(options.GatewayURL) == "" {
			return launchSettingsResult{}, fmt.Errorf("building Claude Code lifecycle hooks: gateway URL is required")
		}
		settings["hooks"] = launchHookSettings(options.GatewayURL)
	}
	statuslineState := "disabled"
	if options.StatuslineEnabled {
		configured, err := claudeStatuslineConfigured()
		if err != nil {
			return launchSettingsResult{}, err
		}
		if configured {
			statuslineState = "preserved"
		} else {
			command, err := launchStatuslineCommand()
			if err != nil {
				return launchSettingsResult{}, err
			}
			settings["statusLine"] = map[string]any{
				"type": "command", "command": command, "padding": 0,
			}
			statuslineState = "injected"
		}
	}
	if len(settings) == 0 {
		return launchSettingsResult{StatuslineState: statuslineState}, nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return launchSettingsResult{}, fmt.Errorf("building Claude Code settings override: %w", err)
	}
	return launchSettingsResult{JSON: string(encoded), StatuslineState: statuslineState}, nil
}

func addLaunchAvailableModels(ctx context.Context, s *store.Store, includeToolDisabled bool, settings map[string]any) error {
	existing, configured, err := claudeAvailableModels()
	if err != nil {
		return err
	}
	aliases, hasRoutable, err := routableModelAliases(ctx, s, includeToolDisabled)
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
	settings["availableModels"] = mergedClaudeModelIDs(baseIDs, aliases)
	return nil
}

func mergedClaudeModelIDs(baseIDs, aliases []string) []string {
	ids := make([]string, 0, len(baseIDs)+len(aliases))
	seen := make(map[string]struct{}, len(baseIDs)+len(aliases))
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
	for _, alias := range aliases {
		id := gateway.DiscoveryIDForAlias(alias)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
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

func claudeStatuslineConfigured() (bool, error) {
	for _, path := range claudeSettingsPaths() {
		configured, err := settingsFileHasNonNullKey(path, "statusLine")
		if err != nil {
			return false, err
		}
		if configured {
			return true, nil
		}
	}
	return false, nil
}

func settingsFileHasNonNullKey(path, key string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading Claude Code settings %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return false, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, fmt.Errorf("parsing Claude Code settings %s: %w", path, err)
	}
	raw, ok := settings[key]
	return ok && string(raw) != "null", nil
}

func launchStatuslineCommand() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("building CCR status line command: %w", err)
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(executable, `"`, `""`) + `" __statusline`, nil
	}
	return "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' __statusline", nil
}
