package cli

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestBuildClaudeConformancePlanUsesSelectiveIDsAndSeparatesToolDisabledAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openClaudeConformanceStore(t, ctx)
	addClaudeConformanceProvider(t, ctx, s, store.Provider{
		Name: "full", Type: "openai-compatible", Protocol: "openai-compatible",
		SupportsTools: true, SupportsStreaming: true, Mode: "full",
	})
	addClaudeConformanceProvider(t, ctx, s, store.Provider{
		Name: "chat", Type: "openai-compatible", Protocol: "openai-compatible",
		SupportsStreaming: true, Mode: "chat-only",
	})
	addClaudeConformanceModel(t, ctx, s, store.Model{
		Alias: "my-sonnet", ProviderName: "full", ProviderModel: "model-full", Status: "full",
	})
	addClaudeConformanceModel(t, ctx, s, store.Model{
		Alias: "chat", ProviderName: "chat", ProviderModel: "model-chat", Status: "chat-only",
	})
	addClaudeConformanceModel(t, ctx, s, store.Model{
		Alias: "blocked", ProviderName: "full", ProviderModel: "model-blocked", Status: "blocked",
	})

	plan, err := buildClaudeConformancePlan(ctx, s, "my-sonnet", true)
	if err != nil {
		t.Fatalf("buildClaudeConformancePlan() error = %v", err)
	}
	if !plan.requireWorkers || !plan.includeAnthropic {
		t.Fatalf("plan flags = %#v", plan)
	}
	if !reflect.DeepEqual(plan.streamAliases, []string{"my-sonnet"}) {
		t.Fatalf("stream aliases = %#v", plan.streamAliases)
	}
	if !reflect.DeepEqual(plan.explicitAliases, []string{"chat"}) {
		t.Fatalf("explicit aliases = %#v", plan.explicitAliases)
	}
	messages := strings.Join(plan.messages, "\n")
	for _, expected := range []string{
		"/model sonnet", "/model anthropic.ccr.my-s%6fnnet",
		claudeConformanceAgentParent, claudeConformanceWorkflowParent,
	} {
		if !strings.Contains(messages, expected) {
			t.Fatalf("plan messages omit %q:\n%s", expected, messages)
		}
	}
	if strings.Contains(messages, "blocked") || strings.Contains(messages, "anthropic.ccr.chat") {
		t.Fatalf("tool-enabled stream includes blocked or chat-only alias:\n%s", messages)
	}
	encoded, err := encodeClaudeStreamInput(plan.messages)
	if err != nil || strings.Count(encoded, `"type":"user"`) != len(plan.messages) {
		t.Fatalf("encodeClaudeStreamInput() count = %d, error = %v", strings.Count(encoded, `"type":"user"`), err)
	}
}

func TestBuildClaudeConformancePlanMakesWorkerChecksNotApplicableForChatOnlyTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openClaudeConformanceStore(t, ctx)
	addClaudeConformanceProvider(t, ctx, s, store.Provider{
		Name: "chat", Type: "openai-compatible", Protocol: "openai-compatible",
		SupportsStreaming: true, Mode: "chat-only",
	})
	addClaudeConformanceModel(t, ctx, s, store.Model{
		Alias: "chat", ProviderName: "chat", ProviderModel: "model-chat", Status: "chat-only",
	})
	plan, err := buildClaudeConformancePlan(ctx, s, "chat", false)
	if err != nil {
		t.Fatalf("buildClaudeConformancePlan() error = %v", err)
	}
	if plan.requireWorkers || len(plan.messages) != 1 || len(plan.markers) != 1 {
		t.Fatalf("chat-only plan = %#v", plan)
	}
}

