package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestClaudeAccountListExplainsActiveCooldown(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	cooldownTime := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	cooldownUntil := cooldownTime.Format(time.RFC3339)
	addAccountForCLI(t, dbPath, store.ClaudeAccount{
		Name:           "personal",
		AccessTokenRef: secret.ClaudeAccountAccessTokenRef("personal"),
		ScopesJSON:     "[]",
		Enabled:        true,
		CooldownUntil:  cooldownUntil,
		LastError:      "model_limit_seven_day_opus",
	})

	out, errOut, err := runCommandWithDeps(
		t, Dependencies{}, "--db", dbPath, "claude-account", "list",
	)
	if err != nil {
		t.Fatalf("claude-account list error = %v, stderr=%q", err, errOut)
	}
	for _, want := range []string{
		"status=cooldown",
		"until=",
		"reason=model_limit_seven_day_opus",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("claude-account list output missing %q: %q", want, out)
		}
	}
	untilField := strings.Split(strings.Split(out, "until=")[1], "\t")[0]
	parsed, parseErr := time.Parse(time.RFC3339Nano, untilField)
	if parseErr != nil || !parsed.Equal(cooldownTime) {
		t.Fatalf("cooldown deadline = %q (%v), want %s", untilField, parseErr, cooldownTime)
	}
}
