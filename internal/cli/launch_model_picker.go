package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type claudeModelPicker struct {
	Options               []claudeModelPickerOption `json:"options"`
	ReplaceBuiltInOptions *bool                     `json:"replaceBuiltInOptions,omitempty"`
}

type claudeModelPickerOption struct {
	Model       string `json:"model"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	BehavesAs   string `json:"behavesAs,omitempty"`
}

func addLaunchModelPicker(settings map[string]any, ids, claudeArgs []string, pinned bool) error {
	if len(ids) == 0 {
		return nil
	}
	picker, err := userClaudeModelPicker(claudeArgs)
	if err != nil {
		return err
	}
	merged := make([]claudeModelPickerOption, 0, len(picker.Options)+len(ids))
	seen := make(map[string]struct{}, len(picker.Options)+len(ids))
	for _, option := range picker.Options {
		// Native validation, picker deduplication and labels match trimmed IDs.
		// Preserve the user's original row and metadata for that identity.
		id := strings.TrimSpace(option.Model)
		if pinned && !slices.Contains(ids, id) {
			continue
		}
		merged = append(merged, option)
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, exists := seen[id]; !exists {
			merged = append(merged, claudeModelPickerOption{Model: id})
			seen[id] = struct{}{}
		}
	}
	picker.Options = merged
	settings["modelPicker"] = picker
	return nil
}

// Native Claude trusts picker configuration only from user, flag, or managed
// settings. Never promote project/local rows into our flag-settings layer.
func userClaudeModelPicker(claudeArgs []string) (claudeModelPicker, error) {
	sources, err := parseNativeSettingsSources(claudeArgs)
	if err != nil {
		return claudeModelPicker{}, err
	}
	if !sources.UserEnabled {
		return claudeModelPicker{}, nil
	}
	directory := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if directory == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return claudeModelPicker{}, fmt.Errorf("finding Claude Code user settings: %w", homeErr)
		}
		directory = filepath.Join(home, ".claude")
	}
	path := filepath.Join(directory, "settings.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || err == nil && strings.TrimSpace(string(data)) == "" {
		return claudeModelPicker{}, nil
	}
	if err != nil {
		return claudeModelPicker{}, fmt.Errorf("reading Claude Code user model picker: %w", err)
	}
	var settings struct {
		Picker json.RawMessage `json:"modelPicker"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return claudeModelPicker{}, fmt.Errorf("parsing Claude Code user settings %s modelPicker: %w", path, err)
	}
	if len(settings.Picker) == 0 {
		return claudeModelPicker{}, nil
	}
	var picker claudeModelPicker
	if err := json.Unmarshal(settings.Picker, &picker); err != nil {
		return claudeModelPicker{}, fmt.Errorf("parsing Claude Code user settings %s modelPicker: %w", path, err)
	}
	if err := validateClaudeModelPickerFields(picker, settings.Picker); err != nil {
		return claudeModelPicker{}, fmt.Errorf("invalid Claude Code user settings %s: %w", path, err)
	}
	return picker, nil
}

// Typed JSON decoding treats null strings and bool pointers as zero values;
// native optional picker fields accept omission but reject explicit null.
func validateClaudeModelPickerFields(decoded claudeModelPicker, data json.RawMessage) error {
	if decoded.Options == nil {
		return fmt.Errorf("modelPicker.options must be an array")
	}
	for index, option := range decoded.Options {
		if strings.TrimSpace(option.Model) == "" {
			return fmt.Errorf("modelPicker.options[%d].model must be non-empty", index)
		}
	}
	var picker struct {
		Options []struct {
			Label       json.RawMessage `json:"label"`
			Description json.RawMessage `json:"description"`
			BehavesAs   json.RawMessage `json:"behavesAs"`
		} `json:"options"`
		ReplaceBuiltInOptions json.RawMessage `json:"replaceBuiltInOptions"`
	}
	if err := json.Unmarshal(data, &picker); err != nil {
		return fmt.Errorf("parsing modelPicker fields: %w", err)
	}
	if strings.TrimSpace(string(picker.ReplaceBuiltInOptions)) == "null" {
		return fmt.Errorf("modelPicker.replaceBuiltInOptions must be a boolean when present")
	}
	for index, option := range picker.Options {
		for _, field := range []struct {
			name  string
			value json.RawMessage
		}{
			{"label", option.Label},
			{"description", option.Description},
			{"behavesAs", option.BehavesAs},
		} {
			if strings.TrimSpace(string(field.value)) == "null" {
				return fmt.Errorf("modelPicker.options[%d].%s must be a string when present", index, field.name)
			}
		}
	}
	return nil
}

type nativeSettingsSources struct {
	UserEnabled bool
	PrefixArgs  int
}

// Forwarded argv owns source selection. Only its leading source-control prefix
// is unambiguous; a later matching token might be another option's private value.
func parseNativeSettingsSources(args []string) (nativeSettingsSources, error) {
	result := nativeSettingsSources{UserEnabled: true}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" && index == result.PrefixArgs {
			return result, nil
		}
		value, found := strings.CutPrefix(arg, "--setting-sources=")
		if !found && arg != "--setting-sources" {
			continue
		}
		if index != result.PrefixArgs {
			return nativeSettingsSources{}, fmt.Errorf("put --setting-sources before other Claude Code options; use --append-system-prompt=<text> for literal flag-like prompt text")
		}
		if arg == "--setting-sources" {
			if index+1 >= len(args) || args[index+1] == "--" {
				return nativeSettingsSources{}, fmt.Errorf("--setting-sources requires a value; use an empty string to disable user, project and local settings")
			}
			index++
			value = args[index]
		}
		userEnabled, err := nativeSourcesIncludeUser(value)
		if err != nil {
			return nativeSettingsSources{}, err
		}
		result.UserEnabled = userEnabled
		result.PrefixArgs = index + 1
	}
	return result, nil
}

func nativeSourcesIncludeUser(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	userEnabled := false
	for _, source := range strings.Split(value, ",") {
		switch strings.TrimSpace(source) {
		case "user":
			userEnabled = true
		case "project", "local":
		default:
			return false, fmt.Errorf("--setting-sources accepts only user, project and local, separated by commas, or an empty string")
		}
	}
	return userEnabled, nil
}
