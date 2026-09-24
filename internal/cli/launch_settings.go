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
	ClaudeConfigDir              string
	GatewayURL                   string
	StatuslineExecutable         string
}

type launchSettingsResult struct {
	JSON            string
	StatuslineState string
}

const claudeCCRModelBehavesAs = "sonnet"

type claudeModelPickerOption struct {
	Model       string `json:"model"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	BehavesAs   string `json:"behavesAs,omitempty"`
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
	if err := addLaunchAvailableModels(ctx, s, options.IncludeToolDisabled, options.ClaudeConfigDir, settings); err != nil {
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
	statusline, statuslineState, err := claudeStatuslineSettingForConfigDir(options.ClaudeConfigDir)
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

func addLaunchAvailableModels(ctx context.Context, s *store.Store, includeToolDisabled bool, configDirOverride string, settings map[string]any) error {
	existing, configured, err := claudeAvailableModelsForConfigDir(configDirOverride)
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
	picker, err := launchModelPickerOptionsForConfigDir(configDirOverride, models)
	if err != nil {
		return fmt.Errorf("building Claude Code model picker mappings: %w", err)
	}
	settings["modelPicker"] = picker
	return nil
}

func launchModelPickerOptions(models []store.Model) (map[string]any, error) {
	return launchModelPickerOptionsForConfigDir("", models)
}

func launchModelPickerOptionsForConfigDir(configDirOverride string, models []store.Model) (map[string]any, error) {
	existing, replaceBuiltInOptions, err := claudeModelPickerSettingsForConfigDir(configDirOverride)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(models))
	for index := range models {
		id, err := gateway.DiscoveryIDForModel(models[index])
		if err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	options := make([]any, 0, len(existing)+len(models))
	for _, raw := range existing {
		var row struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("parsing Claude Code modelPicker row: %w", err)
		}
		if _, replace := ids[strings.TrimSpace(row.Model)]; replace {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("parsing Claude Code modelPicker row value: %w", err)
		}
		options = append(options, value)
	}
	for index := range models {
		id, err := gateway.DiscoveryIDForModel(models[index])
		if err != nil {
			return nil, err
		}
		options = append(options, claudeModelPickerOption{
			Model: id, Label: "CCR " + models[index].Alias,
			Description: "CCR-routed provider model " + models[index].ProviderModel,
			BehavesAs:   claudeCCRModelBehavesAs,
		})
	}
	result := map[string]any{
		"options":               options,
		"replaceBuiltInOptions": replaceBuiltInOptions,
	}
	return result, nil
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
