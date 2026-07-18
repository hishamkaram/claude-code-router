package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/store"
	"github.com/hishamkaram/claude-code-router/internal/teamprofile"
)

type profileImportOptions struct {
	dryRun      bool
	jsonOutput  bool
	credentials []string
}

type profileImportOutput struct {
	SchemaVersion      int      `json:"schema_version"`
	DryRun             bool     `json:"dry_run"`
	ProvidersAdded     int      `json:"providers_added"`
	ModelsAdded        int      `json:"models_added"`
	UnboundCredentials []string `json:"unbound_credentials"`
}

func newProfileCommand(ctx context.Context, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Share provider and model configuration without secrets",
		Long: `Export and import explicit CCR team profiles.

Profiles contain provider routing metadata and model aliases. Raw credentials,
keyring identifiers, and credential file paths are never exported. Environment
variable names are portable and may be retained or replaced during import.`,
	}
	cmd.AddCommand(newProfileExportCommand(ctx, opts), newProfileImportCommand(ctx, opts))
	return cmd
}

func newProfileExportCommand(ctx context.Context, opts *options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "export [path]",
		Short: "Export a deterministic team profile",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "-"
			if len(args) == 1 {
				path = args[0]
			}
			if path == "" {
				return fmt.Errorf("profile export path must not be empty")
			}
			if path == "-" && force {
				return fmt.Errorf("--force requires a file path")
			}
			return runProfileExport(ctx, cmd, opts, path, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing output file")
	return cmd
}

func runProfileExport(ctx context.Context, cmd *cobra.Command, opts *options, path string, force bool) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	storedProviders, err := s.ListProviders(ctx)
	if err != nil {
		return fmt.Errorf("listing providers for profile export: %w", err)
	}
	storedModels, err := s.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("listing models for profile export: %w", err)
	}
	manifest, err := teamprofile.Build(storedProviders, storedModels)
	if err != nil {
		return err
	}
	if path == "-" {
		return teamprofile.Encode(cmd.OutOrStdout(), manifest)
	}
	var encoded bytes.Buffer
	if err := teamprofile.Encode(&encoded, manifest); err != nil {
		return err
	}
	if err := writeAtomicFile(path, encoded.Bytes(), force); err != nil {
		return fmt.Errorf("writing team profile %q: %w", path, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Exported %d providers and %d model aliases to %s\n", len(manifest.Providers), len(manifest.Models), path)
	return nil
}

func newProfileImportCommand(ctx context.Context, opts *options) *cobra.Command {
	flags := &profileImportOptions{}
	cmd := &cobra.Command{
		Use:   "import <path|->",
		Short: "Atomically import a team profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "" {
				return fmt.Errorf("profile import path must not be empty")
			}
			return runProfileImport(ctx, cmd, opts, args[0], *flags)
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "validate and report changes without writing")
	cmd.Flags().BoolVar(&flags.jsonOutput, "json", false, "print stable schema-versioned JSON")
	cmd.Flags().StringArrayVar(&flags.credentials, "credential", nil, "bind provider to local environment variable (provider=ENV_NAME; repeatable)")
	return cmd
}

func runProfileImport(ctx context.Context, cmd *cobra.Command, opts *options, path string, flags profileImportOptions) error {
	manifest, err := readTeamProfile(cmd.InOrStdin(), path)
	if err != nil {
		return err
	}
	bindings, err := parseCredentialBindings(flags.credentials)
	if err != nil {
		return err
	}
	plan, err := manifest.PlanImport(bindings)
	if err != nil {
		return fmt.Errorf("planning team profile import: %w", err)
	}
	result, err := executeProfileImport(ctx, opts, plan.Providers, plan.Models, flags.dryRun)
	if err != nil {
		return fmt.Errorf("importing team profile: %w", err)
	}
	output := profileImportOutput{
		SchemaVersion:      1,
		DryRun:             flags.dryRun,
		ProvidersAdded:     result.ProvidersAdded,
		ModelsAdded:        result.ModelsAdded,
		UnboundCredentials: plan.UnboundCredential,
	}
	if flags.jsonOutput {
		return writeVersionedJSON(cmd.OutOrStdout(), output)
	}
	writeProfileImportSummary(cmd.OutOrStdout(), output)
	return nil
}

func executeProfileImport(ctx context.Context, opts *options, providers []store.Provider, models []store.Model, dryRun bool) (store.ConfigurationImportResult, error) {
	if !dryRun {
		s, _, err := openMigratedStore(ctx, opts)
		if err != nil {
			return store.ConfigurationImportResult{}, err
		}
		defer closeStore(s)
		return s.ImportConfiguration(ctx, providers, models, false)
	}
	dbPath, err := resolveDBPath(opts)
	if err != nil {
		return store.ConfigurationImportResult{}, err
	}
	_, statErr := os.Stat(dbPath)
	if errors.Is(statErr, os.ErrNotExist) {
		return store.ConfigurationImportResult{
			ProvidersAdded: len(providers), ModelsAdded: len(models),
		}, nil
	} else if statErr != nil {
		return store.ConfigurationImportResult{}, fmt.Errorf("inspecting SQLite database for dry-run: %w", statErr)
	}
	s, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return store.ConfigurationImportResult{}, err
	}
	defer closeStore(s)
	return s.PlanConfigurationImport(ctx, providers, models)
}

func readTeamProfile(stdin io.Reader, path string) (teamprofile.Manifest, error) {
	if path == "-" {
		manifest, err := teamprofile.Decode(stdin)
		if err != nil {
			return teamprofile.Manifest{}, err
		}
		return manifest, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return teamprofile.Manifest{}, fmt.Errorf("opening team profile %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := teamprofile.Decode(file)
	if err != nil {
		return teamprofile.Manifest{}, fmt.Errorf("reading team profile %q: %w", path, err)
	}
	return manifest, nil
}

func parseCredentialBindings(values []string) (map[string]string, error) {
	bindings := make(map[string]string, len(values))
	for _, value := range values {
		providerName, envName, ok := strings.Cut(value, "=")
		if !ok || providerName == "" || envName == "" || strings.Contains(envName, "=") {
			return nil, fmt.Errorf("invalid --credential %q; expected provider=ENV_NAME", value)
		}
		if err := validateName("provider name", providerName); err != nil {
			return nil, err
		}
		if err := validateEnvName(envName); err != nil {
			return nil, err
		}
		if _, duplicate := bindings[providerName]; duplicate {
			return nil, fmt.Errorf("duplicate --credential binding for provider %q", providerName)
		}
		bindings[providerName] = envName
	}
	return bindings, nil
}

func writeProfileImportSummary(out io.Writer, result profileImportOutput) {
	action := "Imported"
	if result.DryRun {
		action = "Dry run: would import"
	}
	fmt.Fprintf(out, "%s %d providers and %d model aliases.\n", action, result.ProvidersAdded, result.ModelsAdded)
	if len(result.UnboundCredentials) > 0 {
		unbound := append([]string(nil), result.UnboundCredentials...)
		sort.Strings(unbound)
		fmt.Fprintf(out, "Credentials require local binding: %s\n", strings.Join(unbound, ", "))
	}
}
