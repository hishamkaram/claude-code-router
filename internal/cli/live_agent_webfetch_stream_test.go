package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type liveClaudeStreamEvent struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	IsError         bool   `json:"is_error"`
	Message         struct {
		Content []liveClaudeStreamBlock `json:"content"`
	} `json:"message"`
}

type liveClaudeStreamBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	IsError   bool            `json:"is_error"`
}

type liveAgentWebFetchStreamEvidence struct {
	AgentToolUseCount            int
	GeneralPurposeAgentStarted   bool
	GeneralPurposeAgentCompleted bool
	ExampleComWebFetchStarted    bool
	ExampleComWebFetchCompleted  bool
	SuccessfulCLIResult          bool
}

type liveAgentWebFetchToolCall struct {
	name             string
	parentToolUseID  string
	generalPurpose   bool
	exampleComTarget bool
}

type liveAgentWebFetchStreamParser struct {
	calls    map[string]liveAgentWebFetchToolCall
	evidence liveAgentWebFetchStreamEvidence
}

func parseLiveAgentWebFetchStream(output string) (liveAgentWebFetchStreamEvidence, error) {
	parser := liveAgentWebFetchStreamParser{calls: make(map[string]liveAgentWebFetchToolCall)}
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event liveClaudeStreamEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return liveAgentWebFetchStreamEvidence{}, err
		}
		if event.Type == "" {
			return liveAgentWebFetchStreamEvidence{}, errors.New("stream event has no type")
		}
		if err := parser.observe(event); err != nil {
			return liveAgentWebFetchStreamEvidence{}, err
		}
	}
	return parser.evidence, nil
}

func (p *liveAgentWebFetchStreamParser) observe(event liveClaudeStreamEvent) error {
	switch event.Type {
	case "assistant":
		return p.observeAssistant(event)
	case "user":
		p.observeUser(event)
	case "result":
		if event.Subtype == "success" && !event.IsError {
			p.evidence.SuccessfulCLIResult = true
		}
	}
	return nil
}

func (p *liveAgentWebFetchStreamParser) observeAssistant(event liveClaudeStreamEvent) error {
	for _, block := range event.Message.Content {
		if block.Type != "tool_use" || (!liveClaudeAgentToolName(block.Name) && block.Name != "WebFetch" && block.Name != "SubagentHandback") {
			continue
		}
		if block.ID == "" {
			return errors.New("tool-use event has no id")
		}
		call := liveAgentWebFetchToolCall{name: block.Name, parentToolUseID: event.ParentToolUseID}
		switch block.Name {
		case "Agent", "Task":
			if event.ParentToolUseID != "" {
				continue
			}
			p.evidence.AgentToolUseCount++
			var input struct {
				SubagentType string `json:"subagent_type"`
			}
			if err := json.Unmarshal(block.Input, &input); err != nil {
				return err
			}
			call.generalPurpose = input.SubagentType == "general-purpose"
			if call.generalPurpose {
				p.evidence.GeneralPurposeAgentStarted = true
			}
		case "WebFetch":
			var input struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(block.Input, &input); err != nil {
				return err
			}
			call.exampleComTarget = liveExampleComHTTPS(input.URL)
			if agent, ok := p.calls[event.ParentToolUseID]; ok && liveClaudeAgentToolName(agent.name) && agent.generalPurpose && call.exampleComTarget {
				p.evidence.ExampleComWebFetchStarted = true
			}
		case "SubagentHandback":
			if event.ParentToolUseID == "" {
				continue
			}
		}
		p.calls[block.ID] = call
	}
	return nil
}

func (p *liveAgentWebFetchStreamParser) observeUser(event liveClaudeStreamEvent) {
	for _, block := range event.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		call, ok := p.calls[block.ToolUseID]
		if !ok || block.IsError {
			continue
		}
		switch call.name {
		case "WebFetch":
			agent, ok := p.calls[call.parentToolUseID]
			if ok && liveClaudeAgentToolName(agent.name) && agent.generalPurpose && call.exampleComTarget {
				p.evidence.ExampleComWebFetchCompleted = true
			}
		case "SubagentHandback":
			agent, ok := p.calls[call.parentToolUseID]
			if ok && liveClaudeAgentToolName(agent.name) && agent.generalPurpose {
				p.evidence.GeneralPurposeAgentCompleted = true
			}
		}
	}
}

func liveClaudeAgentToolName(name string) bool {
	return name == "Agent" || name == "Task"
}

func liveExampleComHTTPS(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "example.com") && parsed.User == nil
}

func liveAgentWebFetchEvidenceComplete(evidence liveAgentWebFetchStreamEvidence) bool {
	return evidence.AgentToolUseCount == 1 && evidence.GeneralPurposeAgentStarted && evidence.GeneralPurposeAgentCompleted &&
		evidence.ExampleComWebFetchStarted && evidence.ExampleComWebFetchCompleted && evidence.SuccessfulCLIResult
}

