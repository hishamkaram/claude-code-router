package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	openairesponses "github.com/hishamkaram/claude-code-router/internal/responses"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestWorkflowChildWithoutSystemEnvelopeFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "active", []agentChildDescriptor{{workflow: true, prompt: "workflow child"}})
	_, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "workflow child"}},
	})
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable {
		t.Fatalf("unmarked workflow child validation error = %#v; want visible refusal", validationErr)
	}
}

func TestDynamicWorkflowChildWithoutSystemEnvelopeFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}
	registerAgentChildForTest(t, &h.activeModel, "session-a", "active", []agentChildDescriptor{{workflow: true}})
	_, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{
		Model:    "haiku",
		Messages: []anthropicMessage{{Role: "user", Content: "dynamic worker request"}},
	})
	if validationErr == nil || validationErr.status != http.StatusServiceUnavailable {
		t.Fatalf("unmarked dynamic workflow child validation error = %#v; want visible refusal", validationErr)
	}
}

func TestAgentChildDescriptorsNormalizeTypedResponsesAndEscapedPrompts(t *testing.T) {
	t.Parallel()
	typedResponse := &openairesponses.AnthropicResponse{
		Content: []openairesponses.AnthropicContentBlock{{
			Type: "tool_use", Name: "Agent",
			Input: map[string]any{"prompt": "line one \"quoted\"\nline two", "description": "research"},
		}},
	}
	descriptors := agentChildDescriptorsFromValue(typedResponse)
	if len(descriptors) != 1 || descriptors[0].prompt == "" {
		t.Fatalf("typed response descriptors = %#v, want one Agent descriptor", descriptors)
	}
	req := anthropicRequest{
		Model:    "haiku",
		Messages: []anthropicMessage{{Role: "user", Content: "line one \"quoted\"\nline two"}},
	}
	if !agentChildRequestMatches(req, descriptors[0]) {
		t.Fatalf("escaped child prompt did not match decoded request: descriptor=%#v request=%#v", descriptors[0], req)
	}
}

func TestWorkflowChildDescriptorRequiresSubagentIdentity(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": "phase('Run'); await agent('Return the workflow result', {label: 'worker'})"},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || !descriptors[0].workflow {
		t.Fatalf("Workflow descriptors = %#v, want one workflow reservation", descriptors)
	}
	child := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "Return the workflow result"}},
	}
	if !agentChildRequestMatches(child, descriptors[0]) {
		t.Fatal("workflow child with subagent identity did not match")
	}
	unrelated := anthropicRequest{Model: "sonnet", System: "You are a subagent spawned by a workflow orchestration script.", Messages: []anthropicMessage{{Role: "user", Content: "The workflow is discussed here"}}}
	if agentChildRequestMatches(unrelated, descriptors[0]) {
		t.Fatal("unrelated workflow text consumed workflow child reservation")
	}
}

func TestWorkflowDescriptorsReserveEveryWorkerAndUseCurrentTurnWithSystemIdentity(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": "await agent('worker one'); await agent(\"worker two\")"},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 2 || descriptors[0].prompt != "worker one" || descriptors[1].prompt != "worker two" {
		t.Fatalf("workflow descriptors = %#v, want two worker prompts", descriptors)
	}
	req := anthropicRequest{
		Model:    "haiku",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "worker two"}},
	}
	if !agentChildRequestMatches(req, descriptors[1]) || agentChildRequestMatches(req, descriptors[0]) {
		t.Fatalf("workflow current-turn correlation is incorrect: request=%#v descriptors=%#v", req, descriptors)
	}
	parent := req
	parent.Messages = []anthropicMessage{{Role: "user", Content: "historical worker two"}, {Role: "assistant", Content: "done"}}
	if agentChildRequestMatches(parent, descriptors[1]) {
		t.Fatal("historical worker prompt consumed workflow reservation")
	}
}

func TestWorkflowDescriptorsIgnoreAgentTextInCommentsAndLiterals(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `
// await agent("comment worker")
const example = "await agent('literal worker')"
/* await agent("block comment worker") */
await agent("real worker")
`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || descriptors[0].prompt != "real worker" {
		t.Fatalf("workflow descriptors = %#v, want only the executable worker", descriptors)
	}
}

