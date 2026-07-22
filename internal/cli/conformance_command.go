package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	conformancecheck "github.com/hishamkaram/claude-code-router/internal/conformance"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

type conformanceCheckView struct {
	Name               string `json:"name"`
	Status             string `json:"status"`
	LatencyMS          int64  `json:"latency_ms"`
	Evidence           string `json:"evidence"`
	FailureKind        string `json:"failure_kind,omitempty"`
	HTTPStatus         int    `json:"http_status,omitempty"`
	ProviderHTTPStatus int    `json:"provider_http_status,omitempty"`
}

type conformanceDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	RunID         int64                  `json:"run_id"`
	Alias         string                 `json:"alias"`
	ProviderName  string                 `json:"provider_name"`
	ProviderModel string                 `json:"provider_model"`
	Protocol      string                 `json:"protocol"`
	Scope         string                 `json:"scope"`
	Status        string                 `json:"status"`
	LiveVerified  bool                   `json:"live_verified"`
	Checks        []conformanceCheckView `json:"checks"`
}

type conformanceRunView struct {
	ID            int64  `json:"id"`
	Alias         string `json:"alias"`
	Status        string `json:"status"`
	LiveVerified  bool   `json:"live_verified"`
	Scope         string `json:"scope"`
	ProviderName  string `json:"provider_name"`
	ProviderModel string `json:"provider_model"`
	Protocol      string `json:"protocol"`
	ClaudeVersion string `json:"claude_version,omitempty"`
	StartedAt     string `json:"started_at"`
	CompletedAt   string `json:"completed_at,omitempty"`
}

type conformanceRunOptions struct {
	claude           bool
	includeAnthropic bool
	jsonOutput       bool
	all              bool
}

func newConformanceCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{Use: "conformance", Short: "Run and inspect compatibility checks"}
	runOptions := conformanceRunOptions{}
	run := &cobra.Command{
		Use:   "run [alias]",
		Short: "Run provider checks through the production gateway",
		Args: func(cmd *cobra.Command, args []string) error {
			if runOptions.all {
				if len(args) != 0 {
					return fmt.Errorf("conformance run accepts either one alias or --all, not both")
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; use ccr conformance run <alias> or ccr conformance run --all")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if runOptions.includeAnthropic && !runOptions.claude {
				return fmt.Errorf("--include-anthropic requires --claude")
			}
			if runOptions.all {
				return runConformanceAll(ctx, cmd, opts, deps, runOptions)
			}
			return runConformance(ctx, cmd, opts, deps, args[0], runOptions)
		},
	}
	run.Flags().BoolVar(&runOptions.claude, "claude", false, "Also verify the installed Claude Code CLI")
	run.Flags().BoolVar(&runOptions.includeAnthropic, "include-anthropic", false, "Include first-party Anthropic in the Claude CLI matrix")
	run.Flags().BoolVar(&runOptions.jsonOutput, "json", false, "Emit schema-versioned JSON")
	run.Flags().BoolVar(&runOptions.all, "all", false, "Run conformance for every registered non-blocked routable model alias")
	cmd.AddCommand(run, newConformanceListCommand(ctx, opts))
	return cmd
}

func runConformance(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, alias string, options conformanceRunOptions) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	model, err := s.GetModel(ctx, alias)
	if err != nil {
		return err
	}
	if model.Status == "blocked" {
		return fmt.Errorf("model alias %q is blocked and cannot be tested", alias)
	}
	provider, err := s.GetProvider(ctx, model.ProviderName)
	if err != nil {
		return err
	}
	if providers.IsProviderControlModel(provider.Type, model.ProviderModel) {
		return fmt.Errorf("model alias %q targets provider control model %q and cannot be tested", alias, model.ProviderModel)
	}
	if _, validationErr := validateProviderConfigAndSecret(ctx, deps, provider); validationErr != nil {
		return validationErr
	}
	caps := effectiveProviderCapabilities(provider)
	fmt.Fprintf(cmd.ErrOrStderr(), "Conformance target: alias=%s provider=%s model=%s protocol=%s\n",
		alias, provider.Name, model.ProviderModel, caps.Protocol)
	runID, err := s.CreateConformanceRun(ctx, store.ConformanceRun{
		Alias: alias, Scope: conformanceScope(options), ProviderName: provider.Name,
		ProviderModel: model.ProviderModel, Protocol: caps.Protocol,
	})
	if err != nil {
		return err
	}
	result, runErr := conformancecheck.RunProvider(ctx, conformancecheck.Config{
		Store: s, Secrets: deps.Secrets, Alias: alias,
	})
	if runErr != nil {
		_ = s.CompleteConformanceRun(context.WithoutCancel(ctx), runID, "failed", false, "provider suite could not start")
		return runErr
	}
	if options.claude {
		result.Checks = append(result.Checks, runClaudeConformance(ctx, opts, deps, s, alias, options.includeAnthropic))
	}
	document, persistErr := persistConformanceResult(ctx, s, runID, result, options)
	if persistErr != nil {
		return persistErr
	}
	if options.jsonOutput {
		if err := writeVersionedJSON(cmd.OutOrStdout(), document); err != nil {
			return err
		}
	} else {
		writeHumanConformance(cmd, document)
	}
	if document.Status != conformancecheck.StatusPassed {
		return fmt.Errorf("conformance failed for alias %q", alias)
	}
	return nil
}

