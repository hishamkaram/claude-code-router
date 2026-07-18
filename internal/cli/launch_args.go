package cli

import (
	"fmt"
	"strconv"
	"strings"
)

type launchInvocation struct {
	modelAlias     string
	printMode      bool
	authMode       string
	permissionMode string
	dbPath         string
	dbPathSet      bool
	noHistory      bool
	noLifecycle    bool
	noStatusline   bool
	help           bool
	claudeArgs     []string
}

func (invocation launchInvocation) claudeMetadataArgs() ([]string, bool) {
	if invocation.modelAlias != "" || invocation.printMode ||
		invocation.authMode != launchAuthModePreserve || invocation.permissionMode != "" ||
		invocation.noHistory || invocation.noLifecycle || invocation.noStatusline {
		return nil, false
	}
	args := invocation.claudeArgs
	if len(args) == 2 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		return nil, false
	}
	switch args[0] {
	case "--help", "-h", "--version", "-v":
		return args, true
	default:
		return nil, false
	}
}

func parseLaunchInvocation(args []string) (launchInvocation, error) {
	invocation := launchInvocation{authMode: launchAuthModePreserve}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			invocation.claudeArgs = append(invocation.claudeArgs, args[index:]...)
			break
		}
		if arg == "--help" || arg == "-h" {
			invocation.help = true
			continue
		}
		handled, err := parseLaunchOwnedOption(&invocation, args, &index)
		if err != nil {
			return launchInvocation{}, err
		}
		if !handled {
			invocation.claudeArgs = append(invocation.claudeArgs, arg)
		}
	}
	return invocation, nil
}

func parseLaunchOwnedOption(invocation *launchInvocation, args []string, index *int) (bool, error) {
	arg := args[*index]
	switch arg {
	case "--print", "-p":
		invocation.printMode = true
		return true, nil
	case "--no-history":
		invocation.noHistory = true
		return true, nil
	case "--no-lifecycle":
		invocation.noLifecycle = true
		return true, nil
	case "--no-statusline":
		invocation.noStatusline = true
		return true, nil
	}
	if handled, err := parseLaunchDisableOption(invocation, arg); handled {
		return true, err
	}
	if value, found := launchPrintOptionValue(arg); found {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return true, fmt.Errorf("invalid value for --print: %w", err)
		}
		invocation.printMode = parsed
		return true, nil
	}

	option, value, inline := strings.Cut(arg, "=")
	target, set := launchStringOptionTarget(invocation, option)
	if target == nil {
		return false, nil
	}
	if !inline {
		var err error
		value, *index, err = launchOptionValue(args, *index, option)
		if err != nil {
			return true, err
		}
	}
	*target = value
	if set != nil {
		*set = true
	}
	return true, nil
}

func parseLaunchDisableOption(invocation *launchInvocation, arg string) (bool, error) {
	option, raw, found := strings.Cut(arg, "=")
	if !found {
		return false, nil
	}
	var target *bool
	switch option {
	case "--no-history":
		target = &invocation.noHistory
	case "--no-lifecycle":
		target = &invocation.noLifecycle
	case "--no-statusline":
		target = &invocation.noStatusline
	default:
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return true, fmt.Errorf("invalid value for %s: %w", option, err)
	}
	*target = value
	return true, nil
}

func launchPrintOptionValue(arg string) (string, bool) {
	if value, found := strings.CutPrefix(arg, "--print="); found {
		return value, true
	}
	return strings.CutPrefix(arg, "-p=")
}

func launchStringOptionTarget(invocation *launchInvocation, option string) (target *string, set *bool) {
	switch option {
	case "--model":
		return &invocation.modelAlias, nil
	case "--auth-mode":
		return &invocation.authMode, nil
	case "--permission-mode":
		return &invocation.permissionMode, nil
	case "--db":
		return &invocation.dbPath, &invocation.dbPathSet
	default:
		return nil, nil
	}
}

func launchOptionValue(args []string, index int, option string) (value string, next int, err error) {
	if index+1 >= len(args) || args[index+1] == "--" {
		return "", index, fmt.Errorf("%s requires a value", option)
	}
	return args[index+1], index + 1, nil
}

func launchClaudeArgs(modelID string, printMode, disableTools bool, settings, permissionMode string, passthroughArgs []string) []string {
	args := []string{}
	if printMode {
		args = append(args, "--print")
	}
	if permissionMode != "" {
		args = append(args, "--permission-mode", permissionMode)
	}
	if disableTools {
		args = append(args, "--tools", "")
	}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	if modelID != "" {
		args = append(args, "--model", modelID)
	}
	return append(args, passthroughArgs...)
}

func validateLaunchPassthroughArgs(args []string) error {
	if option := findLaunchOption(args, "--model", "--auth-mode", "--permission-mode", "--print", "-p", "--db", "--no-history", "--no-lifecycle", "--no-statusline"); option != "" {
		return fmt.Errorf("%s is managed by ccr launch; pass its CCR value before other Claude Code options", option)
	}
	if option := findLaunchOption(args, "--fallback-model"); option != "" {
		return fmt.Errorf("%s is not supported by ccr launch because it can bypass the selected model route", option)
	}
	if option := findEnabledLaunchBooleanOption(args, "--bg", "--background"); option != "" {
		return fmt.Errorf("%s is not supported by ccr launch because detached agents outlive the local gateway", option)
	}
	return nil
}

func validateDynamicLaunchPassthroughArgs(args []string, disableTools, hasSettings bool) error {
	if disableTools {
		if option := findLaunchOption(args, "--tools", "--mcp-config", "--plugin-dir", "--plugin-url"); option != "" {
			return fmt.Errorf("selected route does not support tools; %s cannot override CCR's tool disablement", option)
		}
	}
	if hasSettings && findLaunchOption(args, "--settings") != "" {
		return fmt.Errorf("--settings cannot override CCR's model allowlist for this launch")
	}
	return nil
}

func findLaunchOption(args []string, options ...string) string {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		for _, option := range options {
			if arg == option || strings.HasPrefix(arg, option+"=") {
				return option
			}
		}
	}
	return ""
}

func findEnabledLaunchBooleanOption(args []string, options ...string) string {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		for _, option := range options {
			if arg == option {
				return option
			}
			value, found := strings.CutPrefix(arg, option+"=")
			if !found {
				continue
			}
			enabled, err := strconv.ParseBool(value)
			if err != nil || enabled {
				return option
			}
		}
	}
	return ""
}
