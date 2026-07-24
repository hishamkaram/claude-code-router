package claudeaccount

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseCredentialsExtractsOnlyClaudeOAuth(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
	  "mcpOAuth": {"server": {"accessToken": "must-not-be-imported"}},
	  "claudeAiOauth": {
	    "accessToken": "access-value",
	    "refreshToken": "refresh-value",
	    "expiresAt": 1784901600000,
	    "scopes": ["user:profile", "user:inference"]
	  }
	}`)
	got, err := ParseCredentials(raw)
	if err != nil {
		t.Fatalf("ParseCredentials error = %v", err)
	}
	if got.AccessToken != "access-value" || got.RefreshToken != "refresh-value" {
		t.Fatalf("ParseCredentials tokens were not extracted")
	}
	if strings.Contains(got.AccessToken+got.RefreshToken, "must-not-be-imported") {
		t.Fatal("ParseCredentials imported an unrelated MCP token")
	}
	if got.ScopesJSON != `["user:profile","user:inference"]` {
		t.Fatalf("ScopesJSON = %q", got.ScopesJSON)
	}
	wantExpiry := time.UnixMilli(1784901600000).UTC().Format(time.RFC3339)
	if got.ExpiresAt != wantExpiry {
		t.Fatalf("ExpiresAt = %q, want %q", got.ExpiresAt, wantExpiry)
	}
}

func TestParseCredentialsErrorsNeverContainTokens(t *testing.T) {
	t.Parallel()

	const secretValue = "secret-token-that-must-not-leak"
	tests := [][]byte{
		[]byte(`{"claudeAiOauth":{"accessToken":"` + secretValue + ` bad"}}`),
		[]byte(`{"claudeAiOauth":{"accessToken":"valid","refreshToken":"` + secretValue + ` bad"}}`),
		[]byte(`{"claudeAiOauth":{"accessToken":"` + secretValue),
	}
	for _, raw := range tests {
		_, err := ParseCredentials(raw)
		if err == nil {
			t.Fatal("ParseCredentials unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("ParseCredentials leaked a token: %v", err)
		}
	}
}

func TestCredentialsFromTokenValidatesBoundedInput(t *testing.T) {
	t.Parallel()

	got, err := CredentialsFromToken(strings.NewReader("  setup-token-value\n"))
	if err != nil {
		t.Fatalf("CredentialsFromToken error = %v", err)
	}
	if got.AccessToken != "setup-token-value" || got.ScopesJSON != "[]" {
		t.Fatalf("CredentialsFromToken = %#v", got)
	}
	if _, err := CredentialsFromToken(strings.NewReader("bad token")); err == nil {
		t.Fatal("CredentialsFromToken accepted whitespace")
	}
}

func TestCurrentCredentialsPath(t *testing.T) {
	t.Parallel()

	path, err := CurrentCredentialsPath("linux", "/tmp/claude-personal", "")
	if err != nil {
		t.Fatalf("CurrentCredentialsPath with override error = %v", err)
	}
	if path != "/tmp/claude-personal/.credentials.json" {
		t.Fatalf("CurrentCredentialsPath = %q", path)
	}
	path, err = CurrentCredentialsPath("linux", "", "/home/tester")
	if err != nil {
		t.Fatalf("CurrentCredentialsPath with home error = %v", err)
	}
	if path != "/home/tester/.claude/.credentials.json" {
		t.Fatalf("CurrentCredentialsPath = %q", path)
	}
	_, err = CurrentCredentialsPath("darwin", "", "/Users/tester")
	if !errors.Is(err, ErrCurrentCredentialsUnsupported) {
		t.Fatalf("CurrentCredentialsPath darwin error = %v", err)
	}
}
