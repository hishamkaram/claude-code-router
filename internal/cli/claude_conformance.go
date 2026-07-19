package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	conformancecheck "github.com/hishamkaram/claude-code-router/internal/conformance"
	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	claudeConformanceAgentParent    = "CCR_CONFORMANCE_AGENT_PARENT_OK"
	claudeConformanceWorkflowChild  = "CCR_CONFORMANCE_WORKFLOW_CHILD_OK"
	claudeConformanceWorkflowParent = "CCR_CONFORMANCE_WORKFLOW_PARENT_OK"
	claudeConformanceStepTimeout    = 2 * time.Minute
)

type claudeConformancePlan struct {
	targetAlias      string
	includeAnthropic bool
	requireWorkers   bool
	streamAliases    []string
	explicitAliases  []string
	messages         []string
	markers          []string
}

type claudeStreamExpectation struct {
	eventType string
	marker    string
}

type claudeStreamStep struct {
	input       string
	expectation claudeStreamExpectation
}

type claudeStreamObserver struct {
	mu           sync.Mutex
	output       bytes.Buffer
	pending      bytes.Buffer
	expectations []claudeStreamExpectation
	next         int
	observed     chan struct{}
}

func runClaudeConformance(ctx context.Context, opts *options, deps Dependencies, s *store.Store, alias string, includeAnthropic bool) conformancecheck.Check {
	started := time.Now()
	check := conformancecheck.Check{
		Name: "claude_runtime", Status: conformancecheck.StatusPassed,
		Evidence: "Claude Code route, lifecycle, subagent, workflow, and trace matrix passed",
	}
	if _, err := liveclaude.Check(ctx); err != nil {
		check.Status = conformancecheck.StatusFailed
		check.Evidence = "Claude Code CLI is unavailable"
		check.Latency = time.Since(started)
		return check
	}
	plan, err := buildClaudeConformancePlan(ctx, s, alias, includeAnthropic)
	if err == nil {
		err = executeClaudeConformancePlan(ctx, opts, deps, s, plan)
	}
	if err != nil {
		check.Status = conformancecheck.StatusFailed
		check.Evidence = "Claude Code runtime matrix failed without storing CLI or provider content"
	} else if !plan.requireWorkers {
		check.Evidence = "Claude Code route, lifecycle, and trace matrix passed; worker checks were not applicable to the tool-disabled target"
	}
	check.Latency = time.Since(started)
	return check
}

func buildClaudeConformancePlan(ctx context.Context, s *store.Store, alias string, includeAnthropic bool) (claudeConformancePlan, error) {
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return claudeConformancePlan{}, err
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return claudeConformancePlan{}, err
	}
	toolsDisabled, err := modelDisablesClaudeTools(model, provider)
	if err != nil {
		return claudeConformancePlan{}, err
	}
	plan := claudeConformancePlan{
		targetAlias: alias, includeAnthropic: includeAnthropic,
		requireWorkers: !toolsDisabled,
	}
	if !includeAnthropic {
		plan.streamAliases = []string{alias}
		plan.messages = append(plan.messages, "Reply exactly CCR_CONFORMANCE_ROUTE_0 and do not use tools.")
		plan.markers = append(plan.markers, "CCR_CONFORMANCE_ROUTE_0")
		plan.addWorkerMessages()
		return plan, nil
	}

	safeAliases, err := routableModelAliases(ctx, s, false)
	if err != nil {
		return claudeConformancePlan{}, err
	}
	allAliases, err := routableModelAliases(ctx, s, true)
	if err != nil {
		return claudeConformancePlan{}, err
	}
	if err := validateCompleteClaudeAliasMatrix(ctx, s, allAliases); err != nil {
		return claudeConformancePlan{}, err
	}
	plan.streamAliases = safeAliases
	plan.explicitAliases = aliasDifference(allAliases, safeAliases)
	if !containsAlias(allAliases, alias) {
		return claudeConformancePlan{}, fmt.Errorf("target alias %q is not routable by Claude Code", alias)
	}
	plan.messages = append(
		plan.messages,
		"Reply exactly CCR_CONFORMANCE_ANTHROPIC_INITIAL and do not use tools.",
	)
	plan.markers = append(plan.markers, "CCR_CONFORMANCE_ANTHROPIC_INITIAL")
	for index, routeAlias := range safeAliases {
		routeModel, err := s.GetModel(ctx, routeAlias)
		if err != nil {
			return claudeConformancePlan{}, err
		}
		routeID, err := gateway.DiscoveryIDForModel(routeModel)
		if err != nil {
			return claudeConformancePlan{}, err
		}
		routeMarker := fmt.Sprintf("CCR_CONFORMANCE_ALIAS_%d", index)
		returnMarker := fmt.Sprintf("CCR_CONFORMANCE_ANTHROPIC_RETURN_%d", index)
		plan.messages = append(
			plan.messages,
			"/model "+routeID,
			"Reply exactly "+routeMarker+" and do not use tools.",
		)
		plan.markers = append(plan.markers, routeMarker)
		if routeAlias == alias {
			plan.addWorkerMessages()
		}
		plan.messages = append(
			plan.messages,
			"/model sonnet",
			"Reply exactly "+returnMarker+" and do not use tools.",
		)
		plan.markers = append(plan.markers, returnMarker)
	}
	return plan, nil
}

