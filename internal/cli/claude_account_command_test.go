package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type accountTestSecrets struct {
	values         map[string]string
	failAvailable  bool
	failStore      bool
	failDelete     bool
	availableCalls int
	deleted        []string
}

func (s *accountTestSecrets) Available(context.Context) error {
	s.availableCalls++
	if s.failAvailable {
		return errors.New("keychain unavailable")
	}
	return nil
}

func (s *accountTestSecrets) Store(_ context.Context, ref, value string) error {
	if s.failStore {
		return errors.New("keychain unavailable")
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[ref] = value
	return nil
}

func (s *accountTestSecrets) Resolve(_ context.Context, ref string) (string, error) {
	value, ok := s.values[ref]
	if !ok {
		return "", secret.ErrNotFound
	}
	return value, nil
}

func (s *accountTestSecrets) Delete(_ context.Context, ref string) error {
	if s.failDelete {
		return errors.New("keychain delete failed")
	}
	delete(s.values, ref)
	s.deleted = append(s.deleted, ref)
	return nil
}

func TestClaudeAccountImportStoresOnlyKeychainReferences(t *testing.T) {
	t.Parallel()

	const token = "setup-token-must-not-enter-sqlite"
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{}
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader(token + "\n"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "import", "personal", "--oauth-token-stdin")
	if err != nil {
		t.Fatalf("claude-account import error = %v, stderr=%q", err, errOut)
	}
	if strings.Contains(out+errOut, token) {
		t.Fatal("claude-account import output leaked the OAuth token")
	}
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	if secrets.values[ref] != token {
		t.Fatal("OAuth token was not stored under the account keychain reference")
	}

	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	account, err := s.GetClaudeAccount(ctx, "personal")
	if err != nil {
		t.Fatalf("GetClaudeAccount error = %v", err)
	}
	if account.AccessTokenRef != ref || account.RefreshTokenRef != "" {
		t.Fatalf("stored account refs = %#v", account)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile database error = %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("SQLite database contains the OAuth token")
	}
}

func TestClaudeAccountImportValidatesBeforeSideEffects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{}
	_, _, err := runCommandWithDeps(t, Dependencies{Secrets: secrets},
		"--db", dbPath, "claude-account", "import", "personal")
	if err == nil || !strings.Contains(err.Error(), "choose exactly one credential source") {
		t.Fatalf("claude-account import error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database was created before credential source validation: %v", statErr)
	}
	if len(secrets.values) != 0 {
		t.Fatal("secret backend changed before validation")
	}
}

func TestClaudeAccountImportValidatesCredentialBeforeDatabaseSideEffects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{}
	_, _, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("malformed token"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "import", "personal", "--oauth-token-stdin")
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("claude-account import error = %v", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database was created before credential validation: %v", statErr)
	}
	if len(secrets.values) != 0 {
		t.Fatal("secret backend changed before credential validation")
	}
}

func TestClaudeAccountImportDoesNotPersistOnKeychainFailure(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{failStore: true}
	_, output, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("setup-token-value"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "import", "personal", "--oauth-token-stdin")
	if err == nil || !strings.Contains(err.Error(), "storing Claude account access token") {
		t.Fatalf("claude-account import error = %v", err)
	}
	if strings.Contains(output+err.Error(), "setup-token-value") {
		t.Fatal("keychain failure leaked the OAuth token")
	}
	assertNoClaudeAccounts(t, dbPath)
}

func TestClaudeAccountImportUsesAccountSpecificKeychainGuidance(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{failAvailable: true}
	_, _, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("setup-token-value"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "import", "personal", "--oauth-token-stdin")
	if err == nil {
		t.Fatal("claude-account import unexpectedly succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "requires a working OS keychain") ||
		!strings.Contains(message, "configure or unlock it and retry") {
		t.Fatalf("claude-account import guidance = %q", message)
	}
	if strings.Contains(message, "--api-key-env") || strings.Contains(message, "--api-key-file") {
		t.Fatalf("claude-account import suggested unsupported provider flags: %q", message)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database was created before keychain availability validation: %v", statErr)
	}
}

func TestClaudeAccountRefreshRestoresExistingSecretOnStoreFailure(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	secrets := &accountTestSecrets{values: map[string]string{ref: "old-token"}}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
	})
	secrets.failStore = true
	_, _, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("new-token"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "refresh", "personal", "--oauth-token-stdin")
	if err == nil {
		t.Fatal("claude-account refresh unexpectedly succeeded")
	}
	if secrets.values[ref] != "old-token" {
		t.Fatal("failed refresh did not preserve the existing credential")
	}
}

