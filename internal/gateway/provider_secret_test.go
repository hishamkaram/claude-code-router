package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/secret"
)

func TestResolveProviderSecretRejectsClaudeAccountCredentialRef(t *testing.T) {
	t.Parallel()

	ref := secret.ClaudeAccountAccessTokenRef("personal")
	backend := fakeGatewaySecrets{ref: "claude-oauth-token"}
	value, err := resolveProviderSecret(context.Background(), backend, ref)
	if err == nil || value != "" {
		t.Fatalf("resolveProviderSecret() = %q, %v; want rejected Claude account ref", value, err)
	}
	if strings.Contains(err.Error(), "personal") || strings.Contains(err.Error(), "claude-oauth-token") {
		t.Fatalf("resolveProviderSecret() exposed credential metadata: %v", err)
	}
}
