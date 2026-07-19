package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestModelUpdateAndShowCapabilityOverrides(t *testing.T) {
	t.Parallel()
	server := newModelsServer(t, []string{"glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "glm", "glm-5.2[1m]")

	out, _, err := runCommand(t, "--db", dbPath, "model", "update", "glm",
		"--context-window", "1000000", "--max-output-tokens", "65536",
		"--input-modalities", "text,image", "--tools", "false")
	if err != nil {
		t.Fatalf("model update error = %v, output = %s", err, out)
	}
	document := readModelShowDocument(t, dbPath, "glm")
	if document.Overrides.ContextWindowTokens == nil || *document.Overrides.ContextWindowTokens != 1_000_000 ||
		document.Overrides.SupportsTools == nil || *document.Overrides.SupportsTools ||
		document.Effective.Sources["supports_tools"] != "override" {
		t.Fatalf("show document = %#v", document)
	}
	launcher := &fakeLauncher{pid: 4242}
	if _, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, "--db", dbPath, "launch", "--model", "glm"); err != nil {
		t.Fatalf("launch with tools=false error = %v", err)
	}
	if !launcher.hasEnv("CLAUDE_CODE_SIMPLE=1") || !launcher.hasEnv("ENABLE_TOOL_SEARCH=") {
		t.Fatalf("launch env = %#v", launcher.env)
	}

	if _, _, err := runCommand(t, "--db", dbPath, "model", "update", "glm",
		"--context-window", "0", "--tools", "auto"); err != nil {
		t.Fatalf("clear overrides error = %v", err)
	}
	document = readModelShowDocument(t, dbPath, "glm")
	if document.Overrides.ContextWindowTokens != nil || document.Overrides.SupportsTools != nil {
		t.Fatalf("overrides after clear = %#v", document.Overrides)
	}
	if document.Effective.Values.ContextWindowTokens == nil || *document.Effective.Values.ContextWindowTokens != 1_000_000 ||
		document.Effective.Sources["context_window_tokens"] != "model_id_hint" {
		t.Fatalf("effective after clear = %#v", document.Effective)
	}
}

func TestModelRefreshAllGroupsModelsByProviderAndContinuesFailures(t *testing.T) {
	t.Parallel()
	var modelsRequests atomic.Int32
	var metadataRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			modelsRequests.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/model/info":
			metadataRequests.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_tokens":200000,"supports_function_calling":true}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	for alias, model := range map[string]string{"available": "model-a", "missing": "model-b"} {
		if _, _, err := runCommand(t, "--db", dbPath, "model", "add", alias, "--provider", "litellm", "--model", model); err != nil {
			t.Fatalf("model add error = %v", err)
		}
	}
	out, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "--all")
	if err == nil || !strings.Contains(err.Error(), "failed for 1 aliases") {
		t.Fatalf("model refresh --all error = %v, output = %s", err, out)
	}
	if !strings.Contains(out, "available: refreshed") || !strings.Contains(out, "missing: failed") {
		t.Fatalf("model refresh output = %s", out)
	}
	if modelsRequests.Load() != 1 || metadataRequests.Load() != 1 {
		t.Fatalf("requests: models=%d metadata=%d", modelsRequests.Load(), metadataRequests.Load())
	}
	document := readModelShowDocument(t, dbPath, "available")
	if document.Discovered.Values.ContextWindowTokens == nil || *document.Discovered.Values.ContextWindowTokens != 200_000 ||
		document.Discovered.Values.SupportsTools == nil || !*document.Discovered.Values.SupportsTools ||
		document.RefreshedAt == "" {
		t.Fatalf("refreshed show document = %#v", document)
	}
	missing := readModelShowDocument(t, dbPath, "missing")
	if missing.RefreshedAt != "" {
		t.Fatalf("missing model was mutated = %#v", missing)
	}
}