func TestEncodeClaudeStreamStepsSeparatesModelCommandAndPrompt(t *testing.T) {
	t.Parallel()
	steps, err := encodeClaudeStreamSteps([]string{
		"/model anthropic.ccr.fixture",
		"Reply exactly CCR_CONFORMANCE_FIRST.",
		"Reply exactly CCR_CONFORMANCE_SECOND.",
	}, []string{"CCR_CONFORMANCE_FIRST", "CCR_CONFORMANCE_SECOND"})
	if err != nil {
		t.Fatalf("encodeClaudeStreamSteps() error = %v", err)
	}
	if len(steps) != 3 || steps[0].expectation.eventType != "" || steps[0].expectation.marker != "anthropic.ccr.fixture" ||
		steps[1].expectation.eventType != "assistant" || steps[2].expectation.marker != "CCR_CONFORMANCE_SECOND" {
		t.Fatalf("encoded steps = %#v", steps)
	}
	for _, step := range steps {
		if strings.Count(step.input, `"type":"user"`) != 1 {
			t.Fatalf("step input = %q", step.input)
		}
	}
	if _, err := encodeClaudeStreamSteps([]string{"prompt"}, nil); err == nil {
		t.Fatal("encodeClaudeStreamSteps() accepted a prompt without a marker")
	}
	if _, err := encodeClaudeStreamSteps(nil, []string{"marker"}); err == nil {
		t.Fatal("encodeClaudeStreamSteps() accepted a marker without a prompt")
	}
}

func TestClaudeStreamObserverDistinguishesReplayAndAssistantEvents(t *testing.T) {
	t.Parallel()
	observer := newClaudeStreamObserver([]claudeStreamStep{
		{expectation: claudeStreamExpectation{marker: "fixture"}},
		{expectation: claudeStreamExpectation{eventType: "assistant", marker: "CCR_FIRST"}},
	})
	if _, err := observer.Write([]byte("{\"type\":\"assistant\",\"text\":\"startup\"}\n")); err != nil {
		t.Fatalf("Write(startup) error = %v", err)
	}
	select {
	case <-observer.observed:
		t.Fatal("startup event satisfied a model command expectation")
	default:
	}
	if _, err := observer.Write([]byte("{\"type\":\"system\",\"message\":{\"content\":\"model fixture\"}}\n")); err != nil {
		t.Fatalf("Write(model replay) error = %v", err)
	}
	select {
	case <-observer.observed:
	default:
		t.Fatal("model replay was not observed")
	}
	if _, err := observer.Write([]byte("{\"type\":\"user\",\"message\":{\"content\":\"CCR_FIRST\"}}\n")); err != nil {
		t.Fatalf("Write(prompt replay) error = %v", err)
	}
	select {
	case <-observer.observed:
		t.Fatal("prompt replay satisfied an assistant expectation")
	default:
	}
	if _, err := observer.Write([]byte("{\"type\":\"assistant\",\"text\":\"CCR_FIRST\"}\n")); err != nil {
		t.Fatalf("Write(assistant) error = %v", err)
	}
	select {
	case <-observer.observed:
	default:
		t.Fatal("assistant marker was not observed")
	}
	if !strings.Contains(observer.String(), "model fixture") || !strings.Contains(observer.String(), "CCR_FIRST") {
		t.Fatalf("observer output = %q", observer.String())
	}
}

func TestFeedClaudeStreamStepsTimesOutAndClosesPipeWithError(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close() })
	readDone := make(chan error, 1)
	go func() {
		input := make([]byte, len("step\n"))
		if _, err := io.ReadFull(reader, input); err != nil {
			readDone <- err
			return
		}
		buffer := make([]byte, 1)
		_, err := reader.Read(buffer)
		readDone <- err
	}()

	err := feedClaudeStreamStepsWithTimeout(
		context.Background(), writer,
		[]claudeStreamStep{{input: "step\n"}}, make(chan struct{}), 20*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("feedClaudeStreamStepsWithTimeout() error = %v, want timeout", err)
	}
	if readErr := <-readDone; readErr == nil || !strings.Contains(readErr.Error(), "timed out") {
		t.Fatalf("pipe read error = %v, want propagated timeout", readErr)
	}
}

func openClaudeConformanceStore(t *testing.T, ctx context.Context) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return s
}

func addClaudeConformanceProvider(t *testing.T, ctx context.Context, s *store.Store, provider store.Provider) {
	t.Helper()
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider(%s) error = %v", provider.Name, err)
	}
}

func addClaudeConformanceModel(t *testing.T, ctx context.Context, s *store.Store, model store.Model) {
	t.Helper()
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel(%s) error = %v", model.Alias, err)
	}
}
