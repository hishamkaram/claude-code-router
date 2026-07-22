package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
)

func TestOpenReadOnlyHandlesEscapedPathsAndRejectsWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr #1.db")
	writable, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if migrateErr := writable.Migrate(ctx); migrateErr != nil {
		t.Fatalf("Migrate() error = %v", migrateErr)
	}
	if addErr := writable.AddProvider(ctx, Provider{Name: "fixture", Type: "local", BaseURL: "http://localhost:4000"}); addErr != nil {
		t.Fatalf("AddProvider() error = %v", addErr)
	}
	if closeErr := writable.Close(); closeErr != nil {
		t.Fatalf("Close(writable) error = %v", closeErr)
	}
	readOnly, err := OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer func() { _ = readOnly.Close() }()
	providers, err := readOnly.ListProviders(ctx)
	if err != nil || len(providers) != 1 || providers[0].Name != "fixture" {
		t.Fatalf("ListProviders() = %#v, error = %v", providers, err)
	}
	if err := readOnly.AddProvider(ctx, Provider{Name: "forbidden", Type: "local", BaseURL: "http://localhost:5000"}); err == nil {
		t.Fatal("AddProvider() error = nil on read-only store")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenReadOnly(context.Background(), filepath.Join(root, "ccr.db")); err == nil {
		t.Fatal("OpenReadOnly() error = nil for missing database")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly() created parent directory: %v", err)
	}
}

func TestModelCapabilitiesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(modelcap.Values{
		Kind:                modelcap.KindChat,
		ContextWindowTokens: modelcap.Int64(1_000_000),
		SupportsTools:       modelcap.Bool(true),
	}, "litellm:/model/info")
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	want := Model{
		Alias:                   "glm-5-2",
		ProviderName:            "litellm",
		ProviderModel:           "glm-5.2[1m]",
		Status:                  "full",
		DiscoveredCapabilities:  discovered,
		CapabilityOverrides:     modelcap.Values{MaxOutputTokens: modelcap.Int64(64_000)},
		CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}
	if err := s.AddModel(ctx, want); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	got, getErr := s.GetModel(ctx, want.Alias)
	if getErr != nil {
		t.Fatalf("GetModel() error = %v", getErr)
	}
	if got.DiscoveredCapabilities.Values.ContextWindowTokens == nil ||
		*got.DiscoveredCapabilities.Values.ContextWindowTokens != 1_000_000 ||
		got.DiscoveredCapabilities.Sources["supports_tools"] != "litellm:/model/info" ||
		got.CapabilityOverrides.MaxOutputTokens == nil || *got.CapabilityOverrides.MaxOutputTokens != 64_000 ||
		got.CapabilitiesRefreshedAt != want.CapabilitiesRefreshedAt {
		t.Fatalf("GetModel() = %#v", got)
	}
	got.Status = "degraded"
	if err := s.UpdateModel(ctx, got); err != nil {
		t.Fatalf("UpdateModel() error = %v", err)
	}
	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	if len(models) != 1 || models[0].CapabilityOverrides.MaxOutputTokens == nil ||
		*models[0].CapabilityOverrides.MaxOutputTokens != 64_000 {
		t.Fatalf("ListModels() = %#v", models)
	}
}

func TestProviderResponsesOverlayPreservesDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, Provider{
		Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000", SupportsResponses: true,
	}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	got, err := s.GetProvider(ctx, "litellm")
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	if got.Protocol != "openai-compatible" || got.Mode != "degraded" ||
		!got.SupportsTools || !got.SupportsStreaming || !got.SupportsThinking ||
		!got.SupportsModelDiscovery || !got.SupportsCountTokens || !got.SupportsResponses {
		t.Fatalf("GetProvider() = %#v, want litellm defaults plus responses", got)
	}
}

