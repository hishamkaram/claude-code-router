//go:build live

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func (f *liveClaudeConformanceFixture) handleCountTokens(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var payload liveAnthropicMessagePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decoding conformance count_tokens request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if isLiveAnthropicAutoClassifierRequest(payload) {
		if strings.HasPrefix(payload.Model, "fixture-") {
			f.recordSelectedClassifier(true)
		} else {
			f.recordFirstPartyClassifier(true)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
}

func (f *liveClaudeConformanceFixture) recordSelectedClassifier(countTokens bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if countTokens {
		f.selectedClassifierCountTokens++
		return
	}
	f.selectedClassifierMessages++
}

func (f *liveClaudeConformanceFixture) recordFirstPartyClassifier(countTokens bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if countTokens {
		f.firstPartyClassifierCountTokens++
		return
	}
	f.firstPartyClassifierMessages++
}

func (f *liveClaudeConformanceFixture) summary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf(
		"aliases=%v firstParty=%d selectedClassifier=%d/%d firstPartyClassifier=%d/%d agent=%t workflow=%t steps=%v",
		f.aliasModels,
		f.firstParty,
		f.selectedClassifierMessages,
		f.selectedClassifierCountTokens,
		f.firstPartyClassifierMessages,
		f.firstPartyClassifierCountTokens,
		f.agentToolSeen,
		f.workflowSeen,
		f.requestSteps,
	)
}

func (f *liveClaudeConformanceFixture) assertComplete(t *testing.T, out, errOut string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aliasModels["fixture-full-model"] == 0 || f.aliasModels["fixture-chat-model"] == 0 ||
		f.firstParty == 0 || f.selectedClassifierMessages == 0 ||
		f.firstPartyClassifierMessages != 0 || f.firstPartyClassifierCountTokens != 0 ||
		!f.agentToolSeen || !f.workflowSeen {
		t.Fatalf(
			"live conformance fixture incomplete: aliases=%v firstParty=%d selectedClassifier=%d/%d firstPartyClassifier=%d/%d agent=%v workflow=%v\nstdout:\n%s\nstderr:\n%s",
			f.aliasModels,
			f.firstParty,
			f.selectedClassifierMessages,
			f.selectedClassifierCountTokens,
			f.firstPartyClassifierMessages,
			f.firstPartyClassifierCountTokens,
			f.agentToolSeen,
			f.workflowSeen,
			out,
			errOut,
		)
	}
}
