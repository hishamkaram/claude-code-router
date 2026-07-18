//go:build live

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/config"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLiveRealProviderMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_REAL_MATRIX") != "1" {
		t.Skip("set CCR_LIVE_REAL_MATRIX=1 to run the required local real-provider matrix")
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Fatalf("real-provider matrix requires an installed Claude Code CLI: %v", err)
	}
	dbPath := configuredLiveDBPath(t)
	models := configuredLiveModels(t, ctx, dbPath)
	if len(models) == 0 {
		t.Fatal("real-provider matrix requires at least one configured non-blocked model alias")
	}
	primary, chatOnly := partitionLiveModels(t, ctx, dbPath, models)
	beforeLaunchID := latestLiveLaunchID(t, ctx, dbPath)
	runLiveRealSwitchMatrix(t, ctx, dbPath, primary)
	assertLiveRealRoutes(t, ctx, dbPath, beforeLaunchID, primary, true)
	for _, model := range chatOnly {
		runLiveRealChatOnlyAlias(t, ctx, dbPath, model)
		assertLiveRealRoutes(t, ctx, dbPath, beforeLaunchID, []store.Model{model}, false)
		beforeLaunchID = latestLiveLaunchID(t, ctx, dbPath)
	}
}

func TestLiveConfiguredProviderAutoModeAgentWebFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	prompt := `Use the Agent tool to launch one general-purpose subagent. The subagent must use WebFetch on https://example.com and then return exactly CCR_LIVE_REAL_WEBFETCH_CHILD_OK. After the subagent finishes, reply exactly CCR_LIVE_REAL_WEBFETCH_PARENT_OK. Do not use Bash or shell.`
	out, errOut, modelAlias := runConfiguredProviderProbe(t, ctx, prompt)
	assertConfiguredProviderProbe(t, out, errOut, modelAlias, "CCR_LIVE_REAL_WEBFETCH_PARENT_OK")
}

func TestLiveConfiguredProviderAutoModeWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if os.Getenv("CCR_LIVE_CONFIGURED_PROVIDER") != "1" {
		t.Skip("set CCR_LIVE_CONFIGURED_PROVIDER=1 to run against the configured real provider")
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	modelAlias := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_MODEL_ALIAS"))
	if modelAlias == "" {
		modelAlias = "glm-5-2"
	}
	script := fmt.Sprintf(`export const meta = {
  name: 'ccr-live-real-workflow',
  description: 'Return the live matrix sentinel',
  phases: [{ title: 'Run' }],
}
phase('Run')
const result = await agent('Return exactly CCR_LIVE_REAL_WORKFLOW_CHILD_OK.', { label: 'worker', phase: 'Run', model: %q })
return result

`, gateway.DiscoveryIDForAlias(modelAlias))
	prompt := "Call the Workflow tool exactly once with this script, without changing it:\n" + script +
		"After the workflow completes, reply exactly CCR_LIVE_REAL_WORKFLOW_PARENT_OK. Do not use Bash or shell."
	input := liveStreamInput(t, "/model sonnet", prompt)
	dbPath := configuredLiveDBPath(t)
	out, errOut, err := runLiveCommand(
		ctx, Dependencies{In: strings.NewReader(input)},
		"--db", dbPath, "launch", "--print", "--auth-mode", "preserve",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "auto", "--tools", "Workflow",
	)
	if err != nil {
		t.Fatalf("configured Anthropic-to-provider Workflow error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	combined := out + "\n" + errOut
	for _, forbidden := range []string{"temporarily unavailable", "API Error", "InputValidationError"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("configured Workflow output contains %q\nstdout:\n%s\nstderr:\n%s", forbidden, out, errOut)
		}
	}
	for _, want := range []string{
		"Anthropic subscription login and Anthropic API-key auth are preserved",
		"Registered ccr models are available in Claude Code's /model picker",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("configured Workflow diagnostics missing %q\nstdout:\n%s\nstderr:\n%s", want, out, errOut)
		}
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("configured provider Workflow returned no user-facing response\nstderr:\n%s", errOut)
	}
	assertConfiguredWorkflowEvidence(t, ctx, modelAlias)
}

