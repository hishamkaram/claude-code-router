package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

type ClaudeAccount struct {
	ID              int64
	Name            string
	AccessTokenRef  string
	RefreshTokenRef string
	ExpiresAt       string
	ScopesJSON      string
	Enabled         bool
	CooldownUntil   string
	CreatedAt       string
	UpdatedAt       string
	LastUsedAt      string
	LastRefreshAt   string
	LastError       string
}

var ErrClaudeAccountNotFound = fmt.Errorf("claude account not found: %w", sql.ErrNoRows)

func (s *Store) AddClaudeAccount(ctx context.Context, account ClaudeAccount) (int64, error) {
	account, err := normalizeClaudeAccount(account)
	if err != nil {
		return 0, fmt.Errorf("store.AddClaudeAccount: %w", err)
	}
	now := runtimeTimestamp()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO claude_accounts (
  name, access_token_ref, refresh_token_ref, expires_at, scopes_json, enabled,
  cooldown_until, created_at, updated_at, last_used_at, last_refresh_at, last_error
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, account.Name, account.AccessTokenRef, account.RefreshTokenRef, account.ExpiresAt,
		account.ScopesJSON, boolToInt(account.Enabled), account.CooldownUntil,
		now, now, account.LastUsedAt, account.LastRefreshAt, account.LastError)
	if err != nil {
		return 0, fmt.Errorf("store.AddClaudeAccount: inserting account %q: %w", account.Name, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.AddClaudeAccount: reading account id: %w", err)
	}
	return id, nil
}

func (s *Store) UpdateClaudeAccount(ctx context.Context, account ClaudeAccount) error {
	account, err := normalizeClaudeAccount(account)
	if err != nil {
		return fmt.Errorf("store.UpdateClaudeAccount: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE claude_accounts
SET access_token_ref = ?, refresh_token_ref = ?, expires_at = ?, scopes_json = ?,
  enabled = ?, cooldown_until = ?, updated_at = ?, last_refresh_at = ?, last_error = ?
WHERE name = ?
`, account.AccessTokenRef, account.RefreshTokenRef, account.ExpiresAt, account.ScopesJSON,
		boolToInt(account.Enabled), account.CooldownUntil, runtimeTimestamp(),
		account.LastRefreshAt, account.LastError, account.Name)
	if err != nil {
		return fmt.Errorf("store.UpdateClaudeAccount: updating account %q: %w", account.Name, err)
	}
	return requireAffected("store.UpdateClaudeAccount", "Claude account", account.Name, result)
}

func (s *Store) GetClaudeAccount(ctx context.Context, name string) (ClaudeAccount, error) {
	if err := validateClaudeAccountName(name); err != nil {
		return ClaudeAccount{}, fmt.Errorf("store.GetClaudeAccount: %w", err)
	}
	account, err := scanClaudeAccount(s.db.QueryRowContext(ctx, claudeAccountSelectSQL+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return ClaudeAccount{}, fmt.Errorf("store.GetClaudeAccount: account %q: %w", name, ErrClaudeAccountNotFound)
	}
	if err != nil {
		return ClaudeAccount{}, fmt.Errorf("store.GetClaudeAccount: reading account %q: %w", name, err)
	}
	return account, nil
}

func (s *Store) ListClaudeAccounts(ctx context.Context) ([]ClaudeAccount, error) {
	rows, err := s.db.QueryContext(ctx, claudeAccountSelectSQL+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store.ListClaudeAccounts: querying accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var accounts []ClaudeAccount
	for rows.Next() {
		account, scanErr := scanClaudeAccount(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store.ListClaudeAccounts: scanning account: %w", scanErr)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListClaudeAccounts: iterating accounts: %w", err)
	}
	return accounts, nil
}

func (s *Store) RemoveClaudeAccount(ctx context.Context, name string) error {
	if err := validateClaudeAccountName(name); err != nil {
		return fmt.Errorf("store.RemoveClaudeAccount: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM claude_accounts WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store.RemoveClaudeAccount: deleting account %q: %w", name, err)
	}
	return requireAffected("store.RemoveClaudeAccount", "Claude account", name, result)
}

func (s *Store) EnableClaudeAccount(ctx context.Context, name string) error {
	return s.SetClaudeAccountEnabled(ctx, name, true)
}

func (s *Store) DisableClaudeAccount(ctx context.Context, name string) error {
	return s.SetClaudeAccountEnabled(ctx, name, false)
}

func (s *Store) MarkClaudeAccountFailure(ctx context.Context, name string, cooldownUntil time.Time, lastError string) error {
	if err := validateClaudeAccountName(name); err != nil {
		return fmt.Errorf("store.MarkClaudeAccountFailure: %w", err)
	}
	lastError, err := normalizeClaudeAccountError(lastError)
	if err != nil {
		return fmt.Errorf("store.MarkClaudeAccountFailure: %w", err)
	}
	cooldown := ""
	if !cooldownUntil.IsZero() {
		cooldown = formatRuntimeTimestamp(cooldownUntil)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE claude_accounts
SET cooldown_until = ?, last_error = ?, updated_at = ?
WHERE name = ?
`, cooldown, strings.TrimSpace(lastError), runtimeTimestamp(), name)
	if err != nil {
		return fmt.Errorf("store.MarkClaudeAccountFailure: updating account %q: %w", name, err)
	}
	return requireAffected("store.MarkClaudeAccountFailure", "Claude account", name, result)
}

func (s *Store) ClearClaudeAccountFailure(ctx context.Context, name string) error {
	if err := validateClaudeAccountName(name); err != nil {
		return fmt.Errorf("store.ClearClaudeAccountFailure: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE claude_accounts
SET cooldown_until = '', last_error = '', updated_at = ?
WHERE name = ?
`, runtimeTimestamp(), name)
	if err != nil {
		return fmt.Errorf("store.ClearClaudeAccountFailure: updating account %q: %w", name, err)
	}
	return requireAffected("store.ClearClaudeAccountFailure", "Claude account", name, result)
}

func (s *Store) ClaimClaudeAccount(ctx context.Context, now time.Time, excluded []string) (ClaudeAccount, bool, error) {
	query, args, err := buildClaimClaudeAccountQuery(now, excluded)
	if err != nil {
		return ClaudeAccount{}, false, fmt.Errorf("store.ClaimClaudeAccount: %w", err)
	}
	account, err := scanClaudeAccount(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return ClaudeAccount{}, false, nil
	}
	if err != nil {
		return ClaudeAccount{}, false, fmt.Errorf("store.ClaimClaudeAccount: claiming account: %w", err)
	}
	return account, true, nil
}

func (s *Store) ClaimClaudeAccountByName(ctx context.Context, name string, now time.Time) (ClaudeAccount, bool, error) {
	if err := validateClaudeAccountName(name); err != nil {
		return ClaudeAccount{}, false, fmt.Errorf("store.ClaimClaudeAccountByName: %w", err)
	}
	claimTime := formatRuntimeTimestamp(now)
	account, err := scanClaudeAccount(s.db.QueryRowContext(ctx, `
UPDATE claude_accounts
SET last_used_at = ?, updated_at = ?
WHERE name = ?
  AND enabled = 1
  AND (expires_at = '' OR expires_at > ?)
  AND (cooldown_until = '' OR cooldown_until <= ?)
RETURNING id, name, access_token_ref, refresh_token_ref, expires_at, scopes_json,
  enabled, cooldown_until, created_at, updated_at, last_used_at, last_refresh_at,
  last_error
`, claimTime, claimTime, name, claimTime, claimTime))
	if errors.Is(err, sql.ErrNoRows) {
		return ClaudeAccount{}, false, nil
	}
	if err != nil {
		return ClaudeAccount{}, false, fmt.Errorf("store.ClaimClaudeAccountByName: claiming account %q: %w", name, err)
	}
	return account, true, nil
}

func (s *Store) SetClaudeAccountEnabled(ctx context.Context, name string, enabled bool) error {
	if err := validateClaudeAccountName(name); err != nil {
		return fmt.Errorf("store.SetClaudeAccountEnabled: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE claude_accounts
SET enabled = ?, updated_at = ?
WHERE name = ?
`, boolToInt(enabled), runtimeTimestamp(), name)
	if err != nil {
		return fmt.Errorf("store.SetClaudeAccountEnabled: updating account %q: %w", name, err)
	}
	return requireAffected("store.SetClaudeAccountEnabled", "Claude account", name, result)
}

func buildClaimClaudeAccountQuery(now time.Time, excluded []string) (query string, args []any, resultErr error) {
	claimTime := formatRuntimeTimestamp(now)
	args = []any{claimTime, claimTime, claimTime, claimTime}
	var builder strings.Builder
	builder.WriteString(`
UPDATE claude_accounts
SET last_used_at = ?, updated_at = ?
WHERE id = (
  SELECT id
  FROM claude_accounts
  WHERE enabled = 1
    AND (expires_at = '' OR expires_at > ?)
    AND (cooldown_until = '' OR cooldown_until <= ?)
`)
	seen := make(map[string]struct{}, len(excluded))
	var names []string
	for _, name := range excluded {
		if _, ok := seen[name]; ok {
			continue
		}
		if err := validateClaudeAccountName(name); err != nil {
			return "", nil, err
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) > 0 {
		builder.WriteString("    AND name NOT IN (")
		builder.WriteString(strings.TrimRight(strings.Repeat("?,", len(names)), ","))
		builder.WriteString(")\n")
		for _, name := range names {
			args = append(args, name)
		}
	}
	builder.WriteString(`  ORDER BY CASE WHEN last_used_at = '' THEN 0 ELSE 1 END, last_used_at, id
  LIMIT 1
)
RETURNING id, name, access_token_ref, refresh_token_ref, expires_at, scopes_json,
  enabled, cooldown_until, created_at, updated_at, last_used_at, last_refresh_at,
  last_error`)
	return builder.String(), args, nil
}

func normalizeClaudeAccount(account ClaudeAccount) (ClaudeAccount, error) {
	if err := validateClaudeAccountName(account.Name); err != nil {
		return ClaudeAccount{}, err
	}
	if err := validateClaudeAccountSecretRefs(account); err != nil {
		return ClaudeAccount{}, err
	}
	expiresAt, err := normalizeOptionalAccountTimestamp("expires_at", account.ExpiresAt)
	if err != nil {
		return ClaudeAccount{}, err
	}
	scopesJSON, err := normalizeScopesJSON(account.ScopesJSON)
	if err != nil {
		return ClaudeAccount{}, err
	}
	account.ExpiresAt = expiresAt
	account.ScopesJSON = scopesJSON
	if account.CooldownUntil, err = normalizeOptionalAccountTimestamp("cooldown_until", account.CooldownUntil); err != nil {
		return ClaudeAccount{}, err
	}
	if account.LastUsedAt, err = normalizeOptionalAccountTimestamp("last_used_at", account.LastUsedAt); err != nil {
		return ClaudeAccount{}, err
	}
	if account.LastRefreshAt, err = normalizeOptionalAccountTimestamp("last_refresh_at", account.LastRefreshAt); err != nil {
		return ClaudeAccount{}, err
	}
	account.LastError, err = normalizeClaudeAccountError(account.LastError)
	if err != nil {
		return ClaudeAccount{}, err
	}
	return account, nil
}

func validateClaudeAccountName(name string) error {
	if len(name) < 2 || len(name) > 64 || name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("invalid Claude account name %q: use 2-64 lowercase letters, digits, underscores, or hyphens, starting with a letter", name)
	}
	for _, character := range name[1:] {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '_' &&
			character != '-' {
			return fmt.Errorf("invalid Claude account name %q: must not contain whitespace, /, or other unsupported characters", name)
		}
	}
	return nil
}

func normalizeClaudeAccountError(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > 64 {
		return "", fmt.Errorf("last_error must not exceed 64 characters")
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && ((character >= '0' && character <= '9') || character == '_') {
			continue
		}
		return "", fmt.Errorf("last_error must be a lowercase error class")
	}
	return value, nil
}

func normalizeOptionalAccountTimestamp(field, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return normalizeAccountTimestamp(field, value)
}

func validateClaudeAccountSecretRefs(account ClaudeAccount) error {
	if account.AccessTokenRef != secret.ClaudeAccountAccessTokenRef(account.Name) {
		return fmt.Errorf("access token ref must use the OS keychain entry for the same Claude account")
	}
	if account.RefreshTokenRef != "" &&
		account.RefreshTokenRef != secret.ClaudeAccountRefreshTokenRef(account.Name) {
		return fmt.Errorf("refresh token ref must use the OS keychain entry for the same Claude account")
	}
	return nil
}

func normalizeAccountTimestamp(field, value string) (string, error) {
	normalized, err := normalizeRuntimeTimestamp(value)
	if err != nil {
		return "", fmt.Errorf("%s must be an RFC3339 timestamp: %w", field, err)
	}
	return normalized, nil
}

func normalizeScopesJSON(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "[]", nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(value), &scopes); err != nil {
		return "", fmt.Errorf("scopes_json must be a JSON array of strings: %w", err)
	}
	if scopes == nil {
		scopes = []string{}
	}
	for _, scope := range scopes {
		if strings.TrimSpace(scope) == "" {
			return "", fmt.Errorf("scopes_json must not contain blank scopes")
		}
	}
	normalized, err := json.Marshal(scopes)
	if err != nil {
		return "", fmt.Errorf("normalizing scopes_json: %w", err)
	}
	return string(normalized), nil
}

func scanClaudeAccount(row rowScanner) (ClaudeAccount, error) {
	var account ClaudeAccount
	var enabled int
	err := row.Scan(&account.ID, &account.Name, &account.AccessTokenRef,
		&account.RefreshTokenRef, &account.ExpiresAt, &account.ScopesJSON,
		&enabled, &account.CooldownUntil, &account.CreatedAt, &account.UpdatedAt,
		&account.LastUsedAt, &account.LastRefreshAt, &account.LastError)
	account.Enabled = intToBool(enabled)
	return account, err
}

const claudeAccountSelectSQL = `
SELECT id, name, access_token_ref, refresh_token_ref, expires_at, scopes_json,
  enabled, cooldown_until, created_at, updated_at, last_used_at, last_refresh_at,
  last_error
FROM claude_accounts`