func (p *claudeConformancePlan) addWorkerMessages() {
	if !p.requireWorkers {
		return
	}
	p.messages = append(
		p.messages,
		"Use the Agent tool to launch one general-purpose subagent. The subagent must return exactly CCR_CONFORMANCE_AGENT_CHILD_OK. After it finishes, reply exactly "+claudeConformanceAgentParent+". Do not use Bash or shell.",
		"Call the Workflow tool exactly once with a workflow that runs one worker returning exactly "+claudeConformanceWorkflowChild+". After the workflow starts, reply exactly "+claudeConformanceWorkflowParent+". Do not use Bash or shell.",
	)
	p.markers = append(p.markers, claudeConformanceAgentParent, claudeConformanceWorkflowParent)
}

func validateCompleteClaudeAliasMatrix(ctx context.Context, s *store.Store, routable []string) error {
	routableSet := make(map[string]struct{}, len(routable))
	for _, alias := range routable {
		routableSet[alias] = struct{}{}
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return err
	}
	for index := range models {
		model := &models[index]
		if model.Status == "blocked" {
			continue
		}
		if _, ok := routableSet[model.Alias]; !ok {
			return fmt.Errorf("non-blocked alias %q cannot participate in the Claude matrix", model.Alias)
		}
	}
	return nil
}

func aliasDifference(all, selected []string) []string {
	selectedSet := make(map[string]struct{}, len(selected))
	for _, alias := range selected {
		selectedSet[alias] = struct{}{}
	}
	difference := make([]string, 0, len(all))
	for _, alias := range all {
		if _, ok := selectedSet[alias]; !ok {
			difference = append(difference, alias)
		}
	}
	return difference
}

func containsAlias(aliases []string, expected string) bool {
	for _, alias := range aliases {
		if alias == expected {
			return true
		}
	}
	return false
}

func executeClaudeConformancePlan(ctx context.Context, opts *options, deps Dependencies, s *store.Store, plan claudeConformancePlan) error {
	if err := runClaudeStreamMatrix(ctx, opts, deps, s, plan); err != nil {
		return err
	}
	for index, alias := range plan.explicitAliases {
		if err := runClaudeExplicitAliasProbe(ctx, opts, deps, s, alias, index); err != nil {
			return err
		}
	}
	return nil
}

