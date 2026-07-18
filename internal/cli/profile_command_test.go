package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestProfileExportImportRoundTripWithExplicitCredentialBinding(t *testing.T) {
	t.Parallel()
	sourceDB := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, sourceDB, true)
	profilePath := filepath.Join(t.TempDir(), "team.json")
	out, _, err := runCommand(t, "--db", sourceDB, "profile", "export", profilePath)
	if err != nil {
		t.Fatalf("profile export error = %v", err)
	}
	if !strings.Contains(out, "Exported 2 providers and 2 model aliases") {
		t.Fatalf("profile export output = %q", out)
	}
	exported, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, forbidden := range []string{"/private/litellm.key", "file:", "keyring:", "provider/openrouter/api-key"} {
		if strings.Contains(string(exported), forbidden) {
			t.Fatalf("profile leaked %q: %s", forbidden, exported)
		}
	}
	destinationDB := filepath.Join(t.TempDir(), "destination.db")
	out, _, err = runCommand(
		t,
		"--db", destinationDB,
		"profile", "import", profilePath,
		"--credential", "litellm=TEAM_LITELLM_KEY",
		"--credential", "openrouter=TEAM_OPENROUTER_KEY",
		"--json",
	)
	if err != nil {
		t.Fatalf("profile import error = %v", err)
	}
	var result profileImportOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v; output = %s", err, out)
	}
	if result.SchemaVersion != 1 || result.ProvidersAdded != 2 || result.ModelsAdded != 2 || len(result.UnboundCredentials) != 0 {
		t.Fatalf("profile import result = %#v", result)
	}
	providersByName, models := readProfileConfiguration(t, destinationDB)
	if got := providersByName["litellm"].SecretRef; got != secret.EnvRef("TEAM_LITELLM_KEY") {
		t.Fatalf("litellm SecretRef = %q", got)
	}
	if got := providersByName["openrouter"].SecretRef; got != secret.EnvRef("TEAM_OPENROUTER_KEY") {
		t.Fatalf("openrouter SecretRef = %q", got)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
}