func parseLiveLaunchID(diagnostics string) (int64, error) {
	const marker = "(session="
	start := strings.Index(diagnostics, marker)
	if start < 0 {
		return 0, errors.New("launch summary does not contain a session id")
	}
	start += len(marker)
	end := start
	for end < len(diagnostics) && diagnostics[end] >= '0' && diagnostics[end] <= '9' {
		end++
	}
	if end == start {
		return 0, errors.New("launch summary contains an invalid session id")
	}
	launchID, err := strconv.ParseInt(diagnostics[start:end], 10, 64)
	if err != nil || launchID <= 0 {
		return 0, errors.New("launch summary contains an invalid session id")
	}
	return launchID, nil
}

func completedGeneralPurposeAgentOnAlias(output, modelAlias string) (bool, error) {
	var document struct {
		Agents []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			ModelAlias string `json:"model_alias"`
			Status     string `json:"status"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		return false, err
	}
	generalPurposeAgentCount := 0
	completedOnAlias := false
	for _, agent := range document.Agents {
		if agent.Name == "general-purpose" && agent.Kind == "subagent" {
			generalPurposeAgentCount++
			completedOnAlias = completedOnAlias || agent.ModelAlias == modelAlias && agent.Status == "completed"
		}
	}
	return generalPurposeAgentCount == 1 && completedOnAlias, nil
}

func TestParseLiveAgentWebFetchStream(t *testing.T) {
	t.Parallel()

	evidence, err := parseLiveAgentWebFetchStream(liveAgentWebFetchStreamFixture)
	if err != nil {
		t.Fatalf("parse live Agent/WebFetch stream: %v", err)
	}
	if !liveAgentWebFetchEvidenceComplete(evidence) {
		t.Fatalf("live Agent/WebFetch evidence incomplete: %+v", evidence)
	}
}

func TestParseLiveAgentWebFetchStreamRequiresSuccessfulEvidence(t *testing.T) {
	t.Parallel()

	failedFetch := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`"tool_use_id":"webfetch-1","content":"example page"`,
		`"tool_use_id":"webfetch-1","is_error":true,"content":"fetch failed"`,
		1,
	)
	missingFetchResult := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"webfetch-1","content":"example page"}]}}`+"\n",
		"",
		1,
	)
	wrongAgent := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`"parent_tool_use_id":"agent-1"`,
		`"parent_tool_use_id":"other-agent"`,
		2,
	)
	wrongTarget := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`"url":"https://example.com"`,
		`"url":"https://example.net"`,
		1,
	)
	missingSuccessfulCLIResult := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`{"type":"result","subtype":"success","is_error":false}`,
		"",
		1,
	)
	splitAgentEvidence := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"agent-1","name":"Agent","input":{"subagent_type":"general-purpose"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"agent-1","name":"Agent","input":{"subagent_type":"general-purpose"}}]}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"agent-2","name":"Agent","input":{"subagent_type":"general-purpose"}}]}}`,
		1,
	)
	splitAgentEvidence = strings.Replace(
		splitAgentEvidence,
		`{"type":"assistant","parent_tool_use_id":"agent-1","message":{"content":[{"type":"tool_use","id":"handback-1","name":"SubagentHandback","input":{}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"agent-2","message":{"content":[{"type":"tool_use","id":"handback-1","name":"SubagentHandback","input":{}}]}}`,
		1,
	)
	incompleteAgent := strings.Replace(
		liveAgentWebFetchStreamFixture,
		`"name":"SubagentHandback"`,
		`"name":"UnusedTool"`,
		1,
	)
	tests := []struct {
		name                    string
		stream                  string
		wantAgentToolUseCount   int
		wantAgentStarted        bool
		wantAgentCompleted      bool
		wantWebFetchStarted     bool
		wantWebFetchCompleted   bool
		wantSuccessfulCLIResult bool
	}{
		{name: "tool error", stream: failedFetch, wantAgentToolUseCount: 1, wantAgentStarted: true, wantAgentCompleted: true, wantWebFetchStarted: true, wantSuccessfulCLIResult: true},
		{name: "missing tool result", stream: missingFetchResult, wantAgentToolUseCount: 1, wantAgentStarted: true, wantAgentCompleted: true, wantWebFetchStarted: true, wantSuccessfulCLIResult: true},
		{name: "fetch outside requested agent", stream: wrongAgent, wantAgentToolUseCount: 1, wantAgentStarted: true, wantSuccessfulCLIResult: true},
		{name: "unexpected URL", stream: wrongTarget, wantAgentToolUseCount: 1, wantAgentStarted: true, wantAgentCompleted: true, wantSuccessfulCLIResult: true},
		{name: "missing CLI success event", stream: missingSuccessfulCLIResult, wantAgentToolUseCount: 1, wantAgentStarted: true, wantAgentCompleted: true, wantWebFetchStarted: true, wantWebFetchCompleted: true},
		{name: "Agent did not hand back", stream: incompleteAgent, wantAgentToolUseCount: 1, wantAgentStarted: true, wantWebFetchStarted: true, wantWebFetchCompleted: true, wantSuccessfulCLIResult: true},
		{name: "results split across Agents", stream: splitAgentEvidence, wantAgentToolUseCount: 2, wantAgentStarted: true, wantAgentCompleted: true, wantWebFetchStarted: true, wantWebFetchCompleted: true, wantSuccessfulCLIResult: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			evidence, err := parseLiveAgentWebFetchStream(test.stream)
			if err != nil {
				t.Fatalf("parse live Agent/WebFetch stream: %v", err)
			}
			if evidence.AgentToolUseCount != test.wantAgentToolUseCount || evidence.GeneralPurposeAgentStarted != test.wantAgentStarted || evidence.GeneralPurposeAgentCompleted != test.wantAgentCompleted || evidence.ExampleComWebFetchStarted != test.wantWebFetchStarted || evidence.ExampleComWebFetchCompleted != test.wantWebFetchCompleted || evidence.SuccessfulCLIResult != test.wantSuccessfulCLIResult {
				t.Fatalf("unexpected stream evidence: %+v", evidence)
			}
			if liveAgentWebFetchEvidenceComplete(evidence) {
				t.Fatal("expected incomplete or ambiguous stream evidence to be rejected")
			}
		})
	}
}

func TestParseLiveAgentWebFetchStreamRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	if _, err := parseLiveAgentWebFetchStream("not-json\n"); err == nil {
		t.Fatal("expected malformed stream JSON to fail")
	}
}

func TestParseLiveAgentWebFetchStreamAcceptsLegacyTaskTool(t *testing.T) {
	t.Parallel()
	stream := strings.Replace(liveAgentWebFetchStreamFixture, `"name":"Agent"`, `"name":"Task"`, 1)
	evidence, err := parseLiveAgentWebFetchStream(stream)
	if err != nil {
		t.Fatalf("parse live Agent/WebFetch stream using legacy Task tool: %v", err)
	}
	if !liveAgentWebFetchEvidenceComplete(evidence) {
		t.Fatalf("legacy Task tool did not produce complete Agent/WebFetch evidence: %+v", evidence)
	}
}

func TestParseLiveLaunchID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		diagnostics string
		wantID      int64
		wantError   bool
	}{
		{name: "session metadata", diagnostics: "Claude Code launched through gateway (session=340 pid=123)", wantID: 340},
		{name: "missing metadata", diagnostics: "launch started", wantError: true},
		{name: "invalid metadata", diagnostics: "(session=abc pid=123)", wantError: true},
		{name: "zero id", diagnostics: "(session=0 pid=123)", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			id, err := parseLiveLaunchID(test.diagnostics)
			if (err != nil) != test.wantError || id != test.wantID {
				t.Fatalf("parseLiveLaunchID() = (%d, %v), want (%d, error=%v)", id, err != nil, test.wantID, test.wantError)
			}
		})
	}
}

func TestCompletedGeneralPurposeAgentOnAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		output    string
		alias     string
		wantFound bool
		wantError bool
	}{
		{name: "completed on selected alias", output: `{"agents":[{"name":"general-purpose","kind":"subagent","model_alias":"glm-5-2","status":"completed"}]}`, alias: "glm-5-2", wantFound: true},
		{name: "multiple matching agents", output: `{"agents":[{"name":"general-purpose","kind":"subagent","model_alias":"glm-5-2","status":"completed"},{"name":"general-purpose","kind":"subagent","model_alias":"glm-5-2","status":"completed"}]}`, alias: "glm-5-2"},
		{name: "wrong alias", output: `{"agents":[{"name":"general-purpose","kind":"subagent","model_alias":"other","status":"completed"}]}`, alias: "glm-5-2"},
		{name: "still running", output: `{"agents":[{"name":"general-purpose","kind":"subagent","model_alias":"glm-5-2","status":"running"}]}`, alias: "glm-5-2"},
		{name: "not a subagent", output: `{"agents":[{"name":"general-purpose","kind":"task","model_alias":"glm-5-2","status":"completed"}]}`, alias: "glm-5-2"},
		{name: "invalid JSON", output: `not-json`, alias: "glm-5-2", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			found, err := completedGeneralPurposeAgentOnAlias(test.output, test.alias)
			if (err != nil) != test.wantError || found != test.wantFound {
				t.Fatalf("completedGeneralPurposeAgentOnAlias() = (%v, %v), want (%v, error=%v)", found, err != nil, test.wantFound, test.wantError)
			}
		})
	}
}

const liveAgentWebFetchStreamFixture = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"agent-1","name":"Agent","input":{"subagent_type":"general-purpose"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"agent-1","content":"async agent started"}]},"tool_use_result":{"status":"async_launched","isAsync":true}}
{"type":"assistant","parent_tool_use_id":"agent-1","message":{"content":[{"type":"tool_use","id":"webfetch-1","name":"WebFetch","input":{"url":"https://example.com"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"webfetch-1","content":"example page"}]}}
{"type":"assistant","parent_tool_use_id":"agent-1","message":{"content":[{"type":"tool_use","id":"handback-1","name":"SubagentHandback","input":{}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"handback-1","content":"completed"}]}}
{"type":"result","subtype":"success","is_error":false}`
