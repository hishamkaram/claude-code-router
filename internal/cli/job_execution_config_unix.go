//go:build linux || darwin

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

type admissionExecutionConfig struct {
	Version           int
	ModelAlias        string
	ClaudeModel       string
	AuthMode          string
	DisableTools      bool
	ResolutionFailure string
	Models            []store.Model
	Providers         []store.Provider
	Settings          map[string]string
	Environment       map[string]string
}

// Only the resulting digest is persisted. Secret values are never resolved;
// provider authentication is represented by its stored reference.
func executionFingerprint(ctx context.Context, opts *options, deps Dependencies, invocation launchInvocation) (string, error) {
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return "", err
	}
	defer closeStore(s)
	config := admissionExecutionConfig{Version: 1}
	config.Models, err = s.ListModels(ctx)
	if err != nil {
		return "", err
	}
	config.Providers, err = s.ListProviders(ctx)
	if err != nil {
		return "", err
	}
	for i := range config.Models {
		config.Models[i].ID, config.Models[i].CreatedAt, config.Models[i].CapabilitiesRefreshedAt = 0, "", ""
	}
	for i := range config.Providers {
		config.Providers[i].ID, config.Providers[i].CreatedAt = 0, ""
	}
	resolved, resolveErr := resolveLaunch(ctx, deps, s, invocation)
	if resolveErr != nil {
		// Preserve invalid-route startup classification in the owner. This snapshot
		// still binds the configuration that produced the failure, without executing.
		config.ResolutionFailure = resolveErr.Error()
	}
	config.ModelAlias, config.ClaudeModel = resolved.modelAlias, resolved.claudeModelID
	config.AuthMode, config.DisableTools = normalizedAdmissionAuthMode(resolved.authMode), resolved.disableTools
	config.Settings, err = admissionSettingsDigests(invocation.claudeArgs)
	if err != nil {
		return "", err
	}
	config.Environment = admissionExecutionEnvironment()
	return admissionDigest(config)
}

func admissionSettingsDigests(args []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, path := range append(claudeSettingsPaths(), forwardedSettingsPaths(args)...) {
		canonical, err := canonicalAdmissionPath(path)
		if err != nil {
			return nil, err
		}
		digest, err := admissionFileDigest(path)
		if err != nil {
			return nil, err
		}
		result[canonical] = digest
	}
	return result, nil
}

func admissionFileDigest(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", fmt.Errorf("opening admission settings: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("checking admission settings: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return "", fmt.Errorf("admission settings must be a regular file at most 16 MiB")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return "", fmt.Errorf("hashing admission settings: %w", err)
	}
	if count > 16<<20 {
		return "", fmt.Errorf("admission settings exceed 16 MiB")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func admissionExecutionEnvironment() map[string]string {
	result := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if name == "_" || name == "SHLVL" || name == "PWD" || name == "OLDPWD" || name == "CLAUDE_CONFIG_DIR" {
			continue
		}
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") {
			value = "present" // Reference/presence only, never a raw credential.
		}
		result[name] = value
	}
	return result
}

// Inline JSON is already bound byte-for-byte by the request fingerprint. Files
// need their contents bound separately because their path can stay unchanged.
func forwardedSettingsPaths(args []string) []string {
	var paths []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		value, found := strings.CutPrefix(args[i], "--settings=")
		if args[i] == "--settings" && i+1 < len(args) {
			i++
			value, found = args[i], true
		}
		if found && value != "" && !strings.HasPrefix(strings.TrimSpace(value), "{") {
			paths = append(paths, value)
		}
	}
	return paths
}
