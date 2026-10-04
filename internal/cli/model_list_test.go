package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestVersionFlagMatchesCommandWithoutStore(t *testing.T) {
	t.Parallel()
	database := filepath.Join(t.TempDir(), "absent", "ccr.db")
	expected, _, err := runCommand(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	actual, _, err := runCommand(t, "--db", database, "--version")
	if err != nil || actual != expected {
		t.Fatalf("version = %q, error = %v; want %q", actual, err, expected)
	}
	if _, err := os.Stat(filepath.Dir(database)); !os.IsNotExist(err) {
		t.Fatalf("version touched database directory: %v", err)
	}
}

func TestModelListJSONEmpty(t *testing.T) {
	t.Parallel()
	out, _, err := runCommand(t, "--db", filepath.Join(t.TempDir(), "ccr.db"), "model", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var document modelListDocument
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != 1 || document.Models == nil || len(document.Models) != 0 {
		t.Fatalf("document = %+v", document)
	}
}

func TestModelListJSONProjectsSortedEffectiveMetadata(t *testing.T) {
	t.Parallel()
	tools := false
	models := []store.Model{
		{Alias: "z-last", ProviderName: "local", ProviderModel: "model-z", Status: "degraded", CapabilityOverrides: modelcap.Values{SupportsTools: &tools}},
		{Alias: "a-first", ProviderName: "local", ProviderModel: "model-a", Status: "full"},
	}
	var out bytes.Buffer
	if err := writeModelListJSON(&out, models); err != nil {
		t.Fatal(err)
	}
	var document modelListDocument
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != 2 || document.Models[0].Alias != "a-first" || document.Models[1].Alias != "z-last" {
		t.Fatalf("models = %+v", document.Models)
	}
	effective := document.Models[1].Effective
	if effective.Values.SupportsTools == nil || *effective.Values.SupportsTools || effective.Sources["supports_tools"] != "override" {
		t.Fatalf("effective = %+v", effective)
	}
	if strings.Contains(out.String(), "api_key") || strings.Contains(out.String(), "base_url") {
		t.Fatal("unexpected provider configuration in listing")
	}
}

func TestModelListJSONMatchesShowWithoutNetwork(t *testing.T) {
	t.Parallel()
	server := newModelsServer(t, []string{"model-one"})
	db := filepath.Join(t.TempDir(), "ccr.db")
	addCapabilityTestModel(t, db, server.URL, "one", "model-one")
	server.Close()
	shown := readModelShowDocument(t, db, "one")
	output, _, err := runCommandWithDeps(t, Dependencies{Secrets: &fakeSecrets{failResolve: true}}, "--db", db, "model", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed modelListDocument
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Models) != 1 || listed.Models[0].Alias != shown.Alias || listed.Models[0].ProviderModel != shown.ProviderModel {
		t.Fatalf("listing differs from show: %+v", listed)
	}
}
