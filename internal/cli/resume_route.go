package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/routeidentity"
	"github.com/hishamkaram/claude-code-router/internal/session"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func validateResumeRouteInvocation(invocation launchInvocation) error {
	if !invocation.resumeRoute {
		return nil
	}
	if invocation.detach {
		return fmt.Errorf("--resume-route requires a foreground native --resume session")
	}
	if invocation.modelAlias != "" {
		return fmt.Errorf("--resume-route cannot be combined with --model")
	}
	if invocation.authModeSet {
		return fmt.Errorf("--resume-route cannot be combined with --auth-mode")
	}
	if invocation.claudeAccount != "" {
		return fmt.Errorf("--resume-route cannot be combined with --claude-account")
	}
	if _, err := nativeResumeSession(invocation.claudeArgs); err != nil {
		return err
	}
	if option := resumeRouteConflictOption(invocation.claudeArgs); option != "" {
		return fmt.Errorf("--resume-route cannot be combined with native session option %s", option)
	}
	return nil
}

func resumeRouteConflictOption(args []string) string {
	if option := findLaunchOption(args, "--session-id", "--continue", "--fork-session", "--no-session-persistence", "--from-pr", "--teleport"); option != "" {
		return option
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if option := detachedShortSessionOption(arg); option != "" {
			return option
		}
	}
	return ""
}

func nativeResumeSession(args []string) (string, error) {
	values, err := nativeOptionValues(args, "--resume")
	if err != nil {
		return "", fmt.Errorf("--resume-route requires a nonempty native --resume session")
	}
	if len(values) == 0 {
		return "", fmt.Errorf("--resume-route requires a native --resume session")
	}
	if len(values) != 1 {
		return "", fmt.Errorf("--resume-route requires exactly one native --resume session")
	}
	return values[0], nil
}

func nativeKnownSession(args []string) (sessionID, source string, found bool) {
	sessionIDs, sessionIDErr := nativeOptionValues(args, "--session-id")
	resumeIDs, resumeErr := nativeOptionValues(args, "--resume")
	if sessionIDErr != nil || resumeErr != nil || len(sessionIDs)+len(resumeIDs) != 1 {
		return "", "", false
	}
	if len(sessionIDs) == 1 {
		return sessionIDs[0], "startup", true
	}
	return resumeIDs[0], "resume", true
}

func validateNativeSessionIDs(args []string) error {
	for _, option := range [...]string{"--session-id", "--resume"} {
		values, err := nativeOptionValues(args, option)
		if err != nil {
			return err
		}
		for _, value := range values {
			if err := session.ValidateSessionID(value); err != nil {
				return fmt.Errorf("native %s: %w", option, err)
			}
		}
	}
	return nil
}

func nativeOptionValues(args []string, target string) ([]string, error) {
	values := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		if args[index] == "--" {
			break
		}
		option, value, inline := strings.Cut(args[index], "=")
		if option != target {
			continue
		}
		if !inline {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return nil, fmt.Errorf("native %s requires a nonempty value", target)
			}
			index++
			value = args[index]
		}
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("native %s requires a nonempty value", target)
		}
		values = append(values, value)
	}
	return values, nil
}

func resolveRecordedResumeRoute(ctx context.Context, deps Dependencies, s *store.Store, invocation launchInvocation) (resolvedLaunch, error) {
	nativeSessionID, err := nativeResumeSession(invocation.claudeArgs)
	if err != nil {
		return resolvedLaunch{}, err
	}
	route, err := s.FindResumeRoute(ctx, nativeSessionID)
	if err != nil {
		return resolvedLaunch{}, fmt.Errorf("restoring recorded provider route for native Claude session %q: %w", nativeSessionID, err)
	}
	if !providerOnlyLaunchAuthMode(route.SourceAuthMode) {
		return resolvedLaunch{}, fmt.Errorf("native Claude session %q was not launched with provider-only routing", nativeSessionID)
	}
	model, provider, providerSecret, err := validateRecordedResumeRoute(ctx, deps, s, route)
	if err != nil {
		return resolvedLaunch{}, err
	}
	pinnedRoute, err := gateway.NewPinnedRouteWithSecret(model, provider, providerSecret)
	if err != nil {
		return resolvedLaunch{}, err
	}
	disableTools, err := modelDisablesClaudeTools(model, provider)
	if err != nil {
		return resolvedLaunch{}, fmt.Errorf("checking model alias %q capabilities: %w", route.ModelAlias, err)
	}
	claudeModelID, err := launchClaudeModelID(model)
	if err != nil {
		return resolvedLaunch{}, err
	}
	return resolvedLaunch{
		modelAlias: route.ModelAlias, claudeModelID: claudeModelID,
		authMode: route.SourceAuthMode, disableTools: disableTools,
		compatibility: snapshotLaunchCompatibility(provider), pinnedRoute: &pinnedRoute,
	}, nil
}

func validateRecordedResumeRoute(ctx context.Context, deps Dependencies, s *store.Store, route store.ResumeRoute) (store.Model, store.Provider, string, error) {
	model, provider, err := loadModelAliasTargetWithStore(ctx, s, route.ModelAlias)
	if err != nil {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("reading recorded model alias %q: %w", route.ModelAlias, err)
	}
	if model.ProviderName != route.ProviderName || model.ProviderModel != route.ProviderModel {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("recorded provider route for model alias %q no longer matches its current provider configuration", route.ModelAlias)
	}
	if route.Protocol == "" {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("recorded provider route for model alias %q has no protocol", route.ModelAlias)
	}
	if effectiveProviderCapabilities(provider).Protocol != route.Protocol {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("recorded provider route for model alias %q no longer matches its current protocol", route.ModelAlias)
	}
	if validationErr := rejectBlockedModelAlias(model); validationErr != nil {
		return store.Model{}, store.Provider{}, "", validationErr
	}
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("recorded model alias %q targets LiteLLM control model %q and cannot be routed", model.Alias, model.ProviderModel)
	}
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("computing capabilities for model alias %q: %w", model.Alias, err)
	}
	if !modelcap.IsRoutableKind(effective.Values.Kind) {
		return store.Model{}, store.Provider{}, "", fmt.Errorf("model alias %q has non-routable model kind %q", model.Alias, effective.Values.Kind)
	}
	if identityErr := validateRecordedRouteIdentity(model, provider, effective.Values, route); identityErr != nil {
		return store.Model{}, store.Provider{}, "", identityErr
	}
	providerSecret, err := validateProviderConfigAndSecret(ctx, deps, provider)
	if err != nil {
		return store.Model{}, store.Provider{}, "", err
	}
	if capabilitiesErr := validateLaunchModelCapabilities(model, provider); capabilitiesErr != nil {
		return store.Model{}, store.Provider{}, "", capabilitiesErr
	}
	return model, provider, providerSecret, nil
}

func validateRecordedRouteIdentity(model store.Model, provider store.Provider, effective modelcap.Values, recorded store.ResumeRoute) error {
	fingerprint, err := routeidentity.Fingerprint(model, provider, effective)
	if err != nil {
		return fmt.Errorf("identifying recorded provider route for model alias %q: %w", model.Alias, err)
	}
	if fingerprint != recorded.ConfigurationFingerprint {
		return fmt.Errorf("recorded provider route for model alias %q no longer matches its current provider configuration", recorded.ModelAlias)
	}
	return nil
}
