package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type fakeSecrets struct {
	values       map[string]string
	failStore    bool
	failResolve  bool
	resolveCount int
}

func (f *fakeSecrets) Available(ctx context.Context) error {
	return ctx.Err()
}

func (f *fakeSecrets) Store(ctx context.Context, ref string, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.failStore {
		return fmt.Errorf("fake keyring unavailable")
	}
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[ref] = value
	return nil
}

func (f *fakeSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.resolveCount++
	if f.failResolve {
		return "", fmt.Errorf("fake resolve should not be called")
	}
	return f.values[ref], nil
}

func TestRootHelpExplainsRouterConcepts(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "help")
	if err != nil {
		t.Fatalf("help error = %v", err)
	}
	for _, want := range []string{"fixed local gateway", "launch --model <alias>", "SQLite", "never silently fall back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "version")
	if err != nil {
		t.Fatalf("version error = %v", err)
	}
	if !strings.Contains(out, "ccr dev") {
		t.Fatalf("version output = %q", out)
	}
}

func TestUnknownCommandReturnsSuggestion(t *testing.T) {
	t.Parallel()

	_, _, err := runCommand(t, "provder")
	if err == nil {
		t.Fatalf("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestNoArgCommandsRejectStrayArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	tests := [][]string{
		{"--db", dbPath, "init", "unexpected"},
		{"version", "unexpected"},
		{"--db", dbPath, "status", "unexpected"},
		{"--db", dbPath, "doctor", "unexpected"},
		{"sessions", "unexpected"},
		{"agents", "unexpected"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, _, err := runCommand(t, args...)
			if err == nil {
				t.Fatalf("runCommand(%v) unexpectedly succeeded", args)
			}
		})
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database path exists after invalid init: stat err=%v", err)
	}
}

func TestVisibleCommandsDoNotReturnNotImplemented(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt-5", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	tests := []struct {
		name string
		deps Dependencies
		args []string
	}{
		{name: "provider_test", args: []string{"--db", dbPath, "provider", "test", "litellm"}},
		{name: "provider_update_missing_flags", args: []string{"--db", dbPath, "provider", "update", "litellm"}},
		{name: "provider_remove_confirm_required", args: []string{"--db", dbPath, "provider", "remove", "litellm"}},
		{name: "model_test", args: []string{"--db", dbPath, "model", "test", "litellm-gpt-5"}},
		{name: "model_update_missing_flags", args: []string{"--db", dbPath, "model", "update", "litellm-gpt-5"}},
		{name: "model_remove_confirm_required", args: []string{"--db", dbPath, "model", "remove", "litellm-gpt-5"}},
		{name: "conformance_run", args: []string{"--db", dbPath, "conformance", "run", "litellm-gpt-5"}},
		{name: "sessions", args: []string{"--db", dbPath, "sessions"}},
		{name: "agents", args: []string{"--db", dbPath, "agents"}},
		{name: "launch", deps: Dependencies{Launcher: &fakeLauncher{pid: os.Getpid()}}, args: []string{"--db", dbPath, "launch"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, err := runCommandWithDeps(t, tt.deps, tt.args...)
			combined := out + errOut
			if err != nil {
				combined += err.Error()
			}
			if strings.Contains(strings.ToLower(combined), "not implemented yet") {
				t.Fatalf("command returned placeholder output/error:\nstdout=%s\nstderr=%s\nerr=%v", out, errOut, err)
			}
		})
	}
}

