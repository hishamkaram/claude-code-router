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
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func newLaunchCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var modelAlias string
	var printMode bool
	var authMode string
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch Claude Code through the local router",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLaunch(ctx, cmd, opts, deps, modelAlias, printMode, authMode)
		},
	}
	cmd.Flags().StringVar(&modelAlias, "model", "", "Optional model alias to validate before launch")
	cmd.Flags().StringVar(&authMode, "auth-mode", launchAuthModePreserve, "Gateway auth mode: preserve or gateway-token")
	cmd.Flags().BoolVarP(&printMode, "print", "p", false, "Run Claude Code in non-interactive print mode, reading the prompt from stdin")
	return cmd
}

const (
	launchAuthModePreserve     = "preserve"
	launchAuthModeGatewayToken = "gateway-token"
)

func runLaunch(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, modelAlias string, printMode bool, authMode string) error {
	if modelAlias != "" {
		if validateErr := validateName("model alias", modelAlias); validateErr != nil {
			return validateErr
		}
	}
	if err := validateLaunchAuthMode(authMode); err != nil {
		return err
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	resolvedModelAlias, err := resolveLaunchModelAlias(ctx, deps, s, modelAlias)
	if err != nil {
		return err
	}
	if authErr := validateResolvedLaunchAuthMode(authMode, resolvedModelAlias); authErr != nil {
		return authErr
	}
	disableTools, err := launchShouldDisableTools(ctx, s, resolvedModelAlias)
	if err != nil {
		return err
	}

	token, err := gateway.NewToken()
	if err != nil {
		return err
	}
	server, err := gateway.Start(ctx, gateway.Config{
		Store:             s,
		Secrets:           deps.Secrets,
		Token:             token,
		DefaultModelAlias: resolvedModelAlias,
	})
	if err != nil {
		return err
	}
	defer shutdownGateway(ctx, server)

	claudeModelID := launchClaudeModelID(resolvedModelAlias)
	claudeSettings, err := launchClaudeSettingsArg(ctx, s, claudeModelID != "")
	if err != nil {
		return err
	}
	claudeArgs := launchClaudeArgs(claudeModelID, printMode, disableTools, claudeSettings)
	env := launchClaudeEnv(server.URL(), token, resolvedModelAlias, claudeModelID, disableTools, authMode)
	outputLock := &sync.Mutex{}
	out := launchProcessWriter(cmd.OutOrStdout(), outputLock)
	errOut := launchProcessWriter(cmd.ErrOrStderr(), outputLock)
	process, err := deps.Launcher.Start(ctx, claudeArgs, env, cmd.InOrStdin(), out, errOut)
	if err != nil {
		return fmt.Errorf("launching Claude Code through the gateway: %w", err)
	}
	sessionID, err := s.AddSession(ctx, store.Session{
		GatewayURL: server.URL(),
		PID:        process.PID(),
		ModelAlias: resolvedModelAlias,
	})
	if err != nil {
		if cleanupErr := cleanupStartedClaudeProcess(process); cleanupErr != nil {
			return errors.Join(fmt.Errorf("recording launch session: %w", err), fmt.Errorf("cleaning up Claude Code process: %w", cleanupErr))
		}
		return fmt.Errorf("recording launch session: %w", err)
	}
	summaryOut := out
	if printMode {
		summaryOut = errOut
	}
	writeLaunchSummary(ctx, summaryOut, s, server.URL(), sessionID, process.PID(), resolvedModelAlias, disableTools, authMode)
	return process.Wait()
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
		return fmt.Errorf("--auth-mode gateway-token requires --model <alias> or exactly one routable ccr alias; use preserve auth mode for Claude Code default first-party routing")
	}
	return nil
}

