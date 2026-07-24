package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	cuaexecutor "github.com/hishamkaram/claude-code-router/internal/cua/executor"
)

type launchInvocation struct {
	modelAlias     string
	printMode      bool
	authMode       string
	claudeAccount  string
	permissionMode string
	dbPath         string
	dbPathSet      bool
	noHistory      bool
	noLifecycle    bool
	noStatusline   bool
	cuaConfig      cua.Config
	cuaExternalURL string
	cuaTokenEnv    string
	cuaModeSet     bool
	cuaExecutorSet bool
	cuaLimitsSet   bool
	cuaURLSet      bool
	cuaTokenEnvSet bool
	help           bool
	claudeArgs     []string
}

func (invocation launchInvocation) claudeMetadataArgs() ([]string, bool) {
	if invocation.modelAlias != "" || invocation.printMode ||
		invocation.authMode != launchAuthModePreserve || invocation.claudeAccount != "" ||
		invocation.permissionMode != "" ||
		invocation.noHistory || invocation.noLifecycle || invocation.noStatusline ||
		invocation.cuaOptionsConfigured() {
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

func (invocation launchInvocation) cuaOptionsConfigured() bool {
	return invocation.cuaModeSet || invocation.cuaExecutorSet ||
		invocation.cuaLimitsSet || invocation.cuaURLSet || invocation.cuaTokenEnvSet
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
	if err := normalizeLaunchCUAOptions(&invocation); err != nil {
		return launchInvocation{}, err
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
	if handled, err := parseLaunchCUAOption(invocation, args, index); handled {
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
	return parseLaunchStringOption(invocation, args, index)
}

func parseLaunchStringOption(invocation *launchInvocation, args []string, index *int) (bool, error) {
	arg := args[*index]
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

func parseLaunchCUAOption(invocation *launchInvocation, args []string, index *int) (bool, error) {
	arg := args[*index]
	option, value, inline := strings.Cut(arg, "=")
	if !isLaunchCUAOption(option) {
		return false, nil
	}
	if !inline {
		var err error
		value, *index, err = launchOptionValue(args, *index, option)
		if err != nil {
			return true, err
		}
	}
	return true, setLaunchCUAOption(invocation, option, value)
}

func isLaunchCUAOption(option string) bool {
	switch option {
	case "--ccr-cua-mode", "--ccr-cua-executor", "--ccr-cua-external-url",
		"--ccr-cua-external-token-env", "--ccr-cua-max-turns", "--ccr-cua-max-actions", "--ccr-cua-timeout":
		return true
	default:
		return false
	}
}

func setLaunchCUAOption(invocation *launchInvocation, option, value string) error {
	switch option {
	case "--ccr-cua-mode":
		invocation.cuaConfig.Mode = cua.Mode(value)
		invocation.cuaModeSet = true
	case "--ccr-cua-executor":
		executor, err := parseLaunchCUAExecutor(value)
		if err != nil {
			return err
		}
		invocation.cuaConfig.Executor = executor
		invocation.cuaExecutorSet = true
	case "--ccr-cua-external-url":
		externalURL, err := parseLaunchCUAExternalURL(value)
		if err != nil {
			return err
		}
		invocation.cuaExternalURL = externalURL
		invocation.cuaURLSet = true
	case "--ccr-cua-external-token-env":
		tokenEnv, err := parseLaunchCUAExternalTokenEnv(value)
		if err != nil {
			return err
		}
		invocation.cuaTokenEnv = tokenEnv
		invocation.cuaTokenEnvSet = true
	case "--ccr-cua-max-turns":
		return setLaunchCUAPositiveInt(invocation, &invocation.cuaConfig.MaxTurns, option, value)
	case "--ccr-cua-max-actions":
		return setLaunchCUAPositiveInt(invocation, &invocation.cuaConfig.MaxActions, option, value)
	case "--ccr-cua-timeout":
		return setLaunchCUATimeout(invocation, option, value)
	}
	return nil
}

func parseLaunchCUAExecutor(value string) (string, error) {
	value = strings.TrimSpace(value)
	if _, err := cua.ParseExecutor(value); err != nil {
		return "", err
	}
	if _, err := cuaexecutor.ParseTarget(value); err != nil {
		return "", err
	}
	return value, nil
}

func parseLaunchCUAExternalURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("--ccr-cua-external-url must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("--ccr-cua-external-url must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(value, "#") {
		return "", fmt.Errorf("--ccr-cua-external-url must not include query or fragment")
	}
	if err := validateLaunchURLPort(parsed); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func parseLaunchCUAExternalTokenEnv(value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := validateEnvName(value); err != nil {
		return "", fmt.Errorf("--ccr-cua-external-token-env: %w", err)
	}
	if reservedLaunchExternalTokenEnvName(value) {
		return "", fmt.Errorf("--ccr-cua-external-token-env %q is reserved by CCR or Claude Code launch environment", value)
	}
	return value, nil
}

func reservedLaunchExternalTokenEnvName(value string) bool {
	switch value {
	case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_CUSTOM_MODEL_OPTION", "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
		"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_USE_GATEWAY",
		"ENABLE_TOOL_SEARCH", "CCR_LAUNCH_ID", statuslineGatewayURLEnv, statuslineTokenEnv:
		return true
	default:
		return false
	}
}

func validateLaunchURLPort(parsed *url.URL) error {
	port := parsed.Port()
	if port == "" {
		return nil
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("--ccr-cua-external-url port must be between 1 and 65535")
	}
	return nil
}

func parseLaunchPositiveInt(option, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid value for %s: %w", option, err)
	}
	if parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", option)
	}
	return parsed, nil
}

func setLaunchCUAPositiveInt(invocation *launchInvocation, target *int, option, value string) error {
	parsed, err := parseLaunchPositiveInt(option, value)
	if err != nil {
		return err
	}
	*target = parsed
	invocation.cuaLimitsSet = true
	return nil
}

func setLaunchCUATimeout(invocation *launchInvocation, option, value string) error {
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", option, err)
	}
	if timeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", option)
	}
	invocation.cuaConfig.Timeout = timeout
	invocation.cuaLimitsSet = true
	return nil
}

func normalizeLaunchCUAOptions(invocation *launchInvocation) error {
	config, err := invocation.cuaConfig.Normalize()
	if err != nil {
		return err
	}
	invocation.cuaConfig = config
	if !invocation.cuaOptionsConfigured() {
		return nil
	}
	if err := validateLaunchCUAModeOptions(invocation, config); err != nil {
		return err
	}
	return validateLaunchCUAExecutorOptions(invocation, config)
}

func validateLaunchCUAModeOptions(invocation *launchInvocation, config cua.Config) error {
	if invocation.cuaManagedOptionSet() && config.Mode != cua.ModeManaged {
		return fmt.Errorf("managed CUA options require --ccr-cua-mode managed")
	}
	if config.Mode == cua.ModeManaged && !invocation.cuaExecutorSet {
		return fmt.Errorf("--ccr-cua-mode managed requires --ccr-cua-executor")
	}
	return nil
}

func validateLaunchCUAExecutorOptions(invocation *launchInvocation, config cua.Config) error {
	if !invocation.cuaExecutorSet {
		return nil
	}
	kind, err := cua.ParseExecutor(config.Executor)
	if err != nil {
		return err
	}
	if kind == cua.ExecutorExternal {
		return validateLaunchExternalCUAOptions(invocation)
	}
	if invocation.cuaURLSet {
		return fmt.Errorf("--ccr-cua-external-url requires --ccr-cua-executor external:<name>")
	}
	if invocation.cuaTokenEnvSet {
		return fmt.Errorf("--ccr-cua-external-token-env requires --ccr-cua-executor external:<name>")
	}
	return nil
}

func validateLaunchExternalCUAOptions(invocation *launchInvocation) error {
	if !invocation.cuaURLSet {
		return fmt.Errorf("--ccr-cua-executor external:<name> requires --ccr-cua-external-url")
	}
	if !invocation.cuaTokenEnvSet {
		return fmt.Errorf("--ccr-cua-executor external:<name> requires --ccr-cua-external-token-env")
	}
	return nil
}

func (invocation launchInvocation) cuaManagedOptionSet() bool {
	return invocation.cuaExecutorSet || invocation.cuaLimitsSet || invocation.cuaURLSet || invocation.cuaTokenEnvSet
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
	case "--claude-account":
		return &invocation.claudeAccount, nil
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
	if option := findLaunchOption(args, "--model", "--auth-mode", "--claude-account", "--permission-mode", "--print", "-p", "--db", "--no-history", "--no-lifecycle", "--no-statusline"); option != "" {
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
