package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	conformancecheck "github.com/hishamkaram/claude-code-router/internal/conformance"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const maxConcurrentConformanceProviders = 4

type conformanceAllDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Scope         string                 `json:"scope"`
	Status        string                 `json:"status"`
	Total         int                    `json:"total"`
	Passed        int                    `json:"passed"`
	Failed        int                    `json:"failed"`
	Skipped       int                    `json:"skipped"`
	Results       []conformanceAllResult `json:"results"`
}

type conformanceAllResult struct {
	Alias  string               `json:"alias"`
	Status string               `json:"status"`
	Run    *conformanceDocument `json:"run,omitempty"`
	Error  string               `json:"error,omitempty"`
}

type conformanceTarget struct {
	model    store.Model
	provider store.Provider
	runID    int64
}

type conformanceExecution struct {
	target conformanceTarget
	result conformancecheck.Result
	err    error
}

func runConformanceAll(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, options conformanceRunOptions) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	targets, aggregate, err := prepareConformanceTargets(ctx, cmd.ErrOrStderr(), s, options)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		finalizeConformanceAll(&aggregate)
		aggregate.Status = conformancecheck.StatusFailed
		if err := writeConformanceAllResult(cmd, aggregate, options.jsonOutput); err != nil {
			return err
		}
		return fmt.Errorf("conformance found no runnable non-blocked model aliases")
	}
	executions := runProviderConformanceTargets(ctx, deps, s, targets)
	for index := range executions {
		execution := &executions[index]
		if execution.err == nil && options.claude {
			execution.result.Checks = append(execution.result.Checks,
				runClaudeConformance(ctx, opts, deps, s, execution.target.model.Alias, options.includeAnthropic))
		}
		result, persistErr := persistConformanceExecution(ctx, s, *execution, options)
		if persistErr != nil {
			return persistErr
		}
		aggregate.Results = append(aggregate.Results, result)
	}
	sort.Slice(aggregate.Results, func(i, j int) bool { return aggregate.Results[i].Alias < aggregate.Results[j].Alias })
	finalizeConformanceAll(&aggregate)
	if err := writeConformanceAllResult(cmd, aggregate, options.jsonOutput); err != nil {
		return err
	}
	if aggregate.Failed > 0 {
		return fmt.Errorf("conformance failed for %d of %d aliases", aggregate.Failed, aggregate.Total)
	}
	return nil
}

func writeConformanceAllResult(cmd *cobra.Command, document conformanceAllDocument, jsonOutput bool) error {
	if jsonOutput {
		return writeVersionedJSON(cmd.OutOrStdout(), document)
	}
	writeHumanConformanceAll(cmd.OutOrStdout(), document)
	return nil
}

func prepareConformanceTargets(ctx context.Context, diagnostics io.Writer, s *store.Store, options conformanceRunOptions) ([]conformanceTarget, conformanceAllDocument, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, conformanceAllDocument{}, err
	}
	aggregate := conformanceAllDocument{
		SchemaVersion: 1, Scope: conformanceScope(options), Status: conformancecheck.StatusPassed,
		Results: []conformanceAllResult{},
	}
	targets := make([]conformanceTarget, 0, len(models))
	for index := range models {
		model := &models[index]
		if model.Status == "blocked" {
			aggregate.Results = append(aggregate.Results, conformanceAllResult{
				Alias: model.Alias, Status: "skipped", Error: "model alias is blocked",
			})
			continue
		}
		provider, err := s.GetProvider(ctx, model.ProviderName)
		if err != nil {
			return nil, conformanceAllDocument{}, err
		}
		caps := effectiveProviderCapabilities(provider)
		fmt.Fprintf(diagnostics, "Conformance target: alias=%s provider=%s model=%s protocol=%s\n",
			model.Alias, provider.Name, model.ProviderModel, caps.Protocol)
		runID, err := s.CreateConformanceRun(ctx, store.ConformanceRun{
			Alias: model.Alias, Scope: conformanceScope(options), ProviderName: provider.Name,
			ProviderModel: model.ProviderModel, Protocol: caps.Protocol,
		})
		if err != nil {
			return nil, conformanceAllDocument{}, err
		}
		targets = append(targets, conformanceTarget{model: *model, provider: provider, runID: runID})
	}
	return targets, aggregate, nil
}

