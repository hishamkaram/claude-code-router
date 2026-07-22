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
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type doctorCheckView struct {
	Name              string `json:"name"`
	Status            string `json:"status"`
	Evidence          string `json:"evidence"`
	SupportsResponses *bool  `json:"supports_responses,omitempty"`
}

type doctorProbeView struct {
	Alias              string `json:"alias"`
	ProviderName       string `json:"provider_name"`
	ProviderModel      string `json:"provider_model"`
	Protocol           string `json:"protocol"`
	Status             string `json:"status"`
	LatencyMS          int64  `json:"latency_ms"`
	FailedCheck        string `json:"failed_check,omitempty"`
	FailureKind        string `json:"failure_kind,omitempty"`
	HTTPStatus         int    `json:"http_status,omitempty"`
	ProviderHTTPStatus int    `json:"provider_http_status,omitempty"`
	Evidence           string `json:"evidence,omitempty"`
	Action             string `json:"action,omitempty"`
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

const doctorProbeStatusSkipped = "skipped"

type doctorData struct {
	document  doctorDocument
	providers []store.Provider
	models    []store.Model
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
	cmd.Flags().BoolVar(&doctorOpts.all, "all", false, "With --live, contact every probeable alias and report provider control models as skipped")
	cmd.Flags().BoolVar(&doctorOpts.jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func runDoctor(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, options doctorOptions) (doctorDocument, error) {
	s, dbPath, err := openMigratedStore(ctx, opts)
	if err != nil {
		return doctorDocument{}, err
	}
	defer closeStore(s)
	data, err := collectDoctorData(ctx, s, dbPath, deps)
	if err != nil {
		return doctorDocument{}, err
	}
	if options.live {
		runLiveDoctor(ctx, cmd, s, deps, options.all, &data)
	}
	updateDoctorStatus(&data.document)
	return data.document, nil
}

func collectDoctorData(ctx context.Context, s *store.Store, dbPath string, deps Dependencies) (doctorData, error) {
	document := doctorDocument{
		SchemaVersion: 1, Status: "passed", Database: dbPath,
		Checks: []doctorCheckView{}, Probes: []doctorProbeView{},
	}
	storeSchema, err := s.SchemaVersion(ctx)
	if err != nil {
		return doctorData{}, err
	}
	document.StoreSchema = storeSchema
	document.Checks = append(document.Checks, doctorCheckView{
		Name: "sqlite", Status: "passed",
		Evidence: fmt.Sprintf("schema %d is readable", document.StoreSchema),
	})
	configuredProviders, err := s.ListProviders(ctx)
	if err != nil {
		return doctorData{}, err
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return doctorData{}, err
	}
	document.ProviderCount, document.ModelCount = len(configuredProviders), len(models)
	document.Checks = append(document.Checks, offlineProviderChecks(configuredProviders)...)
	document.Checks = append(document.Checks, localDependencyChecks(ctx, deps, s)...)
	return doctorData{document: document, providers: configuredProviders, models: models}, nil
}

func runLiveDoctor(ctx context.Context, cmd *cobra.Command, s *store.Store, deps Dependencies, all bool, data *doctorData) {
	providersByName := doctorProvidersByName(data.providers)
	targets, skipped := selectDoctorTargets(data.models, providersByName, all)
	data.document.Probes = append(data.document.Probes, skipped...)
	if len(targets) == 0 {
		data.document.Checks = append(data.document.Checks, doctorLiveTargetCheck(skipped))
	} else {
		writeDoctorLiveTargets(cmd, targets)
		data.document.Probes = append(data.document.Probes, runDoctorProbes(ctx, s, deps, targets)...)
	}
	sort.SliceStable(data.document.Probes, func(i, j int) bool { return data.document.Probes[i].Alias < data.document.Probes[j].Alias })
}

func doctorProvidersByName(configuredProviders []store.Provider) map[string]store.Provider {
	providersByName := make(map[string]store.Provider, len(configuredProviders))
	for index := range configuredProviders {
		provider := configuredProviders[index]
		providersByName[provider.Name] = provider
	}
	return providersByName
}

func doctorLiveTargetCheck(skipped []doctorProbeView) doctorCheckView {
	if len(skipped) > 0 {
		return doctorCheckView{
			Name: "live_targets", Status: doctorProbeStatusSkipped,
			Evidence: "no eligible aliases require live probing; skipped aliases are excluded from routing",
		}
	}
	return doctorCheckView{Name: "live_targets", Status: "failed", Evidence: "no non-blocked routable aliases are configured"}
}

func writeDoctorLiveTargets(cmd *cobra.Command, targets []store.Model) {
	for index := range targets {
		model := &targets[index]
		fmt.Fprintf(cmd.ErrOrStderr(), "Doctor live target: alias=%s provider=%s model=%s\n",
			model.Alias, model.ProviderName, model.ProviderModel)
	}
}

func updateDoctorStatus(document *doctorDocument) {
	for index := range document.Checks {
		check := &document.Checks[index]
		if check.Status == "failed" {
			document.Status = "failed"
		}
	}
	for index := range document.Probes {
		probe := &document.Probes[index]
		if probe.Status == conformancecheck.StatusFailed {
			document.Status = "failed"
		}
	}
}

func offlineProviderChecks(configuredProviders []store.Provider) []doctorCheckView {
	checks := make([]doctorCheckView, 0, len(configuredProviders))
	for index := range configuredProviders {
		provider := &configuredProviders[index]
		capabilities := effectiveProviderCapabilities(*provider)
		supportsResponses := capabilities.SupportsResponses
		status := "passed"
		evidence := fmt.Sprintf("protocol=%s mode=%s token-count=%s responses=%t",
			capabilities.Protocol, capabilities.Mode, providerTokenCountMode(*provider), supportsResponses)
		if _, err := resolveProviderType(provider.Name, provider.Type); err != nil {
			status, evidence = "failed", "provider type is invalid"
		} else if _, err := resolveBaseURL(provider.Type, provider.BaseURL); err != nil {
			status, evidence = "failed", "provider base URL is invalid"
		}
		checks = append(checks, doctorCheckView{
			Name: "provider:" + provider.Name, Status: status, Evidence: evidence,
			SupportsResponses: &supportsResponses,
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

func selectDoctorTargets(models []store.Model, providersByName map[string]store.Provider, all bool) ([]store.Model, []doctorProbeView) {
	targets := make([]store.Model, 0, len(models))
	skipped := make([]doctorProbeView, 0)
	seenProviders := make(map[string]struct{})
	for index := range models {
		model := &models[index]
		if model.Status == "blocked" {
			continue
		}
		provider, providerFound := providersByName[model.ProviderName]
		if providerFound {
			skipReason, err := liveProbeSkipReason(*model, provider)
			if err != nil {
				skipped = append(skipped, doctorProbeView{
					Alias: model.Alias, ProviderName: model.ProviderName, ProviderModel: model.ProviderModel,
					Protocol: effectiveProviderCapabilities(provider).Protocol, Status: conformancecheck.StatusFailed,
					FailureKind: "configuration", Evidence: err.Error(), Action: "ccr model show " + model.Alias,
				})
				continue
			}
			if skipReason != "" {
				skipped = append(skipped, doctorProbeView{
					Alias: model.Alias, ProviderName: model.ProviderName, ProviderModel: model.ProviderModel,
					Protocol: effectiveProviderCapabilities(provider).Protocol, Status: doctorProbeStatusSkipped,
					Evidence: skipReason,
				})
				continue
			}
		}
		if !all {
			if _, seen := seenProviders[model.ProviderName]; seen {
				continue
			}
			seenProviders[model.ProviderName] = struct{}{}
		}
		targets = append(targets, *model)
	}
	return targets, skipped
}

func runDoctorProbes(ctx context.Context, s *store.Store, deps Dependencies, targets []store.Model) []doctorProbeView {
	results := make([]doctorProbeView, len(targets))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for index := range targets {
		model := &targets[index]
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index] = doctorProbeView{
					Alias: model.Alias, ProviderName: model.ProviderName, ProviderModel: model.ProviderModel,
					Status: conformancecheck.StatusFailed, FailureKind: "canceled",
					Evidence: "live diagnostic was canceled before it could start",
					Action:   fmt.Sprintf("ccr conformance run %s", model.Alias),
				}
				return
			}
			started := time.Now()
			provider, providerErr := s.GetProvider(ctx, model.ProviderName)
			if providerErr != nil {
				results[index] = doctorProbeView{
					Alias: model.Alias, ProviderName: model.ProviderName, ProviderModel: model.ProviderModel,
					Status: conformancecheck.StatusFailed, FailureKind: "configuration",
					Evidence: "configured provider is unreadable", Action: "ccr doctor",
					LatencyMS: time.Since(started).Milliseconds(),
				}
				return
			}
			result, err := conformancecheck.RunProvider(ctx, conformancecheck.Config{
				Store: s, Secrets: deps.Secrets, Alias: model.Alias,
				Timeout: 15 * time.Second, SmokeOnly: true,
			})
			view := doctorProbeView{
				Alias: model.Alias, ProviderName: model.ProviderName,
				ProviderModel: model.ProviderModel, Status: conformancecheck.StatusFailed,
				LatencyMS: time.Since(started).Milliseconds(),
				Protocol:  effectiveProviderCapabilities(provider).Protocol,
			}
			if err != nil {
				view.FailureKind = "suite_start"
				view.Evidence = safeConformanceStartEvidence(err)
				view.Action = doctorProbeAction(*model, provider, view.FailureKind)
			} else {
				view.Protocol, view.Status = result.Protocol, result.Status
				for _, check := range result.Checks {
					if check.Status != conformancecheck.StatusFailed {
						continue
					}
					view.FailedCheck = check.Name
					view.FailureKind = check.FailureKind
					view.HTTPStatus = check.HTTPStatus
					view.ProviderHTTPStatus = check.ProviderHTTPStatus
					view.Evidence = check.Evidence
					view.Action = doctorProbeAction(*model, provider, check.FailureKind)
					break
				}
			}
			results[index] = view
		}()
	}
	wait.Wait()
	sort.SliceStable(results, func(i, j int) bool { return results[i].Alias < results[j].Alias })
	return results
}

func doctorProbeAction(model store.Model, provider store.Provider, failureKind string) string {
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return fmt.Sprintf("ccr model remove %s --yes", model.Alias)
	}
	switch failureKind {
	case "model_absent", "alias_absent":
		return fmt.Sprintf("ccr provider discover-models %s", provider.Name)
	case "credential", "provider_http_status":
		return fmt.Sprintf("ccr provider test %s", provider.Name)
	default:
		return fmt.Sprintf("ccr conformance run %s", model.Alias)
	}
}

func writeHumanDoctor(cmd *cobra.Command, document doctorDocument) {
	fmt.Fprintf(cmd.OutOrStdout(), "SQLite: ok (%s, schema=%d)\n", document.Database, document.StoreSchema)
	fmt.Fprintf(cmd.OutOrStdout(), "Providers: %d\nModels: %d\n", document.ProviderCount, document.ModelCount)
	for index := range document.Checks {
		check := &document.Checks[index]
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
	for index := range document.Probes {
		probe := &document.Probes[index]
		fmt.Fprintf(cmd.OutOrStdout(), "Live %s: %s provider=%s model=%s protocol=%s latency=%dms\n",
			probe.Alias, probe.Status, probe.ProviderName, probe.ProviderModel,
			probe.Protocol, probe.LatencyMS)
		switch probe.Status {
		case conformancecheck.StatusFailed:
			fmt.Fprintf(cmd.OutOrStdout(), "  failure: check=%s kind=%s gateway_http=%d provider_http=%d\n",
				probe.FailedCheck, probe.FailureKind, probe.HTTPStatus, probe.ProviderHTTPStatus)
			fmt.Fprintf(cmd.OutOrStdout(), "  evidence: %s\n", probe.Evidence)
			fmt.Fprintf(cmd.OutOrStdout(), "  action: %s\n", probe.Action)
		case doctorProbeStatusSkipped:
			fmt.Fprintf(cmd.OutOrStdout(), "  skipped: %s\n", probe.Evidence)
		}
	}
}
