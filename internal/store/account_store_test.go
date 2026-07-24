package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaudeAccountCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	account := testClaudeAccount("work", testAccountTime(24*time.Hour))
	id, err := s.AddClaudeAccount(ctx, account)
	if err != nil {
		t.Fatalf("AddClaudeAccount() error = %v", err)
	}
	got, err := s.GetClaudeAccount(ctx, "work")
	if err != nil {
		t.Fatalf("GetClaudeAccount() error = %v", err)
	}
	if got.ID != id || got.AccessTokenRef != account.AccessTokenRef ||
		got.RefreshTokenRef != account.RefreshTokenRef || got.ScopesJSON != `["profile","org"]` ||
		!got.Enabled || got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("GetClaudeAccount() = %#v", got)
	}

	got.ExpiresAt = testAccountTime(48 * time.Hour).Format(time.RFC3339Nano)
	got.ScopesJSON = `["profile"]`
	got.Enabled = false
	got.LastRefreshAt = testAccountTime(time.Hour).Format(time.RFC3339Nano)
	if updateErr := s.UpdateClaudeAccount(ctx, got); updateErr != nil {
		t.Fatalf("UpdateClaudeAccount() error = %v", updateErr)
	}
	updated, err := s.GetClaudeAccount(ctx, "work")
	if err != nil {
		t.Fatalf("GetClaudeAccount(updated) error = %v", err)
	}
	if updated.AccessTokenRef != account.AccessTokenRef || updated.ScopesJSON != `["profile"]` ||
		updated.Enabled || updated.LastRefreshAt == "" {
		t.Fatalf("updated account = %#v", updated)
	}

	if enableErr := s.EnableClaudeAccount(ctx, "work"); enableErr != nil {
		t.Fatalf("EnableClaudeAccount() error = %v", enableErr)
	}
	if disableErr := s.SetClaudeAccountEnabled(ctx, "work", false); disableErr != nil {
		t.Fatalf("SetClaudeAccountEnabled(false) error = %v", disableErr)
	}
	disabled, err := s.GetClaudeAccount(ctx, "work")
	if err != nil {
		t.Fatalf("GetClaudeAccount(disabled) error = %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("SetClaudeAccountEnabled(false) left account enabled: %#v", disabled)
	}
	if enableErr := s.EnableClaudeAccount(ctx, "work"); enableErr != nil {
		t.Fatalf("EnableClaudeAccount(second) error = %v", enableErr)
	}

	accounts, err := s.ListClaudeAccounts(ctx)
	if err != nil {
		t.Fatalf("ListClaudeAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].Name != "work" {
		t.Fatalf("ListClaudeAccounts() = %#v", accounts)
	}
	if err := s.RemoveClaudeAccount(ctx, "work"); err != nil {
		t.Fatalf("RemoveClaudeAccount() error = %v", err)
	}
	if _, err := s.GetClaudeAccount(ctx, "work"); !errors.Is(err, ErrClaudeAccountNotFound) ||
		!errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetClaudeAccount() error = %v, want ErrClaudeAccountNotFound wrapping sql.ErrNoRows", err)
	}
}

func TestClaudeAccountFailureState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	if _, err := s.AddClaudeAccount(ctx, testClaudeAccount("work", testAccountTime(24*time.Hour))); err != nil {
		t.Fatalf("AddClaudeAccount() error = %v", err)
	}
	if err := s.MarkClaudeAccountFailure(ctx, "work", testAccountTime(10*time.Minute), "rate_limited"); err != nil {
		t.Fatalf("MarkClaudeAccountFailure() error = %v", err)
	}
	failed, err := s.GetClaudeAccount(ctx, "work")
	if err != nil {
		t.Fatalf("GetClaudeAccount(failed) error = %v", err)
	}
	if !failed.Enabled || failed.CooldownUntil == "" || failed.LastError != "rate_limited" {
		t.Fatalf("failed account = %#v", failed)
	}
	if clearErr := s.ClearClaudeAccountFailure(ctx, "work"); clearErr != nil {
		t.Fatalf("ClearClaudeAccountFailure() error = %v", clearErr)
	}
	cleared, err := s.GetClaudeAccount(ctx, "work")
	if err != nil {
		t.Fatalf("GetClaudeAccount(cleared) error = %v", err)
	}
	if cleared.CooldownUntil != "" || cleared.LastError != "" {
		t.Fatalf("cleared account = %#v", cleared)
	}
}