func TestUpdateProviderInvalidatesCapabilitiesWhenSourceChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	provider := Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(modelcap.Values{
		ContextWindowTokens: modelcap.Int64(1_000_000), SupportsTools: modelcap.Bool(false),
	}, "litellm:/model/info")
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	model := Model{
		Alias: "glm", ProviderName: provider.Name, ProviderModel: "glm-5.2", Status: "degraded",
		DiscoveredCapabilities: discovered, CapabilityOverrides: modelcap.Values{MaxOutputTokens: modelcap.Int64(64_000)},
		CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}

	storedProvider, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	noChange, err := s.UpdateProvider(ctx, storedProvider)
	if err != nil {
		t.Fatalf("no-op UpdateProvider() error = %v", err)
	}
	if noChange.CapabilitySnapshotsInvalidated != 0 {
		t.Fatalf("no-op UpdateProvider() invalidated %d snapshots", noChange.CapabilitySnapshotsInvalidated)
	}
	beforeSourceChange, err := s.GetModel(ctx, model.Alias)
	if err != nil {
		t.Fatalf("GetModel() before source change error = %v", err)
	}
	if modelcap.IsZeroSnapshot(beforeSourceChange.DiscoveredCapabilities) {
		t.Fatal("no-op provider update cleared discovered capabilities")
	}

	storedProvider.BaseURL = "http://localhost:5000"
	result, err := s.UpdateProvider(ctx, storedProvider)
	if err != nil {
		t.Fatalf("source-changing UpdateProvider() error = %v", err)
	}
	if result.CapabilitySnapshotsInvalidated != 1 {
		t.Fatalf("UpdateProvider() invalidated %d snapshots, want 1", result.CapabilitySnapshotsInvalidated)
	}
	updatedModel, err := s.GetModel(ctx, model.Alias)
	if err != nil {
		t.Fatalf("GetModel() after source change error = %v", err)
	}
	if !modelcap.IsZeroSnapshot(updatedModel.DiscoveredCapabilities) || updatedModel.CapabilitiesRefreshedAt != "" {
		t.Fatalf("stale discovered capabilities survived provider update: %#v", updatedModel)
	}
	if updatedModel.CapabilityOverrides.MaxOutputTokens == nil || *updatedModel.CapabilityOverrides.MaxOutputTokens != 64_000 {
		t.Fatalf("provider update cleared explicit overrides: %#v", updatedModel.CapabilityOverrides)
	}
}

func TestUpdateProviderRollsBackWhenCapabilityInvalidationFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	provider := Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}
	if err := s.AddProvider(ctx, provider); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	discovered, snapshotErr := modelcap.SnapshotFrom(
		modelcap.Values{ContextWindowTokens: modelcap.Int64(1_000_000)}, "litellm:/model/info",
	)
	if snapshotErr != nil {
		t.Fatalf("SnapshotFrom() error = %v", snapshotErr)
	}
	if err := s.AddModel(ctx, Model{
		Alias: "glm", ProviderName: provider.Name, ProviderModel: "glm-5.2", Status: "degraded",
		DiscoveredCapabilities: discovered, CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}); err != nil {
		t.Fatalf("AddModel() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TRIGGER reject_capability_invalidation
BEFORE UPDATE OF discovered_capabilities ON models
BEGIN
  SELECT RAISE(ABORT, 'invalidation rejected');
END;
`); err != nil {
		t.Fatalf("creating invalidation trigger: %v", err)
	}
	storedProvider, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() error = %v", err)
	}
	storedProvider.BaseURL = "http://localhost:5000"
	_, updateErr := s.UpdateProvider(ctx, storedProvider)
	if updateErr == nil || !strings.Contains(updateErr.Error(), "invalidating model capabilities") {
		t.Fatalf("UpdateProvider() error = %v", updateErr)
	}
	rolledBack, err := s.GetProvider(ctx, provider.Name)
	if err != nil {
		t.Fatalf("GetProvider() after rollback error = %v", err)
	}
	if rolledBack.BaseURL != provider.BaseURL {
		t.Fatalf("provider base URL = %q after rollback, want %q", rolledBack.BaseURL, provider.BaseURL)
	}
}

func TestModelCapabilityValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, openErr := Open(ctx, filepath.Join(t.TempDir(), "ccr.db"))
	if openErr != nil {
		t.Fatalf("Open() error = %v", openErr)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, Provider{Name: "litellm", Type: "litellm", BaseURL: "http://localhost:4000"}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	err := s.AddModel(ctx, Model{
		Alias:               "invalid",
		ProviderName:        "litellm",
		ProviderModel:       "invalid",
		Status:              "degraded",
		CapabilityOverrides: modelcap.Values{ContextWindowTokens: modelcap.Int64(0)},
	})
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("AddModel() error = %v", err)
	}
}