func TestProviderAndModelAddRoundTrip(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-env", "OPENROUTER_API_KEY")
	if err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if strings.Contains(out, "sk-") || !strings.Contains(out, secret.EnvRef("OPENROUTER_API_KEY")) {
		t.Fatalf("provider add output did not redact/store env ref as expected: %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "add", "qwen", "--provider", "openrouter", "--model", "qwen/qwen3-coder")
	if err != nil {
		t.Fatalf("model add error = %v", err)
	}
	if !strings.Contains(out, `Model alias "qwen" added`) {
		t.Fatalf("model add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "qwen") || !strings.Contains(out, "openrouter") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestProviderAddZAIAndGenericProtocol(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	out, _, err := runCommand(t, "--db", dbPath, "provider", "add", "zai", "--api-key-env", "ZAI_API_KEY")
	if err != nil {
		t.Fatalf("zai provider add error = %v", err)
	}
	for _, want := range []string{"zai", "protocol=anthropic-compatible", "mode=full", "https://api.z.ai/api/anthropic", "env:ZAI_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Fatalf("zai provider add output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "add", "glm", "--protocol", "anthropic-compatible", "--base-url", "http://localhost:5000", "--no-api-key")
	if err != nil {
		t.Fatalf("generic protocol provider add error = %v", err)
	}
	if !strings.Contains(out, "anthropic-compatible") || !strings.Contains(out, "mode=degraded") {
		t.Fatalf("generic protocol provider add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "status")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	for _, want := range []string{
		"Provider zai: type=zai protocol=anthropic-compatible mode=full token-count=provider",
		"Provider glm: type=anthropic-compatible protocol=anthropic-compatible mode=degraded token-count=estimated",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestProviderAddInteractiveSavesSelectedModelsWithPrefixedAliases(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"glm-5.2[1m]", "qwen/qwen3-coder"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"litellm",  // provider name
		"1",        // LiteLLM/OpenAI-compatible
		server.URL, // base URL
		"3",        // no API key
		"1",        // select models
		"1",        // select first discovered model
		"0",        // finish multiselect
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "litellm")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Provider "litellm" added`) || !strings.Contains(out, "Imported 1 model aliases") {
		t.Fatalf("interactive provider add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "litellm-glm-5-2-1m") || !strings.Contains(out, "model=glm-5.2[1m]") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestProviderAddInteractiveProtocolDefaultForNonTerminal(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"",  // default provider name
		"",  // default provider type from --protocol
		"",  // default base URL from --base-url
		"",  // default auth mode from --no-api-key
		"y", // save after model discovery is rejected
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "glm", "--interactive", "--protocol", "anthropic-compatible", "--base-url", "http://localhost:5000", "--no-api-key")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{`Provider "glm" added`, "protocol=anthropic-compatible", "mode=degraded", "token-count=estimated"} {
		if !strings.Contains(out, want) {
			t.Fatalf("interactive provider add output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "glm\tanthropic-compatible\tprotocol=anthropic-compatible") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderAddInteractiveRecomputesDefaultModeAfterTypeSelection(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"foo", // provider name
		"3",   // Anthropic
		"",    // default base URL
		"3",   // no API key
		"y",   // save after unsupported discovery
	}, "\n") + "\n"

	_, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstderr:\n%s", err, errOut)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "foo\tanthropic\tprotocol=anthropic-compatible\tmode=full") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderPromptModeDefaultRecomputesOnlyUnchangedDefault(t *testing.T) {
	t.Parallel()

	unchanged := applyProviderPromptModeDefault(providerSetupPrompt{providerType: "zai", mode: "degraded"}, true, "degraded")
	if unchanged.mode != "full" {
		t.Fatalf("unchanged default mode = %q, want full", unchanged.mode)
	}

	selected := applyProviderPromptModeDefault(providerSetupPrompt{providerType: "zai", mode: "chat-only"}, true, "degraded")
	if selected.mode != "chat-only" {
		t.Fatalf("selected mode = %q, want chat-only", selected.mode)
	}
}

func TestProviderAddInteractiveTrimsBaseURLBeforeSave(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"litellm",                // provider name
		"1",                      // LiteLLM/OpenAI-compatible
		"  " + server.URL + "  ", // base URL with pasted whitespace
		"3",                      // no API key
		"3",                      // skip model import
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "litellm")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, server.URL) || strings.Contains(out, "  "+server.URL+"  ") {
		t.Fatalf("provider list output did not store trimmed base URL: %q", out)
	}
}

func TestProviderAddInteractiveStoresKeychainAPIKeyFromNonTerminal(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"openrouter", // provider name
		"2",          // OpenRouter
		server.URL,   // custom base URL to avoid external network
		"1",          // store API key in keychain
		"sk-test",    // API key
		"y",          // save after discovery failure
	}, "\n") + "\n"
	fake := &fakeSecrets{}

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In:      newPromptReader(input),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "--interactive", "openrouter")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if strings.Contains(out, "sk-test") || strings.Contains(errOut, "sk-test") {
		t.Fatalf("interactive provider add leaked secret:\nstdout=%s\nstderr=%s", out, errOut)
	}
	ref := secret.KeyringRef("openrouter")
	if got := fake.values[ref]; got != "sk-test" {
		t.Fatalf("stored keyring value for %s = %q, want sk-test", ref, got)
	}
	if !strings.Contains(out, `Provider "openrouter" added`) {
		t.Fatalf("interactive provider add output = %q", out)
	}
}

func TestProviderAddInteractiveDoesNotSaveWhenDiscoveryFailsAndUserDeclines(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"litellm",  // provider name
		"1",        // LiteLLM/OpenAI-compatible
		server.URL, // base URL
		"3",        // no API key
		"n",        // do not save after discovery failure
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "litellm")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Provider "litellm" was not saved.`) {
		t.Fatalf("interactive provider add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "No providers configured.") {
		t.Fatalf("provider was saved after decline: %q", out)
	}
}

func TestProviderAddInteractiveRejectsConflictingAuthFlags(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "--interactive", "litellm", "--api-key-env", "LITELLM_API_KEY", "--no-api-key")
	if err == nil {
		t.Fatalf("interactive provider add unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "choose only one API key source") {
		t.Fatalf("interactive provider add error = %v", err)
	}
}

func TestProviderAddInteractiveRejectsInvalidStaticTypeBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "--interactive", "--type", "bad", "litellm")
	if err == nil {
		t.Fatalf("interactive provider add unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid provider type") {
		t.Fatalf("interactive provider add error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid interactive provider add: stat err=%v", statErr)
	}
}

func TestProviderAddInteractiveShowsManualNextStepForUnsupportedDiscovery(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"anthropic", // provider name
		"3",         // Anthropic
		"",          // default base URL
		"3",         // no API key
		"y",         // save after unsupported discovery
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "anthropic")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if strings.Contains(out, "provider import-models anthropic") {
		t.Fatalf("interactive provider add suggested impossible import-models step: %q", out)
	}
	if !strings.Contains(out, "Next: ccr model add <alias> --provider anthropic --model <provider-model>") {
		t.Fatalf("interactive provider add output = %q", out)
	}
}

func TestProviderDiscoverModelsPrintsModelsWithoutImport(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5", "glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "discover-models", "litellm")
	if err != nil {
		t.Fatalf("discover-models error = %v", err)
	}
	for _, want := range []string{"Discovering models", "gpt-5", "glm-5.2[1m]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("discover-models output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "No model aliases configured.") {
		t.Fatalf("discover-models imported unexpectedly: %q", out)
	}
}

func TestProviderDiscoverModelsRejectsUnsupportedProviderBeforeResolvingSecret(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--api-key-env", "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	fake := &fakeSecrets{failResolve: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: fake,
	}, "--db", dbPath, "provider", "discover-models", "anthropic")
	if err == nil {
		t.Fatalf("discover-models unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "does not support OpenAI-compatible model discovery") {
		t.Fatalf("discover-models error = %v", err)
	}
	if fake.resolveCount != 0 {
		t.Fatalf("secret Resolve called %d times, want 0", fake.resolveCount)
	}
}

func TestProviderImportModelsAllImportsDiscoveredModels(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5", "glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "import-models", "litellm", "--all")
	if err != nil {
		t.Fatalf("import-models --all error = %v", err)
	}
	if !strings.Contains(out, "Imported 2 model aliases") {
		t.Fatalf("import-models output = %q", out)
	}
	if !strings.Contains(out, "compat=degraded") {
		t.Fatalf("import-models output did not surface degraded compatibility: %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	for _, want := range []string{"litellm-gpt-5", "litellm-glm-5-2-1m", "model=glm-5.2[1m]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model list output missing %q:\n%s", want, out)
		}
	}
}

func TestProviderImportModelsAllSkipsExistingAliases(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"glm-5.2[1m]", "gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-glm-5-2-1m", "--provider", "litellm", "--model", "existing/model"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "import-models", "litellm", "--all")
	if err != nil {
		t.Fatalf("import-models --all error = %v", err)
	}
	if !strings.Contains(out, "Imported 1 model aliases") || !strings.Contains(out, "Skipped 1 existing aliases") {
		t.Fatalf("import-models conflict output = %q", out)
	}
}

func TestProviderImportModelsAllSkipsTruncatedAliasCollisions(t *testing.T) {
	t.Parallel()

	modelPrefix := strings.Repeat("a", 80)
	server := newModelsServer(t, []string{modelPrefix + "-one", modelPrefix + "-two"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "import-models", "litellm", "--all")
	if err != nil {
		t.Fatalf("import-models --all error = %v", err)
	}
	if !strings.Contains(out, "Imported 1 model aliases") || !strings.Contains(out, "Skipped 1 existing aliases") {
		t.Fatalf("import-models truncation collision output = %q", out)
	}
}

func TestProviderAddFromStdinStoresKeyringReferenceOnly(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{}
	out, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("sk-test\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err != nil {
		t.Fatalf("provider add stdin error = %v", err)
	}
	if strings.Contains(out, "sk-test") {
		t.Fatalf("provider add leaked secret: %q", out)
	}
	if len(fake.values) != 1 {
		t.Fatalf("stored secrets = %#v, want 1", fake.values)
	}
}

func TestProviderAddWithAPIKeyFileStoresReferenceOnly(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	keyPath := writeAPIKeyFile(t, 0o600)
	out, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-file", keyPath)
	if err != nil {
		t.Fatalf("provider add file error = %v", err)
	}
	if strings.Contains(out, "sk-file-secret") {
		t.Fatalf("provider add leaked secret: %q", out)
	}
	if !strings.Contains(out, "file:***") {
		t.Fatalf("provider add output missing redacted file ref: %q", out)
	}

	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store open error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("store close error = %v", closeErr)
		}
	})
	provider, err := s.GetProvider(context.Background(), "openrouter")
	if err != nil {
		t.Fatalf("GetProvider error = %v", err)
	}
	if provider.SecretRef != secret.FileRef(keyPath) {
		t.Fatalf("SecretRef = %q, want %q", provider.SecretRef, secret.FileRef(keyPath))
	}
	if strings.Contains(provider.SecretRef, "sk-file-secret") {
		t.Fatalf("provider secret ref leaked secret: %q", provider.SecretRef)
	}
}

