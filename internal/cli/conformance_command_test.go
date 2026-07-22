package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestConformanceCommandPersistsChecksAndEmitsVersionedJSON(t *testing.T) {
	t.Parallel()
	server := newCLIConformanceOpenAIServer(t, "model-v1")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "fixture",
		"--type", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "coder",
		"--provider", "fixture", "--model", "model-v1"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	out, errOut, err := runCommand(t, "--db", dbPath, "conformance", "run", "coder", "--json")
	if err != nil {
		t.Fatalf("conformance run error = %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(errOut, "alias=coder provider=fixture model=model-v1") {
		t.Fatalf("conformance target missing from stderr: %s", errOut)
	}
	var document conformanceDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("conformance JSON error = %v\n%s", decodeErr, out)
	}
	if document.SchemaVersion != 1 || document.Status != "passed" ||
		!document.LiveVerified || len(document.Checks) != 9 {
		t.Fatalf("conformance document = %#v", document)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	runs, err := s.ListConformanceRuns(ctx, "coder", 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "passed" {
		t.Fatalf("ListConformanceRuns() = %#v, %v", runs, err)
	}
	checks, err := s.ListConformanceChecks(ctx, runs[0].ID)
	if err != nil || len(checks) != 9 {
		t.Fatalf("ListConformanceChecks() = %#v, %v", checks, err)
	}
	listOut, _, err := runCommand(t, "--db", dbPath, "conformance", "list", "coder", "--json")
	if err != nil {
		t.Fatalf("conformance list error = %v", err)
	}
	if !strings.Contains(listOut, `"schema_version": 1`) || !strings.Contains(listOut, `"live_verified": true`) {
		t.Fatalf("conformance list JSON = %s", listOut)
	}
}

func TestConformanceRunAllContinuesFailuresAndEmitsAggregateJSON(t *testing.T) {
	t.Parallel()
	server := newCLIConformanceOpenAIServer(t, "model-a")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "fixture",
		"--type", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	models := []struct {
		alias, providerModel, status string
	}{
		{alias: "alpha", providerModel: "model-a"},
		{alias: "beta", providerModel: "model-b"},
		{alias: "blocked", providerModel: "model-c", status: "blocked"},
	}
	for _, model := range models {
		args := []string{"--db", dbPath, "model", "add", model.alias, "--provider", "fixture", "--model", model.providerModel}
		if model.status != "" {
			args = append(args, "--compat", model.status)
		}
		if _, _, err := runCommand(t, args...); err != nil {
			t.Fatalf("model add error = %v", err)
		}
	}
	out, errOut, err := runCommand(t, "--db", dbPath, "conformance", "run", "--all", "--json")
	if err == nil || !strings.Contains(err.Error(), "failed for 1 of 3 aliases") {
		t.Fatalf("conformance --all error = %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	var document conformanceAllDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("aggregate JSON error = %v\n%s", decodeErr, out)
	}
	if document.SchemaVersion != 1 || document.Status != "failed" || document.Total != 3 ||
		document.Passed != 1 || document.Failed != 1 || document.Skipped != 1 || len(document.Results) != 3 {
		t.Fatalf("aggregate document = %#v", document)
	}
	if document.Results[0].Alias != "alpha" || document.Results[0].Status != "passed" ||
		document.Results[1].Alias != "beta" || document.Results[1].Status != "failed" ||
		document.Results[2].Alias != "blocked" || document.Results[2].Status != "skipped" {
		t.Fatalf("aggregate results = %#v", document.Results)
	}
	if !strings.Contains(errOut, "alias=alpha") || !strings.Contains(errOut, "alias=beta") {
		t.Fatalf("aggregate diagnostics = %s", errOut)
	}
	if !strings.Contains(errOut, "Conformance provider checks completed: alias=alpha status=passed") ||
		!strings.Contains(errOut, "Conformance provider checks completed: alias=beta status=failed") {
		t.Fatalf("aggregate completion progress = %s", errOut)
	}
}

func TestConformanceSkipsProviderControlModels(t *testing.T) {
	t.Parallel()
	server := newCLIConformanceOpenAIServer(t, "model-a")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "fixture",
		"--type", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "alpha",
		"--provider", "fixture", "--model", "model-a"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if addErr := s.AddModel(ctx, store.Model{
		Alias: "control", ProviderName: "fixture", ProviderModel: "all-proxy-models", Status: "degraded",
	}); addErr != nil {
		t.Fatalf("adding legacy control alias: %v", addErr)
	}

	out, errOut, err := runCommand(t, "--db", dbPath, "conformance", "run", "--all", "--json")
	if err != nil {
		t.Fatalf("conformance --all error = %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	var document conformanceAllDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("aggregate JSON error = %v\n%s", decodeErr, out)
	}
	if document.Status != "passed" || document.Total != 2 || document.Passed != 1 || document.Skipped != 1 ||
		len(document.Results) != 2 || document.Results[1].Alias != "control" || document.Results[1].Status != "skipped" ||
		!strings.Contains(document.Results[1].Error, "control model") {
		t.Fatalf("aggregate control result = %#v", document)
	}
	if !strings.Contains(errOut, "Conformance target skipped: alias=control") {
		t.Fatalf("control-model skip progress missing: %s", errOut)
	}
	_, _, err = runCommand(t, "--db", dbPath, "conformance", "run", "control")
	if err == nil || !strings.Contains(err.Error(), "provider control model") {
		t.Fatalf("single control-model conformance error = %v", err)
	}
}

func TestConformanceRunAllSkipsResponsesAliasWithoutProviderSupport(t *testing.T) {
	t.Parallel()
	server := newCLIConformanceOpenAIServer(t, "model-a")
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "fixture",
		"--type", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "alpha",
		"--provider", "fixture", "--model", "model-a"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if addErr := s.AddModel(ctx, store.Model{
		Alias: "responses", ProviderName: "fixture", ProviderModel: "responses-model", Status: "degraded",
		CapabilityOverrides: modelcap.Values{
			Kind:              modelcap.KindResponses,
			SupportsResponses: modelcap.Bool(true),
		},
	}); addErr != nil {
		_ = s.Close()
		t.Fatalf("adding responses alias: %v", addErr)
	}
	if closeErr := s.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	out, errOut, err := runCommand(t, "--db", dbPath, "conformance", "run", "--all", "--json")
	if err != nil {
		t.Fatalf("conformance --all responses-skip error = %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	var document conformanceAllDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("aggregate JSON error = %v\n%s", decodeErr, out)
	}
	if document.Status != "passed" || document.Total != 2 || document.Passed != 1 ||
		document.Failed != 0 || document.Skipped != 1 || len(document.Results) != 2 {
		t.Fatalf("aggregate responses result = %#v", document)
	}
	if document.Results[1].Alias != "responses" || document.Results[1].Status != "skipped" ||
		!strings.Contains(document.Results[1].Error, "Responses API route is excluded") {
		t.Fatalf("responses conformance diagnosis = %#v", document.Results)
	}
	if !strings.Contains(errOut, "Conformance target skipped: alias=responses") ||
		strings.Contains(errOut, "Conformance target: alias=responses") {
		t.Fatalf("responses conformance progress = %s", errOut)
	}
}

func TestConformanceRunAllStrictlyValidatesScopeBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()
	missingDB := filepath.Join(t.TempDir(), "missing", "ccr.db")
	_, _, err := runCommand(t, "--db", missingDB, "conformance", "run", "alpha", "--all")
	if err == nil || !strings.Contains(err.Error(), "either one alias or --all") {
		t.Fatalf("alias plus --all error = %v", err)
	}
	_, _, err = runCommand(t, "--db", missingDB, "conformance", "run")
	if err == nil || !strings.Contains(err.Error(), "use ccr conformance run <alias> or ccr conformance run --all") {
		t.Fatalf("missing scope error = %v", err)
	}
}

func TestConformanceRunAllFailsWhenNothingIsRunnable(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "conformance", "run", "--all", "--json")
	if err == nil || !strings.Contains(err.Error(), "no runnable non-blocked routable model aliases") {
		t.Fatalf("empty conformance error = %v", err)
	}
	var document conformanceAllDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("empty conformance JSON error = %v\n%s", decodeErr, out)
	}
	if document.Status != "failed" || document.Total != 0 || len(document.Results) != 0 {
		t.Fatalf("empty conformance document = %#v", document)
	}
}

func newCLIConformanceOpenAIServer(t *testing.T, model string) *httptest.Server {
	t.Helper()
	return newCLIConformanceOpenAIModelsServer(t, []string{model})
}

func newCLIConformanceOpenAIModelsServer(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			entries := make([]string, 0, len(models))
			for _, model := range models {
				entries = append(entries, fmt.Sprintf(`{"id":%q}`, model))
			}
			_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(entries, ","))
		case "/model/info":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":9}`)
		case "/v1/chat/completions":
			var payload struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Tools []any `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			for _, message := range payload.Messages {
				if strings.Contains(message.Content, "CCR_CONFORMANCE_CANCEL") {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if len(payload.Tools) > 0 {
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"toolu-1","type":"function","function":{"name":"ccr_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newCLIConformanceAnthropicServer(t *testing.T, model string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"input_tokens":9}`)
		case "/v1/messages":
			var payload struct {
				Stream   bool  `json:"stream"`
				Tools    []any `json:"tools"`
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			for _, message := range payload.Messages {
				if strings.Contains(message.Content, "CCR_CONFORMANCE_CANCEL") {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
			if payload.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":%q,\"usage\":{\"input_tokens\":5}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", model)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if len(payload.Tools) > 0 {
				_, _ = fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"tool_use","id":"toolu-1","name":"ccr_probe","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`, model)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, model)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