func TestModelRefreshPreservesCapabilitiesWhenLiteLLMMetadataDegrades(t *testing.T) {
	t.Parallel()
	var metadataAvailable atomic.Bool
	metadataAvailable.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/model/info":
			if !metadataAvailable.Load() {
				http.Error(w, "private provider detail", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_tokens":1000000}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "model-a", "model-a")
	if _, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("first refresh error = %v", err)
	}
	metadataAvailable.Store(false)
	out, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a")
	if err != nil {
		t.Fatalf("partial refresh error = %v, output = %s", err, out)
	}
	if !strings.Contains(out, "warning: LiteLLM capability metadata unavailable: HTTP 503 Service Unavailable") {
		t.Fatalf("partial refresh warning missing: %s", out)
	}
	document := readModelShowDocument(t, dbPath, "model-a")
	if document.Discovered.Values.ContextWindowTokens == nil || *document.Discovered.Values.ContextWindowTokens != 1_000_000 {
		t.Fatalf("partial refresh erased context capability: %#v", document.Discovered)
	}
}

func TestModelUpdatePreservesCapabilitiesWhenTargetIsUnchanged(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/model/info":
			_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_tokens":1000000,"supports_function_calling":false}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "model-a", "model-a")
	if _, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("model refresh error = %v", err)
	}
	before := readModelShowDocument(t, dbPath, "model-a")
	for _, args := range [][]string{
		{"model", "update", "model-a", "--model", "model-a"},
		{"model", "update", "model-a", "--provider", "litellm"},
	} {
		command := append([]string{"--db", dbPath}, args...)
		if _, _, err := runCommand(t, command...); err != nil {
			t.Fatalf("model update %v error = %v", args, err)
		}
	}
	after := readModelShowDocument(t, dbPath, "model-a")
	if after.RefreshedAt != before.RefreshedAt || after.RefreshedAt == "" ||
		after.Discovered.Values.ContextWindowTokens == nil ||
		*after.Discovered.Values.ContextWindowTokens != 1_000_000 ||
		after.Discovered.Values.SupportsTools == nil || *after.Discovered.Values.SupportsTools {
		t.Fatalf("no-op target update changed capabilities: before=%#v after=%#v", before, after)
	}
}

func TestModelRefreshPersistsNonRoutableDiscoveredKind(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"embedding-model"}]}`))
		case "/model/info":
			_, _ = w.Write([]byte(`{"data":[{"model_name":"embedding-model","model_info":{"mode":"embedding"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "embedding", "embedding-model")
	out, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "embedding")
	if err != nil {
		t.Fatalf("model refresh error = %v, output = %s", err, out)
	}
	if !strings.Contains(out, "provider classified the model as non-routable") {
		t.Fatalf("non-routable warning missing: %s", out)
	}
	document := readModelShowDocument(t, dbPath, "embedding")
	if document.Discovered.Values.Kind != modelcap.KindEmbedding ||
		document.Effective.Values.Kind != modelcap.KindEmbedding {
		t.Fatalf("non-routable kind was not persisted: %#v", document)
	}
	_, _, err = runCommand(t, "--db", dbPath, "model", "test", "embedding")
	if err == nil || !strings.Contains(err.Error(), `non-routable model kind "embedding"`) {
		t.Fatalf("model test error = %v", err)
	}
}

func TestModelRefreshPreservesCapabilitiesWhenLiteLLMMetadataOmitsModel(t *testing.T) {
	t.Parallel()
	var includeMetadata atomic.Bool
	includeMetadata.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/model/info":
			if includeMetadata.Load() {
				_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_tokens":1000000,"supports_function_calling":true}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "model-a", "model-a")
	if _, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("first refresh error = %v", err)
	}
	includeMetadata.Store(false)
	if out, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("omitted metadata refresh error = %v, output = %s", err, out)
	}
	document := readModelShowDocument(t, dbPath, "model-a")
	if document.Discovered.Values.ContextWindowTokens == nil ||
		*document.Discovered.Values.ContextWindowTokens != 1_000_000 ||
		document.Discovered.Values.SupportsTools == nil || !*document.Discovered.Values.SupportsTools {
		t.Fatalf("omitted metadata erased capabilities: %#v", document.Discovered)
	}
}

