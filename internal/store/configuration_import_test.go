package store

import (
	"context"
	"errors"
	"testing"
)

func TestImportConfigurationAtomicAndDryRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	providers := []Provider{{
		Name: "fixture", Type: "litellm", BaseURL: "http://127.0.0.1:4000",
		Protocol: "openai-compatible", SupportsTools: true,
		SupportsStreaming: true, Mode: "degraded",
	}}
	models := []Model{{
		Alias: "coder", ProviderName: "fixture", ProviderModel: "model-v1", Status: "degraded",
	}}
	result, err := s.ImportConfiguration(ctx, providers, models, true)
	if err != nil {
		t.Fatalf("ImportConfiguration(dry-run) error = %v", err)
	}
	if result.ProvidersAdded != 1 || result.ModelsAdded != 1 {
		t.Fatalf("dry-run result = %#v", result)
	}
	if configured, err := s.ListProviders(ctx); err != nil || len(configured) != 0 {
		t.Fatalf("providers after dry-run = %#v, %v", configured, err)
	}
	if _, err := s.ImportConfiguration(ctx, providers, models, false); err != nil {
		t.Fatalf("ImportConfiguration() error = %v", err)
	}
	if configured, err := s.ListModels(ctx); err != nil || len(configured) != 1 {
		t.Fatalf("models after import = %#v, %v", configured, err)
	}
}

func TestImportConfigurationConflictRollsBackEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMigratedStore(t, ctx)
	if err := s.AddProvider(ctx, Provider{Name: "existing", Type: "local", BaseURL: "http://127.0.0.1:4000"}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	providers := []Provider{
		{Name: "existing", Type: "local", BaseURL: "http://127.0.0.1:4000"},
		{Name: "new", Type: "local", BaseURL: "http://127.0.0.1:5000"},
	}
	models := []Model{{Alias: "coder", ProviderName: "new", ProviderModel: "model-v1", Status: "degraded"}}
	_, err := s.ImportConfiguration(ctx, providers, models, false)
	var conflict *ConfigurationConflictError
	if !errors.As(err, &conflict) || len(conflict.Providers) != 1 || conflict.Providers[0] != "existing" {
		t.Fatalf("ImportConfiguration() error = %v", err)
	}
	configured, listErr := s.ListProviders(ctx)
	if listErr != nil || len(configured) != 1 || configured[0].Name != "existing" {
		t.Fatalf("providers after conflict = %#v, %v", configured, listErr)
	}
	configuredModels, listErr := s.ListModels(ctx)
	if listErr != nil || len(configuredModels) != 0 {
		t.Fatalf("models after conflict = %#v, %v", configuredModels, listErr)
	}
}