func launchClaudeArgs(modelID string, printMode, disableTools bool, settings string) []string {
	args := []string{}
	if printMode {
		args = append(args, "--print")
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
	return args
}

func launchClaudeEnv(gatewayURL, token, modelAlias, modelID string, disableTools bool, authMode string) []string {
	env := make([]string, 0, 8)
	env = append(env,
		"ANTHROPIC_BASE_URL="+gatewayURL,
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
	)
	if authMode == launchAuthModeGatewayToken {
		env = append(env,
			"CLAUDE_CODE_USE_GATEWAY=1",
			"ANTHROPIC_AUTH_TOKEN="+token,
		)
	} else {
		env = append(env,
			"CLAUDE_CODE_USE_GATEWAY=",
			"ANTHROPIC_AUTH_TOKEN=",
			"ANTHROPIC_CUSTOM_HEADERS="+launchAnthropicCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), token),
		)
	}
	if disableTools {
		env = append(env, "CLAUDE_CODE_SIMPLE=1")
	}
	if modelID == "" {
		return env
	}
	return append(env,
		"ANTHROPIC_CUSTOM_MODEL_OPTION="+modelID,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=CCR "+modelAlias,
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=Model alias registered in ccr",
	)
}

func launchClaudeModelID(modelAlias string) string {
	if modelAlias == "" {
		return ""
	}
	return gateway.DiscoveryIDForAlias(modelAlias)
}

func launchClaudeSettingsArg(ctx context.Context, s *store.Store, force bool) (string, error) {
	existing, configured, err := claudeAvailableModels()
	if err != nil {
		return "", err
	}
	if !configured && !force {
		return "", nil
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return "", fmt.Errorf("building Claude Code model allowlist extension: %w", err)
	}
	ids := make([]string, 0, len(existing)+len(models))
	seen := make(map[string]struct{}, len(existing)+len(models))
	baseIDs := existing
	if !configured {
		baseIDs = gateway.FirstPartyAnthropicModelIDs()
	}
	for _, id := range baseIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	addedCCR := false
	for _, model := range models {
		if model.Status == "blocked" {
			continue
		}
		id := gateway.DiscoveryIDForAlias(model.Alias)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		addedCCR = true
	}
	if !addedCCR {
		return "", nil
	}
	payload := struct {
		AvailableModels []string `json:"availableModels"`
	}{AvailableModels: ids}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("building Claude Code settings override: %w", err)
	}
	return string(encoded), nil
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
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "settings.local.json"),
		)
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		paths = append(paths,
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
	return model.Status == "chat-only" || providerDisablesClaudeTools(provider), nil
}

func providerDisablesClaudeTools(provider store.Provider) bool {
	caps := effectiveProviderCapabilities(provider)
	return caps.Mode == providers.ModeChatOnly || !caps.SupportsTools
}

func writeLaunchSummary(ctx context.Context, out io.Writer, s *store.Store, gatewayURL string, sessionID int64, pid int, modelAlias string, disableTools bool, authMode string) {
	fmt.Fprintf(out, "Claude Code launched through %s (session=%d pid=%d)\n", gatewayURL, sessionID, pid)
	if modelAlias != "" {
		fmt.Fprintf(out, "Selected ccr model alias %q is exposed to Claude Code and used as the startup model.\n", modelAlias)
		fmt.Fprintf(out, "Unmatched non-Claude model requests in this launch are routed through selected ccr alias %q.\n", modelAlias)
		writeLaunchCompatibilitySummary(ctx, out, s, modelAlias)
		if disableTools {
			fmt.Fprintln(out, "Selected route does not support tools; Claude Code tools are disabled for this launch.")
		}
	} else {
		fmt.Fprintln(out, "No ccr startup model selected; Claude Code will use its configured default model.")
	}
	writeLaunchAuthSummary(out, authMode)
	fmt.Fprintln(out, "Gateway model discovery is enabled; registered aliases are exposed through /v1/models.")
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
	fmt.Fprintf(out, "Provider protocol=%s mode=%s capabilities=%s.\n", caps.Protocol, caps.Mode, providerCapabilitySummary(provider))
	if !caps.SupportsModelDiscovery {
		fmt.Fprintln(out, "Provider model discovery is unavailable; only configured CCR aliases are exposed.")
	}
	if !caps.SupportsCountTokens {
		fmt.Fprintln(out, "Provider count_tokens support is unavailable; gateway count_tokens requests will be rejected visibly.")
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
		if _, _, _, validateErr := validateRoutableModelAliasTargetWithStore(ctx, deps, s, requested, true); validateErr != nil {
			return "", validateErr
		}
		return requested, nil
	}
	aliases, err := routableModelAliases(ctx, s)
	if err != nil {
		return "", err
	}
	switch len(aliases) {
	case 0:
		return "", nil
	case 1:
		if _, _, _, validateErr := validateRoutableModelAliasTargetWithStore(ctx, deps, s, aliases[0], true); validateErr != nil {
			return "", validateErr
		}
		return aliases[0], nil
	default:
		return "", nil
	}
}