func TestProviderAddWithAPIKeyFileRejectsLoosePermissions(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	keyPath := writeAPIKeyFile(t, 0o644)
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-file", keyPath)
	if err == nil {
		t.Fatalf("provider add file unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "sk-file-secret") {
		t.Fatalf("provider add leaked secret in error: %v", err)
	}
	if strings.Contains(err.Error(), keyPath) {
		t.Fatalf("provider add leaked file path in error: %v", err)
	}
	if !strings.Contains(err.Error(), "permissions 0600") {
		t.Fatalf("provider add file error = %v", err)
	}
}

func TestProviderAddRejectsConflictingAPIKeyFileSource(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	keyPath := writeAPIKeyFile(t, 0o600)
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter", "--api-key-file", keyPath, "--api-key-env", "OPENROUTER_API_KEY")
	if err == nil {
		t.Fatalf("provider add conflict unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "choose only one API key source") {
		t.Fatalf("provider add conflict error = %v", err)
	}
}

func TestDuplicateProviderDoesNotOverwriteKeyringSecret(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{}
	if _, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("old-key\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin"); err != nil {
		t.Fatalf("initial provider add error = %v", err)
	}

	_, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("new-key\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err == nil {
		t.Fatalf("duplicate provider add unexpectedly succeeded")
	}

	ref := secret.KeyringRef("anthropic")
	if got := fake.values[ref]; got != "old-key" {
		t.Fatalf("keyring value for %s = %q, want old-key", ref, got)
	}
}

func TestProviderAddDoesNotPersistWhenKeyringStoreFails(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	fake := &fakeSecrets{failStore: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		In:      strings.NewReader("sk-test\n"),
		Secrets: fake,
	}, "--db", dbPath, "provider", "add", "anthropic", "--api-key-stdin")
	if err == nil {
		t.Fatalf("provider add unexpectedly succeeded")
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "No providers configured.") {
		t.Fatalf("provider list output = %q, want no persisted provider", out)
	}
}

func TestProviderAddValidation(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "BadName", "--no-api-key")
	if err == nil {
		t.Fatalf("expected invalid provider name error")
	}
	if !strings.Contains(err.Error(), "invalid provider name") {
		t.Fatalf("error = %v", err)
	}
}

