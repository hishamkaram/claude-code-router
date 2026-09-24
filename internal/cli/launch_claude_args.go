package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func nativeClaudeResumeSession(args []string) (string, error) {
	var sessionID string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if claudePassthroughOptionConsumesNextValue(args, index) {
			index++
			continue
		}
		value, matched, err := nativeClaudeResumeArgument(args, index)
		if err != nil {
			return "", err
		}
		if !matched {
			continue
		}
		if sessionID != "" {
			return "", errors.New("duplicate Claude resume options are not allowed")
		}
		sessionID = value
		if arg == "--resume" || arg == "-r" {
			index++
		}
	}
	return sessionID, nil
}

func detachedNativeSessionOption(args []string) string {
	if option := findLaunchOption(args, "--session-id", "--resume", "--continue", "--fork-session", "--no-session-persistence", "--from-pr", "--teleport"); option != "" {
		return option
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if option := detachedShortSessionOption(arg); option != "" {
			return option
		}
		if claudePassthroughOptionConsumesNextValue(args, index) {
			index++
		}
	}
	return ""
}

func detachedShortSessionOption(arg string) string {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return ""
	}
	for _, option := range arg[1:] {
		switch option {
		case 'c', 'r':
			return "-" + string(option)
		case 'p', 'h', 'v':
			// Claude bundles boolean short options; value-taking options consume the tail.
		default:
			return ""
		}
	}
	return ""
}

// nativeClaudeSessionID returns the explicit identity Claude should use for a
// new conversation. CCR uses this identity to place the isolated profile in a
// durable, session-addressable location so a later --resume can reopen it.
func nativeClaudeSessionID(args []string) (string, error) {
	var sessionID string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if claudePassthroughOptionConsumesNextValue(args, index) {
			index++
			continue
		}
		option, value, inline := strings.Cut(arg, "=")
		if option != "--session-id" {
			continue
		}
		if sessionID != "" {
			return "", errors.New("--session-id must occur exactly once")
		}
		if !inline {
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || args[index+1] == "--" || strings.HasPrefix(strings.TrimSpace(args[index+1]), "-") {
				return "", errors.New("--session-id requires an explicit session ID")
			}
			value = args[index+1]
			index++
		}
		var err error
		sessionID, err = validateNativeClaudeSessionID(value)
		if err != nil {
			return "", err
		}
	}
	return sessionID, nil
}

func validateNativeClaudeSessionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	uuidValue, err := uuid.Parse(value)
	if err != nil || uuidValue.String() != value || uuidValue.Version() != 4 {
		return "", errors.New("--session-id requires a canonical version-4 Claude session ID")
	}
	return value, nil
}

func generatedNativeClaudeSessionID() string {
	return uuid.NewString()
}

func claudeLaunchDisablesSessionPersistence(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if arg == "--no-session-persistence" || arg == "--no-session-persistence=true" {
			return true
		}
		if claudePassthroughOptionConsumesNextValue(args, index) {
			index++
		}
	}
	return false
}

// claudePassthroughOptionConsumesNextValue keeps CCR's launch scanners aligned
// with Claude Code's value-taking options. Without this boundary, a prompt or
// system-prompt value such as "--session-id" can be mistaken for CCR session
// control even though Claude owns the token as data.
func claudePassthroughOptionConsumesNextValue(args []string, index int) bool {
	if index+1 >= len(args) || args[index+1] == "--" {
		return false
	}
	arg := args[index]
	option, _, inline := strings.Cut(arg, "=")
	if inline {
		return false
	}
	switch option {
	case "-n", "--add-dir", "--agent", "--agents", "--allowedTools", "--allowed-tools",
		"--append-system-prompt", "--append-system-prompt-file", "--autocompact", "--betas",
		"--debug-file", "--disallowedTools", "--disallowed-tools", "--effort", "--environment",
		"--file", "--input-format", "--json-schema", "--max-budget-usd", "--mcp-config", "--model",
		"--name", "--output-format", "--permission-mode", "--permission-prompts", "--plugin-dir",
		"--plugin-url", "--remote-control-session-name-prefix", "--setting-sources", "--settings",
		"--system-prompt", "--system-prompt-file", "--system-prompt-snapshot", "--tools":
		return true
	default:
		return false
	}
}