func runClaudeStreamMatrix(ctx context.Context, opts *options, deps Dependencies, s *store.Store, plan claudeConformancePlan) error {
	steps, err := encodeClaudeStreamSteps(plan.messages, plan.markers)
	if err != nil {
		return err
	}
	observer := newClaudeStreamObserver(steps)
	var errOut bytes.Buffer
	inputReader, inputWriter := io.Pipe()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	feedDone := make(chan error, 1)
	go func() {
		feedErr := feedClaudeStreamSteps(streamCtx, inputWriter, steps, observer.observed)
		if feedErr != nil {
			cancelStream()
		}
		feedDone <- feedErr
	}()
	probe := &cobra.Command{}
	probe.SetIn(inputReader)
	probe.SetOut(observer)
	probe.SetErr(&errOut)
	invocation := launchInvocation{
		printMode: true, permissionMode: "auto", noStatusline: true,
		authMode: launchAuthModeGatewayToken,
		claudeArgs: []string{
			"--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages", "--verbose",
		},
	}
	if plan.includeAnthropic {
		invocation.authMode = launchAuthModePreserve
	} else {
		invocation.modelAlias = plan.targetAlias
	}
	launchErr := runLaunch(streamCtx, probe, opts, deps, invocation)
	cancelStream()
	closeErr := inputReader.CloseWithError(launchErr)
	feedErr := <-feedDone
	if feedErr != nil {
		return feedErr
	}
	if launchErr != nil {
		return fmt.Errorf("running Claude stream matrix: %w", launchErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing Claude stream input: %w", closeErr)
	}
	if err := requireClaudeMarkers(observer.String(), plan.markers); err != nil {
		return err
	}
	return verifyLatestClaudeMatrix(ctx, s, plan.streamAliases, plan.includeAnthropic, plan.requireWorkers)
}

func runClaudeExplicitAliasProbe(ctx context.Context, opts *options, deps Dependencies, s *store.Store, alias string, index int) error {
	marker := fmt.Sprintf("CCR_CONFORMANCE_EXPLICIT_%d", index)
	var out bytes.Buffer
	probe := &cobra.Command{}
	probe.SetIn(strings.NewReader(""))
	probe.SetOut(&out)
	probe.SetErr(&out)
	invocation := launchInvocation{
		modelAlias: alias, printMode: true, authMode: launchAuthModeGatewayToken,
		noStatusline: true,
		claudeArgs:   []string{"Reply exactly " + marker + " and do not use tools."},
	}
	if err := runLaunch(ctx, probe, opts, deps, invocation); err != nil {
		return fmt.Errorf("running explicit Claude alias probe: %w", err)
	}
	if err := requireClaudeMarkers(out.String(), []string{marker}); err != nil {
		return err
	}
	return verifyLatestClaudeRoute(ctx, s, alias)
}

func encodeClaudeStreamInput(messages []string) (string, error) {
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, message := range messages {
		payload := map[string]any{
			"type": "user",
			"message": map[string]string{
				"role": "user", "content": message,
			},
			"parent_tool_use_id": nil,
		}
		if err := encoder.Encode(payload); err != nil {
			return "", fmt.Errorf("encoding Claude stream input: %w", err)
		}
	}
	return input.String(), nil
}

func encodeClaudeStreamSteps(messages, markers []string) ([]claudeStreamStep, error) {
	steps := make([]claudeStreamStep, 0, len(messages))
	markerIndex := 0
	for _, message := range messages {
		encoded, err := encodeClaudeStreamInput([]string{message})
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(message, "/model ") {
			steps = append(steps, claudeStreamStep{
				input: encoded,
				expectation: claudeStreamExpectation{
					marker: strings.TrimPrefix(message, "/model "),
				},
			})
			continue
		}
		if markerIndex >= len(markers) {
			return nil, fmt.Errorf("claude stream matrix has a prompt without a response marker")
		}
		steps = append(steps, claudeStreamStep{
			input: encoded,
			expectation: claudeStreamExpectation{
				eventType: "assistant", marker: markers[markerIndex],
			},
		})
		markerIndex++
	}
	if markerIndex != len(markers) {
		return nil, fmt.Errorf("claude stream matrix has a response marker without a prompt")
	}
	return steps, nil
}

func newClaudeStreamObserver(steps []claudeStreamStep) *claudeStreamObserver {
	expectations := make([]claudeStreamExpectation, 0, len(steps))
	for _, step := range steps {
		expectations = append(expectations, step.expectation)
	}
	return &claudeStreamObserver{
		expectations: expectations,
		observed:     make(chan struct{}, len(expectations)),
	}
}

func (o *claudeStreamObserver) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	written, err := o.output.Write(p)
	if _, pendingErr := o.pending.Write(p); pendingErr != nil && err == nil {
		err = pendingErr
	}
	for {
		line, readErr := o.pending.ReadString('\n')
		if readErr != nil {
			_, _ = o.pending.WriteString(line)
			break
		}
		o.observeLine([]byte(line))
	}
	return written, err
}

func (o *claudeStreamObserver) observeLine(line []byte) {
	if o.next >= len(o.expectations) {
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	expected := o.expectations[o.next]
	if (expected.eventType != "" && event.Type != expected.eventType) ||
		!bytes.Contains(line, []byte(expected.marker)) {
		return
	}
	o.next++
	o.observed <- struct{}{}
}

func (o *claudeStreamObserver) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.output.String()
}