func TestClaudeAccountNormalizesNullScopesToArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	account := testClaudeAccount("work", testAccountTime(time.Hour))
	account.ScopesJSON = "null"
	if _, err := s.AddClaudeAccount(ctx, account); err != nil {
		t.Fatalf("AddClaudeAccount() error = %v", err)
	}
	stored, err := s.GetClaudeAccount(ctx, account.Name)
	if err != nil {
		t.Fatalf("GetClaudeAccount() error = %v", err)
	}
	if stored.ScopesJSON != "[]" {
		t.Fatalf("ScopesJSON = %q, want []", stored.ScopesJSON)
	}
}

func TestClaudeAccountValidationRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	tests := []struct {
		name    string
		account ClaudeAccount
		want    string
	}{
		{
			name:    "name slash",
			account: testClaudeAccount("bad/name", testAccountTime(time.Hour)),
			want:    "must not contain",
		},
		{
			name:    "raw access token",
			account: accountWithAccessRef("sk-ant-access-secret"),
			want:    "must use the OS keychain",
		},
		{
			name:    "environment access ref",
			account: accountWithAccessRef("env:CLAUDE_ACCESS"),
			want:    "must use the OS keychain",
		},
		{
			name:    "file access ref",
			account: accountWithAccessRef("file:/tmp/claude-token"),
			want:    "must use the OS keychain",
		},
		{
			name:    "provider keyring ref",
			account: accountWithAccessRef("keyring:provider/work/api-key"),
			want:    "same Claude account",
		},
		{
			name:    "other account keyring ref",
			account: accountWithAccessRef("keyring:claude-account/other/access-token"),
			want:    "same Claude account",
		},
		{
			name: "same refs",
			account: ClaudeAccount{
				Name:            "same",
				AccessTokenRef:  "keyring:claude-account/same/access-token",
				RefreshTokenRef: "keyring:claude-account/same/access-token",
				ExpiresAt:       testAccountTime(time.Hour).Format(time.RFC3339Nano),
				Enabled:         true,
			},
			want: "refresh token ref",
		},
		{
			name: "bad scopes",
			account: ClaudeAccount{
				Name:            "work",
				AccessTokenRef:  "keyring:claude-account/work/access-token",
				RefreshTokenRef: "keyring:claude-account/work/refresh-token",
				ExpiresAt:       testAccountTime(time.Hour).Format(time.RFC3339Nano),
				ScopesJSON:      `{"scope":"profile"}`,
				Enabled:         true,
			},
			want: "scopes_json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.AddClaudeAccount(ctx, test.account)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("AddClaudeAccount() error = %v, want containing %q", err, test.want)
			}
			if strings.Contains(err.Error(), "sk-ant-access-secret") {
				t.Fatalf("AddClaudeAccount() leaked raw token in error: %v", err)
			}
		})
	}
}

func TestClaudeAccountFailureRejectsRawErrorDetails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	account := testClaudeAccount("personal", testAccountTime(time.Hour))
	if _, err := s.AddClaudeAccount(ctx, account); err != nil {
		t.Fatalf("AddClaudeAccount() error = %v", err)
	}
	err := s.MarkClaudeAccountFailure(
		ctx,
		account.Name,
		time.Now().Add(time.Minute),
		"rate limited: bearer secret-value",
	)
	if err == nil || !strings.Contains(err.Error(), "lowercase error class") {
		t.Fatalf("MarkClaudeAccountFailure() error = %v", err)
	}
	stored, err := s.GetClaudeAccount(ctx, account.Name)
	if err != nil {
		t.Fatalf("GetClaudeAccount() error = %v", err)
	}
	if stored.LastError != "" {
		t.Fatalf("LastError = %q, want empty", stored.LastError)
	}
}

func TestClaudeAccountStoresRefsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	rawAccess := "sk-ant-access-secret"
	rawRefresh := "refresh-token-secret"
	_, err := s.AddClaudeAccount(ctx, ClaudeAccount{
		Name:            "raw",
		AccessTokenRef:  rawAccess,
		RefreshTokenRef: rawRefresh,
		ExpiresAt:       testAccountTime(time.Hour).Format(time.RFC3339Nano),
		Enabled:         true,
	})
	if err == nil {
		t.Fatal("AddClaudeAccount() accepted raw token values")
	}
	if strings.Contains(err.Error(), rawAccess) || strings.Contains(err.Error(), rawRefresh) {
		t.Fatalf("AddClaudeAccount() leaked raw token in error: %v", err)
	}
	if _, err := s.AddClaudeAccount(ctx, testClaudeAccount("safe", testAccountTime(time.Hour))); err != nil {
		t.Fatalf("AddClaudeAccount(valid) error = %v", err)
	}
	var accessRef, refreshRef string
	if err := s.db.QueryRowContext(ctx, `
SELECT access_token_ref, refresh_token_ref
FROM claude_accounts
WHERE name = 'safe'
`).Scan(&accessRef, &refreshRef); err != nil {
		t.Fatalf("reading raw stored refs: %v", err)
	}
	if !strings.HasPrefix(accessRef, "keyring:") || !strings.HasPrefix(refreshRef, "keyring:") {
		t.Fatalf("stored account credentials are not refs: access=%q refresh=%q", accessRef, refreshRef)
	}
	if strings.Contains(accessRef+refreshRef, rawAccess) || strings.Contains(accessRef+refreshRef, rawRefresh) {
		t.Fatalf("stored account refs contain raw token material: %q %q", accessRef, refreshRef)
	}
}

func TestClaudeAccountAcceptsAccessTokenOnlyUnknownExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)

	account := ClaudeAccount{
		Name:           "setup-token",
		AccessTokenRef: "keyring:claude-account/setup-token/access-token",
		Enabled:        true,
	}
	if _, err := s.AddClaudeAccount(ctx, account); err != nil {
		t.Fatalf("AddClaudeAccount(access-only) error = %v", err)
	}
	stored, err := s.GetClaudeAccount(ctx, account.Name)
	if err != nil {
		t.Fatalf("GetClaudeAccount(access-only) error = %v", err)
	}
	if stored.RefreshTokenRef != "" || stored.ExpiresAt != "" || stored.ScopesJSON != "[]" {
		t.Fatalf("stored access-only account = %#v", stored)
	}
	claimed, ok, err := s.ClaimClaudeAccount(ctx, testAccountTime(365*24*time.Hour), nil)
	if err != nil {
		t.Fatalf("ClaimClaudeAccount(access-only) error = %v", err)
	}
	if !ok || claimed.Name != account.Name {
		t.Fatalf("ClaimClaudeAccount(access-only) = %#v, %v", claimed, ok)
	}
	claimed, ok, err = s.ClaimClaudeAccountByName(ctx, account.Name, testAccountTime(365*24*time.Hour+time.Second))
	if err != nil {
		t.Fatalf("ClaimClaudeAccountByName(access-only) error = %v", err)
	}
	if !ok || claimed.Name != account.Name || claimed.LastUsedAt == "" {
		t.Fatalf("ClaimClaudeAccountByName(access-only) = %#v, %v", claimed, ok)
	}
}

func TestClaimClaudeAccountUsesDeterministicLRUAndExclusions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	now := testAccountTime(0)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.AddClaudeAccount(ctx, testClaudeAccount(name, now.Add(24*time.Hour))); err != nil {
			t.Fatalf("AddClaudeAccount(%s) error = %v", name, err)
		}
	}

	assertClaimedAccount(t, ctx, s, now, nil, "alpha")
	assertClaimedAccount(t, ctx, s, now.Add(time.Second), []string{"alpha"}, "beta")
	assertClaimedAccount(t, ctx, s, now.Add(2*time.Second), nil, "gamma")
	assertClaimedAccount(t, ctx, s, now.Add(3*time.Second), nil, "alpha")
	_, ok, err := s.ClaimClaudeAccount(ctx, now.Add(4*time.Second), []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatalf("ClaimClaudeAccount(excluded all) error = %v", err)
	}
	if ok {
		t.Fatal("ClaimClaudeAccount(excluded all) returned an account")
	}
}

