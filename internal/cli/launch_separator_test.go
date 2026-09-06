package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLaunchSeparatorNormalization(t *testing.T) {
	t.Parallel()
	for _, tail := range [][]string{
		nil,
		{"--output-format", "json", "--max-turns", "1"},
		{"--input-format=stream-json", "--output-format=stream-json", "--verbose"},
		{"--", "--fallback-model", "literal prompt"},
	} {
		t.Run(strings.Join(tail, " "), func(t *testing.T) {
			args := append([]string{"--model", "gpt", "-p", "--verbose", "--"}, tail...)
			invocation, err := parseLaunchInvocation(args)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{"--verbose"}, tail...)
			if !slices.Equal(invocation.claudeArgs, want) || !invocation.printMode || invocation.modelAlias != "gpt" {
				t.Fatalf("invocation = %#v, want passthrough %q", invocation, want)
			}
		})
	}
}

func TestLaunchSeparatorRejectsUnsafeOptionsBeforeStartup(t *testing.T) {
	t.Parallel()
	options := []string{
		"--model", "--auth-mode", "--claude-account", "--permission-mode", "--print", "-p", "--db",
		"--no-history", "--no-lifecycle", "--no-statusline", "--fallback-model", "--bg", "--background",
		"--ccr-cua-mode", "--ccr-cua-executor", "--ccr-cua-external-url", "--ccr-cua-external-token-env",
		"--ccr-cua-max-turns", "--ccr-cua-max-actions", "--ccr-cua-timeout",
	}
	for _, option := range options {
		for _, tail := range [][]string{{option, "true"}, {option + "=true"}} {
			t.Run(strings.Join(tail, " "), func(t *testing.T) {
				dbPath := filepath.Join(t.TempDir(), "ccr.db")
				launcher := &fakeLauncher{pid: os.Getpid()}
				args := append([]string{"--db", dbPath, "launch", "--"}, tail...)
				_, _, err := runCommandWithDeps(t, Dependencies{Launcher: launcher}, args...)
				if err == nil || !strings.Contains(err.Error(), option) {
					t.Fatalf("error = %v, want rejection of %s", err, option)
				}
				if launcher.starts != 0 {
					t.Fatalf("launcher starts = %d, want zero", launcher.starts)
				}
				if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
					t.Fatalf("database created before validation: %v", err)
				}
			})
		}
	}
}

func TestLaunchSeparatorDynamicValidation(t *testing.T) {
	t.Parallel()
	for _, option := range []string{"--tools", "--mcp-config", "--plugin-dir", "--plugin-url", "--settings"} {
		for _, tail := range [][]string{{option, "value"}, {option + "=value"}} {
			t.Run(strings.Join(tail, " "), func(t *testing.T) {
				invocation, err := parseLaunchInvocation(append([]string{"--"}, tail...))
				if err != nil {
					t.Fatal(err)
				}
				if err := validateDynamicLaunchPassthroughArgs(invocation.claudeArgs, true, true); err == nil {
					t.Fatalf("accepted unsafe option %s", option)
				}
			})
		}
	}
}

func TestLaunchSeparatorMetadata(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--help", "-h", "--version", "-v"} {
		invocation, err := parseLaunchInvocation([]string{"--", flag})
		if err != nil {
			t.Fatal(err)
		}
		if args, ok := invocation.claudeMetadataArgs(); !ok || !slices.Equal(args, []string{flag}) || invocation.help {
			t.Fatalf("metadata %s: args=%q ok=%t help=%t", flag, args, ok, invocation.help)
		}
		invocation, err = parseLaunchInvocation([]string{"--", "--", flag})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := invocation.claudeMetadataArgs(); ok {
			t.Fatalf("literal prompt %s treated as metadata", flag)
		}
	}
}
