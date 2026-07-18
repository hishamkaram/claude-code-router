package cli

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	conformancecheck "github.com/hishamkaram/claude-code-router/internal/conformance"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type doctorCheckView struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

type doctorProbeView struct {
	Alias         string `json:"alias"`
	ProviderName  string `json:"provider_name"`
	ProviderModel string `json:"provider_model"`
	Protocol      string `json:"protocol"`
	Status        string `json:"status"`
	LatencyMS     int64  `json:"latency_ms"`
}

type doctorDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Status        string            `json:"status"`
	Database      string            `json:"database"`
	StoreSchema   int               `json:"store_schema"`
	ProviderCount int               `json:"provider_count"`
	ModelCount    int               `json:"model_count"`
	Checks        []doctorCheckView `json:"checks"`
	Probes        []doctorProbeView `json:"probes"`
}

type doctorOptions struct {
	live       bool
	all        bool
	jsonOutput bool
}

func newDoctorCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	doctorOpts := doctorOptions{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check local configuration and optional live provider routes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if doctorOpts.all && !doctorOpts.live {
				return fmt.Errorf("--all requires --live")
			}
			document, err := runDoctor(ctx, cmd, opts, deps, doctorOpts)
			if err != nil {
				return err
			}
			if doctorOpts.jsonOutput {
				if err := writeVersionedJSON(cmd.OutOrStdout(), document); err != nil {
					return err
				}
			} else {
				writeHumanDoctor(cmd, document)
			}
			if document.Status == "failed" {
				return fmt.Errorf("doctor found required failures")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&doctorOpts.live, "live", false, "Contact one configured alias per provider")
	cmd.Flags().BoolVar(&doctorOpts.all, "all", false, "With --live, contact every non-blocked alias")
	cmd.Flags().BoolVar(&doctorOpts.jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func runDoctor(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, options doctorOptions) (doctorDocument, error) {
	s, dbPath, err := openMigratedStore(ctx, opts)
	if err != nil {
		return doctorDocument{}, err
	}
	defer closeStore(s)
	document := doctorDocument{
		SchemaVersion: 1, Status: "passed", Database: dbPath,
		Checks: []doctorCheckView{}, Probes: []doctorProbeView{},
	}
	if document.StoreSchema, err = s.SchemaVersion(ctx); err != nil {
		return doctorDocument{}, err
	}
	document.Checks = append(document.Checks, doctorCheckView{
		Name: "sqlite", Status: "passed",
		Evidence: fmt.Sprintf("schema %d is readable", document.StoreSchema),
	})
	providers, err := s.ListProviders(ctx)
	if err != nil {
		return doctorDocument{}, err
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return doctorDocument{}, err
	}
	document.ProviderCount, document.ModelCount = len(providers), len(models)
	document.Checks = append(document.Checks, offlineProviderChecks(providers)...)
	document.Checks = append(document.Checks, localDependencyChecks(ctx, deps, s)...)
	if options.live {
		targets := selectDoctorTargets(models, options.all)
		if len(targets) == 0 {
			document.Checks = append(document.Checks, doctorCheckView{
				Name: "live_targets", Status: "failed",
				Evidence: "no non-blocked model aliases are configured",
			})
		} else {
			for index := range targets {
				model := &targets[index]
				fmt.Fprintf(cmd.ErrOrStderr(), "Doctor live target: alias=%s provider=%s model=%s\n",
					model.Alias, model.ProviderName, model.ProviderModel)
			}
			document.Probes = runDoctorProbes(ctx, s, deps, targets)
		}
	}
	for _, check := range document.Checks {
		if check.Status == "failed" {
			document.Status = "failed"
		}
	}
	for _, probe := range document.Probes {
		if probe.Status == conformancecheck.StatusFailed {
			document.Status = "failed"
		}
	}
	return document, nil
}

func offlineProviderChecks(providers []store.Provider) []doctorCheckView {
	checks := make([]doctorCheckView, 0, len(providers))
	for index := range providers {
		provider := &providers[index]
		status := "passed"
		evidence := fmt.Sprintf("protocol=%s mode=%s token-count=%s",
			effectiveProviderCapabilities(*provider).Protocol,
			effectiveProviderCapabilities(*provider).Mode, providerTokenCountMode(*provider))
		if _, err := resolveProviderType(provider.Name, provider.Type); err != nil {
			status, evidence = "failed", "provider type is invalid"
		} else if _, err := resolveBaseURL(provider.Type, provider.BaseURL); err != nil {
			status, evidence = "failed", "provider base URL is invalid"
		}
		checks = append(checks, doctorCheckView{
			Name: "provider:" + provider.Name, Status: status, Evidence: evidence,
		})
	}
	return checks
}

func localDependencyChecks(ctx context.Context, deps Dependencies, s *store.Store) []doctorCheckView {
	checks := make([]doctorCheckView, 0, 3)
	secretCheck := doctorCheckView{Name: "secrets", Status: "passed", Evidence: "secret backend is available"}
	if err := deps.Secrets.Available(ctx); err != nil {
		secretCheck.Status, secretCheck.Evidence = "warning", "secret backend is unavailable"
	}
	checks = append(checks, secretCheck)
	claudeCheck := doctorCheckView{Name: "claude", Status: "passed"}
	if path, err := exec.LookPath("claude"); err == nil {
		claudeCheck.Evidence = "Claude Code found at " + path
	} else {
		claudeCheck.Status, claudeCheck.Evidence = "warning", "Claude Code is not in PATH"
	}
	checks = append(checks, claudeCheck)
	launches, err := s.ListLaunches(ctx)
	stale := 0
	if err == nil {
		for index := range launches {
			launch := &launches[index]
			if launch.State == "running" && processStatus(launch.PID) == "exited" {
				stale++
			}
		}
	}
	launchCheck := doctorCheckView{Name: "launches", Status: "passed", Evidence: "no stale running launches"}
	if err != nil {
		launchCheck.Status, launchCheck.Evidence = "failed", "launch state is unreadable"
	} else if stale > 0 {
		launchCheck.Status = "warning"
		launchCheck.Evidence = fmt.Sprintf("%d launch records are stale", stale)
	}
	return append(checks, launchCheck)
}

func selectDoctorTargets(models []store.Model, all bool) []store.Model {
	targets := make([]store.Model, 0, len(models))
	seenProviders := make(map[string]struct{})
	for index := range models {
		model := &models[index]
		if model.Status == "blocked" {
			continue
		}
		if !all {
			if _, seen := seenProviders[model.ProviderName]; seen {
				continue
			}
			seenProviders[model.ProviderName] = struct{}{}
		}
		targets = append(targets, *model)
	}
	return targets
}

func runDoctorProbes(ctx context.Context, s *store.Store, deps Dependencies, targets []store.Model) []doctorProbeView {
	results := make([]doctorProbeView, len(targets))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for index, model := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index] = doctorProbeView{Alias: model.Alias, Status: "failed"}
				return
			}
			started := time.Now()
			result, err := conformancecheck.RunProvider(ctx, conformancecheck.Config{
				Store: s, Secrets: deps.Secrets, Alias: model.Alias,
				Timeout: 15 * time.Second, SmokeOnly: true,
			})
			view := doctorProbeView{
				Alias: model.Alias, ProviderName: model.ProviderName,
				ProviderModel: model.ProviderModel, Status: conformancecheck.StatusFailed,
				LatencyMS: time.Since(started).Milliseconds(),
			}
			if err == nil {
				view.Protocol, view.Status = result.Protocol, result.Status
			}
			results[index] = view
		}()
	}
	wait.Wait()
	sort.SliceStable(results, func(i, j int) bool { return results[i].Alias < results[j].Alias })
	return results
}