func TestClaimClaudeAccountByNameAppliesEligibility(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	now := testAccountTime(0)
	fixtures := []ClaudeAccount{
		{
			Name:           "setup-token",
			AccessTokenRef: "keyring:claude-account/setup-token/access-token",
			Enabled:        true,
		},
		accountWithEnabled("disabled", now.Add(24*time.Hour), false),
		testClaudeAccount("expired", now.Add(-time.Second)),
		accountWithCooldown("cooling", now.Add(24*time.Hour), now.Add(time.Minute)),
	}
	for _, account := range fixtures {
		if _, err := s.AddClaudeAccount(ctx, account); err != nil {
			t.Fatalf("AddClaudeAccount(%s) error = %v", account.Name, err)
		}
	}

	claimed, ok, err := s.ClaimClaudeAccountByName(ctx, "setup-token", now)
	if err != nil {
		t.Fatalf("ClaimClaudeAccountByName(setup-token) error = %v", err)
	}
	if !ok || claimed.Name != "setup-token" || claimed.ExpiresAt != "" {
		t.Fatalf("ClaimClaudeAccountByName(setup-token) = %#v, %v", claimed, ok)
	}
	for _, name := range []string{"disabled", "expired", "cooling", "missing"} {
		account, ok, err := s.ClaimClaudeAccountByName(ctx, name, now)
		if err != nil {
			t.Fatalf("ClaimClaudeAccountByName(%s) error = %v", name, err)
		}
		if ok {
			t.Fatalf("ClaimClaudeAccountByName(%s) = %#v, want no claim", name, account)
		}
	}
}

func TestClaimClaudeAccountFiltersExpirationCooldownAndEnabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	now := testAccountTime(0)
	fixtures := []ClaudeAccount{
		accountWithEnabled("disabled", now.Add(24*time.Hour), false),
		testClaudeAccount("expired", now.Add(-time.Second)),
		accountWithCooldown("cooling", now.Add(24*time.Hour), now.Add(time.Minute)),
		accountWithCooldown("ready", now.Add(24*time.Hour), now.Add(-time.Second)),
	}
	for _, account := range fixtures {
		if _, err := s.AddClaudeAccount(ctx, account); err != nil {
			t.Fatalf("AddClaudeAccount(%s) error = %v", account.Name, err)
		}
	}
	assertClaimedAccount(t, ctx, s, now, nil, "ready")
	_, ok, err := s.ClaimClaudeAccount(ctx, now, []string{"ready"})
	if err != nil {
		t.Fatalf("ClaimClaudeAccount(no eligible) error = %v", err)
	}
	if ok {
		t.Fatal("ClaimClaudeAccount(no eligible) returned an account")
	}
}

func assertClaimedAccount(
	t *testing.T,
	ctx context.Context,
	s *Store,
	now time.Time,
	excluded []string,
	want string,
) {
	t.Helper()
	account, ok, err := s.ClaimClaudeAccount(ctx, now, excluded)
	if err != nil {
		t.Fatalf("ClaimClaudeAccount() error = %v", err)
	}
	if !ok || account.Name != want {
		t.Fatalf("ClaimClaudeAccount() = %#v, %v, want %q", account, ok, want)
	}
	if account.LastUsedAt != formatRuntimeTimestamp(now) {
		t.Fatalf("claimed LastUsedAt = %q, want %q", account.LastUsedAt, formatRuntimeTimestamp(now))
	}
}

func testClaudeAccount(name string, expiresAt time.Time) ClaudeAccount {
	return accountWithEnabled(name, expiresAt, true)
}

func accountWithEnabled(name string, expiresAt time.Time, enabled bool) ClaudeAccount {
	return ClaudeAccount{
		Name:            name,
		AccessTokenRef:  "keyring:claude-account/" + name + "/access-token",
		RefreshTokenRef: "keyring:claude-account/" + name + "/refresh-token",
		ExpiresAt:       expiresAt.Format(time.RFC3339Nano),
		ScopesJSON:      `["profile","org"]`,
		Enabled:         enabled,
	}
}

func accountWithAccessRef(accessRef string) ClaudeAccount {
	account := testClaudeAccount("work", testAccountTime(time.Hour))
	account.AccessTokenRef = accessRef
	return account
}

func accountWithCooldown(name string, expiresAt, cooldownUntil time.Time) ClaudeAccount {
	account := testClaudeAccount(name, expiresAt)
	account.CooldownUntil = cooldownUntil.Format(time.RFC3339Nano)
	return account
}

func testAccountTime(offset time.Duration) time.Time {
	return time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Add(offset)
}
