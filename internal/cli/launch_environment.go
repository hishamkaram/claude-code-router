package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelrouting"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type launchEnvironmentOptions struct {
	GatewayURL        string
	Token             string
	ObserverToken     string
	LaunchID          int64
	ModelAlias        string
	ModelID           string
	DisableTools      bool
	AuthMode          string
	ClaudeAccountName string
	ExternalTokenEnv  string
}

// launchClaudeEnv owns CCR's immutable overlay. Ordinary caller variables are
// inherited by the workload launchers; only explicit entries below change them.
func launchClaudeEnv(options launchEnvironmentOptions) launchEnvironment {
	env := launchEnvironment{}
	for _, name := range cloudProviderSelectors() {
		env.unset(name, true)
	}
	for _, name := range []string{statuslineGatewayURLEnv, statuslineTokenEnv, statuslineClaudeAccountEnv} {
		env.unset(name, false)
	}
	if options.ExternalTokenEnv != "" {
		env.unset(options.ExternalTokenEnv, false)
	}
	env.set("ANTHROPIC_BASE_URL", options.GatewayURL, true)
	env.set("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1", true)
	env.set("CCR_LAUNCH_ID", fmt.Sprint(options.LaunchID), false)
	if options.ObserverToken != "" {
		env.set(statuslineGatewayURLEnv, options.GatewayURL, false)
		env.set(statuslineTokenEnv, options.ObserverToken, false)
	}
	if options.DisableTools {
		env.set("ENABLE_TOOL_SEARCH", "", true)
		env.set("CLAUDE_CODE_SIMPLE", "1", true)
	} else {
		// This default enables deferred MCP tools behind non-first-party gateways.
		// A native user setting may still opt out.
		env.set("ENABLE_TOOL_SEARCH", "true", false)
	}
	addLaunchAuthentication(&env, options)
	if options.ModelID != "" {
		env.set("ANTHROPIC_CUSTOM_MODEL_OPTION", options.ModelID, true)
		env.set("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "CCR "+options.ModelAlias, true)
		env.set("ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION", "Model alias registered in ccr", true)
		for name, value := range modelrouting.DefaultModelEnvironment() {
			env.set(name, value, true)
		}
	}
	return env
}

func addLaunchAuthentication(env *launchEnvironment, options launchEnvironmentOptions) {
	switch options.AuthMode {
	case launchAuthModeProviderOnly, launchAuthModeGatewayToken, launchAuthModeSubscriptionPool:
		for _, name := range []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS",
			"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
		} {
			env.unset(name, false)
		}
		env.set("ANTHROPIC_AUTH_TOKEN", options.Token, false)
		if options.AuthMode == launchAuthModeSubscriptionPool {
			env.set("ANTHROPIC_CUSTOM_HEADERS", gatewaySessionHeaderValue(options.Token), false)
			env.set(statuslineClaudeAccountEnv, options.ClaudeAccountName, false)
		}
	default:
		env.unset("ANTHROPIC_AUTH_TOKEN", false)
		env.set("ANTHROPIC_CUSTOM_HEADERS", launchAnthropicCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), options.Token), false)
	}
}

func managedCUAExternalToken(invocation launchInvocation) string {
	if invocation.cuaConfig.Mode != cua.ModeManaged || invocation.cuaTokenEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(invocation.cuaTokenEnv))
}

func launchClaudeModelID(model store.Model) (string, error) {
	return gateway.DiscoveryIDForModel(model)
}

func claudeAvailableModels() (models []string, configured bool, resultErr error) {
	for _, path := range claudeSettingsPaths() {
		fileModels, found, err := settingsFileAvailableModels(path)
		if err != nil {
			return nil, false, err
		}
		if found {
			configured = true
			models = append(models, fileModels...)
		}
	}
	return models, configured, nil
}

func claudeSettingsPaths() []string {
	paths := []string{}
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "settings.json"), filepath.Join(configDir, "settings.local.json"))
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude", "settings.local.json"))
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		paths = append(paths, filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	return paths
}

func settingsFileAvailableModels(path string) (models []string, configured bool, resultErr error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading Claude Code settings %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, false, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, false, fmt.Errorf("parsing Claude Code settings %s: %w", path, err)
	}
	raw, ok := settings["availableModels"]
	if !ok {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, false, fmt.Errorf("parsing Claude Code settings %s availableModels: %w", path, err)
	}
	return models, true, nil
}

func launchAnthropicCustomHeaders(existing, token string) string {
	header := gatewaySessionHeaderValue(token)
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "X-CCR-Session-Token") {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines, header)
	return strings.Join(lines, "\n")
}

func gatewaySessionHeaderValue(token string) string {
	return "X-CCR-Session-Token: " + token
}
