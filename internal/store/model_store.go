package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) AddModel(ctx context.Context, model Model) error {
	discoveredJSON, overridesJSON, err := encodeModelCapabilities(model)
	if err != nil {
		return fmt.Errorf("store.AddModel: encoding capabilities for model %q: %w", model.Alias, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
INSERT INTO models (
  alias, provider_id, provider_model, status, discovered_capabilities,
  capability_overrides, capabilities_refreshed_at, created_at
)
SELECT ?, providers.id, ?, ?, ?, ?, ?, ?
FROM providers
WHERE providers.name = ?
`, model.Alias, model.ProviderModel, model.Status, discoveredJSON, overridesJSON,
		model.CapabilitiesRefreshedAt, now, model.ProviderName)
	if err != nil {
		return fmt.Errorf("store.AddModel: inserting model %q: %w", model.Alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.AddModel: reading rows affected for %q: %w", model.Alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.AddModel: provider %q does not exist", model.ProviderName)
	}
	return nil
}

func (s *Store) GetModel(ctx context.Context, alias string) (Model, error) {
	var model Model
	var discoveredJSON, overridesJSON string
	err := s.db.QueryRowContext(ctx, `
SELECT models.id, models.alias, providers.name, models.provider_model, models.status,
  models.discovered_capabilities, models.capability_overrides,
  models.capabilities_refreshed_at, models.created_at
FROM models
JOIN providers ON providers.id = models.provider_id
WHERE models.alias = ?
`, alias).Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status,
		&discoveredJSON, &overridesJSON, &model.CapabilitiesRefreshedAt, &model.CreatedAt)
	if err != nil {
		return Model{}, fmt.Errorf("store.GetModel: reading model %q: %w", alias, err)
	}
	if err := decodeModelCapabilities(&model, discoveredJSON, overridesJSON); err != nil {
		return Model{}, fmt.Errorf("store.GetModel: decoding capabilities for model %q: %w", alias, err)
	}
	return model, nil
}

func (s *Store) UpdateModel(ctx context.Context, model Model) error {
	discoveredJSON, overridesJSON, err := encodeModelCapabilities(model)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: encoding capabilities for model %q: %w", model.Alias, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: beginning transaction for model %q: %w", model.Alias, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var providerID int64
	queryErr := tx.QueryRowContext(ctx, `SELECT id FROM providers WHERE name = ?`, model.ProviderName).Scan(&providerID)
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return fmt.Errorf("store.UpdateModel: provider %q does not exist", model.ProviderName)
		}
		return fmt.Errorf("store.UpdateModel: reading provider %q: %w", model.ProviderName, queryErr)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE models
SET provider_id = ?, provider_model = ?, status = ?, discovered_capabilities = ?,
  capability_overrides = ?, capabilities_refreshed_at = ?
WHERE alias = ?
`, providerID, model.ProviderModel, model.Status, discoveredJSON, overridesJSON,
		model.CapabilitiesRefreshedAt, model.Alias)
	if err != nil {
		return fmt.Errorf("store.UpdateModel: updating model %q: %w", model.Alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdateModel: reading rows affected for model %q: %w", model.Alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.UpdateModel: model %q does not exist", model.Alias)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.UpdateModel: committing update for model %q: %w", model.Alias, err)
	}
	return nil
}

func (s *Store) RemoveModel(ctx context.Context, alias string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE alias = ?`, alias)
	if err != nil {
		return fmt.Errorf("store.RemoveModel: deleting model %q: %w", alias, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RemoveModel: reading rows affected for model %q: %w", alias, err)
	}
	if affected == 0 {
		return fmt.Errorf("store.RemoveModel: model %q does not exist", alias)
	}
	return nil
}

func (s *Store) ModelExists(ctx context.Context, alias string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT 1
FROM models
WHERE alias = ?
`, alias).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.ModelExists: reading model %q: %w", alias, err)
	}
	return true, nil
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT models.id, models.alias, providers.name, models.provider_model, models.status,
  models.discovered_capabilities, models.capability_overrides,
  models.capabilities_refreshed_at, models.created_at
FROM models
JOIN providers ON providers.id = models.provider_id
ORDER BY models.alias
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListModels: querying models: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var models []Model
	for rows.Next() {
		var model Model
		var discoveredJSON, overridesJSON string
		if err := rows.Scan(&model.ID, &model.Alias, &model.ProviderName, &model.ProviderModel, &model.Status,
			&discoveredJSON, &overridesJSON, &model.CapabilitiesRefreshedAt, &model.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListModels: scanning model: %w", err)
		}
		if err := decodeModelCapabilities(&model, discoveredJSON, overridesJSON); err != nil {
			return nil, fmt.Errorf("store.ListModels: decoding capabilities for model %q: %w", model.Alias, err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListModels: iterating models: %w", err)
	}
	return models, nil
}
