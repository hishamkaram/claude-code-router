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
	statuslineGatewayURLEnv  = "CCR_GATEWAY_URL"
	statuslineTokenEnv       = "CCR_OBSERVER_TOKEN"
	statuslineRequestTimeout = 2 * time.Second
)

func newStatuslineCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__statusline",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			line, err := fetchStatusline(cmd.Context(), os.Getenv(statuslineGatewayURLEnv), os.Getenv(statuslineTokenEnv))
			if err == nil && line != "" {
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
		},
	}
}

func fetchStatusline(ctx context.Context, gatewayURL, token string) (string, error) {
	endpoint, err := statuslineEndpoint(gatewayURL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("status line observer token is required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, statuslineRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("creating status line request: %w", err)
	}
	req.Header.Set(observerTokenHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting runtime status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runtime status returned HTTP %d", resp.StatusCode)
	}
	var snapshot session.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&snapshot); err != nil {
		return "", fmt.Errorf("decoding runtime status: %w", err)
	}
	return formatStatusline(snapshot), nil
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

func formatStatusline(snapshot session.Snapshot) string {
	parts := []string{"CCR"}
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
