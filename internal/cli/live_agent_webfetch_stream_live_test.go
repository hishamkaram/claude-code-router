//go:build live

package cli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func assertConfiguredProviderAgentWebFetchProbe(t *testing.T, ctx context.Context, out, errOut, modelAlias, authMode string) {
	t.Helper()
	assertConfiguredProviderProbeWithAuthMode(t, out, errOut, modelAlias, authMode)
	evidence, err := parseLiveAgentWebFetchStream(out)
	if err != nil {
		failLiveRealOutput(t, "configured provider stream-json output is invalid", out, errOut)
		return
	}
	if !liveAgentWebFetchEvidenceComplete(evidence) {
		failLiveRealOutput(t, "configured provider stream does not prove one completed Agent successfully WebFetched example.com", out, errOut)
		return
	}
	assertConfiguredProviderAgentRecorded(t, ctx, out, errOut, modelAlias)
}

func assertConfiguredProviderAgentRecorded(t *testing.T, ctx context.Context, out, errOut, modelAlias string) {
	t.Helper()
	launchID, err := parseLiveLaunchID(out + "\n" + errOut)
	if err != nil {
		failLiveRealOutput(t, "configured provider launch diagnostics do not identify its launch", out, errOut)
		return
	}
	args := make([]string, 0, 5)
	if dbPath := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_DB")); dbPath != "" {
		args = append(args, "--db", dbPath)
	}
	args = append(args, "agents", "--launch", strconv.FormatInt(launchID, 10), "--json")
	agentsOut, agentsErr, err := runLiveCommand(ctx, Dependencies{}, args...)
	if err != nil {
		failLiveRealCommand(t, "configured provider agent inspection", err, agentsOut, agentsErr)
		return
	}
	completed, err := completedGeneralPurposeAgentOnAlias(agentsOut, modelAlias)
	if err != nil {
		failLiveRealOutput(t, "configured provider agent listing is invalid", agentsOut, agentsErr)
		return
	}
	if !completed {
		failLiveRealOutput(t, "CCR did not record a completed general-purpose agent on the selected alias", agentsOut, agentsErr)
	}
}
