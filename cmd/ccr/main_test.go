package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/cli"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestExitCodeKeepsCredentialAvailabilityDistinct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "credential unavailable", err: secret.ErrCredentialUnavailable, want: 77},
		{name: "wrapped credential error", err: fmt.Errorf("launch preflight: %w", secret.ErrCredentialUnavailable), want: 77},
		{name: "ordinary error", err: errors.New("invalid configuration"), want: 1},
		{name: "child exit is not credential classification", err: errors.New("exit status 77"), want: 1},
		{name: "cancellation", err: context.Canceled, want: 1},
		{name: "deadline", err: context.DeadlineExceeded, want: 1},
		{name: "cancellation wins", err: errors.Join(context.Canceled, secret.ErrCredentialUnavailable), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCode(tt.err); got != tt.want {
				t.Fatalf("exitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCredentialFailureSurvivesCLIAndStoreBeforeLaunch(t *testing.T) {
	const credentialEnvironment = "CCR_TEST_CREDENTIAL_UNAVAILABLE"
	t.Setenv(credentialEnvironment, "")
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "ccr.db")
	for _, args := range [][]string{
		{"provider", "add", "fixture", "--type", "anthropic", "--api-key-env", credentialEnvironment},
		{"model", "add", "fixture-model", "--provider", "fixture", "--model", "claude-opus"},
	} {
		var out, diagnostic bytes.Buffer
		if err := cli.Execute(ctx, append([]string{"--db", database}, args...), strings.NewReader(""), &out, &diagnostic); err != nil {
			t.Fatalf("fixture setup: %v", err)
		}
	}
	for _, args := range [][]string{
		{"provider", "test", "fixture"},
		{"launch", "--model", "fixture-model", "--auth-mode", "provider-only", "--print", "must not run"},
	} {
		var out, diagnostic bytes.Buffer
		err := cli.Execute(ctx, append([]string{"--db", database}, args...), strings.NewReader(""), &out, &diagnostic)
		if !errors.Is(err, secret.ErrCredentialUnavailable) || exitCode(err) != credentialUnavailableExitCode {
			t.Fatalf("%v: error=%v, exit=%d; want credential unavailable/77", args, err, exitCode(err))
		}
		if !strings.Contains(err.Error(), credentialEnvironment) || !strings.Contains(err.Error(), "empty or unset") {
			t.Fatalf("credential resolution cause was lost: %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("credential failure produced stdout: %q", out.String())
		}
	}
	s, err := store.OpenReadOnly(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	launches, err := s.ListLaunches(ctx)
	if err != nil || len(launches) != 0 {
		t.Fatalf("credential failure created a Claude launch: %+v, %v", launches, err)
	}
}
