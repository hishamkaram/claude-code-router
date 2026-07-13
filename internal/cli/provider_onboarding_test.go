package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderHelpShowsGuidedInteractiveAndImportReview(t *testing.T) {
	t.Parallel()

	out, _, err := runCommand(t, "provider", "--help")
	if err != nil {
		t.Fatalf("provider help error = %v", err)
	}
	for _, want := range []string{
		"ccr provider add --interactive",
		"ccr provider import-models litellm",
		"ccr provider import-models litellm --all",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider help output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "provider", "add", "--help")
	if err != nil {
		t.Fatalf("provider add help error = %v", err)
	}
	for _, want := range []string{
		"guided provider profile picker",
		"ccr provider add --interactive\n",
		"ccr provider add --interactive openrouter",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider add help output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "provider", "import-models", "--help")
	if err != nil {
		t.Fatalf("provider import-models help error = %v", err)
	}
	for _, want := range []string{"guided searchable multi-select", "--all for deterministic automation"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider import-models help output missing %q:\n%s", want, out)
		}
	}
}

func TestProviderAddInteractiveSavesSelectedModels(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"glm-5.2[1m]", "qwen/qwen3-coder"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"5",        // LiteLLM/OpenAI-compatible
		"",         // default provider name
		server.URL, // base URL
		"3",        // no API key
		"1",        // select models
		"1",        // select first discovered model
		"2",        // select second discovered model
		"0",        // finish multiselect
		"1",        // save reviewed aliases
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "litellm")
	if err != nil {
		t.Fatalf("interactive provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, `Provider "litellm" added`) || !strings.Contains(out, "Imported 2 model aliases") {
		t.Fatalf("interactive provider add output = %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	for _, want := range []string{"litellm-glm-5-2-1m", "model=glm-5.2[1m]", "litellm-qwen-qwen3-coder", "model=qwen/qwen3-coder"} {
		if !strings.Contains(out, want) {
			t.Fatalf("model list output missing %q: %q", want, out)
		}
	}
}

func TestProviderAddInteractiveRejectsEmptyModelSelectionBeforeSaving(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"5",        // LiteLLM/OpenAI-compatible
		"",         // default provider name
		server.URL, // base URL
		"3",        // no API key
		"1",        // select models
		"0",        // finish without selecting a model
	}, "\n") + "\n"

	out, _, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "litellm")
	if err == nil || !strings.Contains(err.Error(), "select at least one model before continuing") {
		t.Fatalf("interactive provider add error = %v, want required-selection error", err)
	}
	if strings.Contains(out, `Provider "litellm" added`) {
		t.Fatalf("interactive provider add persisted provider after empty selection: %q", out)
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if strings.TrimSpace(out) != "No providers configured." {
		t.Fatalf("provider list after empty selection = %q", out)
	}
}