func runConfiguredProviderProbe(t *testing.T, ctx context.Context, prompt string, claudeArgs ...string) (string, string, string) {
	t.Helper()
	if os.Getenv("CCR_LIVE_CONFIGURED_PROVIDER") != "1" {
		t.Skip("set CCR_LIVE_CONFIGURED_PROVIDER=1 to run against the configured real provider")
	}
	modelAlias := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_MODEL_ALIAS"))
	if modelAlias == "" {
		modelAlias = "glm-5-2"
	}
	args := []string{"launch", "--model", modelAlias, "--print", "--auth-mode", "gateway-token", "--permission-mode", "auto"}
	args = append(args, claudeArgs...)
	if dbPath := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_DB")); dbPath != "" {
		args = append([]string{"--db", dbPath}, args...)
	}
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, args...)
	if err != nil {
		t.Fatalf("configured provider launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	return out, errOut, modelAlias
}

func assertConfiguredProviderProbe(t *testing.T, out, errOut, modelAlias string, sentinels ...string) {
	t.Helper()
	combined := out + "\n" + errOut
	for _, sentinel := range sentinels {
		if !strings.Contains(out, sentinel) {
			t.Fatalf("configured provider output missing %q\nstdout:\n%s\nstderr:\n%s", sentinel, out, errOut)
		}
	}
	for _, forbidden := range []string{"temporarily unavailable", "API Error"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("configured provider output contains %q\nstdout:\n%s\nstderr:\n%s", forbidden, out, errOut)
		}
	}
	for _, want := range []string{
		`Selected ccr model alias "` + modelAlias + `"`,
		"Gateway accepts only the generated local ANTHROPIC_AUTH_TOKEN",
		"Original Anthropic subscription login and Anthropic API-key auth are not active",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("configured provider diagnostics missing %q\nstdout:\n%s\nstderr:\n%s", want, out, errOut)
		}
	}
}

func assertConfiguredWorkflowEvidence(t *testing.T, ctx context.Context, modelAlias string) {
	t.Helper()
	dbPath := configuredLiveDBPath(t)
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) == 0 {
		t.Fatalf("ListLaunches() = %#v, error = %v", launches, err)
	}
	launch := launches[0]
	if launch.State != "completed" || launch.LifecycleState != "observed" {
		t.Fatalf("Workflow launch = %#v", launch)
	}
	agents, err := s.ListRuntimeAgents(ctx, launch.ID, 0, false)
	if err != nil {
		t.Fatalf("ListRuntimeAgents() error = %v", err)
	}
	completedAgent := false
	for _, agent := range agents {
		// SubagentStart does not expose an explicit Workflow worker model. The
		// lifecycle row keeps its spawn-time route; route events below prove the
		// model that actually served the worker request.
		if agent.Status == "completed" && agent.Name == "workflow-subagent" {
			completedAgent = true
			break
		}
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 1000})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	succeededRoutes := 0
	firstPartyRoute := false
	started, stopped := false, false
	for _, event := range events {
		if event.Kind == "route" && event.Status == "succeeded" &&
			event.Route.RouteKind == "registered" && event.Route.ModelAlias == modelAlias {
			succeededRoutes++
		}
		firstPartyRoute = firstPartyRoute || event.Kind == "route" && event.Status == "succeeded" &&
			event.Route.RouteKind == "first-party-anthropic"
		started = started || event.Kind == "lifecycle" && event.Name == "SubagentStart"
		stopped = stopped || event.Kind == "lifecycle" && event.Name == "SubagentStop"
	}
	if !completedAgent || !started || !stopped || succeededRoutes < 1 || !firstPartyRoute {
		t.Fatalf("Workflow evidence incomplete: completed_agent=%t start=%t stop=%t succeeded_routes=%d first_party=%t agents=%#v events=%#v",
			completedAgent, started, stopped, succeededRoutes, firstPartyRoute, agents, events)
	}
}

func configuredLiveDBPath(t *testing.T) string {
	t.Helper()
	if dbPath := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_DB")); dbPath != "" {
		absolute, err := filepath.Abs(dbPath)
		if err != nil {
			t.Fatalf("resolving CCR_LIVE_CONFIGURED_DB: %v", err)
		}
		return absolute
	}
	dbPath, err := config.DefaultDBPath()
	if err != nil {
		t.Fatalf("resolving default CCR database: %v", err)
	}
	return dbPath
}

func configuredLiveModels(t *testing.T, ctx context.Context, dbPath string) []store.Model {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	selected := make([]store.Model, 0, len(models))
	for _, model := range models {
		if model.Status != "blocked" {
			selected = append(selected, model)
		}
	}
	return selected
}

