package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLaunchManagedCUAStartsLaunchScopedRuntime(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CCR_TEST_CUA_TOKEN", "test-external-token")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	seedManagedCUAStore(t, ctx, dbPath, true)

	managed := newCLITestManagedCUA(t)
	launcher := &fakeLauncher{pid: os.Getpid()}
	starts := 0
	validatedURL := false
	var gatewayConfig gateway.Config
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		ValidateExternalCUAURL: func(_ context.Context, raw string) error {
			validatedURL = raw == "https://executor.example/cua"
			return nil
		},
		StartManagedCUA: func(_ context.Context, start managedCUAStart) (*managedCUALaunch, error) {
			starts++
			if start.Config.Mode != cua.ModeManaged || start.Config.Executor != "external:fixture" || start.ExternalURL != "https://executor.example/cua" || start.Project == "" {
				t.Fatal("managed CUA start did not preserve the requested non-secret configuration")
			}
			if start.ExternalToken != "test-external-token" {
				t.Fatal("managed CUA start did not resolve the external token")
			}
			return managed, nil
		},
		StartGateway: func(startCtx context.Context, cfg gateway.Config) (*gateway.Server, error) {
			gatewayConfig = cfg
			return gateway.Start(startCtx, cfg)
		},
	}, "--db", dbPath, "launch", "--model", "cua", "--ccr-cua-mode", "managed", "--ccr-cua-executor", "external:fixture", "--ccr-cua-external-url", "https://executor.example/cua", "--ccr-cua-external-token-env", "CCR_TEST_CUA_TOKEN")
	if err != nil {
		t.Fatalf("launch error = %v", err)
	}
	if starts != 1 || launcher.starts != 1 {
		t.Fatalf("managed CUA starts=%d launcher starts=%d, want 1 and 1", starts, launcher.starts)
	}
	if !validatedURL {
		t.Fatal("managed CUA launch did not validate the external executor URL before startup")
	}
	if gatewayConfig.ManagedCUA != managed.Runtime() || gatewayConfig.ManagedCUAProject == "" {
		t.Fatalf("gateway managed CUA config = %#v", gatewayConfig)
	}
	if !launcher.unsetsEnv("CCR_TEST_CUA_TOKEN") {
		t.Fatalf("Claude Code child inherited external CUA token: %s", launcher.environmentSummary())
	}
}

func TestLaunchManagedCUARejectsUnsupportedModelBeforeRuntimeStart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	seedManagedCUAStore(t, ctx, dbPath, false)

	starts := 0
	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		StartManagedCUA: func(context.Context, managedCUAStart) (*managedCUALaunch, error) {
			starts++
			return nil, nil
		},
	}, "--db", dbPath, "launch", "--model", "cua", "--ccr-cua-mode", "managed", "--ccr-cua-executor", "external:fixture", "--ccr-cua-external-url", "https://executor.example/cua", "--ccr-cua-external-token-env", "CCR_TEST_CUA_TOKEN")
	if err == nil || starts != 0 || launcher.starts != 0 {
		t.Fatalf("launch error=%v starts=%d launcher=%d", err, starts, launcher.starts)
	}
}

func TestLaunchManagedCUARejectsUnsafeExternalURLBeforeCreatingLaunch(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CCR_TEST_CUA_TOKEN", "test-external-token")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	seedManagedCUAStore(t, ctx, dbPath, true)

	launcher := &fakeLauncher{pid: os.Getpid()}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
	}, "--db", dbPath, "launch", "--model", "cua", "--ccr-cua-mode", "managed",
		"--ccr-cua-executor", "external:fixture", "--ccr-cua-external-url", "https://127.0.0.1/cua",
		"--ccr-cua-external-token-env", "CCR_TEST_CUA_TOKEN")
	if err == nil || !strings.Contains(err.Error(), "validating external managed CUA URL") {
		t.Fatalf("launch error = %v, want unsafe external URL rejection", err)
	}
	if launcher.starts != 0 {
		t.Fatalf("launcher starts = %d, want 0", launcher.starts)
	}
	s, openErr := store.Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	launches, listErr := s.ListLaunches(ctx)
	if listErr != nil {
		t.Fatalf("ListLaunches() error = %v", listErr)
	}
	if len(launches) != 0 {
		t.Fatalf("launch records = %d, want 0 after validation failure", len(launches))
	}
}

func TestManagedCUAReadyTimeoutAllowsFirstDockerPull(t *testing.T) {
	t.Parallel()

	if got := managedCUAReadyTimeout(string(cua.ExecutorDocker)); got < 2*time.Minute {
		t.Fatalf("Docker managed CUA ready timeout = %s, want at least 2m for first image pulls", got)
	}
	if got := managedCUAReadyTimeout(string(cua.ExecutorLocalBrowser)); got != defaultManagedCUAReadyTimeout {
		t.Fatalf("local-browser managed CUA ready timeout = %s, want %s", got, defaultManagedCUAReadyTimeout)
	}
}

func seedManagedCUAStore(t *testing.T, ctx context.Context, dbPath string, supportsComputerUse bool) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, store.Provider{Name: "openai", Type: "openai-compatible", BaseURL: "https://provider.example", SupportsTools: true, SupportsStreaming: true, SupportsResponses: true}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	if err := s.AddModel(ctx, store.Model{Alias: "cua", ProviderName: "openai", ProviderModel: "computer-model", Status: "degraded", CapabilityOverrides: modelcap.Values{
		Kind: modelcap.KindResponses, SupportsResponses: modelcap.Bool(true), SupportsComputerUse: modelcap.Bool(supportsComputerUse),
	}}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
}

func newCLITestManagedCUA(t *testing.T) *managedCUALaunch {
	t.Helper()
	runtime, err := cua.NewManagedRuntime(context.Background(), cua.Config{Mode: cua.ModeManaged, Executor: "external:fixture"}, cliManagedCUAExecutor{}, cliManagedCUAAuthorizer{}, nil)
	if err != nil {
		t.Fatalf("NewManagedRuntime() error = %v", err)
	}
	return &managedCUALaunch{runtime: runtime}
}

type cliManagedCUAExecutor struct{}

func (cliManagedCUAExecutor) Name() string                { return "external:fixture" }
func (cliManagedCUAExecutor) Check(context.Context) error { return nil }
func (cliManagedCUAExecutor) Execute(context.Context, cua.Action) (cua.Observation, error) {
	return cua.Observation{}, nil
}
func (cliManagedCUAExecutor) Close() error { return nil }

type cliManagedCUAAuthorizer struct{}

func (cliManagedCUAAuthorizer) Authorize(context.Context, string, cua.Action) (cua.Decision, error) {
	return cua.DecisionApprove, nil
}