func persistConformanceResult(ctx context.Context, s *store.Store, runID int64, result conformancecheck.Result, options conformanceRunOptions) (conformanceDocument, error) {
	document := conformanceDocument{
		SchemaVersion: 1, RunID: runID, Alias: result.Alias,
		ProviderName: result.ProviderName, ProviderModel: result.ProviderModel,
		Protocol: result.Protocol, Scope: conformanceScope(options),
		Status: conformancecheck.StatusPassed, LiveVerified: true,
		Checks: []conformanceCheckView{},
	}
	for _, check := range result.Checks {
		if check.Status == conformancecheck.StatusFailed {
			document.Status = conformancecheck.StatusFailed
			document.LiveVerified = false
		}
		view := conformanceCheckView{
			Name: check.Name, Status: check.Status,
			LatencyMS: check.Latency.Milliseconds(), Evidence: check.Evidence,
			FailureKind: check.FailureKind, HTTPStatus: check.HTTPStatus,
			ProviderHTTPStatus: check.ProviderHTTPStatus,
		}
		document.Checks = append(document.Checks, view)
		if _, err := s.AddConformanceCheck(ctx, store.ConformanceCheck{
			RunID: runID, Name: view.Name, Status: view.Status,
			LatencyMS: view.LatencyMS, Evidence: view.Evidence,
		}); err != nil {
			return conformanceDocument{}, err
		}
	}
	details := fmt.Sprintf("checks=%d scope=%s", len(document.Checks), document.Scope)
	if err := s.CompleteConformanceRun(ctx, runID, document.Status, document.LiveVerified, details); err != nil {
		return conformanceDocument{}, err
	}
	return document, nil
}

func conformanceScope(options conformanceRunOptions) string {
	if options.claude {
		return "claude"
	}
	return "provider"
}

func writeHumanConformance(cmd *cobra.Command, document conformanceDocument) {
	fmt.Fprintf(cmd.OutOrStdout(), "Conformance run %d for %q: %s\n", document.RunID, document.Alias, document.Status)
	for _, check := range document.Checks {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\t%dms\t%s\n", check.Name, check.Status, check.LatencyMS, check.Evidence)
	}
}

func newConformanceListCommand(ctx context.Context, opts *options) *cobra.Command {
	var jsonOutput bool
	var limit int
	cmd := &cobra.Command{
		Use:   "list [alias]",
		Short: "List persisted conformance runs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 1000 {
				return fmt.Errorf("--limit must be between 1 and 1000")
			}
			alias := ""
			if len(args) == 1 {
				if err := validateName("model alias", args[0]); err != nil {
					return err
				}
				alias = args[0]
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			runs, err := s.ListConformanceRuns(ctx, alias, limit)
			if err != nil {
				return err
			}
			if jsonOutput {
				views := make([]conformanceRunView, 0, len(runs))
				for index := range runs {
					views = append(views, newConformanceRunView(runs[index]))
				}
				return writeVersionedJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int                  `json:"schema_version"`
					Runs          []conformanceRunView `json:"runs"`
				}{SchemaVersion: 1, Runs: views})
			}
			for index := range runs {
				run := &runs[index]
				fmt.Fprintf(cmd.OutOrStdout(), "%d\talias=%s\tscope=%s\tstatus=%s\tlive=%t\tcompleted=%s\n",
					run.ID, run.Alias, run.Scope, run.Status, run.LiveVerified, run.CompletedAt)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum runs to return (1-1000)")
	return cmd
}

func newConformanceRunView(run store.ConformanceRun) conformanceRunView {
	return conformanceRunView{
		ID: run.ID, Alias: run.Alias, Status: run.Status, LiveVerified: run.LiveVerified,
		Scope: run.Scope, ProviderName: run.ProviderName, ProviderModel: run.ProviderModel,
		Protocol: run.Protocol, ClaudeVersion: run.ClaudeVersion,
		StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
	}
}