func TestGenerateProviderModelAliasUsesProviderPrefix(t *testing.T) {
	t.Parallel()

	got := generateProviderModelAlias("litellm", "glm-5.2[1m]")
	if got != "litellm-glm-5-2-1m" {
		t.Fatalf("generateProviderModelAlias() = %q", got)
	}
	got = generateProviderModelAlias("openrouter", "glm-5.2[1m]")
	if got != "openrouter-glm-5-2-1m" {
		t.Fatalf("generateProviderModelAlias(openrouter) = %q", got)
	}

	got = generateProviderModelAlias("litellm", strings.Repeat("a", 120))
	if len(got) > 64 {
		t.Fatalf("generateProviderModelAlias(long) length = %d, want <= 64: %q", len(got), got)
	}
	if err := validateName("model alias", got); err != nil {
		t.Fatalf("generateProviderModelAlias(long) produced invalid alias %q: %v", got, err)
	}
}

func TestDoctorUsesDatabaseAndReportsClaudeCode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "doctor")
	if err != nil {
		t.Fatalf("doctor error = %v", err)
	}
	for _, want := range []string{"SQLite: ok", "Secrets: ok", "Claude Code:", "Providers: 1", "Provider litellm: protocol=openai-compatible mode=degraded token-count=provider"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func runCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return runCommandWithDeps(t, Dependencies{}, args...)
}

