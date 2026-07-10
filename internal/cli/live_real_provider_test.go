//go:build live

package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/liveclaude"
)

func TestLiveConfiguredProviderAutoModeAgentWebFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	prompt := `Use the Agent tool to launch one general-purpose subagent. The subagent must use WebFetch on https://example.com and then return exactly CCR_LIVE_REAL_WEBFETCH_CHILD_OK. After the subagent finishes, reply exactly CCR_LIVE_REAL_WEBFETCH_PARENT_OK. Do not use Bash or shell.`
	out, errOut, modelAlias := runConfiguredProviderProbe(t, ctx, prompt)
	assertConfiguredProviderProbe(t, out, errOut, modelAlias, "CCR_LIVE_REAL_WEBFETCH_PARENT_OK")
}

func TestLiveConfiguredProviderAutoModeWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := liveclaude.Check(ctx); err != nil {
		t.Skipf("live Claude Code unavailable: %v", err)
	}
	prompt := `Use a workflow now. The workflow should run one worker that returns exactly CCR_LIVE_REAL_WORKFLOW_CHILD_OK. After the workflow finishes, report exactly CCR_LIVE_REAL_WORKFLOW_PARENT_OK with the worker result. Do not use Bash or shell.`
	out, errOut, modelAlias := runConfiguredProviderProbe(t, ctx, prompt)
	assertConfiguredProviderProbe(t, out, errOut, modelAlias, "CCR_LIVE_REAL_WORKFLOW_PARENT_OK", "CCR_LIVE_REAL_WORKFLOW_CHILD_OK")
}

func runConfiguredProviderProbe(t *testing.T, ctx context.Context, prompt string) (string, string, string) {
	t.Helper()
	if os.Getenv("CCR_LIVE_CONFIGURED_PROVIDER") != "1" {
		t.Skip("set CCR_LIVE_CONFIGURED_PROVIDER=1 to run against the configured real provider")
	}
	modelAlias := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_MODEL_ALIAS"))
	if modelAlias == "" {
		modelAlias = "glm-5-2"
	}
	args := []string{"launch", "--model", modelAlias, "--print", "--auth-mode", "gateway-token", "--permission-mode", "auto"}
	if dbPath := strings.TrimSpace(os.Getenv("CCR_LIVE_CONFIGURED_DB")); dbPath != "" {
		args = append([]string{"--db", dbPath}, args...)
	}
	out, errOut, err := runLiveCommand(ctx, Dependencies{In: strings.NewReader(prompt + "\n")}, args...)
	if err != nil {
		t.Fatalf("configured provider launch error = %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	return out, errOut, modelAlias
}

func assertConfiguredProviderProbe(t *testing.T, out, errOut, modelAlias string, sentinels ...string) {
	t.Helper()
	combined := out + "\n" + errOut
	for _, sentinel := range sentinels {
		if !strings.Contains(out, sentinel) {
			t.Fatalf("configured provider output missing %q\nstdout:\n%s\nstderr:\n%s", sentinel, out, errOut)
		}
	}
	for _, forbidden := range []string{"temporarily unavailable", "API Error"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("configured provider output contains %q\nstdout:\n%s\nstderr:\n%s", forbidden, out, errOut)
		}
	}
	for _, want := range []string{
		`Selected ccr model alias "` + modelAlias + `"`,
		"Gateway accepts only the generated local ANTHROPIC_AUTH_TOKEN",
		"Original Anthropic subscription login and Anthropic API-key auth are not active",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("configured provider diagnostics missing %q\nstdout:\n%s\nstderr:\n%s", want, out, errOut)
		}
	}
}
