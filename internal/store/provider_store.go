package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) AddProvider(ctx context.Context, provider Provider) error {
	if err := validateSecretRef(provider.SecretRef); err != nil {
		return fmt.Errorf("store.AddProvider: provider %q: %w", provider.Name, err)
	}
	provider = providerWithMetadataDefaults(provider)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO providers (
  name, type, base_url, secret_ref, protocol, supports_tools, supports_streaming,
  supports_thinking, supports_model_discovery, supports_count_tokens, supports_responses, mode, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, provider.Name, provider.Type, provider.BaseURL, provider.SecretRef, provider.Protocol, boolToInt(provider.SupportsTools),
		boolToInt(provider.SupportsStreaming), boolToInt(provider.SupportsThinking), boolToInt(provider.SupportsModelDiscovery),
		boolToInt(provider.SupportsCountTokens), boolToInt(provider.SupportsResponses), provider.Mode, now)
	if err != nil {
		return fmt.Errorf("store.AddProvider: inserting provider %q: %w", provider.Name, err)
	}
	return nil
}

func (s *Store) UpdateProvider(ctx context.Context, provider Provider) (ProviderUpdateResult, error) {
	if err := validateSecretRef(provider.SecretRef); err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: provider %q: %w", provider.Name, err)
	}
	provider = providerWithMetadataDefaults(provider)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: starting transaction for provider %q: %w", provider.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing Provider
	err = tx.QueryRowContext(ctx, `
SELECT id, type, base_url, secret_ref, protocol
FROM providers
WHERE name = ?
`, provider.Name).Scan(&existing.ID, &existing.Type, &existing.BaseURL, &existing.SecretRef, &existing.Protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: provider %q does not exist", provider.Name)
	}
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading provider %q before update: %w", provider.Name, err)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE providers
SET type = ?, base_url = ?, secret_ref = ?, protocol = ?, supports_tools = ?,
  supports_streaming = ?, supports_thinking = ?, supports_model_discovery = ?,
  supports_count_tokens = ?, supports_responses = ?, mode = ?
WHERE name = ?
`, provider.Type, provider.BaseURL, provider.SecretRef, provider.Protocol, boolToInt(provider.SupportsTools),
		boolToInt(provider.SupportsStreaming), boolToInt(provider.SupportsThinking), boolToInt(provider.SupportsModelDiscovery),
		boolToInt(provider.SupportsCountTokens), boolToInt(provider.SupportsResponses), provider.Mode, provider.Name)
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: updating provider %q: %w", provider.Name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading rows affected for provider %q: %w", provider.Name, err)
	}
	if affected == 0 {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: provider %q does not exist", provider.Name)
	}

	updateResult := ProviderUpdateResult{}
	if providerCapabilitySourceChanged(existing, provider) {
		invalidated, invalidateErr := tx.ExecContext(ctx, `
UPDATE models
SET discovered_capabilities = '{}', capabilities_refreshed_at = ''
WHERE provider_id = ?
  AND (capabilities_refreshed_at <> '' OR discovered_capabilities NOT IN ('{}', '{"values":{}}'))
`, existing.ID)
		if invalidateErr != nil {
			return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: invalidating model capabilities for provider %q: %w", provider.Name, invalidateErr)
		}
		updateResult.CapabilitySnapshotsInvalidated, err = invalidated.RowsAffected()
		if err != nil {
			return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: reading invalidated model count for provider %q: %w", provider.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ProviderUpdateResult{}, fmt.Errorf("store.UpdateProvider: committing provider %q update: %w", provider.Name, err)
	}
	return updateResult, nil
}

func providerCapabilitySourceChanged(existing, updated Provider) bool {
	return existing.Type != updated.Type || existing.BaseURL != updated.BaseURL ||
		existing.SecretRef != updated.SecretRef || existing.Protocol != updated.Protocol
}

func (s *Store) GetProvider(ctx context.Context, name string) (Provider, error) {
	var provider Provider
	var supportsTools, supportsStreaming, supportsThinking, supportsModelDiscovery, supportsCountTokens, supportsResponses int
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, supports_responses, mode, created_at
FROM providers
WHERE name = ?
`, name).Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef, &provider.Protocol,
		&supportsTools, &supportsStreaming, &supportsThinking, &supportsModelDiscovery, &supportsCountTokens, &supportsResponses,
		&provider.Mode, &provider.CreatedAt)
	if err != nil {
		return Provider{}, fmt.Errorf("store.GetProvider: reading provider %q: %w", name, err)
	}
	provider.SupportsTools = intToBool(supportsTools)
	provider.SupportsStreaming = intToBool(supportsStreaming)
	provider.SupportsThinking = intToBool(supportsThinking)
	provider.SupportsModelDiscovery = intToBool(supportsModelDiscovery)
	provider.SupportsCountTokens = intToBool(supportsCountTokens)
	provider.SupportsResponses = intToBool(supportsResponses)
	return provider, nil
}

func (s *Store) ProviderExists(ctx context.Context, name string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM providers
WHERE name = ?
`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.ProviderExists: reading provider %q: %w", name, err)
	}
	return true, nil
}

func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, supports_responses, mode, created_at
FROM providers
ORDER BY name
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListProviders: querying providers: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var providers []Provider
	for rows.Next() {
		var provider Provider
		var supportsTools, supportsStreaming, supportsThinking, supportsModelDiscovery, supportsCountTokens, supportsResponses int
		if err := rows.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.SecretRef,
			&provider.Protocol, &supportsTools, &supportsStreaming, &supportsThinking, &supportsModelDiscovery,
			&supportsCountTokens, &supportsResponses, &provider.Mode, &provider.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListProviders: scanning provider: %w", err)
		}
		provider.SupportsTools = intToBool(supportsTools)
		provider.SupportsStreaming = intToBool(supportsStreaming)
		provider.SupportsThinking = intToBool(supportsThinking)
		provider.SupportsModelDiscovery = intToBool(supportsModelDiscovery)
		provider.SupportsCountTokens = intToBool(supportsCountTokens)
		provider.SupportsResponses = intToBool(supportsResponses)
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListProviders: iterating providers: %w", err)
	}
	return providers, nil
}

func (s *Store) RemoveProvider(ctx context.Context, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: beginning transaction for provider %q: %w", name, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var providerID int64
	queryErr := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE name = ?`, name).Scan(&providerID)
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return 0, fmt.Errorf("store.RemoveProvider: provider %q does not exist", name)
		}
		return 0, fmt.Errorf("store.RemoveProvider: reading provider %q: %w", name, queryErr)
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM models WHERE provider_id = ?`, providerID)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: deleting models for provider %q: %w", name, err)
	}
	modelsRemoved, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: reading model rows affected for provider %q: %w", name, err)
	}

	result, err = tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, providerID)
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: deleting provider %q: %w", name, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: reading provider rows affected for %q: %w", name, err)
	}
	if removed != 1 {
		return 0, fmt.Errorf("store.RemoveProvider: provider %q does not exist", name)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.RemoveProvider: committing removal for provider %q: %w", name, err)
	}
	return modelsRemoved, nil
}
