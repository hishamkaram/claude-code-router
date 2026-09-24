package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func isClaudeSettingsFile(relative string) bool {
	clean := filepath.Clean(relative)
	return clean == "settings.json" || clean == "settings.local.json"
}

func copyClaudeSettingsFileWithOptions(source, destination string, mode os.FileMode, options claudeProfileCopyOptions) error {
	raw, err := readClaudeProfileFile(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading Claude settings file %s: %w", source, err)
	}
	encoded, err := sanitizedClaudeSettings(raw, source, options)
	if err != nil {
		return err
	}
	if err := writeClaudeProfileFile(destination, encoded, mode); err != nil {
		return fmt.Errorf("writing private Claude settings file %s: %w", destination, err)
	}
	return nil
}

func refreshClaudeSettingsFile(source, destination string, options claudeProfileCopyOptions) error {
	sourceInfo, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return removeClaudeSettingsFile(destination)
	}
	if err != nil {
		return fmt.Errorf("inspecting native Claude settings file %s: %w", source, err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		// The initial profile copy intentionally omits symlinked settings so an
		// arbitrary link cannot import configuration from outside the native
		// profile. Reuse must preserve that same visible degradation instead of
		// turning a previously valid session into a hard failure.
		recordClaudeProfileDegradation(options.degradations, filepath.Base(source))
		return removeClaudeSettingsFile(destination)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("native Claude settings file %s is not a regular file", source)
	}
	sourceRaw, err := readClaudeProfileFile(source)
	if err != nil {
		return fmt.Errorf("reading native Claude settings file %s: %w", source, err)
	}
	encoded, err := sanitizedClaudeSettings(sourceRaw, source, options)
	if err != nil {
		return err
	}

	destinationInfo, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return writeClaudeSettings(destination, encoded, 0o600)
	}
	if err != nil {
		return fmt.Errorf("inspecting private Claude settings file %s: %w", destination, err)
	}
	if destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.Mode().IsRegular() {
		return fmt.Errorf("private Claude settings file %s is not a regular file", destination)
	}
	destinationRaw, err := readClaudeProfileFile(destination)
	if err != nil {
		return fmt.Errorf("reading private Claude settings file %s: %w", destination, err)
	}
	var destinationDocument map[string]any
	if err := json.Unmarshal(destinationRaw, &destinationDocument); err != nil {
		// Do not silently repair or replace a corrupted private profile. The
		// caller can inspect the visible error and decide whether to remove the
		// profile; detached conversation state remains recoverable meanwhile.
		return fmt.Errorf("parsing private Claude settings file %s: %w", destination, err)
	}
	// Settings are bootstrap/customization input, not detached conversation
	// state. The native profile is their source of truth; replacing the complete
	// sanitized document removes permissions, model-picker, and env values that
	// the user deleted natively instead of retaining stale private settings.
	if err := writeClaudeSettings(destination, encoded, destinationInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("refreshing private Claude settings file %s: %w", destination, err)
	}
	return nil
}

func sanitizedClaudeSettings(raw []byte, source string, options claudeProfileCopyOptions) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parsing Claude settings file %s: %w", source, err)
	}
	sanitizeClaudeSettingsEnvironmentWithOptions(document, options)
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encoding private Claude settings file %s: %w", source, err)
	}
	return encoded, nil
}

func writeClaudeSettings(destination string, encoded []byte, mode os.FileMode) error {
	if err := writeClaudeProfileFile(destination, encoded, mode); err != nil {
		return fmt.Errorf("writing private Claude settings file %s: %w", destination, err)
	}
	return nil
}

func removeClaudeSettingsFile(destination string) error {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting private Claude settings file %s: %w", destination, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("private Claude settings file %s is not a regular file", destination)
	}
	if err := os.Remove(destination); err != nil {
		return fmt.Errorf("removing stale private Claude settings file %s: %w", destination, err)
	}
	return nil
}

