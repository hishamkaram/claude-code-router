package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateV7ToV8ClearsOnlyLegacyUnclassifiedCooldowns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.ExecContext(ctx, bootstrapSchemaSQL+migrateV6ToV7ClaudeAccountsSQL+`
UPDATE schema_version SET version = 7 WHERE id = 1;
`); err != nil {
		t.Fatalf("seeding v7 schema: %v", err)
	}

	cooldown := testAccountTime(24 * time.Hour)
	cooldownText := formatRuntimeTimestamp(cooldown)
	for _, account := range []ClaudeAccount{
		testClaudeAccount("legacy", cooldown),
		testClaudeAccount("subscription", cooldown),
		testClaudeAccount("transient", cooldown),
		testClaudeAccount("credential", cooldown),
	} {
		if _, err := s.AddClaudeAccount(ctx, account); err != nil {
			t.Fatalf("AddClaudeAccount(%q) error = %v", account.Name, err)
		}
	}
	failures := map[string]string{
		"legacy":       "rate_limited",
		"subscription": "subscription_limit_seven_day",
		"transient":    "transient_rate_limit",
		"credential":   "credential_unavailable",
	}
	for name, failure := range failures {
		if err := s.MarkClaudeAccountFailure(ctx, name, cooldown, failure); err != nil {
			t.Fatalf("MarkClaudeAccountFailure(%q) error = %v", name, err)
		}
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	assertClaudeAccountFailure(t, ctx, s, "legacy", "", "")
	assertClaudeAccountFailure(t, ctx, s, "subscription", cooldownText, failures["subscription"])
	assertClaudeAccountFailure(t, ctx, s, "transient", cooldownText, failures["transient"])
	assertClaudeAccountFailure(t, ctx, s, "credential", cooldownText, failures["credential"])

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	assertClaudeAccountFailure(t, ctx, s, "legacy", "", "")
}

func assertClaudeAccountFailure(
	t *testing.T,
	ctx context.Context,
	s *Store,
	name string,
	wantCooldown string,
	wantFailure string,
) {
	t.Helper()
	account, err := s.GetClaudeAccount(ctx, name)
	if err != nil {
		t.Fatalf("GetClaudeAccount(%q) error = %v", name, err)
	}
	if account.LastError != wantFailure {
		t.Fatalf("GetClaudeAccount(%q).LastError = %q, want %q", name, account.LastError, wantFailure)
	}
	if account.CooldownUntil != wantCooldown {
		t.Fatalf(
			"GetClaudeAccount(%q).CooldownUntil = %q, want %q",
			name,
			account.CooldownUntil,
			wantCooldown,
		)
	}
}
