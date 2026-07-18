package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

type providerView struct {
	Name                   string `json:"name"`
	Type                   string `json:"type"`
	BaseURL                string `json:"base_url"`
	Protocol               string `json:"protocol"`
	Mode                   string `json:"mode"`
	SupportsTools          bool   `json:"supports_tools"`
	SupportsStreaming      bool   `json:"supports_streaming"`
	SupportsThinking       bool   `json:"supports_thinking"`
	SupportsModelDiscovery bool   `json:"supports_model_discovery"`
	SupportsCountTokens    bool   `json:"supports_count_tokens"`
}

type modelView struct {
	Alias         string `json:"alias"`
	ProviderName  string `json:"provider_name"`
	ProviderModel string `json:"provider_model"`
	Compatibility string `json:"compatibility"`
}

type launchView struct {
	ID              int64  `json:"id"`
	GatewayURL      string `json:"gateway_url"`
	PID             int    `json:"pid"`
	ModelAlias      string `json:"model_alias,omitempty"`
	State           string `json:"state"`
	ProcessState    string `json:"process_state"`
	LifecycleState  string `json:"lifecycle_state"`
	StatuslineState string `json:"statusline_state"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at,omitempty"`
	EndedAt         string `json:"ended_at,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	EndReason       string `json:"end_reason,omitempty"`
}

type claudeSessionView struct {
	ID                  int64  `json:"id"`
	LaunchID            int64  `json:"launch_id"`
	ClaudeSessionID     string `json:"claude_session_id"`
	Source              string `json:"source"`
	State               string `json:"state"`
	ActiveRouteKind     string `json:"active_route_kind,omitempty"`
	ActiveModelAlias    string `json:"active_model_alias,omitempty"`
	ActiveProviderName  string `json:"active_provider_name,omitempty"`
	ActiveProviderModel string `json:"active_provider_model,omitempty"`
	StartedAt           string `json:"started_at"`
	LastSeenAt          string `json:"last_seen_at"`
	EndedAt             string `json:"ended_at,omitempty"`
	EndReason           string `json:"end_reason,omitempty"`
}

type agentView struct {
	ID         int64  `json:"id"`
	LaunchID   int64  `json:"launch_id,omitempty"`
	SessionID  int64  `json:"session_id,omitempty"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	ModelAlias string `json:"model_alias,omitempty"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	EndedAt    string `json:"ended_at,omitempty"`
}

type taskView struct {
	ID           int64  `json:"id"`
	LaunchID     int64  `json:"launch_id"`
	SessionID    int64  `json:"session_id"`
	ExternalID   string `json:"external_id"`
	TeammateName string `json:"teammate_name,omitempty"`
	TeamName     string `json:"team_name,omitempty"`
	ModelAlias   string `json:"model_alias,omitempty"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

func newProviderView(provider store.Provider) providerView {
	caps := effectiveProviderCapabilities(provider)
	return providerView{
		Name: provider.Name, Type: provider.Type, BaseURL: provider.BaseURL,
		Protocol: caps.Protocol, Mode: caps.Mode, SupportsTools: caps.SupportsTools,
		SupportsStreaming: caps.SupportsStreaming, SupportsThinking: caps.SupportsThinking,
		SupportsModelDiscovery: caps.SupportsModelDiscovery,
		SupportsCountTokens:    caps.SupportsCountTokens,
	}
}

func newModelView(model store.Model) modelView {
	return modelView{
		Alias: model.Alias, ProviderName: model.ProviderName,
		ProviderModel: model.ProviderModel, Compatibility: model.Status,
	}
}

func newLaunchView(launch store.Launch) launchView {
	processState := "not-started"
	if launch.EndedAt != "" || launch.State == "completed" || launch.State == "failed" || launch.State == "canceled" {
		processState = "exited"
	} else if launch.PID > 0 {
		processState = processStatus(launch.PID)
	}
	return launchView{
		ID: launch.ID, GatewayURL: launch.GatewayURL, PID: launch.PID,
		ModelAlias: launch.ModelAlias, State: launch.State, ProcessState: processState,
		LifecycleState: launch.LifecycleState, StatuslineState: launch.StatuslineState,
		CreatedAt: launch.CreatedAt, StartedAt: launch.StartedAt, EndedAt: launch.EndedAt,
		ExitCode: launch.ExitCode, EndReason: launch.EndReason,
	}
}

func newClaudeSessionView(session store.ClaudeSession) claudeSessionView {
	return claudeSessionView{
		ID: session.ID, LaunchID: session.LaunchID, ClaudeSessionID: session.ClaudeSessionID,
		Source: session.Source, State: session.State, ActiveRouteKind: session.ActiveRouteKind,
		ActiveModelAlias: session.ActiveModelAlias, ActiveProviderName: session.ActiveProviderName,
		ActiveProviderModel: session.ActiveProviderModel, StartedAt: session.StartedAt,
		LastSeenAt: session.LastSeenAt, EndedAt: session.EndedAt, EndReason: session.EndReason,
	}
}

func newAgentView(agent store.Agent) agentView {
	return agentView{
		ID: agent.ID, LaunchID: agent.LaunchID, SessionID: agent.SessionID,
		ExternalID: agent.ExternalID, Name: agent.Name, Kind: agent.Kind,
		ModelAlias: agent.ModelAlias, Status: agent.Status, CreatedAt: agent.CreatedAt,
		UpdatedAt: agent.UpdatedAt, EndedAt: agent.EndedAt,
	}
}

func newTaskView(task store.Task) taskView {
	return taskView{
		ID: task.ID, LaunchID: task.LaunchID, SessionID: task.SessionID,
		ExternalID: task.ExternalID, TeammateName: task.TeammateName,
		TeamName: task.TeamName, ModelAlias: task.ModelAlias, Status: task.Status,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, CompletedAt: task.CompletedAt,
	}
}

func writeVersionedJSON(out io.Writer, payload any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return fmt.Errorf("writing JSON output: %w", err)
	}
	return nil
}