func TestClaudeAccountRefreshReplacesCredentialAndClearsCooldown(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	secrets := &accountTestSecrets{values: map[string]string{ref: "old-token"}}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: false,
		CooldownUntil: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		LastError:     "rate_limited",
	})
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("new-token"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "refresh", "personal", "--oauth-token-stdin")
	if err != nil {
		t.Fatalf("claude-account refresh error = %v, stderr=%q", err, errOut)
	}
	account := getAccountForCLI(t, dbPath, "personal")
	if secrets.values[ref] != "new-token" || account.Enabled ||
		account.CooldownUntil != "" || account.LastError != "" || account.LastRefreshAt == "" {
		t.Fatalf("refreshed account metadata = %#v", account)
	}
	if strings.Contains(out+errOut, "new-token") {
		t.Fatal("refresh output leaked the replacement credential")
	}
}

func TestClaudeAccountRefreshRepairsMissingCredential(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("repairable")
	secrets := &accountTestSecrets{values: map[string]string{}}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "repairable", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
		LastError: "credential_unavailable",
	})

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("replacement-token"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "refresh", "repairable", "--oauth-token-stdin")
	if err != nil {
		t.Fatalf("claude-account refresh error = %v, stderr=%q", err, errOut)
	}
	if secrets.values[ref] != "replacement-token" {
		t.Fatal("refresh did not repair the missing keychain credential")
	}
	account := getAccountForCLI(t, dbPath, "repairable")
	if account.LastError != "" || account.LastRefreshAt == "" {
		t.Fatalf("repaired account metadata = %#v", account)
	}
	if strings.Contains(out+errOut, "replacement-token") {
		t.Fatal("refresh output leaked the replacement credential")
	}
}

func TestClaudeAccountSecretRollbackRemovesNewRefreshToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	accessRef := secret.ClaudeAccountAccessTokenRef("personal")
	refreshRef := secret.ClaudeAccountRefreshTokenRef("personal")
	secrets := &accountTestSecrets{values: map[string]string{accessRef: "old-token"}}
	existing := store.ClaudeAccount{Name: "personal", AccessTokenRef: accessRef}
	replacement := store.ClaudeAccount{
		Name: "personal", AccessTokenRef: accessRef, RefreshTokenRef: refreshRef,
	}
	rollback, err := storeClaudeAccountSecrets(
		ctx,
		secrets,
		existing,
		replacement,
		claudeaccount.Credentials{AccessToken: "new-token", RefreshToken: "new-refresh"},
		true,
	)
	if err != nil {
		t.Fatalf("storeClaudeAccountSecrets() error = %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback() error = %v", err)
	}
	if secrets.values[accessRef] != "old-token" {
		t.Fatal("rollback did not restore the prior access token")
	}
	if _, exists := secrets.values[refreshRef]; exists {
		t.Fatal("rollback left a newly introduced refresh token in the keychain")
	}
}

func TestClaudeAccountRefreshDeleteFailurePreservesMetadataAndSecrets(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	accessRef := secret.ClaudeAccountAccessTokenRef("personal")
	refreshRef := secret.ClaudeAccountRefreshTokenRef("personal")
	secrets := &accountTestSecrets{
		values:     map[string]string{accessRef: "old-token", refreshRef: "old-refresh"},
		failDelete: true,
	}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: accessRef, RefreshTokenRef: refreshRef,
		ScopesJSON: "[]", Enabled: true,
	})
	_, _, err := runCommandWithDeps(t, Dependencies{
		In: strings.NewReader("new-token"), Secrets: secrets,
	}, "--db", dbPath, "claude-account", "refresh", "personal", "--oauth-token-stdin")
	if err == nil || !strings.Contains(err.Error(), "removing obsolete") {
		t.Fatalf("claude-account refresh error = %v", err)
	}
	account := getAccountForCLI(t, dbPath, "personal")
	if account.RefreshTokenRef != refreshRef ||
		secrets.values[accessRef] != "old-token" ||
		secrets.values[refreshRef] != "old-refresh" {
		t.Fatalf("failed refresh orphaned credential state: account=%#v", account)
	}
}

func TestClaudeAccountListShowTestAndRemoveAreRedacted(t *testing.T) {
	t.Parallel()

	const token = "account-token-value"
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	secrets := &accountTestSecrets{values: map[string]string{ref: token}}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
	})

	for _, args := range [][]string{
		{"--db", dbPath, "claude-account", "list"},
		{"--db", dbPath, "claude-account", "show", "personal"},
		{"--db", dbPath, "claude-account", "test", "personal"},
	} {
		out, errOut, err := runCommandWithDeps(t, Dependencies{Secrets: secrets}, args...)
		if err != nil {
			t.Fatalf("%v error = %v", args, err)
		}
		if strings.Contains(out+errOut, token) || strings.Contains(out+errOut, ref) {
			t.Fatalf("%v leaked account credentials: %q %q", args, out, errOut)
		}
	}
	out, _, err := runCommandWithDeps(t, Dependencies{Secrets: secrets},
		"--db", dbPath, "claude-account", "remove", "personal", "--yes")
	if err != nil {
		t.Fatalf("claude-account remove error = %v", err)
	}
	if !strings.Contains(out, "removed") || len(secrets.deleted) != 1 {
		t.Fatalf("remove output=%q deleted=%v", out, secrets.deleted)
	}
	assertNoClaudeAccounts(t, dbPath)
}