func TestWorkflowDescriptorsIgnoreAgentIdentifierPrefixes(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `const agentConfig = {enabled: true}; const agent$worker = "not a call"`},
	}
	if descriptors := agentChildDescriptorsFromValue(value); len(descriptors) != 0 {
		t.Fatalf("workflow descriptors = %#v, want no child reservation for identifier prefixes", descriptors)
	}
}

func TestWorkflowDescriptorsIgnoreAgentPropertyReferences(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `workflow.agent; const options = {agent: true}`},
	}
	if descriptors := agentChildDescriptorsFromValue(value); len(descriptors) != 0 {
		t.Fatalf("workflow property references produced reservations: %#v", descriptors)
	}
}

func TestWorkflowDescriptorsIgnoreAgentTextInRegexLiterals(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `const assignment = /agent("regex worker")/;
const characterClass = /agent[()"']/gi;
await agent("real worker")`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || descriptors[0].prompt != "real worker" {
		t.Fatalf("Workflow descriptors = %#v, want only executable worker", descriptors)
	}
}

func TestWorkflowDescriptorsStillFindAgentAfterDivision(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `const value = total / agent("real worker")`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || descriptors[0].prompt != "real worker" {
		t.Fatalf("Workflow descriptors = %#v, want executable worker after division", descriptors)
	}
}

func TestWorkflowDescriptorsRetainGenericReservationForDynamicWorker(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `await agent("literal worker"); await agent(followUpPrompt)`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 2 || descriptors[0].prompt != "literal worker" || !descriptors[1].workflow || descriptors[1].prompt != "" {
		t.Fatalf("workflow descriptors = %#v, want literal and generic reservations", descriptors)
	}

	selection := newActiveModelSelection("active")
	registerAgentChildForTest(t, &selection, "session-a", "", descriptors)
	literal := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "literal worker"}},
	}
	literalReservation := selection.reserveAgentChild("session-a", literal)
	if !literalReservation.pending || literalReservation.ambiguous || literalReservation.alias != "active" {
		t.Fatalf("literal workflow reservation = %#v, want specific active route", literalReservation)
	}
	literalReservation.rollback()

	dynamic := anthropicRequest{
		Model:    "haiku",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "subagent dynamic worker"}},
	}
	dynamicReservation := selection.reserveAgentChild("session-a", dynamic)
	if !dynamicReservation.pending || dynamicReservation.ambiguous || dynamicReservation.alias != "active" {
		t.Fatalf("dynamic workflow reservation = %#v, want generic active route", dynamicReservation)
	}
}

func TestWorkflowDescriptorsReserveAliasedAgentCall(t *testing.T) {
	t.Parallel()

	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `const spawn = agent; await spawn("aliased worker")`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || !descriptors[0].workflow || descriptors[0].prompt != "" {
		t.Fatalf("aliased workflow descriptors = %#v, want one generic child reservation", descriptors)
	}
	child := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "aliased worker"}},
	}
	if !agentChildRequestMatches(child, descriptors[0]) {
		t.Fatal("aliased workflow child did not match its marker-bound generic reservation")
	}
}

func TestDynamicWorkflowSiblingCollisionIsAmbiguous(t *testing.T) {
	t.Parallel()
	now := time.Now()
	descriptor := agentChildDescriptor{workflow: true}
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent spawned by a workflow orchestration script.",
		Messages: []anthropicMessage{{Role: "user", Content: "dynamic workflow worker"}},
	}
	active := pendingAgentChild{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "existing-workflow-child",
		continuationIdentity: agentChildContinuationIdentity(req, descriptor),
		active:               true,
		expiresAt:            now.Add(time.Minute),
		rejectUntil:          now.Add(time.Minute),
	}
	sibling := pendingAgentChild{
		id:          2,
		descriptor:  descriptor,
		spawnAlias:  "new-workflow-child",
		expiresAt:   now.Add(time.Minute),
		rejectUntil: now.Add(time.Minute),
	}
	kept, selected, expired, ambiguous := selectPendingAgentChild([]pendingAgentChild{active, sibling}, req, now)
	if selected.id != 0 || expired || !ambiguous {
		t.Fatalf("selection = selected:%#v expired:%v ambiguous:%v; want visible ambiguity", selected, expired, ambiguous)
	}
	if len(kept) != 2 || !containsPendingAgentChild(kept, active.id) || !containsPendingAgentChild(kept, sibling.id) {
		t.Fatalf("kept reservations = %#v; want both dynamic workflow reservations preserved", kept)
	}
}

