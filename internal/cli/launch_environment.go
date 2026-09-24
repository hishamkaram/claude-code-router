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
	ClaudeConfigDir        string
	Token                  string
	ObserverToken          string
	LaunchID               int64
	ModelAlias             string
	ModelID                string
	DisableTools           bool
	AuthMode               string
	ClaudeAccountName      string
	ProviderSecretEnvNames []string
	ExternalTokenEnv       string
	OAuthToken             string
	OAuthRefreshToken      string
	OAuthScopes            string
}

func launchClaudeEnv(options launchEnvironmentOptions) ClaudeEnvironment {
	unset := []string{
		"CLAUDE_CODE_USE_GATEWAY",
		"CLAUDE_CONFIG_DIR",
		statuslineGatewayURLEnv,
		statuslineTokenEnv,
		statuslineClaudeAccountEnv,
	}
	for _, name := range options.ProviderSecretEnvNames {
		if options.AuthMode == launchAuthModePreserve && name == "ANTHROPIC_API_KEY" {
			continue
		}
		unset = append(unset, name)
	}
	// A CCR launch owns child routing, including sessions that select their
	// alias later with /model. Inherited Claude child-model overrides can make
	// Claude emit an explicit native child model and skip the gateway's
	// request-correlated reservation, so they must not cross this boundary.
	unset = append(unset, "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE")
	if options.ExternalTokenEnv != "" {
		unset = append(unset, options.ExternalTokenEnv)
	}
	env := ClaudeEnvironment{Set: make([]string, 0, 14), Unset: unset}
	env.Set = append(
		env.Set,
		"ANTHROPIC_BASE_URL="+options.GatewayURL,
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		fmt.Sprintf("CCR_LAUNCH_ID=%d", options.LaunchID),
	)
	if strings.TrimSpace(options.ClaudeConfigDir) != "" {
		env.Set = append(env.Set, "CLAUDE_CONFIG_DIR="+options.ClaudeConfigDir)
	}
	if options.ObserverToken != "" {
		env.Set = append(env.Set,
			statuslineGatewayURLEnv+"="+options.GatewayURL,
			statuslineTokenEnv+"="+options.ObserverToken,
		)
	}
	if options.DisableTools {
		env.Set = append(env.Set, "ENABLE_TOOL_SEARCH=")
	} else {
		// Claude Code disables deferred MCP tool search behind non-first-party gateways
		// unless this is enabled. CCR translates the resulting tool_reference blocks.
		env.Set = append(env.Set, "ENABLE_TOOL_SEARCH=true")
	}
	appendClaudeLaunchAuth(&env, options)
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

func appendClaudeLaunchAuth(env *ClaudeEnvironment, options launchEnvironmentOptions) {
	switch options.AuthMode {
	case launchAuthModeProviderOnly, launchAuthModeGatewayToken:
		env.Unset = append(env.Unset,
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_API_KEY",
			"ANTHROPIC_CUSTOM_HEADERS",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_OAUTH_SCOPES",
		)
		// Current Claude Code requires an API-key-shaped credential to enter its
		// first-party custom-endpoint discovery path. This is a generated CCR
		// session token, not a provider secret; the gateway accepts it only for
		// this launch and the explicit CCR header makes that boundary visible.
		env.Set = append(env.Set,
			"ANTHROPIC_API_KEY="+options.Token,
			"ANTHROPIC_CUSTOM_HEADERS="+gatewaySessionHeaderValue(options.Token),
		)
	case launchAuthModeSubscriptionPool:
		env.Unset = append(env.Unset,
			"ANTHROPIC_API_KEY",
			"ANTHROPIC_CUSTOM_HEADERS",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
			"CLAUDE_CODE_OAUTH_SCOPES",
		)
		env.Set = append(env.Set,
			"ANTHROPIC_AUTH_TOKEN="+options.Token,
			"ANTHROPIC_CUSTOM_HEADERS="+gatewaySessionHeaderValue(options.Token),
			statuslineClaudeAccountEnv+"="+options.ClaudeAccountName,
		)
	default:
		env.Unset = append(env.Unset, "ANTHROPIC_AUTH_TOKEN")
		env.Set = append(env.Set,
			"ANTHROPIC_CUSTOM_HEADERS="+launchAnthropicCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), options.Token),
		)
		if options.OAuthToken != "" {
			env.Set = append(env.Set, "CLAUDE_CODE_OAUTH_TOKEN="+options.OAuthToken)
			if scopes := claudeOAuthScopes(options.OAuthScopes); scopes != "" {
				if options.OAuthRefreshToken != "" {
					env.Set = append(env.Set, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN="+options.OAuthRefreshToken)
				}
				env.Set = append(env.Set, "CLAUDE_CODE_OAUTH_SCOPES="+scopes)
			}
		}
	}
	if options.DisableTools {
		env.Set = append(env.Set, "CLAUDE_CODE_SIMPLE=1")
	}
}

