package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/cua/approval"
	"github.com/hishamkaram/claude-code-router/internal/cua/audit"
	"github.com/hishamkaram/claude-code-router/internal/cua/executor"
	"github.com/hishamkaram/claude-code-router/internal/cua/policy"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/observability"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	defaultManagedCUAReadyTimeout = 15 * time.Second
	dockerManagedCUAReadyTimeout  = 2 * time.Minute
)

type managedCUAStart struct {
	Config        cua.Config
	ExternalURL   string
	ExternalToken string
	LaunchID      int64
	Project       string
	Out           io.Writer
	Recorder      *observability.Recorder
}

type managedCUALaunch struct {
	runtime  *cua.ManagedRuntime
	approval *approval.Server
}

func (l *managedCUALaunch) Runtime() *cua.ManagedRuntime {
	if l == nil {
		return nil
	}
	return l.runtime
}

func (l *managedCUALaunch) Shutdown(ctx context.Context) error {
	if l == nil {
		return nil
	}
	var closeErr error
	if l.runtime != nil {
		closeErr = errors.Join(closeErr, l.runtime.Close())
	}
	if l.approval != nil {
		closeErr = errors.Join(closeErr, shutdownManagedCUAApproval(ctx, l.approval))
	}
	return closeErr
}

func validateManagedCUALaunch(ctx context.Context, s *store.Store, deps Dependencies, invocation launchInvocation, resolved resolvedLaunch) error {
	if invocation.cuaConfig.Mode != cua.ModeManaged {
		return nil
	}
	model, err := managedCUAModel(ctx, s, resolved)
	if err != nil {
		return err
	}
	if err := validateManagedCUAModelCapabilities(ctx, s, model); err != nil {
		return err
	}
	return validateManagedCUAExecutor(ctx, deps, invocation)
}

func managedCUAModel(ctx context.Context, s *store.Store, resolved resolvedLaunch) (store.Model, error) {
	if resolved.modelAlias == "" {
		return store.Model{}, fmt.Errorf("--ccr-cua-mode managed requires --model <alias>")
	}
	if resolved.disableTools {
		return store.Model{}, fmt.Errorf("--ccr-cua-mode managed requires a tool-capable model alias")
	}
	model, err := s.GetModel(ctx, resolved.modelAlias)
	if err != nil {
		return store.Model{}, fmt.Errorf("reading managed computer-use model %q: %w", resolved.modelAlias, err)
	}
	return model, nil
}

func validateManagedCUAModelCapabilities(ctx context.Context, s *store.Store, model store.Model) error {
	effective, err := modelcap.Effective(model.DiscoveredCapabilities, model.CapabilityOverrides, model.ProviderModel)
	if err != nil {
		return fmt.Errorf("reading effective capabilities for managed computer use: %w", err)
	}
	if !explicitlyTrue(effective.Values.SupportsComputerUse) {
		return fmt.Errorf("model alias %q does not have effective computer-use support", model.Alias)
	}
	if !usesResponsesAPI(effective.Values) {
		return fmt.Errorf("managed computer use for model alias %q requires effective OpenAI Responses support", model.Alias)
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return fmt.Errorf("reading provider for managed computer use: %w", err)
	}
	capabilities := effectiveProviderCapabilities(provider)
	if capabilities.Protocol != providers.ProtocolOpenAICompatible || !capabilities.SupportsResponses {
		return fmt.Errorf("managed computer use for model alias %q requires an OpenAI Responses-capable provider", model.Alias)
	}
	return nil
}

func validateManagedCUAExecutor(ctx context.Context, deps Dependencies, invocation launchInvocation) error {
	target, err := executor.ParseTarget(invocation.cuaConfig.Executor)
	if err != nil {
		return err
	}
	if target.Kind == cua.ExecutorExternal {
		if strings.TrimSpace(invocation.cuaTokenEnv) == "" {
			return fmt.Errorf("--ccr-cua-executor external:<name> requires --ccr-cua-external-token-env")
		}
		if strings.TrimSpace(os.Getenv(invocation.cuaTokenEnv)) == "" {
			return fmt.Errorf("external CUA token environment variable %q is empty or unset", invocation.cuaTokenEnv)
		}
		if err := deps.ValidateExternalCUAURL(ctx, invocation.cuaExternalURL); err != nil {
			return fmt.Errorf("validating external managed CUA URL: %w", err)
		}
	}
	if strings.HasPrefix(invocation.cuaConfig.Executor, string(cua.ExecutorMacOSPreview)) && runtime.GOOS != "darwin" {
		return fmt.Errorf("--ccr-cua-executor macos-preview requires macOS")
	}
	return nil
}

func validateExternalCUAURL(ctx context.Context, raw string) error {
	if _, err := executor.ValidateExternalBaseURL(ctx, raw, nil); err != nil {
		return err
	}
	return nil
}

func explicitlyTrue(value *bool) bool {
	return value != nil && *value
}

func usesResponsesAPI(values modelcap.Values) bool {
	return values.Kind == modelcap.KindResponses || explicitlyTrue(values.SupportsResponses)
}