func sanitizeClaudeSettingsEnvironmentWithOptions(document map[string]any, options claudeProfileCopyOptions) {
	env, ok := document["env"].(map[string]any)
	if !ok {
		return
	}
	for name := range env {
		if shouldRemoveClaudeSettingsEnvironment(name, options) {
			delete(env, name)
		}
	}
}

func shouldRemoveClaudeSettingsEnvironment(name string, options claudeProfileCopyOptions) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if options.preserveAnthropicAPIKey && upper == "ANTHROPIC_API_KEY" {
		return false
	}
	if isConfiguredProviderSecretEnvironment(upper, options) {
		return true
	}
	return isCCRManagedClaudeEnvironmentName(upper)
}

func isConfiguredProviderSecretEnvironment(name string, options claudeProfileCopyOptions) bool {
	for _, configured := range options.providerSecretEnvNames {
		if strings.EqualFold(strings.TrimSpace(configured), name) {
			return true
		}
	}
	return false
}

func isCCRManagedClaudeEnvironmentName(name string) bool {
	switch {
	case name == "CLAUDE_CONFIG_DIR",
		name == "CLAUDE_CODE_USE_GATEWAY",
		name == "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY",
		name == "CLAUDE_CODE_SIMPLE",
		name == "ENABLE_TOOL_SEARCH":
		return true
	case strings.HasPrefix(name, "ANTHROPIC_"),
		strings.HasPrefix(name, "CLAUDE_CODE_OAUTH_"),
		strings.HasPrefix(name, "CLAUDE_CODE_SUBAGENT_"),
		strings.HasPrefix(name, "CCR_"):
		return true
	default:
		return false
	}
}

func copyClaudeBootstrapState(source, destination string) error {
	raw, err := readClaudeProfileFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Claude bootstrap state %s: %w", source, err)
	}
	var document map[string]any
	if parseErr := json.Unmarshal(raw, &document); parseErr != nil {
		return fmt.Errorf("parsing Claude bootstrap state %s: %w", source, parseErr)
	}
	sanitized := sanitizedClaudeBootstrapState(document)
	if len(sanitized) == 0 {
		return nil
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return fmt.Errorf("encoding Claude bootstrap state: %w", err)
	}
	if writeErr := writeClaudeProfileFile(destination, encoded, 0o600); writeErr != nil {
		return fmt.Errorf("writing private Claude bootstrap state: %w", writeErr)
	}
	return nil
}

func sanitizedClaudeBootstrapState(document map[string]any) map[string]any {
	sanitized := make(map[string]any, 4)
	for _, key := range []string{"hasCompletedOnboarding", "theme", "preferredLanguage"} {
		if value, ok := document[key]; ok {
			sanitized[key] = value
		}
	}
	if projects, ok := document["projects"].(map[string]any); ok {
		safeProjects := make(map[string]any, len(projects))
		for project, rawState := range projects {
			safeState := sanitizedClaudeProjectState(rawState)
			if len(safeState) > 0 {
				safeProjects[project] = safeState
			}
		}
		if len(safeProjects) > 0 {
			sanitized["projects"] = safeProjects
		}
	}
	// MCP servers are user configuration, not runtime/session state. Preserve
	// them so the isolated CCR profile has the same tools as native Claude.
	// OAuth credentials and other Claude auth state remain deliberately
	// excluded by the surrounding bootstrap whitelist.
	if servers, ok := document["mcpServers"].(map[string]any); ok && len(servers) > 0 {
		sanitized["mcpServers"] = servers
	}
	return sanitized
}

func sanitizedClaudeProjectState(rawState any) map[string]any {
	state, ok := rawState.(map[string]any)
	if !ok {
		return nil
	}
	sanitized := make(map[string]any, 2)
	for _, key := range []string{"hasTrustDialogAccepted", "projectOnboardingSeenCount"} {
		if value, ok := state[key]; ok {
			sanitized[key] = value
		}
	}
	return sanitized
}
