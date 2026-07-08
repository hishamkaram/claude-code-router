package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateAndProviderModelRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	s, openErr := Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()

	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}

	if addProviderErr := s.AddProvider(ctx, Provider{Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api", SecretRef: "env:OPENROUTER_API_KEY"}); addProviderErr != nil {
		t.Fatalf("AddProvider() error = %v", addProviderErr)
	}
	if addModelErr := s.AddModel(ctx, Model{Alias: "qwen", ProviderName: "openrouter", ProviderModel: "qwen/qwen3-coder", Status: "degraded"}); addModelErr != nil {
		t.Fatalf("AddModel() error = %v", addModelErr)
	}

	providers, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders() error = %v", err)
	}
	if len(providers) != 1 || providers[0].SecretRef != "env:OPENROUTER_API_KEY" {
		t.Fatalf("ListProviders() = %#v", providers)
	}

	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0].Alias != "qwen" || models[0].ProviderName != "openrouter" {
		t.Fatalf("ListModels() = %#v", models)
	}
}
