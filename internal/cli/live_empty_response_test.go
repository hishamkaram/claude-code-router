//go:build live

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLiveClaudeRecoversAfterEmptyProviderStream(t *testing.T) {
	ctx := liveCompactionContext(t)
	var streams atomic.Int64
	var recoveries atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-compact-model"}]}`)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = fmt.Fprint(w, `{"input_tokens":9}`)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			text := "OK"
			if streams.Load() > 0 {
				recoveries.Add(1)
				text = "CCR_EMPTY_RETRY_OK"
			}
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, text)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		text := ""
		if streams.Add(1) > 1 {
			recoveries.Add(1)
			text = "CCR_EMPTY_RETRY_OK"
		}
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", text)
	}))
	defer provider.Close()
	dbPath := configureLiveCompactionModel(t, ctx, provider.URL)
	run := startLiveCompactionRun(t, ctx, dbPath, "a9d9014c-621c-4d16-9222-4304c5d94276")
	result, err := liveRealTurn(ctx, run, liveStreamInput(t, "Reply exactly CCR_EMPTY_RETRY_OK."))
	if err != nil || strings.TrimSpace(result) != "CCR_EMPTY_RETRY_OK" {
		t.Fatalf("empty stream recovery: result=%q, error=%v, streams=%d", result, err, streams.Load())
	}
	if streams.Load() == 0 || recoveries.Load() == 0 {
		t.Fatal("empty provider response was not retried")
	}
	if _, _, err := run.finish(ctx); err != nil {
		t.Fatal(err)
	}
}
