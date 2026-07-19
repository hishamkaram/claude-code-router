package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func newLaunchCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launch [Claude Code options and prompt]",
		Short: "Launch Claude Code through the local router",
		Long: `Launch Claude Code through the local router.

CCR owns --model, --auth-mode, --permission-mode, --print/-p, and --db. All
other options and positional arguments are passed to Claude Code unchanged
unless they would override CCR's selected model, generated model allowlist, or
tool-safety restrictions. For example ccr launch --chrome starts Claude Code
with its Chrome integration.

Fallback and detached background modes are rejected because they cannot preserve
CCR's selected route and local gateway ownership.

Without --model, Claude Code starts on its normal configured model. CCR adds
registered, compatible aliases to the visual /model picker alongside the
permitted Anthropic models while preserving subscription or API-key
authentication. Pass --model <alias> when you want that CCR alias to be the
startup model.

Use ccr launch --help for router-specific help. To ask Claude Code for its own
help, use ccr launch -- --help.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			invocation, err := parseLaunchInvocation(args)
			if err != nil {
				return err
			}
			if invocation.help {
				return cmd.Help()
			}
			if metadataArgs, ok := invocation.claudeMetadataArgs(); ok {
				return runClaudeMetadata(ctx, cmd, deps, metadataArgs)
			}
			if invocation.dbPathSet {
				opts.dbPath = invocation.dbPath
			}
			return runLaunch(ctx, cmd, opts, deps, invocation)
		},
	}
	cmd.Flags().String("model", "", "Optional CCR model alias to use as the startup model")
	cmd.Flags().String("auth-mode", launchAuthModePreserve, "Gateway auth mode: preserve or gateway-token")
	cmd.Flags().String("permission-mode", "", "Optional Claude Code permission mode to pass through")
	cmd.Flags().BoolP("print", "p", false, "Run Claude Code in non-interactive print mode, reading the prompt from stdin")
	cmd.Flags().Bool("no-history", false, "Disable redacted route history for this launch")
	cmd.Flags().Bool("no-lifecycle", false, "Disable Claude lifecycle hooks for this launch")
	cmd.Flags().Bool("no-statusline", false, "Disable CCR status-line injection for this launch")
	return cmd
}

func runClaudeMetadata(ctx context.Context, cmd *cobra.Command, deps Dependencies, args []string) error {
	process, err := deps.Launcher.Start(ctx, args, ClaudeEnvironment{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("running Claude Code metadata command: %w", err)
	}
	return process.Wait()
}

const (
	launchAuthModePreserve     = "preserve"
	launchAuthModeGatewayToken = "gateway-token"
)

type resolvedLaunch struct {
	modelAlias    string
	claudeModelID string
	disableTools  bool
}

type launchExecution struct {
	store         *store.Store
	launchID      int64
	server        *gateway.Server
	finalizer     *launchFinalizer
	token         string
	observerToken string
	resolved      resolvedLaunch
	invocation    launchInvocation
}

func runLaunch(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, invocation launchInvocation) (resultErr error) {
	if err := validateLaunchInputs(invocation.modelAlias, invocation.authMode, invocation.permissionMode); err != nil {
		return err
	}
	if err := validateLaunchPassthroughArgs(invocation.claudeArgs); err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	resolved, err := resolveLaunch(ctx, deps, s, invocation)
	if err != nil {
		return err
	}
	if passthroughErr := validateResolvedLaunchPassthroughArgs(ctx, s, invocation, resolved); passthroughErr != nil {
		return passthroughErr
	}
	lifecycleState, statuslineState := launchObservationStates(invocation)
	launchID, err := s.CreateLaunch(ctx, resolved.modelAlias, lifecycleState, statuslineState)
	if err != nil {
		return fmt.Errorf("creating launch record: %w", err)
	}
	finalizer := &launchFinalizer{store: s, launchID: launchID}
	execution := &launchExecution{
		store: s, launchID: launchID, finalizer: finalizer,
		resolved: resolved, invocation: invocation,
	}
	defer func() {
		if finalizer.finished {
			return
		}
		resultErr = errors.Join(resultErr, finalizer.Finish(ctx, "failed", "startup_failed", nil))
	}()

	if startErr := startObservableGateway(ctx, deps, execution); startErr != nil {
		return startErr
	}
	defer shutdownGateway(ctx, execution.server)

	claudeSettings, err := launchClaudeSettingsArg(ctx, s, launchSettingsOptions{
		IncludeToolDisabled: resolved.disableTools, LifecycleEnabled: !invocation.noLifecycle,
		StatuslineEnabled: !invocation.noStatusline, GatewayURL: execution.server.URL(),
	})
	if err != nil {
		return err
	}
	if statusErr := s.SetLaunchStatuslineState(ctx, launchID, claudeSettings.StatuslineState); statusErr != nil {
		return statusErr
	}
	if passthroughErr := validateDynamicLaunchPassthroughArgs(invocation.claudeArgs, resolved.disableTools, claudeSettings.JSON != ""); passthroughErr != nil {
		return passthroughErr
	}
	return runClaudeLaunchProcess(ctx, cmd, deps, execution, claudeSettings.JSON)
}

func validateResolvedLaunchPassthroughArgs(ctx context.Context, s *store.Store, invocation launchInvocation, resolved resolvedLaunch) error {
	if err := validateDynamicLaunchPassthroughArgs(invocation.claudeArgs, resolved.disableTools, false); err != nil {
		return err
	}
	if findLaunchOption(invocation.claudeArgs, "--settings") == "" {
		return nil
	}
	hasSettings, err := launchWillInjectSettings(ctx, s, invocation, resolved.disableTools)
	if err != nil {
		return err
	}
	return validateDynamicLaunchPassthroughArgs(invocation.claudeArgs, resolved.disableTools, hasSettings)
}

func launchWillInjectSettings(ctx context.Context, s *store.Store, invocation launchInvocation, includeToolDisabled bool) (bool, error) {
	if !invocation.noLifecycle {
		return true, nil
	}
	if !invocation.noStatusline {
		configured, err := claudeStatuslineConfigured()
		if err != nil {
			return false, err
		}
		if !configured {
			return true, nil
		}
	}
	settings := make(map[string]any, 1)
	if err := addLaunchAvailableModels(ctx, s, includeToolDisabled, settings); err != nil {
		return false, err
	}
	return len(settings) > 0, nil
}

func resolveLaunch(ctx context.Context, deps Dependencies, s *store.Store, invocation launchInvocation) (resolvedLaunch, error) {
	modelAlias, err := resolveLaunchModelAlias(ctx, deps, s, invocation.modelAlias)
	if err != nil {
		return resolvedLaunch{}, err
	}
	if authErr := validateResolvedLaunchAuthMode(invocation.authMode, modelAlias); authErr != nil {
		return resolvedLaunch{}, authErr
	}
	disableTools, err := launchShouldDisableTools(ctx, s, modelAlias)
	if err != nil {
		return resolvedLaunch{}, err
	}
	claudeModelID := ""
	if modelAlias != "" {
		model, err := s.GetModel(ctx, modelAlias)
		if err != nil {
			return resolvedLaunch{}, err
		}
		claudeModelID, err = launchClaudeModelID(model)
		if err != nil {
			return resolvedLaunch{}, err
		}
	}
	return resolvedLaunch{
		modelAlias: modelAlias, claudeModelID: claudeModelID,
		disableTools: disableTools,
	}, nil
}

func launchObservationStates(invocation launchInvocation) (lifecycleState, statuslineState string) {
	lifecycleState, statuslineState = "pending", "pending"
	if invocation.noLifecycle {
		lifecycleState = "disabled"
	}
	if invocation.noStatusline {
		statuslineState = "disabled"
	}
	return lifecycleState, statuslineState
}

func startObservableGateway(ctx context.Context, deps Dependencies, execution *launchExecution) error {
	var err error
	execution.token, err = gateway.NewToken()
	if err != nil {
		return fmt.Errorf("creating gateway session token: %w", err)
	}
	execution.observerToken, err = gateway.NewToken()
	if err != nil {
		return fmt.Errorf("creating lifecycle observer token: %w", err)
	}
	recorder := observability.NewRecorder(ctx, observability.Config{
		Store: execution.store, LaunchID: execution.launchID,
		Enabled: !execution.invocation.noHistory,
	})
	tracker, err := session.NewTracker(session.Config{
		Store: execution.store, Recorder: recorder, LaunchID: execution.launchID,
		Enabled:           !execution.invocation.noLifecycle,
		DefaultModelAlias: execution.resolved.modelAlias,
	})
	if err != nil {
		return fmt.Errorf("creating lifecycle tracker: %w", err)
	}
	execution.finalizer.tracker = tracker
	execution.server, err = deps.StartGateway(ctx, gateway.Config{
		Store: execution.store, Secrets: deps.Secrets,
		Token: execution.token, ObserverToken: execution.observerToken,
		DefaultModelAlias: execution.resolved.modelAlias,
		Recorder:          recorder, Tracker: tracker,
	})
	if err != nil {
		return fmt.Errorf("starting local gateway: %w", err)
	}
	return nil
}

func runClaudeLaunchProcess(ctx context.Context, cmd *cobra.Command, deps Dependencies, execution *launchExecution, claudeSettings string) error {
	invocation := execution.invocation
	resolved := execution.resolved
	claudeArgs := launchClaudeArgs(resolved.claudeModelID, invocation.printMode, resolved.disableTools,
		claudeSettings, invocation.permissionMode, invocation.claudeArgs)
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: execution.server.URL(), Token: execution.token,
		ObserverToken: execution.observerToken, LaunchID: execution.launchID,
		ModelAlias: resolved.modelAlias, ModelID: resolved.claudeModelID,
		DisableTools: resolved.disableTools, AuthMode: invocation.authMode,
	})
	outputLock := &sync.Mutex{}
	out := launchProcessWriter(cmd.OutOrStdout(), outputLock)
	errOut := launchProcessWriter(cmd.ErrOrStderr(), outputLock)
	process, err := deps.Launcher.Start(ctx, claudeArgs, env, cmd.InOrStdin(), out, errOut)
	if err != nil {
		return fmt.Errorf("launching Claude Code through the gateway: %w", err)
	}
	if err := execution.store.ActivateLaunch(ctx, execution.launchID, execution.server.URL(), process.PID()); err != nil {
		if cleanupErr := cleanupStartedClaudeProcess(process); cleanupErr != nil {
			return errors.Join(fmt.Errorf("activating launch record: %w", err), fmt.Errorf("cleaning up Claude Code process: %w", cleanupErr))
		}
		return fmt.Errorf("activating launch record: %w", err)
	}
	summaryOut := out
	if invocation.printMode {
		summaryOut = errOut
	}
	writeLaunchSummary(ctx, summaryOut, execution.store, execution.server.URL(), execution.launchID,
		process.PID(), resolved.modelAlias, resolved.disableTools, invocation.authMode, invocation.permissionMode)
	waitErr := process.Wait()
	shutdownGateway(ctx, execution.server)
	state, reason := launchExitState(ctx, waitErr)
	finishErr := execution.finalizer.Finish(ctx, state, reason, launchExitCode(waitErr))
	return errors.Join(waitErr, finishErr)
}

func validateLaunchInputs(modelAlias, authMode, permissionMode string) error {
	if modelAlias != "" {
		if err := validateName("model alias", modelAlias); err != nil {
			return err
		}
	}
	if err := validateLaunchAuthMode(authMode); err != nil {
		return err
	}
	return validateClaudePermissionMode(permissionMode)
}

func validateLaunchAuthMode(value string) error {
	switch value {
	case launchAuthModePreserve, launchAuthModeGatewayToken:
		return nil
	default:
		return fmt.Errorf("invalid launch auth mode %q; expected %s or %s", value, launchAuthModePreserve, launchAuthModeGatewayToken)
	}
}

func validateResolvedLaunchAuthMode(authMode, modelAlias string) error {
	if authMode == launchAuthModeGatewayToken && modelAlias == "" {
		return fmt.Errorf("--auth-mode gateway-token requires --model <alias>; use preserve auth mode for Claude Code default first-party routing")
	}
	return nil
}

func validateClaudePermissionMode(mode string) error {
	switch mode {
	case "", "default", "manual", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions":
		return nil
	default:
		return fmt.Errorf("invalid Claude Code permission mode %q", mode)
	}
}

type launchEnvironmentOptions struct {
	GatewayURL    string
	Token         string
	ObserverToken string
	LaunchID      int64
	ModelAlias    string
	ModelID       string
	DisableTools  bool
	AuthMode      string
}

func launchClaudeEnv(options launchEnvironmentOptions) ClaudeEnvironment {
	env := ClaudeEnvironment{
		Set: make([]string, 0, 13),
		Unset: []string{
			"CLAUDE_CODE_USE_GATEWAY",
		},
	}
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
		env.Set = append(
			env.Set,
			"ANTHROPIC_AUTH_TOKEN="+options.Token,
		)
	} else {
		env.Unset = append(env.Unset, "ANTHROPIC_AUTH_TOKEN")
		env.Set = append(
			env.Set,
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

func launchClaudeModelID(model store.Model) (string, error) {
	return gateway.DiscoveryIDForModel(model)
}

func claudeAvailableModels() (models []string, configured bool, err error) {
	paths := claudeSettingsPaths()
	for _, path := range paths {
		fileModels, ok, readErr := settingsFileAvailableModels(path)
		if readErr != nil {
			return nil, false, readErr
		}
		if ok {
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
		paths = append(
			paths,
			filepath.Join(configDir, "settings.json"),
			filepath.Join(configDir, "settings.local.json"),
		)
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(
			paths,
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "settings.local.json"),
		)
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		paths = append(
			paths,
			filepath.Join(cwd, ".claude", "settings.json"),
			filepath.Join(cwd, ".claude", "settings.local.json"),
		)
	}
	return paths
}

func settingsFileAvailableModels(path string) (models []string, configured bool, err error) {
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

func launchShouldDisableTools(ctx context.Context, s *store.Store, modelAlias string) (bool, error) {
	if modelAlias == "" {
		return false, nil
	}
	model, err := s.GetModel(ctx, modelAlias)
	if err != nil {
		return false, fmt.Errorf("checking model alias %q launch compatibility: %w", modelAlias, err)
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return false, fmt.Errorf("checking provider %q launch compatibility: %w", model.ProviderName, err)
	}
	disabled, err := modelDisablesClaudeTools(model, provider)
	if err != nil {
		return false, fmt.Errorf("checking model alias %q capabilities: %w", modelAlias, err)
	}
	return disabled, nil
}

func providerDisablesClaudeTools(provider store.Provider) bool {
	caps := effectiveProviderCapabilities(provider)
	return caps.Mode == providers.ModeChatOnly || !caps.SupportsTools
}

func modelDisablesClaudeTools(model store.Model, provider store.Provider) (bool, error) {
	if model.Status == "chat-only" || providerDisablesClaudeTools(provider) {
		return true, nil
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return false, err
	}
	return effective.Values.SupportsTools != nil && !*effective.Values.SupportsTools, nil
}

func writeLaunchSummary(ctx context.Context, out io.Writer, s *store.Store, gatewayURL string, sessionID int64, pid int, modelAlias string, disableTools bool, authMode, permissionMode string) {
	fmt.Fprintf(out, "Claude Code launched through %s (session=%d pid=%d)\n", gatewayURL, sessionID, pid)
	if modelAlias != "" {
		fmt.Fprintf(out, "Selected ccr model alias %q is exposed to Claude Code and used as the startup model.\n", modelAlias)
		fmt.Fprintf(out, "Requests selecting ccr alias %q are routed through its registered provider.\n", modelAlias)
		writeLaunchCompatibilitySummary(ctx, out, s, modelAlias)
		if disableTools {
			fmt.Fprintln(out, "Selected route does not support tools; Claude Code tools are disabled for this launch.")
		}
	} else {
		fmt.Fprintln(out, "No ccr startup model selected; Claude Code will use its configured default model.")
	}
	writeLaunchAuthSummary(out, authMode)
	if authMode == launchAuthModeGatewayToken {
		if permissionMode == "auto" {
			fmt.Fprintln(out, "Claude Code auto mode may require first-party Anthropic access for safety classification; use --auth-mode preserve if Agent or Workflow actions are denied.")
		}
		fmt.Fprintln(out, "Gateway model discovery is requested; registered aliases are exposed through /v1/models.")
		return
	}
	writePreserveAuthModelGuidance(ctx, out, s, disableTools)
}

func writePreserveAuthModelGuidance(ctx context.Context, out io.Writer, s *store.Store, includeToolDisabled bool) {
	models, _, err := routableModels(ctx, s, includeToolDisabled)
	if err != nil {
		fmt.Fprintf(out, "Registered ccr model guidance unavailable: %v\n", err)
		return
	}
	if len(models) == 0 {
		return
	}
	fmt.Fprintln(out, "Registered ccr models are available in Claude Code's /model picker:")
	for index := range models {
		model := &models[index]
		id, err := gateway.DiscoveryIDForModel(*model)
		if err != nil {
			fmt.Fprintf(out, "  %s unavailable: %v\n", model.Alias, err)
			continue
		}
		fmt.Fprintf(out, "  /model %s\n", id)
	}
}

func writeLaunchAuthSummary(out io.Writer, authMode string) {
	if authMode == launchAuthModeGatewayToken {
		fmt.Fprintln(out, "Gateway accepts only the generated local ANTHROPIC_AUTH_TOKEN for this process.")
		fmt.Fprintln(out, "Original Anthropic subscription login and Anthropic API-key auth are not active in --auth-mode gateway-token.")
		return
	}
	fmt.Fprintln(out, "Gateway accepts the generated local X-CCR-Session-Token for this process.")
	fmt.Fprintln(out, "Original Anthropic subscription login and Anthropic API-key auth are preserved for first-party Anthropic routes.")
}

func writeLaunchCompatibilitySummary(ctx context.Context, out io.Writer, s *store.Store, modelAlias string) {
	model, err := s.GetModel(ctx, modelAlias)
	if err != nil {
		fmt.Fprintf(out, "Compatibility metadata unavailable for %q: %v\n", modelAlias, err)
		return
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		fmt.Fprintf(out, "Compatibility metadata unavailable for provider %q: %v\n", model.ProviderName, err)
		return
	}
	caps := effectiveProviderCapabilities(provider)
	fmt.Fprintf(out, "Provider protocol=%s mode=%s token-count=%s capabilities=%s.\n", caps.Protocol, caps.Mode, providerTokenCountMode(provider), providerCapabilitySummary(provider))
	if !caps.SupportsModelDiscovery {
		fmt.Fprintln(out, "Provider model discovery is unavailable; only configured CCR aliases are exposed.")
	}
}

func launchProcessWriter(writer io.Writer, lock *sync.Mutex) io.Writer {
	if _, ok := writer.(*os.File); ok {
		return writer
	}
	return synchronizedWriter{lock: lock, writer: writer}
}

type synchronizedWriter struct {
	lock   *sync.Mutex
	writer io.Writer
}

func (w synchronizedWriter) Write(p []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.writer.Write(p)
}

func resolveLaunchModelAlias(ctx context.Context, deps Dependencies, s *store.Store, requested string) (string, error) {
	if requested != "" {
		model, provider, _, validateErr := validateRoutableModelAliasTargetWithStore(ctx, deps, s, requested, true)
		if validateErr != nil {
			return "", validateErr
		}
		effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
		if err != nil {
			return "", fmt.Errorf("checking model alias %q launch capabilities: %w", requested, err)
		}
		if !supportsClaudeStreaming(effectiveProviderCapabilities(provider), effective.Values) {
			return "", fmt.Errorf("model alias %q cannot be launched through Claude Code because its effective provider/model capabilities do not support streaming; it is excluded from /model", requested)
		}
		if !supportsClaudeSystemMessages(effective.Values) {
			return "", fmt.Errorf("model alias %q cannot be launched through Claude Code because its effective model capabilities do not support system messages; it is excluded from /model", requested)
		}
		return requested, nil
	}
	return "", nil
}

func routableModelAliases(ctx context.Context, s *store.Store, includeToolDisabled bool) ([]string, error) {
	models, _, err := routableModels(ctx, s, includeToolDisabled)
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(models))
	for index := range models {
		aliases = append(aliases, models[index].Alias)
	}
	return aliases, nil
}

func routableModels(ctx context.Context, s *store.Store, includeToolDisabled bool) (models []store.Model, hasRoutable bool, err error) {
	storedModels, err := s.ListModels(ctx)
	if err != nil {
		return nil, false, err
	}
	routable := make([]store.Model, 0, len(storedModels))
	for index := range storedModels {
		model := &storedModels[index]
		eligible, toolsDisabled, eligibilityErr := modelLaunchEligibility(ctx, s, model)
		if eligibilityErr != nil {
			return nil, false, eligibilityErr
		}
		if !eligible {
			continue
		}
		hasRoutable = true
		if !includeToolDisabled && toolsDisabled {
			continue
		}
		routable = append(routable, *model)
	}
	slices.SortFunc(routable, func(a, b store.Model) int { return strings.Compare(a.Alias, b.Alias) })
	return routable, hasRoutable, nil
}

func modelLaunchEligibility(ctx context.Context, s *store.Store, model *store.Model) (eligible, toolsDisabled bool, err error) {
	if model.Status == "blocked" {
		return false, false, nil
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return false, false, err
	}
	caps := effectiveProviderCapabilities(provider)
	if caps.Protocol != providers.ProtocolOpenAICompatible && caps.Protocol != providers.ProtocolAnthropicCompatible {
		return false, false, nil
	}
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return false, false, nil
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return false, false, err
	}
	if !modelcap.IsRoutableKind(effective.Values.Kind) {
		return false, false, nil
	}
	if !supportsClaudeStreaming(caps, effective.Values) {
		return false, false, nil
	}
	if !supportsClaudeSystemMessages(effective.Values) {
		return false, false, nil
	}
	toolsDisabled = model.Status == "chat-only" || providerDisablesClaudeTools(provider) ||
		effective.Values.SupportsTools != nil && !*effective.Values.SupportsTools
	return true, toolsDisabled, nil
}

func supportsClaudeStreaming(provider providers.Capabilities, model modelcap.Values) bool {
	return provider.SupportsStreaming &&
		(model.SupportsStreaming == nil || *model.SupportsStreaming)
}

func supportsClaudeSystemMessages(model modelcap.Values) bool {
	return model.SupportsSystemMessages == nil || *model.SupportsSystemMessages
}

func cleanupStartedClaudeProcess(process ClaudeProcess) error {
	if process == nil {
		return nil
	}
	stopErr := process.Stop()
	if stopErr != nil {
		return stopErr
	}
	_ = process.Wait()
	return nil
}

func shutdownGateway(parent context.Context, server *gateway.Server) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func processStatus(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return "unknown"
	}
	err = process.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return "running"
	case errors.Is(err, os.ErrProcessDone):
		return "exited"
	case errors.Is(err, syscall.EPERM):
		return "running"
	default:
		return "exited"
	}
}
