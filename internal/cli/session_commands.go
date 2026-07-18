package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

type launchSessionsView struct {
	Launch   launchView          `json:"launch"`
	Sessions []claudeSessionView `json:"sessions"`
}

type sessionsDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Launches      []launchSessionsView `json:"launches"`
}

func newSessionsCommand(ctx context.Context, opts *options) *cobra.Command {
	var launchID int64
	var activeOnly bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List launches and hook-observed Claude sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if launchID < 0 {
				return fmt.Errorf("--launch must be positive")
			}
			document, err := loadSessionsDocument(ctx, opts, launchID, activeOnly)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeVersionedJSON(cmd.OutOrStdout(), document)
			}
			writeHumanSessions(cmd, document)
			return nil
		},
	}
	cmd.Flags().Int64Var(&launchID, "launch", 0, "Filter by launch ID")
	cmd.Flags().BoolVar(&activeOnly, "active", false, "Show only active launches and sessions")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func loadSessionsDocument(ctx context.Context, opts *options, launchID int64, activeOnly bool) (sessionsDocument, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return sessionsDocument{}, err
	}
	defer closeStore(s)
	document := sessionsDocument{SchemaVersion: 1, Launches: []launchSessionsView{}}
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		return sessionsDocument{}, err
	}
	found := launchID == 0
	for launchIndex := range launches {
		launch := &launches[launchIndex]
		if launchID != 0 && launch.ID != launchID {
			continue
		}
		found = true
		if activeOnly && launch.State != "running" {
			continue
		}
		sessions, err := s.ListClaudeSessions(ctx, launch.ID, activeOnly)
		if err != nil {
			return sessionsDocument{}, err
		}
		entry := launchSessionsView{Launch: newLaunchView(*launch), Sessions: []claudeSessionView{}}
		for sessionIndex := range sessions {
			entry.Sessions = append(entry.Sessions, newClaudeSessionView(sessions[sessionIndex]))
		}
		document.Launches = append(document.Launches, entry)
	}
	if !found {
		return sessionsDocument{}, fmt.Errorf("launch %d does not exist", launchID)
	}
	return document, nil
}

func writeHumanSessions(cmd *cobra.Command, document sessionsDocument) {
	if len(document.Launches) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No launch sessions tracked.")
		return
	}
	for entryIndex := range document.Launches {
		entry := &document.Launches[entryIndex]
		launch := entry.Launch
		model := launch.ModelAlias
		if model == "" {
			model = "(request-selected)"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Launch %d\tpid=%d\tstate=%s\tstatus=%s\tmodel=%s\tlifecycle=%s\tcreated=%s\n",
			launch.ID, launch.PID, launch.State, launch.ProcessState, model,
			launch.LifecycleState, launch.CreatedAt)
		if len(entry.Sessions) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "  No Claude lifecycle sessions observed.")
			continue
		}
		for sessionIndex := range entry.Sessions {
			observed := &entry.Sessions[sessionIndex]
			fmt.Fprintf(cmd.OutOrStdout(), "  Session %d\tclaude=%s\tstate=%s\tsource=%s\troute=%s\tprovider=%s/%s\n",
				observed.ID, observed.ClaudeSessionID, observed.State, observed.Source,
				observed.ActiveModelAlias, observed.ActiveProviderName, observed.ActiveProviderModel)
		}
	}
}

type agentsDocument struct {
	SchemaVersion int         `json:"schema_version"`
	Agents        []agentView `json:"agents"`
	Tasks         []taskView  `json:"tasks"`
}

func newAgentsCommand(ctx context.Context, opts *options) *cobra.Command {
	var launchID int64
	var sessionID int64
	var activeOnly bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List hook-observed subagents, teammates, and tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if launchID < 0 || sessionID < 0 {
				return fmt.Errorf("--launch and --session must be positive")
			}
			document, err := loadAgentsDocument(ctx, opts, launchID, sessionID, activeOnly)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeVersionedJSON(cmd.OutOrStdout(), document)
			}
			writeHumanAgents(cmd, document)
			return nil
		},
	}
	cmd.Flags().Int64Var(&launchID, "launch", 0, "Filter by launch ID")
	cmd.Flags().Int64Var(&sessionID, "session", 0, "Filter by numeric CCR session ID")
	cmd.Flags().BoolVar(&activeOnly, "active", false, "Show only active agents and tasks")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func loadAgentsDocument(ctx context.Context, opts *options, launchID, sessionID int64, activeOnly bool) (agentsDocument, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return agentsDocument{}, err
	}
	defer closeStore(s)
	agents, err := s.ListRuntimeAgents(ctx, launchID, sessionID, activeOnly)
	if err != nil {
		return agentsDocument{}, err
	}
	tasks, err := s.ListTasks(ctx, launchID, sessionID, activeOnly)
	if err != nil {
		return agentsDocument{}, err
	}
	document := agentsDocument{SchemaVersion: 1, Agents: []agentView{}, Tasks: []taskView{}}
	for index := range agents {
		document.Agents = append(document.Agents, newAgentView(agents[index]))
	}
	for index := range tasks {
		document.Tasks = append(document.Tasks, newTaskView(tasks[index]))
	}
	return document, nil
}

func writeHumanAgents(cmd *cobra.Command, document agentsDocument) {
	if len(document.Agents) == 0 && len(document.Tasks) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No agents observed. No tasks observed.")
		return
	}
	for index := range document.Agents {
		agent := &document.Agents[index]
		fmt.Fprintf(cmd.OutOrStdout(), "Agent %d\tlaunch=%d\tsession=%d\texternal=%s\tname=%s\tkind=%s\tmodel=%s\tstatus=%s\n",
			agent.ID, agent.LaunchID, agent.SessionID, agent.ExternalID, agent.Name,
			agent.Kind, agent.ModelAlias, agent.Status)
	}
	for index := range document.Tasks {
		task := &document.Tasks[index]
		fmt.Fprintf(cmd.OutOrStdout(), "Task %d\tlaunch=%d\tsession=%d\texternal=%s\tteammate=%s\tteam=%s\tmodel=%s\tstatus=%s\n",
			task.ID, task.LaunchID, task.SessionID, task.ExternalID, task.TeammateName,
			task.TeamName, task.ModelAlias, task.Status)
	}
}
