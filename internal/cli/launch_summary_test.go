package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLaunchSummaryUsesAdmittedProviderCompatibilityAfterMutation(t *testing.T) {
	server := newModelsServer(t, []string{"gpt-5"})
	database := filepath.Join(t.TempDir(), "ccr.db")
	if _, _, err := runCommand(t, "--db", database, "provider", "add", "litellm", "--base-url", server.URL, "--no-api-key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, "--db", database, "model", "add", "coder", "--provider", "litellm", "--model", "gpt-5"); err != nil {
		t.Fatal(err)
	}

	var admitted, mutated launchCompatibilitySummary
	launcher := &fakeLauncher{pid: os.Getpid()}
	out, _, err := runCommandWithDeps(t, Dependencies{
		Launcher: launcher,
		launchPrepared: func(resolved resolvedLaunch) error {
			if resolved.compatibility == nil {
				return fmt.Errorf("normal launch did not capture provider compatibility")
			}
			admitted = *resolved.compatibility

			ctx := context.Background()
			s, err := store.Open(ctx, database)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			provider, err := s.GetProvider(ctx, "litellm")
			if err != nil {
				return err
			}
			if current := snapshotLaunchCompatibility(provider); *current != admitted {
				return fmt.Errorf("admitted compatibility = %#v, current provider compatibility = %#v", admitted, *current)
			}

			provider.Protocol = "anthropic-compatible"
			provider.Mode = "full"
			provider.SupportsThinking = !provider.SupportsThinking
			if _, updateErr := s.UpdateProvider(ctx, provider); updateErr != nil {
				return updateErr
			}
			updatedProvider, err := s.GetProvider(ctx, "litellm")
			if err != nil {
				return err
			}
			mutated = *snapshotLaunchCompatibility(updatedProvider)
			if mutated == admitted {
				return fmt.Errorf("provider mutation did not change compatibility summary: %#v", mutated)
			}
			return nil
		},
	}, "--db", database, "launch", "--model", "coder", "--auth-mode", launchAuthModeProviderOnly)
	if err != nil {
		t.Fatal(err)
	}

	summaryLine := func(summary launchCompatibilitySummary) string {
		return fmt.Sprintf("Provider protocol=%s mode=%s token-count=%s capabilities=%s.",
			summary.protocol, summary.mode, summary.tokenCount, summary.capabilities)
	}
	if got, want := summaryLine(admitted), "Provider protocol=openai-compatible mode=degraded "; !strings.Contains(got, want) {
		t.Fatalf("test did not admit the expected initial provider compatibility: %q", got)
	}
	if !strings.Contains(out, summaryLine(admitted)) {
		t.Fatalf("launch summary = %q, want admitted compatibility %q", out, summaryLine(admitted))
	}
	if strings.Contains(out, summaryLine(mutated)) {
		t.Fatalf("launch summary reported provider compatibility changed after admission: %q", out)
	}
}