func runProviderConformanceTargets(ctx context.Context, deps Dependencies, s *store.Store, targets []conformanceTarget) []conformanceExecution {
	if len(targets) == 0 {
		return nil
	}
	jobs := make(chan conformanceTarget, len(targets))
	results := make(chan conformanceExecution, len(targets))
	for index := range targets {
		jobs <- targets[index]
	}
	close(jobs)
	workers := min(maxConcurrentConformanceProviders, len(targets))
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for target := range jobs {
				if _, err := validateProviderConfigAndSecret(ctx, deps, target.provider); err != nil {
					results <- conformanceExecution{target: target, err: err}
					continue
				}
				result, err := conformancecheck.RunProvider(ctx, conformancecheck.Config{
					Store: s, Secrets: deps.Secrets, Alias: target.model.Alias,
				})
				results <- conformanceExecution{target: target, result: result, err: err}
			}
		}()
	}
	for range workers {
		<-done
	}
	close(results)
	executions := make([]conformanceExecution, 0, len(targets))
	for result := range results {
		executions = append(executions, result)
	}
	sort.Slice(executions, func(i, j int) bool {
		return executions[i].target.model.Alias < executions[j].target.model.Alias
	})
	return executions
}

func persistConformanceExecution(ctx context.Context, s *store.Store, execution conformanceExecution, options conformanceRunOptions) (conformanceAllResult, error) {
	if execution.err != nil {
		execution.result = failedConformanceStartResult(execution.target, execution.err)
	}
	document, err := persistConformanceResult(ctx, s, execution.target.runID, execution.result, options)
	if err != nil {
		return conformanceAllResult{}, err
	}
	result := conformanceAllResult{Alias: execution.target.model.Alias, Status: document.Status, Run: &document}
	if execution.err != nil {
		result.Error = safeConformanceStartEvidence(execution.err)
	}
	return result, nil
}

func failedConformanceStartResult(target conformanceTarget, err error) conformancecheck.Result {
	now := time.Now().UTC()
	return conformancecheck.Result{
		Alias: target.model.Alias, ProviderName: target.provider.Name,
		ProviderModel: target.model.ProviderModel, Protocol: effectiveProviderCapabilities(target.provider).Protocol,
		Status: conformancecheck.StatusFailed, StartedAt: now, CompletedAt: now,
		Checks: []conformancecheck.Check{{
			Name: "suite_start", Status: conformancecheck.StatusFailed,
			FailureKind: "suite_start", Evidence: safeConformanceStartEvidence(err),
		}},
	}
}

func safeConformanceStartEvidence(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "conformance suite canceled or timed out before checks started"
	}
	evidence := strings.Join(strings.Fields(err.Error()), " ")
	if len(evidence) > 200 {
		evidence = evidence[:200]
	}
	return "conformance suite could not start: " + evidence
}

func finalizeConformanceAll(document *conformanceAllDocument) {
	document.Total = len(document.Results)
	for _, result := range document.Results {
		switch result.Status {
		case conformancecheck.StatusPassed:
			document.Passed++
		case conformancecheck.StatusFailed:
			document.Failed++
		case "skipped":
			document.Skipped++
		}
	}
	if document.Failed > 0 {
		document.Status = conformancecheck.StatusFailed
	}
}

func writeHumanConformanceAll(out io.Writer, document conformanceAllDocument) {
	for _, result := range document.Results {
		fmt.Fprintf(out, "%s: %s", result.Alias, result.Status)
		if result.Run != nil {
			fmt.Fprintf(out, " run=%d", result.Run.RunID)
		}
		if result.Error != "" {
			fmt.Fprintf(out, " reason=%s", result.Error)
		}
		fmt.Fprintln(out)
		if result.Run != nil {
			for _, check := range result.Run.Checks {
				if check.Status == conformancecheck.StatusFailed {
					fmt.Fprintf(out, "  %s: %s\n", check.Name, check.Evidence)
				}
			}
		}
	}
	fmt.Fprintf(out, "Conformance all: passed=%d failed=%d skipped=%d total=%d\n",
		document.Passed, document.Failed, document.Skipped, document.Total)
}
