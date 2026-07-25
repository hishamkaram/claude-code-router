package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestClaudeAccountTestAllLiveReportsQuotaAndDuplicateIdentity(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{values: map[string]string{}}
	for _, name := range []string{"account-1", "account-2"} {
		ref := secret.ClaudeAccountAccessTokenRef(name)
		secrets.values[ref] = name + "-oauth-token"
		addAccountForCLI(t, dbPath, store.ClaudeAccount{
			Name: name, AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
		})
	}
	probe := func(_ context.Context, _ string) (claudeaccount.AccountDiagnostics, error) {
		return claudeaccount.AccountDiagnostics{
			IdentityFingerprint: "same-identity",
			Plan:                "max",
			Windows: []claudeaccount.QuotaWindow{
				{Kind: "five_hour", Utilization: 15, ResetsAt: time.Date(2026, 7, 25, 17, 0, 0, 0, time.UTC)},
				{Kind: "seven_day", Utilization: 100, ResetsAt: time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)},
			},
		}, nil
	}

	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets, ProbeClaudeAccount: probe,
	}, "--db", dbPath, "claude-account", "test", "--all", "--live")
	if err == nil || !strings.Contains(err.Error(), "duplicate Claude subscription credentials") {
		t.Fatalf("claude-account test --all --live error = %v", err)
	}
	for _, want := range []string{
		"account-1: checking local credential",
		"account-2: querying live identity and quota",
		"identity=same-identity",
		"five_hour=15.00%",
		"seven_day=100.00%",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("live account diagnostics missing %q: %q", want, out)
		}
	}
	if strings.Contains(out+errOut+err.Error(), "oauth-token") ||
		strings.Contains(out+errOut+err.Error(), "keyring:") {
		t.Fatalf("live account diagnostics leaked credentials: out=%q stderr=%q error=%v", out, errOut, err)
	}
}

func TestClaudeAccountTestLocalDoesNotCallLiveProbe(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("personal")
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "personal", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
	})
	called := false
	out, _, err := runCommandWithDeps(t, Dependencies{
		Secrets: &accountTestSecrets{values: map[string]string{ref: "personal-oauth-token"}},
		ProbeClaudeAccount: func(context.Context, string) (claudeaccount.AccountDiagnostics, error) {
			called = true
			return claudeaccount.AccountDiagnostics{}, nil
		},
	}, "--db", dbPath, "claude-account", "test", "personal")
	if err != nil {
		t.Fatalf("local account test error = %v", err)
	}
	if called {
		t.Fatal("local account test called live probe")
	}
	if !strings.Contains(out, "no network request") {
		t.Fatalf("local account test output = %q", out)
	}
}

func TestClaudeAccountTestValidatesAllTargetSelection(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"claude-account", "test"},
		{"claude-account", "test", "personal", "--all"},
	} {
		_, _, err := runCommandWithDeps(t, Dependencies{}, args...)
		if err == nil {
			t.Fatalf("%v error = nil", args)
		}
	}
}

func TestClaudeAccountTestAllLocalContinuesAfterCredentialFailure(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	secrets := &accountTestSecrets{values: map[string]string{}}
	for _, name := range []string{"account-1", "account-2", "account-3"} {
		ref := secret.ClaudeAccountAccessTokenRef(name)
		if name != "account-2" {
			secrets.values[ref] = name + "-private-token"
		}
		addAccountForCLI(t, dbPath, store.ClaudeAccount{
			Name: name, AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
		})
	}
	probeCalled := false
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: secrets,
		ProbeClaudeAccount: func(context.Context, string) (claudeaccount.AccountDiagnostics, error) {
			probeCalled = true
			return claudeaccount.AccountDiagnostics{}, nil
		},
	}, "--db", dbPath, "claude-account", "test", "--all")
	if err == nil || !strings.Contains(err.Error(), `account-2`) {
		t.Fatalf("claude-account test --all error = %v", err)
	}
	if probeCalled {
		t.Fatal("local --all account test called live probe")
	}
	for _, want := range []string{
		"account-1: passed",
		"account-2: checking local credential",
		"account-3: passed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("local --all output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out+errOut+err.Error(), "private-token") ||
		strings.Contains(out+errOut+err.Error(), "keyring:") {
		t.Fatalf("local --all account test leaked credentials: out=%q stderr=%q error=%v", out, errOut, err)
	}
}

func TestClaudeAccountTestHelpDocumentsTargetsAndModes(t *testing.T) {
	t.Parallel()
	out, _, err := runCommandWithDeps(t, Dependencies{}, "claude-account", "test", "--help")
	if err != nil {
		t.Fatalf("claude-account test --help error = %v", err)
	}
	for _, want := range []string{
		"Usage:",
		"ccr claude-account test [name]",
		"--all",
		"--live",
		"identity and quota",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("claude-account test --help missing %q: %q", want, out)
		}
	}
}
