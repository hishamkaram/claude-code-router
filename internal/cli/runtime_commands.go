package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func newLaunchCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	var modelAlias string
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch Claude Code through the local router",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLaunch(ctx, cmd, opts, deps, modelAlias)
		},
	}
	cmd.Flags().StringVar(&modelAlias, "model", "", "Optional model alias to validate before launch")
	return cmd
}

func runLaunch(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, modelAlias string) error {
	if modelAlias != "" {
		if validateErr := validateName("model alias", modelAlias); validateErr != nil {
			return validateErr
		}
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	resolvedModelAlias, err := resolveLaunchModelAlias(ctx, deps, s, modelAlias)
	if err != nil {
		return err
	}

	token, err := gateway.NewToken()
	if err != nil {
		return err
	}
	server, err := gateway.Start(ctx, gateway.Config{
		Store:            s,
		Secrets:          deps.Secrets,
		Token:            token,
		ForcedModelAlias: resolvedModelAlias,
	})
	if err != nil {
		return err
	}
	defer shutdownGateway(ctx, server)

	claudeArgs := []string{"--tools", ""}
	env := []string{
		"ANTHROPIC_BASE_URL=" + server.URL(),
		"ANTHROPIC_AUTH_TOKEN=" + token,
		"CLAUDE_CODE_USE_GATEWAY=1",
		"CLAUDE_CODE_SIMPLE=1",
	}
	outputLock := &sync.Mutex{}
	out := launchProcessWriter(cmd.OutOrStdout(), outputLock)
	errOut := launchProcessWriter(cmd.ErrOrStderr(), outputLock)
	process, err := deps.Launcher.Start(ctx, claudeArgs, env, cmd.InOrStdin(), out, errOut)
	if err != nil {
		return fmt.Errorf("launching Claude Code through the gateway: %w", err)
	}
	sessionID, err := s.AddSession(ctx, store.Session{
		GatewayURL: server.URL(),
		PID:        process.PID(),
		ModelAlias: resolvedModelAlias,
	})
	if err != nil {
		if cleanupErr := cleanupStartedClaudeProcess(process); cleanupErr != nil {
			return errors.Join(fmt.Errorf("recording launch session: %w", err), fmt.Errorf("cleaning up Claude Code process: %w", cleanupErr))
		}
		return fmt.Errorf("recording launch session: %w", err)
	}
	fmt.Fprintf(out, "Claude Code launched through %s (session=%d pid=%d)\n", server.URL(), sessionID, process.PID())
	fmt.Fprintf(out, "Default route for Claude Code model names is model alias %q; configured request aliases are honored.\n", resolvedModelAlias)
	fmt.Fprintln(out, "Gateway accepts only the generated local ANTHROPIC_AUTH_TOKEN for this process.")
	fmt.Fprintln(out, "Gateway launch uses Claude Code simple mode with tools disabled for this OpenAI-compatible text route.")
	return process.Wait()
}

func launchProcessWriter(writer io.Writer, lock *sync.Mutex) io.Writer {
	if _, ok := writer.(*os.File); ok {
		return writer
	}
	return synchronizedWriter{lock: lock, writer: writer}
}

type synchronizedWriter struct {
	lock   *sync.Mutex
	writer io.Writer
}

func (w synchronizedWriter) Write(p []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.writer.Write(p)
}

func resolveLaunchModelAlias(ctx context.Context, deps Dependencies, s *store.Store, requested string) (string, error) {
	if requested != "" {
		if _, _, _, validateErr := validateRoutableModelAliasTargetWithStore(ctx, deps, s, requested, true); validateErr != nil {
			return "", validateErr
		}
		return requested, nil
	}
	aliases, err := routableModelAliases(ctx, s)
	if err != nil {
		return "", err
	}
	switch len(aliases) {
	case 0:
		return "", fmt.Errorf("ccr launch requires one routable OpenAI-compatible model alias; add one with ccr model add or pass --model <alias>")
	case 1:
		if _, _, _, validateErr := validateRoutableModelAliasTargetWithStore(ctx, deps, s, aliases[0], true); validateErr != nil {
			return "", validateErr
		}
		return aliases[0], nil
	default:
		return "", fmt.Errorf("ccr launch requires --model when multiple routable model aliases exist: %s", strings.Join(aliases, ", "))
	}
}

func routableModelAliases(ctx context.Context, s *store.Store) ([]string, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(models))
	for _, model := range models {
		if model.Status == "blocked" {
			continue
		}
		provider, err := s.GetProvider(ctx, model.ProviderName)
		if err != nil {
			return nil, err
		}
		if providers.SupportsOpenAICompatibleRouting(provider.Type) {
			aliases = append(aliases, model.Alias)
		}
	}
	slices.Sort(aliases)
	return aliases, nil
}

func cleanupStartedClaudeProcess(process ClaudeProcess) error {
	if process == nil {
		return nil
	}
	stopErr := process.Stop()
	if stopErr != nil {
		return stopErr
	}
	_ = process.Wait()
	return nil
}

func shutdownGateway(parent context.Context, server *gateway.Server) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func newSessionsCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List tracked Claude Code sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			sessions, err := s.ListSessions(ctx)
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No launch sessions tracked.")
				return nil
			}
			for _, session := range sessions {
				model := session.ModelAlias
				if model == "" {
					model = "(request-selected)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tpid=%d\tstatus=%s\tgateway=%s\tmodel=%s\tcreated=%s\n", session.ID, session.PID, processStatus(session.PID), session.GatewayURL, model, session.CreatedAt)
			}
			return nil
		},
	}
}

func newAgentsCommand(ctx context.Context, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "List tracked Claude Code agents and workers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			agents, err := s.ListAgents(ctx)
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No agents observed.")
				return nil
			}
			for _, agent := range agents {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\tsession=%d\tname=%s\tkind=%s\tmodel=%s\tstatus=%s\tcreated=%s\n", agent.ID, agent.SessionID, agent.Name, agent.Kind, agent.ModelAlias, agent.Status, agent.CreatedAt)
			}
			return nil
		},
	}
}

func newConformanceCommand(ctx context.Context, opts *options, deps Dependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conformance",
		Short: "Run model compatibility checks",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "run <alias>",
		Short: "Run conformance checks for a model alias",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("model alias is required; example: ccr conformance run qwen")
			}
			return validateName("model alias", args[0])
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConformance(ctx, cmd, opts, deps, args[0])
		},
	})
	return cmd
}

func runConformance(ctx context.Context, cmd *cobra.Command, opts *options, deps Dependencies, alias string) error {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)

	model, provider, discovered, err := validateRoutableModelAliasTargetWithStore(ctx, deps, s, alias, true)
	if err != nil {
		return err
	}
	details := fmt.Sprintf("provider=%s type=%s model=%s compat=%s", provider.Name, provider.Type, model.ProviderModel, model.Status)
	if providers.SupportsOpenAIModelDiscovery(provider.Type) {
		details = fmt.Sprintf("%s discovered_models=%d", details, discovered)
	}
	recordID, err := s.AddConformanceRecord(ctx, store.ConformanceRecord{
		Alias:        alias,
		Status:       "local-verified",
		LiveVerified: false,
		Details:      details,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Conformance record %d for %q: local-verified\n", recordID, alias)
	fmt.Fprintln(cmd.OutOrStdout(), "Live runtime status: unverified until live Claude Code E2E passes.")
	return nil
}

func processStatus(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return "unknown"
	}
	err = process.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return "running"
	case errors.Is(err, os.ErrProcessDone):
		return "exited"
	case errors.Is(err, syscall.EPERM):
		return "running"
	default:
		return "exited"
	}
}
