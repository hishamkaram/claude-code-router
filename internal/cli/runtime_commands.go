package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/cua"
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
	cmd.Flags().String("ccr-cua-mode", "client", "Computer-use owner: client or managed")
	cmd.Flags().String("ccr-cua-executor", "", "Managed CUA executor: docker, local-browser, macos-preview, or external:<name>")
	cmd.Flags().String("ccr-cua-external-url", "", "Public HTTPS base URL for an external managed CUA executor")
	cmd.Flags().String("ccr-cua-external-token-env", "", "Environment variable containing the required external managed CUA bearer token")
	cmd.Flags().Int("ccr-cua-max-turns", 0, "Maximum managed CUA model turns for this launch")
	cmd.Flags().Int("ccr-cua-max-actions", 0, "Maximum managed CUA actions for this launch")
	cmd.Flags().String("ccr-cua-timeout", "", "Maximum total managed CUA duration, for example 10m")
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
	recorder      *observability.Recorder
	managedCUA    *managedCUALaunch
	cuaProject    string
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
	if cuaErr := validateManagedCUALaunch(ctx, s, deps, invocation, resolved); cuaErr != nil {
		return cuaErr
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
	execution.recorder = observability.NewRecorder(ctx, observability.Config{
		Store: execution.store, LaunchID: execution.launchID,
		Enabled: !execution.invocation.noHistory,
	})
	if startErr := startLaunchManagedCUA(ctx, cmd, deps, execution); startErr != nil {
		return startErr
	}
	defer func() {
		resultErr = errors.Join(resultErr, shutdownManagedCUA(ctx, &execution.managedCUA))
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

func startLaunchManagedCUA(ctx context.Context, cmd *cobra.Command, deps Dependencies, execution *launchExecution) error {
	if execution.invocation.cuaConfig.Mode != cua.ModeManaged {
		return nil
	}
	project, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("reading managed computer-use project directory: %w", err)
	}
	execution.cuaProject = project
	managedCUA, err := deps.StartManagedCUA(ctx, managedCUAStart{
		Config: execution.invocation.cuaConfig, ExternalURL: execution.invocation.cuaExternalURL,
		ExternalToken: managedCUAExternalToken(execution.invocation),
		LaunchID:      execution.launchID, Project: project, Out: cmd.ErrOrStderr(), Recorder: execution.recorder,
	})
	if err != nil {
		return fmt.Errorf("starting managed computer use: %w", err)
	}
	execution.managedCUA = managedCUA
	return nil
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
	recorder := execution.recorder
	if recorder == nil {
		recorder = observability.NewRecorder(ctx, observability.Config{
			Store: execution.store, LaunchID: execution.launchID,
			Enabled: !execution.invocation.noHistory,
		})
		execution.recorder = recorder
	}
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
		ManagedCUA: execution.managedCUA.Runtime(), ManagedCUAProject: execution.cuaProject,
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
	providerSecretEnvNames, err := configuredProviderSecretEnvNames(ctx, execution.store)
	if err != nil {
		return fmt.Errorf("reading configured provider secret environment names: %w", err)
	}
	env := launchClaudeEnv(launchEnvironmentOptions{
		GatewayURL: execution.server.URL(), Token: execution.token,
		ObserverToken: execution.observerToken, LaunchID: execution.launchID,
		ModelAlias: resolved.modelAlias, ModelID: resolved.claudeModelID,
		DisableTools: resolved.disableTools, AuthMode: invocation.authMode,
		ProviderSecretEnvNames: providerSecretEnvNames, ExternalTokenEnv: invocation.cuaTokenEnv,
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
	managedCUAErr := shutdownManagedCUA(ctx, &execution.managedCUA)
	state, reason := launchExitState(ctx, waitErr)
	finishErr := execution.finalizer.Finish(ctx, state, reason, launchExitCode(waitErr))
	return errors.Join(waitErr, managedCUAErr, finishErr)
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