func runCommandWithDeps(t *testing.T, deps Dependencies, args ...string) (string, string, error) {
	t.Helper()
	var out bytes.Buffer
	var errOut bytes.Buffer
	if deps.In == nil {
		deps.In = strings.NewReader("")
	}
	deps.Out = &out
	deps.Err = &errOut
	if deps.Secrets == nil {
		deps.Secrets = &fakeSecrets{}
	}
	cmd := NewRootCommand(context.Background(), deps)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func newModelsServer(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		parts := make([]string, 0, len(models))
		for _, model := range models {
			parts = append(parts, fmt.Sprintf(`{"id":%q}`, model))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(parts, ","))
	}))
	t.Cleanup(server.Close)
	return server
}

func writeAPIKeyFile(t *testing.T, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte("sk-file-secret\n"), perm); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	return path
}

type promptReader struct {
	reader *strings.Reader
}

func newPromptReader(input string) *promptReader {
	return &promptReader{reader: strings.NewReader(input)}
}

func (r *promptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.reader.Read(p[:1])
}

type fakeLauncher struct {
	pid     int
	args    []string
	env     []string
	out     io.Writer
	errOut  io.Writer
	waitErr error
	starts  int
	process *fakeProcess
}

func (f *fakeLauncher) Start(ctx context.Context, args, env []string, in io.Reader, out, errOut io.Writer) (ClaudeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.starts++
	f.args = append([]string(nil), args...)
	f.env = append([]string(nil), env...)
	f.out = out
	f.errOut = errOut
	f.process = &fakeProcess{pid: f.pid, waitErr: f.waitErr}
	return f.process, nil
}

func (f *fakeLauncher) hasEnvPrefix(prefix string) bool {
	for _, item := range f.env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) hasEnv(value string) bool {
	for _, item := range f.env {
		if item == value {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) envValue(name string) (string, bool) {
	prefix := name + "="
	for _, item := range f.env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", false
}

func (f *fakeLauncher) hasArg(value string) bool {
	for _, item := range f.args {
		if item == value {
			return true
		}
	}
	return false
}

func (f *fakeLauncher) settingsArgValue() (string, bool) {
	for index, item := range f.args {
		if item == "--settings" && index+1 < len(f.args) {
			return f.args[index+1], true
		}
	}
	return "", false
}

type fakeProcess struct {
	pid     int
	waitErr error
	stopped bool
	waited  bool
}

func (p *fakeProcess) PID() int {
	return p.pid
}

func (p *fakeProcess) Wait() error {
	p.waited = true
	return p.waitErr
}

func (p *fakeProcess) Stop() error {
	p.stopped = true
	return nil
}
