package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

func TestResolveDiscoveryAPIKeyRejectsClaudeAccountCredentialRef(t *testing.T) {
	t.Parallel()

	ref := secret.ClaudeAccountAccessTokenRef("personal")
	deps := Dependencies{Secrets: &accountTestSecrets{values: map[string]string{
		ref: "claude-oauth-token",
	}}}
	value, err := resolveDiscoveryAPIKey(
		context.Background(),
		deps,
		secretPlan{ref: ref},
	)
	if err == nil || value != "" {
		t.Fatalf("resolveDiscoveryAPIKey() = %q, %v; want rejected Claude account ref", value, err)
	}
	if strings.Contains(err.Error(), "personal") || strings.Contains(err.Error(), "claude-oauth-token") {
		t.Fatalf("resolveDiscoveryAPIKey() exposed credential metadata: %v", err)
	}
}