func TestProviderImportModelsGuidedReviewRenamesAlias(t *testing.T) {
	t.Parallel()

	server := newModelsServer(t, []string{"gpt-5", "glm-5.2[1m]"})
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", dbPath, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatalf("provider add error = %v", err)
	}
	input := strings.Join([]string{
		"1",          // select first discovered model
		"0",          // finish multiselect
		"2",          // rename an alias during review
		"1",          // choose first planned alias
		"review-gpt", // replacement alias
		"1",          // save aliases
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "import-models", "litellm")
	if err != nil {
		t.Fatalf("import-models guided error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{"Review model aliases before saving", "Alias review-gpt -> gpt-5 (compat=degraded)", "Launch command: ccr launch", "/model: choose CCR review-gpt"} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-models output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "review-gpt\tprovider=litellm\tmodel=gpt-5\tcompat=degraded") || strings.Contains(out, "litellm-gpt-5") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestModelImportOutputRespectsEditedCompatibility(t *testing.T) {
	t.Parallel()

	planned := []plannedModelImport{
		{alias: "blocked-alias", providerModel: "blocked-model", status: "blocked"},
		{alias: "chat-alias", providerModel: "chat-model", status: "chat-only"},
		{alias: "full-alias", providerModel: "full-model", status: "full"},
	}
	var out strings.Builder

	printModelImportSummary(&out, "litellm", modelImportSummary{imported: len(planned)})
	printModelImportDetails(&out, planned)
	printModelLaunchGuidance(&out, planned, false)

	got := out.String()
	for _, want := range []string{
		`Imported 3 model aliases for provider "litellm".`,
		"Alias blocked-alias -> blocked-model (compat=blocked)",
		"Alias chat-alias -> chat-model (compat=chat-only)",
		"Alias full-alias -> full-model (compat=full)",
		"/model: choose CCR full-alias",
		"Launch command: ccr launch --model chat-alias",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("model import output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"Imported 3 model aliases for provider \"litellm\" (compat=degraded)",
		"CCR blocked-alias",
		"CCR chat-alias",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("model import output contains %q:\n%s", unwanted, got)
		}
	}
}

func TestModelImportReviewRemovalDoesNotCountAsExistingSkip(t *testing.T) {
	t.Parallel()

	planned := []plannedModelImport{
		{alias: "drop-me", providerModel: "drop-model", status: "degraded"},
		{alias: "keep-me", providerModel: "keep-model", status: "degraded"},
	}
	input := strings.Join([]string{
		"4", // remove a model
		"1", // choose first planned alias
		"1", // save remaining aliases
	}, "\n") + "\n"

	reviewed, removed, err := promptModelImportReview(t.Context(), Dependencies{
		In: newPromptReader(input),
	}, planned, map[string]struct{}{})
	if err != nil {
		t.Fatalf("promptModelImportReview error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if len(reviewed) != 1 || reviewed[0].alias != "keep-me" {
		t.Fatalf("reviewed = %#v, want only keep-me", reviewed)
	}

	var out strings.Builder
	printModelImportSummary(&out, "litellm", modelImportSummary{imported: len(reviewed), removed: removed})
	got := out.String()
	if !strings.Contains(got, "Removed 1 aliases during review.") {
		t.Fatalf("summary missing review removal:\n%s", got)
	}
	if strings.Contains(got, "Skipped 1 existing aliases.") {
		t.Fatalf("summary counts review removal as existing skip:\n%s", got)
	}
}

func TestProviderAddInteractiveManualModelFormSavesAlias(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"1",             // Anthropic
		"",              // default provider name
		"",              // default base URL
		"3",             // no API key
		"1",             // add a manual model alias
		"claude-manual", // alias
		"claude-opus",   // provider model ID
		"1",             // degraded compatibility
		"2",             // finish and save provider with aliases
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "anthropic")
	if err != nil {
		t.Fatalf("interactive manual provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{
		`Provider "anthropic" added`,
		"Config and credential validation passed; live routing is still unverified",
		"Alias claude-manual -> claude-opus (compat=degraded)",
		"Launch command: ccr launch",
		"/model: choose CCR claude-manual",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("interactive manual output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "model", "list")
	if err != nil {
		t.Fatalf("model list error = %v", err)
	}
	if !strings.Contains(out, "claude-manual\tprovider=anthropic\tmodel=claude-opus\tcompat=degraded") {
		t.Fatalf("model list output = %q", out)
	}
}

func TestProviderAddInteractiveManualProviderCanSaveUnresolvedEnvRef(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	input := strings.Join([]string{
		"",  // default Anthropic profile
		"",  // default provider name
		"",  // default base URL
		"",  // default env auth mode
		"",  // default env var name from --api-key-env
		"2", // save provider only after validation failure
	}, "\n") + "\n"

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: newPromptReader(input),
	}, "--db", dbPath, "provider", "add", "--interactive", "anthropic", "--api-key-env", "MISSING_ENV")
	if err != nil {
		t.Fatalf("interactive manual provider add error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{
		`Provider validation failed for provider "anthropic"`,
		"Provider validation failed",
		`Provider "anthropic" added`,
		"Next: ccr model add <alias> --provider anthropic --model <provider-model>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("interactive manual provider output missing %q:\n%s", want, out)
		}
	}

	out, _, err = runCommand(t, "--db", dbPath, "provider", "list")
	if err != nil {
		t.Fatalf("provider list error = %v", err)
	}
	if !strings.Contains(out, "anthropic\tanthropic\tprotocol=anthropic-compatible") || !strings.Contains(out, "secret=env:MISSING_ENV") {
		t.Fatalf("provider list output = %q", out)
	}
}