func feedClaudeStreamSteps(ctx context.Context, writer *io.PipeWriter, steps []claudeStreamStep, observed <-chan struct{}) (resultErr error) {
	return feedClaudeStreamStepsWithTimeout(ctx, writer, steps, observed, claudeConformanceStepTimeout)
}

func feedClaudeStreamStepsWithTimeout(ctx context.Context, writer *io.PipeWriter, steps []claudeStreamStep, observed <-chan struct{}, stepTimeout time.Duration) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, writer.CloseWithError(resultErr))
	}()
	for _, step := range steps {
		if _, err := io.WriteString(writer, step.input); err != nil {
			return fmt.Errorf("writing Claude stream step: %w", err)
		}
		if err := waitForClaudeStreamStep(ctx, observed, stepTimeout); err != nil {
			return err
		}
	}
	return nil
}

func waitForClaudeStreamStep(ctx context.Context, observed <-chan struct{}, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-observed:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for Claude stream step: %w", ctx.Err())
	case <-timer.C:
		return fmt.Errorf("waiting for Claude stream step: timed out after %s", timeout)
	}
}

func requireClaudeMarkers(output string, markers []string) error {
	for _, marker := range markers {
		if !strings.Contains(output, marker) {
			return fmt.Errorf("claude output omitted a runtime matrix marker")
		}
	}
	return nil
}

func verifyLatestClaudeMatrix(ctx context.Context, s *store.Store, aliases []string, requireAnthropic, requireWorkers bool) error {
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) == 0 {
		return fmt.Errorf("reading Claude launch evidence")
	}
	launch := launches[0]
	if launch.State != "completed" || launch.LifecycleState != "observed" {
		return fmt.Errorf("claude launch or lifecycle did not complete")
	}
	sessions, err := s.ListClaudeSessions(ctx, launch.ID, false)
	if err != nil || len(sessions) == 0 {
		return fmt.Errorf("claude lifecycle hook evidence is missing")
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 1000})
	if err != nil {
		return err
	}
	if err := verifyClaudeRoutes(events, aliases, requireAnthropic); err != nil {
		return err
	}
	if requireWorkers {
		return verifyClaudeWorkers(ctx, s, launch.ID, events)
	}
	return nil
}

func verifyLatestClaudeRoute(ctx context.Context, s *store.Store, alias string) error {
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) == 0 {
		return fmt.Errorf("reading explicit Claude launch evidence")
	}
	launch := launches[0]
	if launch.State != "completed" {
		return fmt.Errorf("explicit Claude launch did not complete")
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{LaunchID: launch.ID, Limit: 1000})
	if err != nil {
		return err
	}
	return verifyClaudeRoutes(events, []string{alias}, false)
}

func verifyClaudeRoutes(events []store.TraceEvent, aliases []string, requireAnthropic bool) error {
	routed := make(map[string]bool, len(aliases))
	anthropic := false
	for index := range events {
		event := &events[index]
		if event.Kind != "route" || event.Status != "succeeded" {
			continue
		}
		if event.Route.RouteKind == "first-party-anthropic" {
			anthropic = true
		}
		if event.Route.RouteKind == "registered" {
			routed[event.Route.ModelAlias] = true
		}
	}
	for _, alias := range aliases {
		if !routed[alias] {
			return fmt.Errorf("claude route trace omitted a configured alias")
		}
	}
	if requireAnthropic && !anthropic {
		return fmt.Errorf("claude route trace omitted first-party Anthropic")
	}
	return nil
}

func verifyClaudeWorkers(ctx context.Context, s *store.Store, launchID int64, events []store.TraceEvent) error {
	agents, err := s.ListRuntimeAgents(ctx, launchID, 0, false)
	if err != nil {
		return err
	}
	starts, stops := 0, 0
	for index := range events {
		event := &events[index]
		if event.Kind == "lifecycle" && event.Lifecycle.Name == "SubagentStart" {
			starts++
		}
		if event.Kind == "lifecycle" && event.Lifecycle.Name == "SubagentStop" {
			stops++
		}
	}
	if len(agents) < 2 || starts < 2 || stops < 2 {
		return fmt.Errorf("claude subagent or workflow lifecycle evidence is incomplete")
	}
	return nil
}
