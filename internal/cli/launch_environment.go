package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type launchEnvironmentOptions struct {
	GatewayURL             string
	Token                  string
	ObserverToken          string
	LaunchID               int64
	ModelAlias             string
	ModelID                string
	DisableTools           bool
	AuthMode               string
	ProviderSecretEnvNames []string
	ExternalTokenEnv       string
}

func launchClaudeEnv(options launchEnvironmentOptions) ClaudeEnvironment {
	unset := []string{"CLAUDE_CODE_USE_GATEWAY"}
	for _, name := range options.ProviderSecretEnvNames {
		if options.AuthMode == launchAuthModePreserve && name == "ANTHROPIC_API_KEY" {
			continue
		}
		unset = append(unset, name)
	}
	if options.ExternalTokenEnv != "" {
		unset = append(unset, options.ExternalTokenEnv)
	}
	env := ClaudeEnvironment{Set: make([]string, 0, 13), Unset: unset}
	env.Set = append(
		env.Set,
		"ANTHROPIC_BASE_URL="+options.GatewayURL,
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		statuslineGatewayURLEnv+"="+options.GatewayURL,
		statuslineTokenEnv+"="+options.ObserverToken,
		fmt.Sprintf("CCR_LAUNCH_ID=%d", options.LaunchID),
	)
	if options.DisableTools {
		env.Set = append(env.Set, "ENABLE_TOOL_SEARCH=")
	} else {
		// Claude Code disables deferred MCP tool search behind non-first-party gateways
		// unless this is enabled. CCR translates the resulting tool_reference blocks.
		env.Set = append(env.Set, "ENABLE_TOOL_SEARCH=true")
	}
	if options.AuthMode == launchAuthModeGatewayToken {
		env.Unset = append(env.Unset, "ANTHROPIC_API_KEY")
		env.Set = append(env.Set, "ANTHROPIC_AUTH_TOKEN="+options.Token)
	} else {
		env.Unset = append(env.Unset, "ANTHROPIC_AUTH_TOKEN")
		env.Set = append(env.Set,
			"ANTHROPIC_CUSTOM_HEADERS="+launchAnthropicCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), options.Token),
		)
	}
	if options.DisableTools {
		env.Set = append(env.Set, "CLAUDE_CODE_SIMPLE=1")
	}
	if options.ModelID == "" {
		return env
	}
	env.Set = append(
		env.Set,
		"ANTHROPIC_CUSTOM_MODEL_OPTION="+options.ModelID,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=CCR "+options.ModelAlias,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=Model alias registered in ccr",
	)
	return env
}

func managedCUAExternalToken(invocation launchInvocation) string {
	if invocation.cuaConfig.Mode != cua.ModeManaged || invocation.cuaTokenEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(invocation.cuaTokenEnv))
}

func configuredProviderSecretEnvNames(ctx context.Context, s *store.Store) ([]string, error) {
	configuredProviders, err := s.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	for index := range configuredProviders {
		provider := &configuredProviders[index]
		if name, found := strings.CutPrefix(provider.SecretRef, "env:"); found && name != "" {
			names[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
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