func claudeOAuthScopes(scopesJSON string) string {
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		return ""
	}
	clean := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope = strings.TrimSpace(scope); scope != "" {
			clean = append(clean, scope)
		}
	}
	return strings.Join(clean, " ")
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

func claudeAvailableModelsForConfigDir(configDirOverride string) (models []string, configured bool, resultErr error) {
	for _, path := range claudeSettingsPathsForConfigDir(configDirOverride) {
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
	return claudeSettingsPathsForConfigDir("")
}

func claudeSettingsPathsForConfigDir(configDirOverride string) []string {
	paths := make([]string, 0, 4)
	appendPath := func(path string) {
		path = filepath.Clean(path)
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		for _, existing := range paths {
			if existing == path {
				return
			}
		}
		paths = append(paths, path)
	}
	configDir := strings.TrimSpace(configDirOverride)
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if configDir != "" {
		appendPath(filepath.Join(configDir, "settings.json"))
		appendPath(filepath.Join(configDir, "settings.local.json"))
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		appendPath(filepath.Join(home, ".claude", "settings.json"))
		appendPath(filepath.Join(home, ".claude", "settings.local.json"))
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		appendPath(filepath.Join(cwd, ".claude", "settings.json"))
		appendPath(filepath.Join(cwd, ".claude", "settings.local.json"))
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

func claudeModelPickerSettingsForConfigDir(configDirOverride string) (options []json.RawMessage, replaceBuiltInOptions bool, resultErr error) {
	for _, path := range claudeSettingsPathsForConfigDir(configDirOverride) {
		fileOptions, fileReplace, fileReplaceConfigured, found, err := settingsFileModelPicker(path)
		if err != nil {
			return nil, false, err
		}
		if !found {
			continue
		}
		options = append(options, fileOptions...)
		if fileReplaceConfigured {
			replaceBuiltInOptions = fileReplace
		}
	}
	return options, replaceBuiltInOptions, nil
}

func settingsFileModelPicker(path string) (options []json.RawMessage, replaceBuiltInOptions, replaceConfigured, configured bool, resultErr error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, false, false, nil
	}
	if err != nil {
		return nil, false, false, false, fmt.Errorf("reading Claude Code settings %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, false, false, false, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, false, false, false, fmt.Errorf("parsing Claude Code settings %s: %w", path, err)
	}
	raw, ok := settings["modelPicker"]
	if !ok {
		return nil, false, false, false, nil
	}
	var picker struct {
		Options               []json.RawMessage `json:"options"`
		ReplaceBuiltInOptions *bool             `json:"replaceBuiltInOptions"`
	}
	if err := json.Unmarshal(raw, &picker); err != nil {
		return nil, false, false, false, fmt.Errorf("parsing Claude Code settings %s modelPicker: %w", path, err)
	}
	if picker.ReplaceBuiltInOptions == nil {
		return picker.Options, false, false, true, nil
	}
	return picker.Options, *picker.ReplaceBuiltInOptions, true, true, nil
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