func TestClaudeAccountListAndShowEmitVersionedRedactedJSON(t *testing.T) {
	t.Parallel()

	const token = "account-json-token-value"
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	accessRef := secret.ClaudeAccountAccessTokenRef("personal")
	refreshRef := secret.ClaudeAccountRefreshTokenRef("personal")
	secrets := &accountTestSecrets{values: map[string]string{
		accessRef: token, refreshRef: "account-json-refresh-value",
	}}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: accessRef, RefreshTokenRef: refreshRef,
		ScopesJSON: "[]", Enabled: true,
	})

	listOut, listErrOut, err := runCommandWithDeps(
		t, Dependencies{Secrets: secrets},
		"--db", dbPath, "claude-account", "list", "--json",
	)
	if err != nil {
		t.Fatalf("claude-account list --json error = %v, stderr=%q", err, listErrOut)
	}
	var listDocument struct {
		SchemaVersion int                 `json:"schema_version"`
		Accounts      []claudeAccountView `json:"accounts"`
	}
	if decodeErr := json.Unmarshal([]byte(listOut), &listDocument); decodeErr != nil {
		t.Fatalf("decoding claude-account list JSON: %v\n%s", decodeErr, listOut)
	}
	if listDocument.SchemaVersion != 1 || len(listDocument.Accounts) != 1 {
		t.Fatalf("claude-account list JSON = %#v", listDocument)
	}
	assertRedactedClaudeAccountJSON(t, listOut+listErrOut, listDocument.Accounts[0], token, accessRef, refreshRef)

	showOut, showErrOut, err := runCommandWithDeps(
		t, Dependencies{Secrets: secrets},
		"--db", dbPath, "claude-account", "show", "personal", "--json",
	)
	if err != nil {
		t.Fatalf("claude-account show --json error = %v, stderr=%q", err, showErrOut)
	}
	var showDocument struct {
		SchemaVersion int               `json:"schema_version"`
		Account       claudeAccountView `json:"account"`
	}
	if decodeErr := json.Unmarshal([]byte(showOut), &showDocument); decodeErr != nil {
		t.Fatalf("decoding claude-account show JSON: %v\n%s", decodeErr, showOut)
	}
	if showDocument.SchemaVersion != 1 {
		t.Fatalf("claude-account show schema version = %d", showDocument.SchemaVersion)
	}
	assertRedactedClaudeAccountJSON(t, showOut+showErrOut, showDocument.Account, token, accessRef, refreshRef)
}

func assertRedactedClaudeAccountJSON(
	t *testing.T,
	output string,
	account claudeAccountView,
	token, accessRef, refreshRef string,
) {
	t.Helper()
	if account.Name != "personal" ||
		account.AccessTokenRef != "keyring:***" ||
		account.RefreshTokenRef != "keyring:***" {
		t.Fatalf("redacted Claude account JSON view = %#v", account)
	}
	if strings.Contains(output, token) ||
		strings.Contains(output, accessRef) ||
		strings.Contains(output, refreshRef) {
		t.Fatalf("Claude account JSON leaked credential material: %s", output)
	}
}

func TestClaudeAccountRemoveDeleteFailureRetainsMetadata(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	secrets := &accountTestSecrets{
		values: map[string]string{ref: "account-token-value"}, failDelete: true,
	}
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
	})
	out, errOut, err := runCommandWithDeps(t, Dependencies{Secrets: secrets},
		"--db", dbPath, "claude-account", "remove", "personal", "--yes")
	if err == nil || !strings.Contains(err.Error(), "metadata was retained") {
		t.Fatalf("claude-account remove error = %v", err)
	}
	if strings.Contains(out+errOut+err.Error(), "account-token-value") {
		t.Fatal("keychain delete failure leaked the account credential")
	}
	account := getAccountForCLI(t, dbPath, "personal")
	if account.AccessTokenRef != ref || secrets.values[ref] != "account-token-value" {
		t.Fatalf("remove failure lost retry metadata: account=%#v", account)
	}
}

func addAccountForCLI(t *testing.T, dbPath string, account store.ClaudeAccount) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	if _, err := s.AddClaudeAccount(ctx, account); err != nil {
		t.Fatalf("AddClaudeAccount error = %v", err)
	}
}

func getAccountForCLI(t *testing.T, dbPath, name string) store.ClaudeAccount {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	account, err := s.GetClaudeAccount(ctx, name)
	if err != nil {
		t.Fatalf("GetClaudeAccount(%s) error = %v", name, err)
	}
	return account
}

func assertNoClaudeAccounts(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open error = %v", err)
	}
	defer closeStore(s)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("store.Migrate error = %v", migrateErr)
	}
	accounts, err := s.ListClaudeAccounts(ctx)
	if err != nil {
		t.Fatalf("ListClaudeAccounts error = %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts persisted unexpectedly: %#v", accounts)
	}
}
