package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestClaudeLaunchAuthDetectorSelectsPlatformSource(t *testing.T) {
	t.Parallel()

	statusErr := errors.New("status unavailable")
	credentialsErr := errors.New("credentials unavailable")
	tests := []struct {
		name                string
		goos                string
		env                 map[string]string
		credentialsPresent  bool
		credentialsErr      error
		cliStatusPresent    bool
		cliStatusErr        error
		want                bool
		wantErr             error
		wantCredentialCalls int
		wantCLIStatusCalls  int
	}{
		{
			name: "environment auth short circuits platform lookup", goos: "darwin",
			env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant"}, want: true,
		},
		{
			name: "macOS signed in", goos: "darwin", cliStatusPresent: true, want: true,
			wantCLIStatusCalls: 1,
		},
		{
			name: "macOS signed out", goos: "darwin", want: false,
			wantCLIStatusCalls: 1,
		},
		{
			name: "macOS unknown", goos: "darwin", cliStatusErr: statusErr,
			wantErr: statusErr, wantCLIStatusCalls: 1,
		},
		{
			name: "Linux credentials", goos: "linux", credentialsPresent: true, want: true,
			wantCredentialCalls: 1,
		},
		{
			name: "Linux credentials unknown", goos: "linux", credentialsErr: credentialsErr,
			wantErr: credentialsErr, wantCredentialCalls: 1,
		},
		{
			name: "Windows signed out", goos: "windows", want: false,
			wantCredentialCalls: 1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			credentialCalls := 0
			cliStatusCalls := 0
			detector := claudeLaunchAuthDetector{
				goos:   tt.goos,
				getenv: func(name string) string { return tt.env[name] },
				credentialsPresent: func() (bool, error) {
					credentialCalls++
					return tt.credentialsPresent, tt.credentialsErr
				},
				cliStatus: func(context.Context) (bool, error) {
					cliStatusCalls++
					return tt.cliStatusPresent, tt.cliStatusErr
				},
			}

			got, err := detector.Detect(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Detect() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Detect() = %t, want %t", got, tt.want)
			}
			if credentialCalls != tt.wantCredentialCalls || cliStatusCalls != tt.wantCLIStatusCalls {
				t.Fatalf(
					"Detect() source calls = credentials:%d cli-status:%d, want credentials:%d cli-status:%d",
					credentialCalls, cliStatusCalls, tt.wantCredentialCalls, tt.wantCLIStatusCalls,
				)
			}
		})
	}
}

func TestEvaluateClaudeAuthStatus(t *testing.T) {
	t.Parallel()

	exitErr := errors.New("exit status 1")
	tests := []struct {
		name    string
		raw     string
		runErr  error
		want    bool
		wantErr error
	}{
		{name: "signed in", raw: `{"loggedIn":true,"authMethod":"oauth"}`, want: true},
		{name: "signed out nonzero exit", raw: `{"loggedIn":false,"authMethod":"none"}`, runErr: exitErr},
		{name: "missing field", raw: `{}`},
		{name: "malformed successful output", raw: `{`},
		{name: "command failure takes precedence over malformed output", runErr: exitErr, wantErr: exitErr},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := evaluateClaudeAuthStatus([]byte(tt.raw), tt.runErr)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("evaluateClaudeAuthStatus() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.name == "missing field" || tt.name == "malformed successful output" {
				if err == nil {
					t.Fatal("evaluateClaudeAuthStatus() error = nil, want parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluateClaudeAuthStatus() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("evaluateClaudeAuthStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestCappedOutputBufferConsumesExcessInput(t *testing.T) {
	t.Parallel()

	buffer := cappedOutputBuffer{limit: 4}
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", written, err)
	}
	if got := buffer.buffer.String(); got != "abcd" {
		t.Fatalf("buffer content = %q, want %q", got, "abcd")
	}
	if !buffer.exceeded {
		t.Fatal("buffer exceeded = false, want true")
	}
}

func TestClaudeLaunchAuthEnvPresent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "empty", env: map[string]string{}, want: false},
		{name: "anthropic api key", env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant"}, want: true},
		{name: "oauth access", env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth-token"}, want: true},
		{name: "oauth refresh", env: map[string]string{"CLAUDE_CODE_OAUTH_REFRESH_TOKEN": "refresh-token"}, want: true},
		{name: "custom authorization header", env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer oauth-token"}, want: true},
		{name: "custom api key header", env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "x-api-key: sk-ant"}, want: true},
		{name: "local gateway token ignored", env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "local-token"}, want: false},
		{name: "custom gateway header ignored", env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "X-CCR-Session-Token: local-token"}, want: false},
		{name: "blank custom auth header ignored", env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: \t"}, want: false},
		{name: "blank values ignored", env: map[string]string{"ANTHROPIC_API_KEY": " \t"}, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := claudeLaunchAuthEnvPresent(func(name string) string { return tt.env[name] })
			if got != tt.want {
				t.Fatalf("claudeLaunchAuthEnvPresent() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestProviderOnlyGuidanceAliasesCapsLongList(t *testing.T) {
	t.Parallel()

	got := providerOnlyGuidanceAliases([]string{"a", "b", "c", "d", "e", "f"})
	want := []string{"a", "b", "c", "d", "e", "..."}
	if !slices.Equal(got, want) {
		t.Fatalf("providerOnlyGuidanceAliases() = %#v, want %#v", got, want)
	}
}

func TestCurrentClaudeCredentialsPresentInvalidFileIsUnknown(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("current credentials file detection is unsupported on %s", runtime.GOOS)
	}

	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	got, err := currentClaudeCredentialsPresent()
	if err == nil {
		t.Fatal("currentClaudeCredentialsPresent() error = nil, want invalid credentials error")
	}
	if got {
		t.Fatal("currentClaudeCredentialsPresent() = true, want false for invalid credentials file")
	}
}

func TestCurrentClaudeCredentialsPresentWithoutClaudeOAuthIsSignedOut(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("current credentials file detection is unsupported on %s", runtime.GOOS)
	}

	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	raw := []byte(`{"mcpOAuth":{"server":{"accessToken":"unrelated-token"}}}`)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), raw, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	got, err := currentClaudeCredentialsPresent()
	if err != nil {
		t.Fatalf("currentClaudeCredentialsPresent() error = %v", err)
	}
	if got {
		t.Fatal("currentClaudeCredentialsPresent() = true, want false without Claude OAuth")
	}
}