func partitionLiveModels(t *testing.T, ctx context.Context, dbPath string, models []store.Model) ([]store.Model, []store.Model) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	var primary, chatOnly []store.Model
	for _, model := range models {
		provider, err := s.GetProvider(ctx, model.ProviderName)
		if err != nil {
			t.Fatalf("GetProvider(%s) error = %v", model.ProviderName, err)
		}
		if model.Status == "chat-only" || providerDisablesClaudeTools(provider) {
			chatOnly = append(chatOnly, model)
		} else {
			primary = append(primary, model)
		}
	}
	return primary, chatOnly
}

func latestLiveLaunchID(t *testing.T, ctx context.Context, dbPath string) int64 {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		t.Fatalf("ListLaunches() error = %v", err)
	}
	if len(launches) == 0 {
		return 0
	}
	return launches[0].ID
}

func runLiveRealSwitchMatrix(t *testing.T, ctx context.Context, dbPath string, models []store.Model) {
	t.Helper()
	messages := make([]string, 0, 2+4*len(models))
	messages = append(messages, "/model sonnet", "Reply exactly CCR_LIVE_REAL_ANTHROPIC_INITIAL.")
	for index, model := range models {
		aliasSentinel := fmt.Sprintf("CCR_LIVE_REAL_ALIAS_%d", index)
		returnSentinel := fmt.Sprintf("CCR_LIVE_REAL_ANTHROPIC_RETURN_%d", index)
		messages = append(
			messages,
			"/model "+gateway.DiscoveryIDForAlias(model.Alias),
			"Reply exactly "+aliasSentinel+".",
			"/model sonnet",
			"Reply exactly "+returnSentinel+".",
		)
	}
	input := liveStreamInput(t, messages...)
	out, errOut, err := runLiveCommand(
		ctx, Dependencies{In: strings.NewReader(input)},
		"--db", dbPath, "launch", "--print",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "auto",
	)
	if err != nil {
		t.Fatalf("real provider switch matrix error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for index := range models {
		for _, sentinel := range []string{
			fmt.Sprintf("CCR_LIVE_REAL_ALIAS_%d", index),
			fmt.Sprintf("CCR_LIVE_REAL_ANTHROPIC_RETURN_%d", index),
		} {
			if !strings.Contains(out, sentinel) {
				t.Fatalf("real provider matrix output missing %q\nstdout:\n%s\nstderr:\n%s", sentinel, out, errOut)
			}
		}
	}
	if !strings.Contains(out, "CCR_LIVE_REAL_ANTHROPIC_INITIAL") {
		t.Fatalf("real provider matrix output missing first-party sentinel\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

func runLiveRealChatOnlyAlias(t *testing.T, ctx context.Context, dbPath string, model store.Model) {
	t.Helper()
	sentinel := "CCR_LIVE_REAL_CHAT_" + strings.ToUpper(strings.ReplaceAll(model.Alias, "-", "_"))
	out, errOut, err := runLiveCommand(
		ctx, Dependencies{In: strings.NewReader("Reply exactly " + sentinel + ".\n")},
		"--db", dbPath, "launch", "--model", model.Alias, "--print", "--auth-mode", "gateway-token",
	)
	if err != nil {
		t.Fatalf("real chat-only alias %q error = %v\nstdout:\n%s\nstderr:\n%s", model.Alias, err, out, errOut)
	}
	if !strings.Contains(out, sentinel) {
		t.Fatalf("real chat-only alias %q output missing %q\nstdout:\n%s\nstderr:\n%s", model.Alias, sentinel, out, errOut)
	}
}

func assertLiveRealRoutes(t *testing.T, ctx context.Context, dbPath string, afterLaunchID int64, models []store.Model, requireAnthropic bool) {
	t.Helper()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) == 0 || launches[0].ID <= afterLaunchID {
		t.Fatalf("new real matrix launch not found: launches=%#v error=%v", launches, err)
	}
	launch := launches[0]
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 5_000})
	if err != nil {
		t.Fatalf("ListTraceEvents() error = %v", err)
	}
	seenAliases := make(map[string]bool, len(models))
	seenAnthropic := false
	for _, event := range events {
		if event.Kind != "route" || event.Status != "succeeded" {
			continue
		}
		if event.Route.RouteKind == "first-party-anthropic" {
			seenAnthropic = true
		}
		if event.Route.RouteKind == "registered" {
			seenAliases[event.Route.ModelAlias] = true
		}
	}
	for _, model := range models {
		if !seenAliases[model.Alias] {
			t.Fatalf("real matrix trace missing alias %q: %#v", model.Alias, events)
		}
	}
	if requireAnthropic && !seenAnthropic {
		t.Fatalf("real matrix trace missing first-party Anthropic route: %#v", events)
	}
}