func routableModelAliases(ctx context.Context, s *store.Store) ([]string, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(models))
	for _, model := range models {
		if model.Status == "blocked" {
			continue
		}
		provider, err := s.GetProvider(ctx, model.ProviderName)
		if err != nil {
			return nil, err
		}
		caps := effectiveProviderCapabilities(provider)
		if caps.Protocol == providers.ProtocolOpenAICompatible || caps.Protocol == providers.ProtocolAnthropicCompatible {
			aliases = append(aliases, model.Alias)
		}
	}
	slices.Sort(aliases)
	return aliases, nil
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

func newSessionsCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List tracked Claude Code sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			sessions, err := s.ListSessions(ctx)
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No launch sessions tracked.")
				return nil
			}
			for _, session := range sessions {
				model := session.ModelAlias
				if model == "" {
					model = "(request-selected)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tpid=%d\tstatus=%s\tgateway=%s\tmodel=%s\tcreated=%s\n", session.ID, session.PID, processStatus(session.PID), session.GatewayURL, model, session.CreatedAt)
			}
			return nil
		},
	}
}

func newAgentsCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "List tracked Claude Code agents and workers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			agents, err := s.ListAgents(ctx)
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No agents observed.")
				return nil
			}
			for _, agent := range agents {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tsession=%d\tname=%s\tkind=%s\tmodel=%s\tstatus=%s\tcreated=%s\n", agent.ID, agent.SessionID, agent.Name, agent.Kind, agent.ModelAlias, agent.Status, agent.CreatedAt)
			}
			return nil
		},
	}
}

func newConformanceCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run model compatibility checks",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "run <alias>",
		Short: "Run conformance checks for a model alias",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr conformance run qwen")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConformance(ctx, cmd, opts, deps, args[0])
		},
	})
	return cmd
}

func runConformance(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, alias string) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	model, provider, discovered, err := validateRoutableModelAliasTargetWithStore(ctx, deps, s, alias, true)
	if err != nil {
		return err
	}
	caps := effectiveProviderCapabilities(provider)
	details := fmt.Sprintf("provider=%s type=%s protocol=%s model=%s compat=%s", provider.Name, provider.Type, caps.Protocol, model.ProviderModel, model.Status)
	if caps.Protocol == providers.ProtocolOpenAICompatible && caps.SupportsModelDiscovery {
		details = fmt.Sprintf("%s discovered_models=%d", details, discovered)
	}
	recordID, err := s.AddConformanceRecord(ctx, store.ConformanceRecord{
		Alias:        alias,
		Status:       "local-verified",
		LiveVerified: false,
		Details:      details,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Conformance record %d for %q: local-verified\n", recordID, alias)
	fmt.Fprintf(cmd.OutOrStdout(), "Compatibility: %s\n", details)
	fmt.Fprintln(cmd.OutOrStdout(), "Live runtime status: unverified until live Claude Code E2E passes.")
	return nil
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
