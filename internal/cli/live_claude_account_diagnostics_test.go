//go:build live

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestLiveRealClaudeAccountDiagnostics(t *testing.T) {
	if os.Getenv("CCR_LIVE_REAL_ACCOUNT_DIAGNOSTICS") != "1" {
		t.Skip("set CCR_LIVE_REAL_ACCOUNT_DIAGNOSTICS=1 to query real account diagnostics")
	}
	token := strings.TrimSpace(os.Getenv("CCR_LIVE_REAL_SUBSCRIPTION_OAUTH_TOKEN"))
	if token == "" {
		credentials, err := claudeaccount.ReadCurrentCredentials()
		if err != nil {
			t.Skipf("real Claude credentials unavailable: %v", err)
		}
		token = credentials.AccessToken
	}

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	ref := secret.ClaudeAccountAccessTokenRef("live-diagnostics")
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name: "live-diagnostics", AccessTokenRef: ref, ScopesJSON: "[]", Enabled: true,
	})
	out, errOut, err := runCommandWithDeps(t, Dependencies{
		Secrets: &accountTestSecrets{values: map[string]string{ref: token}},
	}, "--db", dbPath, "claude-account", "test", "live-diagnostics", "--live")
	if err != nil {
		t.Fatalf("real Claude account diagnostics error = %v, stderr=%q", err, errOut)
	}
	for _, want := range []string{"identity=", "plan=", "five_hour=", "seven_day="} {
		if !strings.Contains(out, want) {
			t.Fatalf("real Claude account diagnostics missing %q: %q", want, out)
		}
	}
	if strings.Contains(out+errOut, token) || strings.Contains(out+errOut, "keyring:") {
		t.Fatal("real Claude account diagnostics leaked credentials")
	}
}