func TestProfileImportDryRunMakesNoConfigurationChanges(t *testing.T) {
	t.Parallel()
	sourceDB := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, sourceDB, false)
	profilePath := filepath.Join(t.TempDir(), "team.json")
	if _, _, err := runCommand(t, "--db", sourceDB, "profile", "export", profilePath); err != nil {
		t.Fatalf("profile export error = %v", err)
	}
	destinationRoot := filepath.Join(t.TempDir(), "missing")
	destinationDB := filepath.Join(destinationRoot, "destination.db")
	out, _, err := runCommand(t, "--db", destinationDB, "profile", "import", profilePath, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("profile import --dry-run error = %v", err)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"providers_added": 1`) {
		t.Fatalf("dry-run output = %s", out)
	}
	if _, statErr := os.Stat(destinationRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dry-run created database directory: %v", statErr)
	}
}

func TestProfileImportDryRunDoesNotMigrateExistingDatabase(t *testing.T) {
	t.Parallel()
	sourceDB := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, sourceDB, false)
	profilePath := filepath.Join(t.TempDir(), "team.json")
	if _, _, err := runCommand(t, "--db", sourceDB, "profile", "export", profilePath); err != nil {
		t.Fatalf("profile export error = %v", err)
	}
	destinationDB := filepath.Join(t.TempDir(), "destination.db")
	withProfileStore(t, destinationDB, func(*store.Store) {})
	setProfileSchemaVersion(t, destinationDB, 3)
	before, err := os.ReadFile(destinationDB)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	if _, _, dryRunErr := runCommand(t, "--db", destinationDB, "profile", "import", profilePath, "--dry-run"); dryRunErr != nil {
		t.Fatalf("profile import --dry-run error = %v", dryRunErr)
	}
	after, err := os.ReadFile(destinationDB)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run changed existing database bytes")
	}
	if version := profileSchemaVersion(t, destinationDB); version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
	}
}

func TestProfileImportConflictRollsBackEntireProfile(t *testing.T) {
	t.Parallel()
	sourceDB := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, sourceDB, true)
	profilePath := filepath.Join(t.TempDir(), "team.json")
	if _, _, err := runCommand(t, "--db", sourceDB, "profile", "export", profilePath); err != nil {
		t.Fatalf("profile export error = %v", err)
	}
	destinationDB := filepath.Join(t.TempDir(), "destination.db")
	withProfileStore(t, destinationDB, func(s *store.Store) {
		if err := s.AddProvider(context.Background(), cliProfileProvider("litellm", "litellm", "http://localhost:5000", "")); err != nil {
			t.Fatalf("AddProvider() error = %v", err)
		}
	})
	_, _, err := runCommand(t, "--db", destinationDB, "profile", "import", profilePath)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("profile import error = %v, want conflict", err)
	}
	providersByName, models := readProfileConfiguration(t, destinationDB)
	if len(providersByName) != 1 || providersByName["openrouter"].Name != "" || len(models) != 0 {
		t.Fatalf("conflicting import was partial: providers=%#v models=%#v", providersByName, models)
	}
}

func TestProfileExportRefusesOverwriteUnlessForced(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, dbPath, false)
	profilePath := filepath.Join(t.TempDir(), "team.json")
	if err := os.WriteFile(profilePath, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "profile", "export", profilePath); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("profile export error = %v, want overwrite refusal", err)
	}
	contents, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(contents) != "original" {
		t.Fatalf("existing file changed to %q", contents)
	}
	if _, _, exportErr := runCommand(t, "--db", dbPath, "profile", "export", profilePath, "--force"); exportErr != nil {
		t.Fatalf("profile export --force error = %v", exportErr)
	}
	contents, err = os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile() after force error = %v", err)
	}
	if !strings.Contains(string(contents), `"kind": "ccr-team-profile"`) {
		t.Fatalf("forced profile contents = %s", contents)
	}
}

func TestProfileImportReadsStdinAndReportsUnboundCredential(t *testing.T) {
	t.Parallel()
	sourceDB := filepath.Join(t.TempDir(), "source.db")
	seedProfileStore(t, sourceDB, false)
	profile, _, err := runCommand(t, "--db", sourceDB, "profile", "export")
	if err != nil {
		t.Fatalf("profile export stdout error = %v", err)
	}
	destinationDB := filepath.Join(t.TempDir(), "destination.db")
	out, _, err := runCommandWithDeps(t, Dependencies{In: strings.NewReader(profile)}, "--db", destinationDB, "profile", "import", "-")
	if err != nil {
		t.Fatalf("profile import stdin error = %v", err)
	}
	if !strings.Contains(out, "Credentials require local binding: litellm") {
		t.Fatalf("profile import output = %q", out)
	}
}

func seedProfileStore(t *testing.T, dbPath string, includeOpenRouter bool) {
	t.Helper()
	withProfileStore(t, dbPath, func(s *store.Store) {
		ctx := context.Background()
		if err := s.AddProvider(ctx, cliProfileProvider("litellm", "litellm", "http://localhost:4000", secret.FileRef("/private/litellm.key"))); err != nil {
			t.Fatalf("AddProvider(litellm) error = %v", err)
		}
		if err := s.AddModel(ctx, store.Model{Alias: "team-model", ProviderName: "litellm", ProviderModel: "team/model", Status: "full"}); err != nil {
			t.Fatalf("AddModel(team-model) error = %v", err)
		}
		if !includeOpenRouter {
			return
		}
		if err := s.AddProvider(ctx, cliProfileProvider("openrouter", "openrouter", "https://openrouter.ai/api", secret.KeyringRef("openrouter"))); err != nil {
			t.Fatalf("AddProvider(openrouter) error = %v", err)
		}
		if err := s.AddModel(ctx, store.Model{Alias: "router-model", ProviderName: "openrouter", ProviderModel: "vendor/model", Status: "degraded"}); err != nil {
			t.Fatalf("AddModel(router-model) error = %v", err)
		}
	})
}

func cliProfileProvider(name, providerType, baseURL, secretRef string) store.Provider {
	caps := providers.DefaultCapabilities(providerType)
	return store.Provider{
		Name: name, Type: providerType, BaseURL: baseURL, SecretRef: secretRef,
		Protocol: caps.Protocol, SupportsTools: caps.SupportsTools,
		SupportsStreaming: caps.SupportsStreaming, SupportsThinking: caps.SupportsThinking,
		SupportsModelDiscovery: caps.SupportsModelDiscovery, SupportsCountTokens: caps.SupportsCountTokens,
		Mode: caps.Mode,
	}
}

func withProfileStore(t *testing.T, dbPath string, run func(*store.Store)) {
	t.Helper()
	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	run(s)
}

func readProfileConfiguration(t *testing.T, dbPath string) (map[string]store.Provider, []store.Model) {
	t.Helper()
	providersByName := make(map[string]store.Provider)
	var models []store.Model
	withProfileStore(t, dbPath, func(s *store.Store) {
		storedProviders, err := s.ListProviders(context.Background())
		if err != nil {
			t.Fatalf("ListProviders() error = %v", err)
		}
		for _, provider := range storedProviders {
			providersByName[provider.Name] = provider
		}
		models, err = s.ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels() error = %v", err)
		}
	})
	return providersByName, models
}

func setProfileSchemaVersion(t *testing.T, dbPath string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE schema_version SET version = ? WHERE id = 1`, version); err != nil {
		t.Fatalf("updating schema version: %v", err)
	}
}

func profileSchemaVersion(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("sql.Open(read-only) error = %v", err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version WHERE id = 1`).Scan(&version); err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	return version
}