func nativeClaudeResumeArgument(args []string, index int) (value string, matched bool, err error) {
	arg := args[index]
	if arg == "--continue" || arg == "-c" {
		return "", true, fmt.Errorf("%s cannot be isolated safely by CCR; use --resume=<session-id> or CCR-owned detached continuation", arg)
	}
	if value, ok := strings.CutPrefix(arg, "--resume="); ok {
		if strings.TrimSpace(value) == "" {
			return "", true, errors.New("--resume requires an explicit session ID when launched through CCR")
		}
		return validateNativeClaudeResumeSessionID(value)
	}
	if arg == "--resume" || arg == "-r" {
		if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || args[index+1] == "--" || strings.HasPrefix(strings.TrimSpace(args[index+1]), "-") {
			return "", true, fmt.Errorf("%s requires an explicit session ID when launched through CCR", arg)
		}
		return validateNativeClaudeResumeSessionID(args[index+1])
	}
	if value, ok := strings.CutPrefix(arg, "-r"); ok && value != "" {
		candidate := strings.TrimSpace(strings.TrimPrefix(value, "="))
		return validateNativeClaudeResumeSessionID(candidate)
	}
	return "", false, nil
}

func validateNativeClaudeResumeSessionID(value string) (sessionID string, matched bool, err error) {
	value = strings.TrimSpace(value)
	uuidValue, err := uuid.Parse(value)
	if err != nil || uuidValue.String() != value || uuidValue.Version() != 4 {
		return "", true, errors.New("--resume requires a canonical version-4 Claude session ID")
	}
	return value, true, nil
}

// detachedOwnerArgs serializes the already-parsed launch invocation for the
// durable owner process. CCR-owned launch options must remain available to the
// owner, while Claude passthrough arguments must be copied from the normalized
// slice so option values cannot be reclassified as CCR options on the second
// parse.
func detachedOwnerArgs(invocation launchInvocation) []string {
	args := make([]string, 0, len(invocation.claudeArgs)+16)
	args = appendDetachedOwnerModelArgs(args, invocation)
	args = appendDetachedOwnerAuthArgs(args, invocation)
	args = appendDetachedOwnerRuntimeArgs(args, invocation)
	args = appendDetachedOwnerCUAArgs(args, invocation)
	if invocation.claudeArgsSeparated {
		// The durable owner reparses these arguments as a fresh CCR invocation.
		// Keep the original boundary so a forwarded Claude option such as
		// --model cannot become the owner's CCR model selection.
		args = append(args, "--")
	}
	return append(args, invocation.claudeArgs...)
}

func appendDetachedOwnerModelArgs(args []string, invocation launchInvocation) []string {
	if invocation.modelAlias != "" {
		args = appendDetachedOwnerOption(args, "--model", invocation.modelAlias)
	}
	if invocation.printMode {
		args = append(args, "--print")
	}
	return args
}

func appendDetachedOwnerAuthArgs(args []string, invocation launchInvocation) []string {
	if invocation.authModeSet || invocation.authMode != launchAuthModeAuto {
		args = appendDetachedOwnerOption(args, "--auth-mode", invocation.authMode)
	}
	if invocation.claudeAccount != "" {
		args = appendDetachedOwnerOption(args, "--claude-account", invocation.claudeAccount)
	}
	if invocation.permissionMode != "" {
		args = appendDetachedOwnerOption(args, "--permission-mode", invocation.permissionMode)
	}
	return args
}

func appendDetachedOwnerRuntimeArgs(args []string, invocation launchInvocation) []string {
	if invocation.noHistory {
		args = append(args, "--no-history")
	}
	if invocation.noLifecycle {
		args = append(args, "--no-lifecycle")
	}
	if invocation.noStatusline {
		args = append(args, "--no-statusline")
	}
	return args
}

func appendDetachedOwnerCUAArgs(args []string, invocation launchInvocation) []string {
	if invocation.cuaModeSet {
		args = appendDetachedOwnerOption(args, "--ccr-cua-mode", string(invocation.cuaConfig.Mode))
	}
	if invocation.cuaExecutorSet {
		args = appendDetachedOwnerOption(args, "--ccr-cua-executor", invocation.cuaConfig.Executor)
	}
	if invocation.cuaLimitsSet {
		args = appendDetachedOwnerOption(args, "--ccr-cua-max-turns", strconv.Itoa(invocation.cuaConfig.MaxTurns))
		args = appendDetachedOwnerOption(args, "--ccr-cua-max-actions", strconv.Itoa(invocation.cuaConfig.MaxActions))
		args = appendDetachedOwnerOption(args, "--ccr-cua-timeout", invocation.cuaConfig.Timeout.String())
	}
	if invocation.cuaURLSet {
		args = appendDetachedOwnerOption(args, "--ccr-cua-external-url", invocation.cuaExternalURL)
	}
	if invocation.cuaTokenEnvSet {
		args = appendDetachedOwnerOption(args, "--ccr-cua-external-token-env", invocation.cuaTokenEnv)
	}
	return args
}

func appendDetachedOwnerOption(args []string, option, value string) []string {
	return append(args, option, value)
}
