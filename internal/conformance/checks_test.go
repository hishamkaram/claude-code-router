package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

func TestProbeBudgetsLeaveReasoningHeadroomAndRespectModelLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int64{0, 1536, 65536} {
		for _, check := range []struct {
			name          string
			want, unknown int
			run           func(checkRunner, context.Context) (string, error)
		}{
			{"text", 2048, 32, checkRunner.checkText},
			{"stream", 2048, 32, checkRunner.checkStream},
			{"tool", 2048, 128, checkRunner.checkForcedTool},
			{"thinking", 32768, 1200, checkRunner.checkThinking},
		} {
			t.Run(fmt.Sprintf("%s/limit%d", check.name, limit), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload struct {
						MaxTokens int `json:"max_tokens"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					want := check.want
					if limit == 0 {
						want = check.unknown
					}
					if limit > 0 && int64(want) > limit {
						want = int(limit)
					}
					if payload.MaxTokens != want {
						t.Errorf("max_tokens = %d, want %d", payload.MaxTokens, want)
					}
					if check.name == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
						return
					}
					_, _ = fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"OK"},{"type":"tool_use","id":"tool-1","name":"ccr_probe","input":{}}]}`)
				}))
				defer server.Close()
				runner := checkRunner{gatewayURL: server.URL, client: server.Client()}
				if limit > 0 {
					runner.target.modelCapabilities = modelcap.Values{MaxOutputTokens: &limit}
				}
				if _, err := check.run(runner, context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRequireTextResponseRejectsEmptyOrNonTextMessages(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{}`, `{"type":"message","content":[]}`,
		`{"type":"message","content":[{"type":"text","text":" "}]}`,
		`{"type":"message","content":[{"type":"thinking","thinking":"reasoning only"}]}`,
		`{"type":"error","content":[{"type":"text","text":"not a reply"}]}`,
	} {
		if err := requireTextResponse([]byte(body)); err == nil {
			t.Errorf("accepted invalid reply: %s", body)
		}
	}
	if err := requireTextResponse([]byte(`{"type":"message","content":[{"type":"text","text":"OK"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestForcedToolProbeRequiresActualNamedToolCall(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"content":[{"type":"text","text":"tool_use"}]}`,
		`{"content":[{"type":"tool_use","id":"t","name":"wrong","input":{}}]}`,
		`{"content":[{"type":"tool_use","id":"t","name":"ccr_probe","input":null}]}`,
		`{"content":[{"type":"tool_use","name":"ccr_probe","input":{}}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body)
		}))
		runner := checkRunner{gatewayURL: server.URL, client: server.Client()}
		_, err := runner.checkForcedTool(context.Background())
		server.Close()
		if err == nil {
			t.Errorf("accepted invalid tool reply: %s", body)
		}
	}
}
