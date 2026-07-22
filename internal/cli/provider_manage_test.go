package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestProviderRemoveDeletesProviderAndModels(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "litellm-gpt-5", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatalf("model add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "litellm", "--yes")
	if err != nil {
		t.Fatalf("provider remove error = %v", err)
	}
	for _, want := range []string{`Provider "litellm" removed`, "Removed 1 model aliases", "Secret reference removed from SQLite"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider remove output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "No providers configured.") {
		t.Fatalf("provider list output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "No model aliases configured.") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestResolveProviderUpdateBaseClearsResponsesOnTypeChange(t *testing.T) {
	t.Parallel()

	existing := store.Provider{
		Name: "fixture", Type: "openai-compatible", BaseURL: "https://responses.example",
		Protocol: "openai-compatible", SupportsTools: true, SupportsStreaming: true, SupportsResponses: true,
	}
	updated, err := resolveProviderUpdateBase(existing, providerSetupPrompt{
		providerType: "anthropic-compatible", baseURL: "https://anthropic.example",
	})
	if err != nil {
		t.Fatalf("resolveProviderUpdateBase() error = %v", err)
	}
	if updated.SupportsResponses {
		t.Fatalf("updated provider retained Responses support: %#v", updated)
	}
}

func TestProviderUpdateInteractivePreservesResponsesForUnchangedProfile(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openai", "--type", "openai-compatible", "--base-url", "https://responses.example", "--api-key-env", "CCR_TEST_OPENAI_KEY", "--responses"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"", // keep the Responses-capable profile
		"", // keep the base URL
		"", // keep the secret reference
	}, "\n") + "\n"
	if _, errOut, err := runCommandWithDeps(t, Dependencies{In: newPromptReader(input)}, "--db", dbPath, "provider", "update", "openai", "--interactive"); err != nil {
		t.Fatalf("interactive provider update error = %v\nstderr:\n%s", err, errOut)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "responses") {
		t.Fatalf("provider list dropped Responses support: %q", out)
	}
}

func TestProviderUpdateWithUnchangedTypeOrProtocolPreservesResponsesCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "type", args: []string{"--type", "openai-compatible"}},
		{name: "protocol", args: []string{"--protocol", "openai-compatible"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dbPath := filepath.Join(t.TempDir(), "ccr.db")
			if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openai", "--type", "openai-compatible", "--base-url", "https://responses.example", "--api-key-env", "CCR_TEST_OPENAI_KEY", "--responses"); err != nil {
				t.Fatalf("provider add error = %v", err)
			}
			args := append([]string{"--db", dbPath, "provider", "update", "openai"}, test.args...)
			if _, _, err := runCommand(t, args...); err != nil {
				t.Fatalf("provider update error = %v", err)
			}
			out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
			if err != nil {
				t.Fatalf("provider list error = %v", err)
			}
			if !strings.Contains(out, "responses") {
				t.Fatalf("provider list dropped Responses support: %q", out)
			}
		})
	}
}

func TestProviderRemoveMissingProviderFailsBeforeSideEffects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "missing", "--yes")
	if err == nil {
		t.Fatalf("provider remove unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), `provider "missing" does not exist`) {
		t.Fatalf("provider remove error = %v", err)
	}
}

