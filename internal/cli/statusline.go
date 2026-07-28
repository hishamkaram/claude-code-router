package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/session"
)

const (
	statuslineGatewayURLEnv    = "CCR_GATEWAY_URL"
	statuslineTokenEnv         = "CCR_OBSERVER_TOKEN"
	statuslineClaudeAccountEnv = "CCR_CLAUDE_ACCOUNT"
	statuslineRequestTimeout   = 2 * time.Second
)

func newStatuslineCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__statusline",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			line, err := fetchStatusline(
				cmd.Context(),
				os.Getenv(statuslineGatewayURLEnv),
				os.Getenv(statuslineTokenEnv),
				os.Getenv(statuslineClaudeAccountEnv),
			)
			if err == nil && line != "" {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
		},
	}
}

func newStatuslineAccountCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__statusline-account",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			account := strings.TrimSpace(os.Getenv(statuslineClaudeAccountEnv))
			gatewayURL := strings.TrimSpace(os.Getenv(statuslineGatewayURLEnv))
			token := strings.TrimSpace(os.Getenv(statuslineTokenEnv))
			if gatewayURL == "" || token == "" {
				writeStatuslineAccount(cmd.OutOrStdout(), account)
				return
			}
			status, err := fetchRuntimeStatus(
				cmd.Context(),
				gatewayURL,
				token,
			)
			if err == nil && strings.TrimSpace(status.Auth.ActiveClaudeAccount) != "" {
				account = status.Auth.ActiveClaudeAccount
			} else {
				account = "unknown"
			}
			writeStatuslineAccount(cmd.OutOrStdout(), account)
		},
	}
}

func writeStatuslineAccount(out io.Writer, account string) {
	if account = strings.TrimSpace(account); account != "" {
		fmt.Fprintln(out, account)
	}
}

type statuslineAuthStatus struct {
	Mode                string `json:"mode"`
	ActiveClaudeAccount string `json:"active_claude_account"`
}

type statuslineRuntimeStatus struct {
	session.Snapshot
	Auth statuslineAuthStatus `json:"auth"`
}

func fetchStatusline(ctx context.Context, gatewayURL, token, claudeAccount string) (string, error) {
	status, err := fetchRuntimeStatus(ctx, gatewayURL, token)
	if err != nil {
		return "", err
	}
	if account := strings.TrimSpace(status.Auth.ActiveClaudeAccount); account != "" {
		claudeAccount = account
	}
	return formatStatusline(status.Snapshot, claudeAccount), nil
}

func fetchRuntimeStatus(ctx context.Context, gatewayURL, token string) (statuslineRuntimeStatus, error) {
	endpoint, err := statuslineEndpoint(gatewayURL)
	if err != nil {
		return statuslineRuntimeStatus{}, err
	}
	if strings.TrimSpace(token) == "" {
		return statuslineRuntimeStatus{}, fmt.Errorf("status line observer token is required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, statuslineRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return statuslineRuntimeStatus{}, fmt.Errorf("creating status line request: %w", err)
	}
	req.Header.Set(observerTokenHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return statuslineRuntimeStatus{}, fmt.Errorf("requesting runtime status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statuslineRuntimeStatus{}, fmt.Errorf("runtime status returned HTTP %d", resp.StatusCode)
	}
	var status statuslineRuntimeStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status); err != nil {
		return statuslineRuntimeStatus{}, fmt.Errorf("decoding runtime status: %w", err)
	}
	return status, nil
}

func statuslineEndpoint(gatewayURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return "", fmt.Errorf("status line gateway URL is invalid")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("status line gateway URL must use a loopback address")
	}
	parsed.Path = "/internal/v1/status"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func formatStatusline(snapshot session.Snapshot, claudeAccount string) string {
	parts := []string{"CCR"}
	if claudeAccount = strings.TrimSpace(claudeAccount); claudeAccount != "" {
		parts = append(parts, "account="+claudeAccount, "limits=unknown")
	}
	if snapshot.Route.ModelAlias == "" {
		parts = append(parts, "waiting for route")
	} else {
		parts = append(parts, snapshot.Route.ModelAlias)
		providerModel := snapshot.Route.ProviderModel
		if snapshot.Route.ProviderName != "" && providerModel != "" {
			providerModel = snapshot.Route.ProviderName + "/" + providerModel
		}
		if providerModel != "" {
			parts = append(parts, providerModel)
		}
	}
	if snapshot.ActiveAgents > 0 {
		parts = append(parts, fmt.Sprintf("agents %d", snapshot.ActiveAgents))
	}
	if snapshot.ActiveTasks > 0 {
		parts = append(parts, fmt.Sprintf("tasks %d", snapshot.ActiveTasks))
	}
	if !snapshot.Observability.Healthy {
		parts = append(parts, "history degraded")
	}
	if snapshot.LifecycleEnabled && snapshot.LifecycleState == "error" {
		parts = append(parts, "lifecycle degraded")
	}
	return strings.Join(parts, " | ")
}
