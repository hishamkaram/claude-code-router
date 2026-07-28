package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type claudeStatuslineState uint8

const (
	claudeStatuslineAbsent claudeStatuslineState = iota
	claudeStatuslinePresent
	claudeStatuslineDisabled
)

func claudeStatuslineSetting() (setting map[string]any, state claudeStatuslineState, resultErr error) {
	var effective map[string]any
	effectiveState := claudeStatuslineAbsent
	for _, path := range claudeSettingsPaths() {
		setting, found, err := settingsFileStatusline(path)
		if err != nil {
			return nil, claudeStatuslineAbsent, err
		}
		if found {
			effective = setting
			effectiveState = claudeStatuslinePresent
			if setting == nil {
				effectiveState = claudeStatuslineDisabled
			}
		}
	}
	return effective, effectiveState, nil
}

func settingsFileStatusline(path string) (statusline map[string]any, found bool, resultErr error) {
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
	raw, ok := settings["statusLine"]
	if !ok {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	if err := json.Unmarshal(raw, &statusline); err != nil {
		return nil, false, fmt.Errorf("parsing Claude Code statusLine in %s: %w", path, err)
	}
	if statusline == nil {
		return nil, false, fmt.Errorf("statusLine in Claude Code settings %s must be an object", path)
	}
	return statusline, true, nil
}

func isolateClaudeStatuslineCredentials(statusline map[string]any) (map[string]any, error) {
	statuslineType, _ := statusline["type"].(string)
	command, _ := statusline["command"].(string)
	if statuslineType != "command" || strings.TrimSpace(command) == "" {
		return nil, errors.New("existing Claude Code statusLine must have type \"command\" and a non-empty command in subscription-pool mode")
	}
	isolated := make(map[string]any, len(statusline))
	for key, value := range statusline {
		isolated[key] = value
	}
	isolated["command"] = isolatedStatuslineShellCommand(command)
	return isolated, nil
}

func statuslineCredentialIsolationSupported(goos string) bool {
	return goos != "windows"
}

func isolatedStatuslineShellCommand(command string) string {
	var isolated strings.Builder
	isolated.WriteString("env")
	for _, name := range [...]string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_CUSTOM_HEADERS",
		statuslineTokenEnv,
	} {
		isolated.WriteString(" -u ")
		isolated.WriteString(name)
	}
	isolated.WriteString(" sh -c ")
	isolated.WriteString(quotePOSIXShellArg(command))
	return isolated.String()
}

func quotePOSIXShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
