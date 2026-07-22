package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type ConfigurationImportResult struct {
	ProvidersAdded int
	ModelsAdded    int
}

type ConfigurationConflictError struct {
	Providers []string
	Models    []string
}

func (e *ConfigurationConflictError) Error() string {
	parts := make([]string, 0, 2)
	if len(e.Providers) > 0 {
		parts = append(parts, "providers="+strings.Join(e.Providers, ","))
	}
	if len(e.Models) > 0 {
		parts = append(parts, "models="+strings.Join(e.Models, ","))
	}
	return "configuration import conflicts with existing " + strings.Join(parts, " ")
}

func (s *Store) ImportConfiguration(ctx context.Context, providers []Provider, models []Model, dryRun bool) (ConfigurationImportResult, error) {
	if err := validateProviderSecretRefs(providers); err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: %w", err)
	}
	if dryRun {
		return s.PlanConfigurationImport(ctx, providers, models)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := planConfigurationImport(ctx, tx, providers, models)
	if err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: %w", err)
	}
	now := runtimeTimestamp()
	providerIDs := make(map[string]int64, len(providers))
	for index := range providers {
		provider := &providers[index]
		result, err := tx.ExecContext(ctx, `
INSERT INTO providers (
  name, type, base_url, secret_ref, protocol, supports_tools,
  supports_streaming, supports_thinking, supports_model_discovery,
  supports_count_tokens, supports_responses, mode, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, provider.Name, provider.Type, provider.BaseURL, provider.SecretRef,
			provider.Protocol, boolToInt(provider.SupportsTools),
			boolToInt(provider.SupportsStreaming), boolToInt(provider.SupportsThinking),
			boolToInt(provider.SupportsModelDiscovery), boolToInt(provider.SupportsCountTokens),
			boolToInt(provider.SupportsResponses), provider.Mode, now)
		if err != nil {
			return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: inserting provider %q: %w", provider.Name, err)
		}
		providerID, err := result.LastInsertId()
		if err != nil {
			return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: reading provider %q id: %w", provider.Name, err)
		}
		providerIDs[provider.Name] = providerID
	}
	for index := range models {
		model := &models[index]
		providerID, ok := providerIDs[model.ProviderName]
		if !ok {
			return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: model %q references provider %q outside the import", model.Alias, model.ProviderName)
		}
		discoveredJSON, overridesJSON, err := encodeModelCapabilities(*model)
		if err != nil {
			return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: encoding capabilities for model %q: %w", model.Alias, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO models (
  alias, provider_id, provider_model, status, discovered_capabilities,
  capability_overrides, capabilities_refreshed_at, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, model.Alias, providerID, model.ProviderModel, model.Status, discoveredJSON,
			overridesJSON, model.CapabilitiesRefreshedAt, now); err != nil {
			return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: inserting model %q: %w", model.Alias, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.ImportConfiguration: committing import: %w", err)
	}
	return result, nil
}

func (s *Store) PlanConfigurationImport(ctx context.Context, providers []Provider, models []Model) (ConfigurationImportResult, error) {
	if err := validateProviderSecretRefs(providers); err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.PlanConfigurationImport: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.PlanConfigurationImport: starting read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := planConfigurationImport(ctx, tx, providers, models)
	if err != nil {
		return ConfigurationImportResult{}, fmt.Errorf("store.PlanConfigurationImport: %w", err)
	}
	return result, nil
}

func validateProviderSecretRefs(providers []Provider) error {
	for index := range providers {
		provider := &providers[index]
		if err := validateSecretRef(provider.SecretRef); err != nil {
			return fmt.Errorf("provider %q: %w", provider.Name, err)
		}
	}
	return nil
}

func planConfigurationImport(ctx context.Context, tx *sql.Tx, providers []Provider, models []Model) (ConfigurationImportResult, error) {
	conflicts, err := configurationConflicts(ctx, tx, providers, models)
	if err != nil {
		return ConfigurationImportResult{}, err
	}
	if len(conflicts.Providers) > 0 || len(conflicts.Models) > 0 {
		return ConfigurationImportResult{}, conflicts
	}
	return ConfigurationImportResult{ProvidersAdded: len(providers), ModelsAdded: len(models)}, nil
}

func configurationConflicts(ctx context.Context, tx *sql.Tx, providers []Provider, models []Model) (*ConfigurationConflictError, error) {
	conflicts := &ConfigurationConflictError{}
	for index := range providers {
		provider := &providers[index]
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM providers WHERE name = ?`, provider.Name).Scan(&exists)
		if err == nil {
			conflicts.Providers = append(conflicts.Providers, provider.Name)
		} else if !isNoRows(err) {
			return nil, fmt.Errorf("checking provider %q: %w", provider.Name, err)
		}
	}
	for index := range models {
		model := &models[index]
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM models WHERE alias = ?`, model.Alias).Scan(&exists)
		if err == nil {
			conflicts.Models = append(conflicts.Models, model.Alias)
		} else if !isNoRows(err) {
			return nil, fmt.Errorf("checking model %q: %w", model.Alias, err)
		}
	}
	sort.Strings(conflicts.Providers)
	sort.Strings(conflicts.Models)
	return conflicts, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
