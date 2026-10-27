package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
)

func TestLaunchRejectsAmbiguousSettingsSourcesBeforeRuntimeEffects(t *testing.T) {
	setTestClaudeHome(t)
	for _, args := range [][]string{
		{"--setting-sources"},
		{"--setting-sources=synthetic-private-marker"},
		{"--setting-sources", " "},
		{"--setting-sources", "user,"},
		{"--setting-sources", ",user"},
		{"--setting-sources", "--bare"},
		{"--bare", "--setting-sources", ""},
		{"--append-system-prompt", "--setting-sources=synthetic-private-marker"},
		{"--append-system-prompt", "--", "--setting-sources=synthetic-private-marker"},
	} {
		if _, builderErr := userClaudeModelPicker(args); builderErr == nil {
			t.Fatal("picker builder accepted ambiguous settings-source arguments")
		}
		database := filepath.Join(t.TempDir(), "ccr.db")
		launcher := &fakeLauncher{pid: os.Getpid()}
		authCalls, gatewayStarts := 0, 0
		deps := Dependencies{
			Launcher: launcher,
			DetectClaudeAuth: func(context.Context) (bool, error) {
				authCalls++
				return false, errors.New("auth must not run")
			},
			StartGateway: func(context.Context, gateway.Config) (*gateway.Server, error) {
				gatewayStarts++
				return nil, errors.New("gateway must not start")
			},
		}
		command := append([]string{"--db", database, "launch"}, args...)
		_, _, err := runCommandWithDeps(t, deps, command...)
		if err == nil || !strings.Contains(err.Error(), "--setting-sources") {
			t.Fatalf("expected settings-source argument rejection, got %v", err)
		}
		if strings.Contains(err.Error(), "synthetic-private-marker") {
			t.Fatal("argument diagnostic exposed private prompt text")
		}
		if authCalls != 0 || gatewayStarts != 0 || launcher.starts != 0 {
			t.Fatal("invalid settings-source arguments caused runtime side effects")
		}
		if _, statErr := os.Stat(database); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("invalid settings-source arguments created a database")
		}
	}
}

func TestLaunchSettingsSourcePrefixPreservesLiteralNativeValues(t *testing.T) {
	modelPickerTestStore(t)
	writeModelPickerTestSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"modelPicker":{"options":[{"model":"user-model"}]}}`)
	for _, test := range []struct {
		args []string
		user bool
	}{
		{[]string{"--setting-sources", "project, user", "--verbose"}, true},
		{[]string{"--setting-sources=project", "--setting-sources=user", "--verbose"}, true},
		{[]string{"--setting-sources", "", "--verbose"}, false},
		{[]string{"--append-system-prompt=--setting-sources=project"}, true},
		{[]string{"use --setting-sources docs"}, true},
		{[]string{"--", "--setting-sources=project"}, true},
		{[]string{"--setting-sources=project", "--", "--setting-sources=user"}, false},
	} {
		if err := validateLaunchPassthroughArgs(test.args); err != nil {
			t.Fatalf("valid settings-source prefix rejected: %v", err)
		}
		picker, err := userClaudeModelPicker(test.args)
		if err != nil {
			t.Fatal(err)
		}
		if included := len(picker.Options) == 1; included != test.user {
			t.Fatalf("native prompt text changed user-settings scope: included=%t want=%t", included, test.user)
		}
	}
}
