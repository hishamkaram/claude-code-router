//go:build live

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentFixturesDistinguishInheritedToolHistory(t *testing.T) {
	for _, parentResult := range []bool{false, true} {
		name := "inherited-tool"
		if parentResult {
			name = "parent-agent-result"
		}
		t.Run(name, func(t *testing.T) {
			for _, conformance := range []bool{false, true} {
				child := "CCR_LIVE_SWITCH_CHILD_OK"
				parent := "CCR_LIVE_SWITCH_PARENT_OK"
				callID := "toolu_switch_agent"
				if conformance {
					child = "CCR_CONFORMANCE_AGENT_CHILD_OK"
					parent = claudeConformanceAgentParent
					callID = "toolu_agent_conformance"
				}
				if !parentResult {
					callID = "unrelated-read"
				}
				payload := map[string]any{
					"model": "qwen-5",
					"messages": []map[string]string{
						{"role": "user", "content": "Return exactly " + child},
						{"role": "tool", "tool_call_id": callID, "content": "Inherited history mentions " + child},
					},
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				if conformance {
					fixture := &liveClaudeConformanceFixture{aliasModels: make(map[string]int)}
					fixture.handleOpenAI(t, recorder, request)
				} else {
					state := &liveSwitchedAgentProviderState{regularModels: []string{"qwen-5"}}
					state.handleChat(t, recorder, request)
				}
				want := child
				if parentResult {
					want = parent
				}
				if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("conformance=%t parentResult=%t: response = %d %s, want %s", conformance, parentResult, recorder.Code, recorder.Body.String(), want)
				}
			}
		})
	}
}

func TestAgentResultDeliveryAcknowledgment(t *testing.T) {
	const delivered = `  This agent's report was delivered to you as a message from "worker" (its SubagentHandback call). Read it there; it is not repeated here.`
	for _, tc := range []struct {
		name    string
		message liveOpenAIChatMessage
		want    bool
	}{
		{"delivered", liveOpenAIChatMessage{Role: "tool", ToolCallID: "agent-call", Content: delivered}, true},
		{"unrelated", liveOpenAIChatMessage{Role: "tool", ToolCallID: "read-call", Content: delivered}, false},
		{"user-text", liveOpenAIChatMessage{Role: "user", ToolCallID: "agent-call", Content: delivered}, false},
		{"failed", liveOpenAIChatMessage{Role: "tool", ToolCallID: "agent-call", Content: "Error: subagent failed"}, false},
		{"incomplete", liveOpenAIChatMessage{Role: "tool", ToolCallID: "agent-call", Content: "This agent's report was delivered to you as a message from"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIMessagesContainAgentResult([]liveOpenAIChatMessage{tc.message}, "agent-call", "CHILD_OK"); got != tc.want {
				t.Fatalf("Agent completion = %t, want %t", got, tc.want)
			}
		})
	}
}