func TestWorkflowDescriptorsTreatInterpolatedPromptAsDynamic(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": "await agent(`inspect ${issue}`)"},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || !descriptors[0].workflow || descriptors[0].prompt != "" {
		t.Fatalf("interpolated workflow descriptors = %#v, want one generic dynamic reservation", descriptors)
	}
}

func TestWorkflowDescriptorsScanExecutableTemplateInterpolations(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": "const result = `${await agent(\"nested worker\")}`"},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || descriptors[0].prompt != "nested worker" {
		t.Fatalf("template interpolation descriptors = %#v, want one executable worker", descriptors)
	}
}

func TestWorkflowDescriptorsDecodeJavaScriptEscapes(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `await agent("Return \u00e9 and \u{1F600}")`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || descriptors[0].prompt != "Return é and 😀" {
		t.Fatalf("Workflow descriptors = %#v, want decoded Unicode prompt", descriptors)
	}
}

func TestWorkflowDescriptorsUseGenericReservationForUnsupportedEscapes(t *testing.T) {
	t.Parallel()
	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": `await agent("Return \q")`},
	}
	descriptors := agentChildDescriptorsFromValue(value)
	if len(descriptors) != 1 || !descriptors[0].workflow || descriptors[0].prompt != "" {
		t.Fatalf("Workflow descriptors = %#v, want one generic dynamic reservation", descriptors)
	}
}

func TestWorkflowWithoutAgentCallDoesNotReserveChild(t *testing.T) {
	t.Parallel()

	value := map[string]any{
		"type": "tool_use", "name": "Workflow",
		"input": map[string]any{"script": "phase('Run'); return 'done'"},
	}
	if descriptors := agentChildDescriptorsFromValue(value); len(descriptors) != 0 {
		t.Fatalf("workflow without agent call produced reservations: %#v", descriptors)
	}
}

func TestMalformedWorkflowInputDoesNotReserveAChild(t *testing.T) {
	t.Parallel()

	for _, input := range []map[string]any{
		{},
		{"script": nil},
		{"script": 42},
		{"script": ""},
		{"script": "   "},
	} {
		value := map[string]any{
			"type": "tool_use", "name": "Workflow", "input": input,
		}
		if descriptors := agentChildDescriptorsFromValue(value); len(descriptors) != 0 {
			t.Fatalf("malformed Workflow input %#v produced reservations: %#v", input, descriptors)
		}
		if message := invalidChildToolInputMessage("Workflow", input); message == "" {
			t.Fatalf("malformed Workflow input %#v did not produce a visible compatibility error", input)
		}
	}
}

func TestAgentChildPromptMatchingIgnoresParentSubstringAndToolInput(t *testing.T) {
	t.Parallel()
	descriptor := agentChildDescriptor{prompt: "Return the child marker"}
	parent := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "Please explain why Return the child marker is important."}},
	}
	if agentChildRequestMatches(parent, descriptor) {
		t.Fatal("parent substring consumed Agent child reservation")
	}
	parentWithChildWords := anthropicRequest{
		Model:    "sonnet",
		Messages: []anthropicMessage{{Role: "user", Content: "Explain the subagent plan, including why Return the child marker is important."}},
	}
	if agentChildRequestMatches(parentWithChildWords, descriptor) {
		t.Fatal("parent subagent discussion consumed Agent child reservation")
	}
	toolContext := anthropicRequest{
		Model: "sonnet",
		Messages: []anthropicMessage{{Role: "assistant", Content: []any{map[string]any{
			"type": "tool_use", "name": "Agent", "input": map[string]any{"prompt": "Return the child marker"},
		}}}},
	}
	if agentChildRequestMatches(toolContext, descriptor) {
		t.Fatal("Agent tool input consumed its own child reservation")
	}
	parentWithTranscript := anthropicRequest{
		Model: "sonnet",
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "name": "Agent", "input": map[string]any{"prompt": "Return the child marker"},
			}}},
			{Role: "user", Content: "Return the child marker"},
		},
	}
	if agentChildRequestMatches(parentWithTranscript, descriptor) {
		t.Fatal("parent Agent transcript consumed child reservation from matching latest prompt")
	}
}
