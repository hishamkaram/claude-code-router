package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestClaudeAccountClearCooldownByName(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCoolingAccountForCLI(t, dbPath, "main")
	addCoolingAccountForCLI(t, dbPath, "personal")

	out, errOut, err := runCommandWithDeps(
		t,
		Dependencies{},
		"--db", dbPath, "claude-account", "clear-cooldown", "main",
	)
	if err != nil {
		t.Fatalf("claude-account clear-cooldown error = %v, stderr=%q", err, errOut)
	}
	if !strings.Contains(out, "Claude account main: cooldown cleared") {
		t.Fatalf("clear-cooldown output = %q", out)
	}
	main := getAccountForCLI(t, dbPath, "main")
	personal := getAccountForCLI(t, dbPath, "personal")
	if main.CooldownUntil != "" || main.LastError != "" {
		t.Fatalf("main cooldown was not cleared: %#v", main)
	}
	if personal.CooldownUntil == "" || personal.LastError != "rate_limited" {
		t.Fatalf("personal cooldown changed unexpectedly: %#v", personal)
	}
}

func TestClaudeAccountClearCooldownAll(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	for _, name := range []string{"main", "personal", "work"} {
		addCoolingAccountForCLI(t, dbPath, name)
	}

	out, errOut, err := runCommandWithDeps(
		t,
		Dependencies{},
		"--db", dbPath, "claude-account", "clear-cooldown", "--all",
	)
	if err != nil {
		t.Fatalf("claude-account clear-cooldown --all error = %v, stderr=%q", err, errOut)
	}
	if !strings.Contains(out, "Claude account cooldowns cleared: 3") {
		t.Fatalf("clear-cooldown --all output = %q", out)
	}
	for _, name := range []string{"main", "personal", "work"} {
		account := getAccountForCLI(t, dbPath, name)
		if account.CooldownUntil != "" || account.LastError != "" {
			t.Fatalf("%s cooldown was not cleared: %#v", name, account)
		}
		if !account.Enabled || account.AccessTokenRef != secret.ClaudeAccountAccessTokenRef(name) {
			t.Fatalf("%s account metadata changed unexpectedly: %#v", name, account)
		}
	}
}

func TestClaudeAccountClearCooldownValidatesBeforeOpeningStore(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"claude-account", "clear-cooldown"},
		{"claude-account", "clear-cooldown", "main", "personal"},
		{"claude-account", "clear-cooldown", "main", "--all"},
		{"claude-account", "clear-cooldown", "invalid name"},
	} {
		dbPath := filepath.Join(t.TempDir(), "ccr.db")
		fullArgs := append([]string{"--db", dbPath}, args...)
		_, _, err := runCommandWithDeps(t, Dependencies{}, fullArgs...)
		if err == nil {
			t.Fatalf("clear-cooldown args %v succeeded unexpectedly", args)
		}
		if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
			t.Fatalf("clear-cooldown args %v opened store before validation: %v", args, statErr)
		}
	}
}

func TestClaudeAccountClearCooldownRejectsMissingAccount(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	addCoolingAccountForCLI(t, dbPath, "main")

	_, _, err := runCommandWithDeps(
		t,
		Dependencies{},
		"--db", dbPath, "claude-account", "clear-cooldown", "missing",
	)
	if err == nil || !strings.Contains(err.Error(), "Claude account missing does not exist") {
		t.Fatalf("clear-cooldown missing account error = %v", err)
	}
}

func addCoolingAccountForCLI(t *testing.T, dbPath, name string) {
	t.Helper()
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name:            name,
		AccessTokenRef:  secret.ClaudeAccountAccessTokenRef(name),
		ScopesJSON:      "[]",
		Enabled:         true,
		CooldownUntil:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		LastError:       "rate_limited",
		LastRefreshAt:   time.Now().UTC().Format(time.RFC3339Nano),
		RefreshTokenRef: "",
	})
}