func TestProviderRemoveWithoutConfirmationFailsAndPreservesRows(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommand(t, "--db", dbPath, "provider", "remove", "litellm")
	if err == nil {
		t.Fatalf("provider remove unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires confirmation") {
		t.Fatalf("provider remove error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "litellm") {
		t.Fatalf("provider was removed without confirmation: %q", out)
	}
}

func TestProviderRemoveInteractiveCanCancel(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader("n\n"),
	}, "--db", dbPath, "provider", "remove", "litellm", "--interactive")
	if err != nil {
		t.Fatalf("interactive provider remove error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Provider "litellm" was not removed.`) {
		t.Fatalf("interactive provider remove output = %q", out)
	}
}

func TestProviderAddRequiresAPIKeyForOpenRouter(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "openrouter")
	if err == nil {
		t.Fatalf("expected missing API key error")
	}
	if !strings.Contains(err.Error(), "API key required") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderUpdateWithFlags(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--base-url", "http://localhost:5000", "--api-key-env", "LITELLM_API_KEY")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	if !strings.Contains(out, "http://localhost:5000") || !strings.Contains(out, "env:LITELLM_API_KEY") {
		t.Fatalf("provider update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "http://localhost:5000") || !strings.Contains(out, "env:LITELLM_API_KEY") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderResponsesCapabilityCanBeConfiguredAtAddAndUpdate(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "responses", "--type", "openai-compatible", "--base-url", "http://localhost:4000", "--no-api-key", "--responses"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "caps=tools,streaming,thinking,models,responses,") {
		t.Fatalf("provider list after add = %q", out)
	}
	if _, _, updateErr := runCommand(t, "--db", dbPath, "provider", "update", "responses", "--responses=false"); updateErr != nil {
		t.Fatalf("provider update error = %v", updateErr)
	}
	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if strings.Contains(out, "responses,") {
		t.Fatalf("provider list retained disabled Responses capability: %q", out)
	}
}

func TestProviderResponsesCapabilityRejectsAnthropicCompatibleProvider(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--type", "anthropic-compatible", "--base-url", "https://anthropic.example", "--no-api-key", "--responses")
	if err == nil || !strings.Contains(err.Error(), `responses API capability requires provider protocol "openai-compatible"`) {
		t.Fatalf("provider add error = %v", err)
	}

	if _, _, addErr := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--type", "anthropic-compatible", "--base-url", "https://anthropic.example", "--no-api-key"); addErr != nil {
		t.Fatalf("provider add without Responses capability error = %v", addErr)
	}
	_, _, err = runCommand(t, "--db", dbPath, "provider", "update", "anthropic", "--responses")
	if err == nil || !strings.Contains(err.Error(), `responses API capability requires provider protocol "openai-compatible"`) {
		t.Fatalf("provider update error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if strings.Contains(out, "responses,") {
		t.Fatalf("provider list advertised Responses capability: %q", out)
	}
}

func TestProviderUpdateInvalidatesDiscoveredModelCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "model", "add", "glm", "--provider", "litellm", "--model", "glm-5.2"); err != nil {
		t.Fatalf("model add error = %v", err)
	}
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	model, err := s.GetModel(ctx, "glm")
	if err != nil {
		_ = s.Close()
		t.Fatalf("GetModel() error = %v", err)
	}
	model.DiscoveredCapabilities, err = modelcap.SnapshotFrom(
		modelcap.Values{ContextWindowTokens: modelcap.Int64(1_000_000)}, "litellm:/model/info",
	)
	if err != nil {
		_ = s.Close()
		t.Fatalf("SnapshotFrom() error = %v", err)
	}
	model.CapabilityOverrides.MaxOutputTokens = modelcap.Int64(64_000)
	model.CapabilitiesRefreshedAt = "2026-07-18T12:00:00Z"
	if updateErr := s.UpdateModel(ctx, model); updateErr != nil {
		_ = s.Close()
		t.Fatalf("UpdateModel() error = %v", updateErr)
	}
	if closeErr := s.Close(); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--base-url", "http://localhost:5000")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	for _, want := range []string{"Cleared discovered capabilities for 1 model alias(es)", "Next: ccr model refresh --all"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider update output missing %q:\n%s", want, out)
		}
	}
	document := readModelShowDocument(t, dbPath, "glm")
	if !modelcap.IsZeroSnapshot(document.Discovered) || document.RefreshedAt != "" {
		t.Fatalf("provider update retained stale capabilities: %#v", document)
	}
	if document.Overrides.MaxOutputTokens == nil || *document.Overrides.MaxOutputTokens != 64_000 {
		t.Fatalf("provider update cleared overrides: %#v", document.Overrides)
	}
}