func TestModelRefreshMergesPartialLiteLLMMetadataRows(t *testing.T) {
	t.Parallel()
	var completeMetadata atomic.Bool
	completeMetadata.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/model/info":
			if completeMetadata.Load() {
				_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_tokens":1000000,"supports_function_calling":false,"supports_tool_choice":false}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"model_name":"model-a","model_info":{"mode":"chat","max_output_tokens":4096}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, dbPath, server.URL, "model-a", "model-a")
	if _, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("complete refresh error = %v", err)
	}
	completeMetadata.Store(false)
	if out, _, err := runCommand(t, "--db", dbPath, "model", "refresh", "model-a"); err != nil {
		t.Fatalf("partial-row refresh error = %v, output = %s", err, out)
	}
	document := readModelShowDocument(t, dbPath, "model-a")
	if document.Discovered.Values.ContextWindowTokens == nil ||
		*document.Discovered.Values.ContextWindowTokens != 1_000_000 ||
		document.Discovered.Values.MaxOutputTokens == nil ||
		*document.Discovered.Values.MaxOutputTokens != 4096 ||
		document.Discovered.Values.SupportsTools == nil || *document.Discovered.Values.SupportsTools ||
		document.Discovered.Values.SupportsToolChoice == nil || *document.Discovered.Values.SupportsToolChoice {
		t.Fatalf("partial metadata row erased prior capabilities: %#v", document.Discovered)
	}
}

func TestModelCommandsRejectLiteLLMControlModelsAndInvalidRefreshScope(t *testing.T) {
	t.Parallel()
	server := newModelsServer(t, []string{"all-proxy-models"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	_, _, err := runCommand(t, "--db", dbPath, "model", "add", "control", "--provider", "litellm", "--model", "all-proxy-models")
	if err == nil || !strings.Contains(err.Error(), "control model") {
		t.Fatalf("control model add error = %v", err)
	}
	s, openErr := store.Open(context.Background(), dbPath)
	if openErr != nil {
		t.Fatalf("store.Open() error = %v", openErr)
	}
	if addErr := s.AddModel(context.Background(), store.Model{
		Alias: "legacy-control", ProviderName: "litellm", ProviderModel: "all-proxy-models", Status: "degraded",
	}); addErr != nil {
		_ = s.Close()
		t.Fatalf("AddModel(legacy-control) error = %v", addErr)
	}
	if closeErr := s.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	for _, args := range [][]string{
		{"model", "update", "legacy-control", "--model", "all-proxy-models"},
		{"model", "update", "legacy-control", "--compat", "blocked"},
	} {
		command := append([]string{"--db", dbPath}, args...)
		_, _, err = runCommand(t, command...)
		if err == nil || !strings.Contains(err.Error(), "control model") {
			t.Fatalf("legacy control update %v error = %v", args, err)
		}
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist", "ccr.db")
	_, _, err = runCommand(t, "--db", missingDB, "model", "refresh", "alias", "--all")
	if err == nil || !strings.Contains(err.Error(), "either one alias or --all") {
		t.Fatalf("invalid refresh scope error = %v", err)
	}
}

func addCapabilityTestModel(t *testing.T, dbPath, baseURL, alias, providerModel string) {
	t.Helper()
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", baseURL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", alias, "--provider", "litellm", "--model", providerModel); err != nil {
		t.Fatalf("model add error = %v", err)
	}
}

func readModelShowDocument(t *testing.T, dbPath, alias string) modelShowDocument {
	t.Helper()
	out, _, err := runCommand(t, "--db", dbPath, "model", "show", alias, "--json")
	if err != nil {
		t.Fatalf("model show error = %v, output = %s", err, out)
	}
	var document modelShowDocument
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	if document.SchemaVersion != 1 || document.Alias != alias {
		t.Fatalf("show document = %s", fmt.Sprintf("%#v", document))
	}
	return document
}