func writeHumanDoctor(cmd *cobra.Command, document doctorDocument) {
	fmt.Fprintf(cmd.OutOrStdout(), "SQLite: ok (%s, schema=%d)\n", document.Database, document.StoreSchema)
	fmt.Fprintf(cmd.OutOrStdout(), "Providers: %d\nModels: %d\n", document.ProviderCount, document.ModelCount)
	for _, check := range document.Checks {
		if providerName, ok := strings.CutPrefix(check.Name, "provider:"); ok {
			fmt.Fprintf(cmd.OutOrStdout(), "Provider %s: %s\n", providerName, check.Evidence)
			continue
		}
		switch check.Name {
		case "secrets":
			if check.Status == "passed" {
				fmt.Fprintln(cmd.OutOrStdout(), "Secrets: ok")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Secrets: unavailable")
			}
		case "claude":
			fmt.Fprintf(cmd.OutOrStdout(), "Claude Code: %s\n", check.Evidence)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s)\n", check.Name, check.Status, check.Evidence)
		}
	}
	for _, probe := range document.Probes {
		fmt.Fprintf(cmd.OutOrStdout(), "Live %s: %s provider=%s model=%s protocol=%s latency=%dms\n",
			probe.Alias, probe.Status, probe.ProviderName, probe.ProviderModel,
			probe.Protocol, probe.LatencyMS)
	}
}
