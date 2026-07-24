package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

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
	switch authMode {
	case launchAuthModeGatewayToken:
		fmt.Fprintln(out, "Gateway accepts only the generated local ANTHROPIC_AUTH_TOKEN for this process.")
		fmt.Fprintln(out, "Original Anthropic subscription login and Anthropic API-key auth are not active in --auth-mode gateway-token.")
	case launchAuthModeSubscriptionPool:
		fmt.Fprintln(out, "Gateway accepts the generated local X-CCR-Session-Token for this process.")
		fmt.Fprintln(out, "The selected Claude account OAuth identity is fixed for this process; inherited Claude login and Anthropic API-key auth are not active.")
	default:
		fmt.Fprintln(out, "Gateway accepts the generated local X-CCR-Session-Token for this process.")
		fmt.Fprintln(out, "Original Anthropic subscription login and Anthropic API-key auth are preserved for first-party Anthropic routes.")
	}
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
		capabilities := effectiveProviderCapabilities(provider)
		if requiresUnavailableResponsesAPI(capabilities, effective.Values) {
			return "", fmt.Errorf("model alias %q cannot be launched through Claude Code because it requires the OpenAI Responses API, but its effective provider/model capabilities do not support it; it is excluded from /model", requested)
		}
		if !supportsClaudeStreaming(capabilities, effective.Values) {
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
	if requiresUnavailableResponsesAPI(caps, effective.Values) {
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

func requiresUnavailableResponsesAPI(provider providers.Capabilities, model modelcap.Values) bool {
	return usesResponsesAPI(model) && (!provider.SupportsResponses || model.SupportsResponses != nil && !*model.SupportsResponses)
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
	return stopClaudeProcessAndWait(process, process.Done(), claudeProcessStopTimeout)
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
