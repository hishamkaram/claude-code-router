package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestClaudeAccountImportRejectsExistingAccountBeforeCredentialAccess(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name:           "personal",
		AccessTokenRef: secret.ClaudeAccountAccessTokenRef("personal"),
		ScopesJSON:     "[]",
		Enabled:        true,
	})
	input := strings.NewReader("must-not-be-read")
	secrets := &accountTestSecrets{failAvailable: true}

	_, _, err := runCommandWithDeps(t, Dependencies{In: input, Secrets: secrets},
		"--db", dbPath, "claude-account", "import", "personal", "--oauth-token-stdin")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("claude-account import error = %v", err)
	}
	assertClaudeAccountCredentialUntouched(t, input, secrets)
}

func TestClaudeAccountRefreshRejectsMissingAccountBeforeCredentialAccess(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name:           "other",
		AccessTokenRef: secret.ClaudeAccountAccessTokenRef("other"),
		ScopesJSON:     "[]",
		Enabled:        true,
	})
	input := strings.NewReader("must-not-be-read")
	secrets := &accountTestSecrets{failAvailable: true}

	_, _, err := runCommandWithDeps(t, Dependencies{In: input, Secrets: secrets},
		"--db", dbPath, "claude-account", "refresh", "missing", "--oauth-token-stdin")
	if err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("claude-account refresh error = %v", err)
	}
	assertClaudeAccountCredentialUntouched(t, input, secrets)
}

func assertClaudeAccountCredentialUntouched(
	t *testing.T,
	input *strings.Reader,
	secrets *accountTestSecrets,
) {
	t.Helper()
	if input.Len() != len("must-not-be-read") {
		t.Fatal("command consumed OAuth token input before account-state validation")
	}
	if secrets.availableCalls != 0 {
		t.Fatal("command accessed the keychain before account-state validation")
	}
}
