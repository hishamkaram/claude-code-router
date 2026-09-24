//go:build linux || darwin

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

type admissionContext struct {
	Directory string
	Database  string
	JobRoot   string
	User      int
	Profile   map[string]string
}

type admissionOptions struct {
	Model          string
	Print          bool
	AuthMode       string
	ClaudeAccount  string
	PermissionMode string
	NoHistory      bool
	NoLifecycle    bool
	NoStatusline   bool
	CUA            cua.Config
	CUAExternalURL string
	CUATokenEnv    string
	CUAExplicit    [5]bool
}

type admissionRequest struct {
	Version          int
	Prompt           []byte
	Options          admissionOptions
	Forwarded        []string
	RequestedSession string
	ExpectedParent   string
	Context          admissionContext
}

func requestFingerprint(invocation launchInvocation, prompt []byte, execution admissionContext) (string, error) {
	request := admissionRequest{
		Version: 1, Prompt: prompt, Context: execution,
		RequestedSession: invocation.resumeSession, ExpectedParent: invocation.expectedParent,
		Forwarded: append([]string{}, invocation.claudeArgs...),
		Options: admissionOptions{
			Model: invocation.modelAlias, Print: invocation.printMode,
			AuthMode:      normalizedAdmissionAuthMode(invocation.authMode),
			ClaudeAccount: invocation.claudeAccount, PermissionMode: invocation.permissionMode,
			NoHistory: invocation.noHistory, NoLifecycle: invocation.noLifecycle, NoStatusline: invocation.noStatusline,
			CUA: invocation.cuaConfig, CUAExternalURL: invocation.cuaExternalURL, CUATokenEnv: invocation.cuaTokenEnv,
			CUAExplicit: [5]bool{invocation.cuaModeSet, invocation.cuaExecutorSet, invocation.cuaLimitsSet, invocation.cuaURLSet, invocation.cuaTokenEnvSet},
		},
	}
	return admissionDigest(request)
}

func admissionDigest[T any](value T) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding admission fingerprint: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalAdmissionContext(database, root string) (admissionContext, error) {
	directory, err := os.Getwd()
	if err != nil {
		return admissionContext{}, fmt.Errorf("resolving admission directory: %w", err)
	}
	result := admissionContext{User: os.Geteuid(), Profile: make(map[string]string)}
	for _, target := range []struct {
		input  string
		output *string
	}{
		{directory, &result.Directory}, {database, &result.Database}, {root, &result.JobRoot},
	} {
		*target.output, err = canonicalAdmissionPath(target.input)
		if err != nil {
			return admissionContext{}, err
		}
	}
	// Preserve absent versus explicitly empty settings. Paths are normalized only
	// when nonempty; environment defaults otherwise remain distinguishable.
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR"} {
		if value, exists := os.LookupEnv(name); exists {
			if value != "" {
				value, err = canonicalAdmissionPath(value)
				if err != nil {
					return admissionContext{}, err
				}
			}
			result.Profile[name] = value
		}
	}
	return result, nil
}

// Resolve existing ancestors too: a not-yet-created database or profile under
// a symlink still belongs to the same canonical execution namespace.
func canonicalAdmissionPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving admission path: %w", err)
	}
	var suffix []string
	candidate := absolute
	for {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(resolveErr) || filepath.Dir(candidate) == candidate {
			return "", fmt.Errorf("canonicalizing admission path: %w", resolveErr)
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = filepath.Dir(candidate)
	}
}

func normalizedAdmissionAuthMode(mode string) string {
	if mode == launchAuthModeGatewayToken {
		return launchAuthModeProviderOnly
	}
	return mode
}
