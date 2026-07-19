package conformance

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func RunProvider(ctx context.Context, config Config) (Result, error) {
	if config.Store == nil {
		return Result{}, fmt.Errorf("conformance.RunProvider: store is required")
	}
	if strings.TrimSpace(config.Alias) == "" {
		return Result{}, fmt.Errorf("conformance.RunProvider: alias is required")
	}
	if config.Secrets == nil {
		config.Secrets = secret.DefaultBackend{}
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	probeTarget, err := loadTarget(ctx, config.Store, config.Alias)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Alias: config.Alias, ProviderName: probeTarget.provider.Name,
		ProviderModel: probeTarget.model.ProviderModel, Protocol: probeTarget.capabilities.Protocol,
		Status: StatusPassed, StartedAt: time.Now().UTC(),
	}
	result.Checks = append(result.Checks, Check{
		Name: "configuration", Status: StatusPassed,
		Evidence: "model and provider configuration are routable",
	})

	token, err := gateway.NewToken()
	if err != nil {
		return Result{}, fmt.Errorf("conformance.RunProvider: creating gateway token: %w", err)
	}
	server, err := gateway.Start(ctx, gateway.Config{
		Store: config.Store, Secrets: config.Secrets, HTTPClient: config.HTTPClient,
		Token: token, DefaultModelAlias: config.Alias,
	})
	if err != nil {
		return Result{}, fmt.Errorf("conformance.RunProvider: starting production gateway: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	runner := checkRunner{
		config: config, target: probeTarget, gatewayURL: server.URL(), token: token,
		client: &http.Client{Timeout: config.Timeout},
	}
	result.Checks = append(result.Checks, runner.run(ctx)...)
	for _, check := range result.Checks {
		if check.Status == StatusFailed {
			result.Status = StatusFailed
			break
		}
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func loadTarget(ctx context.Context, s *store.Store, alias string) (target, error) {
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return target{}, fmt.Errorf("conformance.loadTarget: reading alias %q: %w", alias, err)
	}
	if model.Status == "blocked" {
		return target{}, fmt.Errorf("conformance.loadTarget: alias %q is blocked", alias)
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return target{}, fmt.Errorf("conformance.loadTarget: reading provider %q: %w", model.ProviderName, err)
	}
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return target{}, fmt.Errorf("conformance.loadTarget: alias %q targets LiteLLM control model %q and cannot be routed", alias, model.ProviderModel)
	}
	caps := providers.NormalizeCapabilities(provider.Type, providers.Capabilities{
		Protocol: provider.Protocol, SupportsTools: provider.SupportsTools,
		SupportsStreaming: provider.SupportsStreaming, SupportsThinking: provider.SupportsThinking,
		SupportsModelDiscovery: provider.SupportsModelDiscovery,
		SupportsCountTokens:    provider.SupportsCountTokens, Mode: provider.Mode,
	})
	if caps.Protocol != providers.ProtocolOpenAICompatible && caps.Protocol != providers.ProtocolAnthropicCompatible {
		return target{}, fmt.Errorf("conformance.loadTarget: provider protocol %q is unsupported", caps.Protocol)
	}
	effectiveModel, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return target{}, fmt.Errorf("conformance.loadTarget: computing model capabilities: %w", err)
	}
	if !modelcap.IsRoutableKind(effectiveModel.Values.Kind) {
		return target{}, fmt.Errorf("conformance.loadTarget: alias %q has non-routable model kind %q", alias, effectiveModel.Values.Kind)
	}
	return target{model: model, provider: provider, capabilities: caps, modelCapabilities: effectiveModel.Values}, nil
}