func startManagedCUA(ctx context.Context, start managedCUAStart) (_ *managedCUALaunch, resultErr error) {
	config, err := start.Config.Normalize()
	if err != nil {
		return nil, err
	}
	if config.Mode != cua.ModeManaged {
		return nil, fmt.Errorf("managed computer-use launch requires mode managed")
	}
	if strings.TrimSpace(start.Project) == "" {
		return nil, fmt.Errorf("managed computer-use launch requires a project directory")
	}
	fprintf(start.Out, "CCR: starting managed CUA executor %s\n", config.Executor)
	managedExecutor, err := newManagedCUAExecutor(ctx, config, start)
	if err != nil {
		return nil, err
	}
	var approvalServer *approval.Server
	var manager *policy.Manager
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeManagedCUAStartup(ctx, managedExecutor, manager, approvalServer))
		}
	}()
	if readyErr := waitForManagedCUAExecutor(ctx, managedExecutor, start.Out); readyErr != nil {
		return nil, readyErr
	}
	approvalServer, err = approval.Start(ctx, approval.Config{Notify: managedCUAApprovalNotifier(start.Out)})
	if err != nil {
		return nil, fmt.Errorf("starting managed CUA approval endpoint: %w", err)
	}
	manager, err = policy.NewManager(policy.Config{Approver: approvalServer, Executor: managedExecutor.Name()})
	if err != nil {
		return nil, err
	}
	managedRuntime, err := cua.NewManagedRuntime(ctx, config, managedExecutor, manager, &launchCUAAuditor{
		memory: audit.NewRecorder(audit.Config{}), recorder: start.Recorder,
	})
	if err != nil {
		return nil, err
	}
	fprintf(start.Out, "CCR: managed CUA executor %s is ready\n", managedRuntime.ExecutorName())
	return &managedCUALaunch{runtime: managedRuntime, approval: approvalServer}, nil
}

func closeManagedCUAStartup(ctx context.Context, managedExecutor cua.Executor, manager *policy.Manager, approvalServer *approval.Server) error {
	var closeErr error
	if manager != nil {
		closeErr = errors.Join(closeErr, manager.Close())
	}
	if approvalServer != nil {
		closeErr = errors.Join(closeErr, shutdownManagedCUAApproval(ctx, approvalServer))
	}
	if managedExecutor != nil {
		closeErr = errors.Join(closeErr, managedExecutor.Close())
	}
	return closeErr
}

func shutdownManagedCUAApproval(ctx context.Context, approvalServer *approval.Server) error {
	if approvalServer == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return approvalServer.Shutdown(shutdownCtx)
}

func newManagedCUAExecutor(ctx context.Context, config cua.Config, start managedCUAStart) (cua.Executor, error) {
	target, err := executor.ParseTarget(config.Executor)
	if err != nil {
		return nil, err
	}
	switch target.Kind {
	case cua.ExecutorDocker:
		return executor.NewDockerBrowser(ctx, executor.DockerBrowserOptions{LaunchID: strconv.FormatInt(start.LaunchID, 10)})
	case cua.ExecutorLocalBrowser:
		return executor.NewLocalBrowser(ctx, executor.LocalBrowserOptions{})
	case cua.ExecutorExternal:
		return executor.NewExternalHTTP(ctx, target.ExternalName, executor.ExternalOptions{BaseURL: start.ExternalURL, BearerToken: start.ExternalToken})
	case cua.ExecutorMacOSPreview:
		return executor.NewMacOSPreview(ctx, executor.MacOSPreviewOptions{})
	default:
		return nil, fmt.Errorf("unsupported managed CUA executor %q", config.Executor)
	}
}

func waitForManagedCUAExecutor(ctx context.Context, managedExecutor cua.Executor, diagnostics io.Writer) error {
	timeout := managedCUAReadyTimeout(managedExecutor.Name())
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	progress := time.NewTicker(10 * time.Second)
	defer progress.Stop()
	for {
		if err := managedExecutor.Check(readyCtx); err == nil {
			return nil
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("managed CUA executor %s did not become ready within %s", managedExecutor.Name(), timeout)
		case <-progress.C:
			fprintf(diagnostics, "CCR: waiting for managed CUA executor %s to become ready (%s elapsed)\n",
				managedExecutor.Name(), time.Since(started).Round(time.Second))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func managedCUAReadyTimeout(executorName string) time.Duration {
	if executorName == string(cua.ExecutorDocker) {
		return dockerManagedCUAReadyTimeout
	}
	return defaultManagedCUAReadyTimeout
}

func managedCUAApprovalNotifier(out io.Writer) approval.NotifyFunc {
	return func(_ context.Context, prompt approval.Prompt) error {
		fprintf(out, "CCR: approve managed CUA %s action at %s (expires %s)\n", prompt.Request.Kind, prompt.URL, prompt.ExpiresAt.UTC().Format(time.RFC3339))
		return nil
	}
}

func shutdownManagedCUA(ctx context.Context, managedCUA **managedCUALaunch) error {
	if managedCUA == nil || *managedCUA == nil {
		return nil
	}
	launch := *managedCUA
	*managedCUA = nil
	return launch.Shutdown(ctx)
}

type launchCUAAuditor struct {
	memory   *audit.Recorder
	recorder *observability.Recorder
}

func (a *launchCUAAuditor) Record(ctx context.Context, event cua.AuditEvent) error {
	if a == nil {
		return nil
	}
	if a.memory != nil {
		if err := a.memory.Record(ctx, event); err != nil {
			return err
		}
	}
	if a.recorder != nil {
		a.recorder.RecordLifecycle(ctx, observability.LifecycleEvent{
			Name: "CUAAction", Status: event.Status, ActorName: event.Executor,
			ActorKind: string(event.Action), Reason: string(event.Risk) + ":" + string(event.Decision),
		})
	}
	return nil
}

func fprintf(writer io.Writer, format string, args ...any) {
	if writer == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, format, args...)
}