func TestProviderUpdateWithAPIKeyFile(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	keyPath := writeAPIKeyFile(t, 0o600)

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--api-key-file", keyPath)
	if err != nil {
		t.Fatalf("provider update file error = %v", err)
	}
	if strings.Contains(out, "sk-file-secret") || !strings.Contains(out, "file:***") {
		t.Fatalf("provider update output did not redact file secret: %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if strings.Contains(out, "sk-file-secret") || !strings.Contains(out, "file:***") {
		t.Fatalf("provider list output did not redact file secret: %q", out)
	}
}

func TestProviderUpdateTypeRequiresExplicitAuthDecision(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--type", "openrouter")
	if err == nil {
		t.Fatalf("provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("provider update error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--type", "openrouter", "--no-api-key")
	if err != nil {
		t.Fatalf("provider update explicit no-key error = %v", err)
	}
	if !strings.Contains(out, "openrouter") {
		t.Fatalf("provider update explicit no-key output = %q", out)
	}
}

func TestProviderUpdateProtocolOnlyPreservesExistingType(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "foo", "--type", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "foo", "--protocol", "openai-compatible")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	if !strings.Contains(out, `Provider "foo" updated (litellm, protocol=openai-compatible, mode=degraded`) {
		t.Fatalf("provider update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "foo\tlitellm\tprotocol=openai-compatible\tmode=degraded") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateTypeRecomputesOldDefaultMode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "anthropic", "--type", "litellm", "--base-url", "http://localhost:4000", "--no-api-key")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	if !strings.Contains(out, "mode=degraded") {
		t.Fatalf("provider update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "anthropic\tlitellm\tprotocol=openai-compatible\tmode=degraded\tcaps=tools,streaming,thinking,models") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateTypePreservesExistingChatOnlyMode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--mode", "chat-only", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "update", "litellm", "--type", "anthropic-compatible", "--base-url", "http://localhost:5000", "--no-api-key")
	if err != nil {
		t.Fatalf("provider update error = %v", err)
	}
	if !strings.Contains(out, "mode=chat-only") {
		t.Fatalf("provider update output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "litellm\tanthropic-compatible\tprotocol=anthropic-compatible\tmode=chat-only\tcaps=streaming,thinking") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateInteractiveTypeRecomputesOldDefaultMode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"",                      // default provider type from --type
		"http://localhost:4000", // base URL
		"",                      // default auth mode from --no-api-key
	}, "\n") + "\n"

	_, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "anthropic", "--interactive", "--type", "litellm", "--no-api-key")
	if err != nil {
		t.Fatalf("interactive provider update error = %v\nstderr:\n%s", err, errOut)
	}

	out, _, err := runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "anthropic\tlitellm\tprotocol=openai-compatible\tmode=degraded\tcaps=tools,streaming,thinking,models") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateInvalidStaticFlagsFailBeforeDatabaseOpen(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	_, _, err := runCommand(t, "--db", dbPath, "provider", "update", "missing", "--type", "bad")
	if err == nil {
		t.Fatalf("provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid provider type") {
		t.Fatalf("provider update error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("database exists after invalid provider update: stat err=%v", statErr)
	}
}

func TestProviderUpdateInteractiveInvalidStaticFlagsFailBeforePrompt(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}

	_, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader("\n\n\n"),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive", "--type", "bad")
	if err == nil {
		t.Fatalf("interactive provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "invalid provider type") {
		t.Fatalf("interactive provider update error = %v", err)
	}
}

func TestProviderUpdateInteractive(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"5",                     // LiteLLM/OpenAI-compatible
		"http://localhost:5000", // base URL
		"4",                     // no API key
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive")
	if err != nil {
		t.Fatalf("interactive provider update error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "http://localhost:5000") {
		t.Fatalf("interactive provider update output = %q", out)
	}
}

func TestProviderUpdateInteractivePreservesExistingChatOnlyMode(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--mode", "chat-only", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"",                      // default provider type from --type
		"http://localhost:5000", // base URL
		"",                      // default auth mode from --no-api-key
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive", "--type", "anthropic-compatible", "--no-api-key")
	if err != nil {
		t.Fatalf("interactive provider update error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "litellm\tanthropic-compatible\tprotocol=anthropic-compatible\tmode=chat-only\tcaps=streaming,thinking") {
		t.Fatalf("provider list output = %q", out)
	}
}

func TestProviderUpdateInteractiveRequiresAuthDecisionWhenTypeStartsRequiringKey(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", "http://localhost:4000", "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"4", // OpenRouter
		"",  // keep existing base URL
		"1", // keep current empty secret reference
	}, "\n") + "\n"

	_, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "update", "litellm", "--interactive")
	if err == nil {
		t.Fatalf("interactive provider update unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("interactive provider update error = %v", err)
	}
}

func TestProviderTestOpenAICompatibleAndAnthropic(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	out, _, err := runCommand(t, "--db", dbPath, "provider", "test", "litellm")
	if err != nil {
		t.Fatalf("provider test error = %v", err)
	}
	if !strings.Contains(out, "discovered 1 routable models") {
		t.Fatalf("provider test output = %q", out)
	}

	if _, _, addErr := runCommand(t, "--db", dbPath, "provider", "add", "anthropic", "--no-api-key"); addErr != nil {
		t.Fatalf("anthropic provider add error = %v", addErr)
	}
	out, _, err = runCommand(t, "--db", dbPath, "provider", "test", "anthropic")
	if err != nil {
		t.Fatalf("anthropic provider test error = %v", err)
	}
	if !strings.Contains(out, "Anthropic live routing is outside this pass") {
		t.Fatalf("anthropic provider test output = %q", out)
	}
}
