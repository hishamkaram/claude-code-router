package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/claudeaccount"
)

func currentClaudeBootstrapStatePath(source string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating Claude Code bootstrap state: %w", err)
	}
	// Claude Code prefers the alternate global bootstrap file when it exists;
	// mirror that precedence before falling back to the config-directory state.
	alternate := filepath.Join(home, ".config.json")
	defaultSource := filepath.Join(home, ".claude")
	if filepath.Clean(source) != filepath.Clean(defaultSource) || strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return filepath.Join(source, ".claude.json"), nil
	}
	if _, statErr := os.Lstat(alternate); statErr == nil {
		return alternate, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspecting Claude Code alternate bootstrap state: %w", statErr)
	}
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return filepath.Join(source, ".claude.json"), nil
	}
	return filepath.Join(home, ".claude.json"), nil
}

type claudeGatewayModelCache struct {
	BaseURL   string                    `json:"baseUrl"`
	FetchedAt int64                     `json:"fetchedAt"`
	Models    []claudeGatewayModelEntry `json:"models"`
}

type claudeGatewayModelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func seedClaudeGatewayModelCache(configDir, baseURL, settingsJSON string) error {
	cachePath := filepath.Join(configDir, "cache", "gateway-models.json")
	if strings.TrimSpace(settingsJSON) == "" {
		return removeClaudeGatewayModelCache(cachePath)
	}
	var settings struct {
		AvailableModels []string `json:"availableModels"`
	}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return fmt.Errorf("parsing private Claude model catalog settings: %w", err)
	}
	models := make([]claudeGatewayModelEntry, 0, len(settings.AvailableModels))
	for _, id := range settings.AvailableModels {
		id = strings.TrimSpace(id)
		alias, ok := strings.CutPrefix(id, "anthropic.ccr.")
		if !ok || alias == "" {
			continue
		}
		models = append(models, claudeGatewayModelEntry{ID: id, DisplayName: "CCR " + alias})
	}
	if len(models) == 0 {
		return removeClaudeGatewayModelCache(cachePath)
	}
	cache := claudeGatewayModelCache{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), FetchedAt: time.Now().UnixMilli(), Models: models,
	}
	encoded, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("encoding private Claude model catalog: %w", err)
	}
	cacheDir := filepath.Join(configDir, "cache")
	if err := ensureClaudeProfileDirectory(cacheDir); err != nil {
		return fmt.Errorf("creating private Claude model catalog directory: %w", err)
	}
	if err := writeClaudeProfileFile(cachePath, encoded, 0o600); err != nil {
		return fmt.Errorf("writing private Claude model catalog: %w", err)
	}
	return nil
}

func removeClaudeGatewayModelCache(path string) error {
	if err := rejectClaudeProfilePathSymlinks(filepath.Dir(path)); err != nil {
		return fmt.Errorf("checking private Claude model catalog directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale private Claude model catalog: %w", err)
	}
	return nil
}

func shouldSkipClaudeProfilePath(relative string) bool {
	if strings.TrimSpace(relative) == "" {
		return false
	}
	relative = filepath.ToSlash(relative)
	parts := strings.Split(relative, "/")
	if !isClaudeProfilePathAllowed(parts) {
		// CLAUDE_CONFIG_DIR is user-configurable and may be pointed at a broad
		// directory such as $HOME. Unknown top-level paths are never Claude
		// profile state, so do not recursively import unrelated files such as
		// .ssh, .env, or project data into the isolated profile.
		return true
	}
	if len(parts) > 0 {
		switch parts[0] {
		case "backups", "cache", "debug", "paste-cache", "sessions", "shell-snapshots":
			return true
		case "downloads", "file-history", "projects", "session-env", "telemetry":
			return true
		}
	}
	if len(parts) > 1 && parts[0] == "plugins" {
		switch parts[1] {
		case "cache", "marketplaces", "synced":
			// Installed plugin payloads are immutable user-level assets. Claude
			// records their absolute install paths in installed_plugins.json and
			// the marketplace paths in known_marketplaces.json, both of which are
			// copied below. Keeping those payloads in the native profile avoids
			// copying hundreds of megabytes into every isolated CCR profile while
			// preserving the same plugin resolution behavior.
			return true
		case "data", ".trash":
			// Plugin runtime state and deleted payloads are not part of the
			// installed plugin surface. They are intentionally isolated so a
			// launch cannot reuse stale execution state from another profile.
			return true
		}
	}
	switch parts[len(parts)-1] {
	case ".claude.json", ".credentials.json", "history.jsonl", "stats-cache.json", ".last-cleanup":
		return true
	default:
		return false
	}
}

func isClaudeProfilePathAllowed(parts []string) bool {
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return true
	}
	switch parts[0] {
	case "CLAUDE.md", "settings.json", "settings.local.json",
		"agents", "commands", "hooks", "plugins", "rules", "skills":
		return true
	default:
		return false
	}
}

func shouldShareClaudeProfileAssetDirectory(relative string) bool {
	clean := filepath.ToSlash(filepath.Clean(relative))
	switch clean {
	case "agents", "commands", "hooks", "rules", "skills":
		// These trees contain user-authored definitions and are immutable inputs
		// during a Claude launch. Share them instead of copying them into every
		// session profile; mutable runtime state remains outside these roots.
		return true
	}
	parts := strings.Split(clean, "/")
	if len(parts) != 3 || parts[0] != "plugins" {
		return false
	}
	// Plugin payloads under cache/marketplaces/synced are already shared by
	// absolute paths in installed_plugins.json. Share only plugin definition
	// subtrees here; plugin-local caches and runtime data continue to be copied
	// or excluded by shouldSkipClaudeProfilePath.
	if parts[2] != ".claude-plugin" && parts[2] != "agents" && parts[2] != "commands" && parts[2] != "hooks" && parts[2] != "rules" && parts[2] != "skills" {
		return false
	}
	return parts[1] != "cache" && parts[1] != "marketplaces" && parts[1] != "synced" && parts[1] != "data" && parts[1] != ".trash"
}

type preservedClaudeOAuth struct {
	AccessToken  string
	RefreshToken string
	ScopesJSON   string
}

func preservedClaudeOAuthEnvironment(authMode, sourceOverride string) (preservedClaudeOAuth, error) {
	if authMode != launchAuthModePreserve || claudeLaunchAuthEnvPresent(os.Getenv) {
		return preservedClaudeOAuth{}, nil
	}
	if runtime.GOOS == "darwin" {
		return preservedClaudeOAuth{}, nil
	}
	source, err := claudeConfigDirForOverride(sourceOverride)
	if err != nil {
		return preservedClaudeOAuth{}, fmt.Errorf("locating Claude subscription profile for isolated launch: %w", err)
	}
	path, err := claudeaccount.CurrentCredentialsPath(runtime.GOOS, source, "")
	if err != nil {
		return preservedClaudeOAuth{}, fmt.Errorf("locating Claude subscription credentials for isolated launch: %w", err)
	}
	if _, pathInfoErr := os.Lstat(path); errors.Is(pathInfoErr, os.ErrNotExist) {
		return preservedClaudeOAuth{}, nil
	} else if pathInfoErr != nil {
		return preservedClaudeOAuth{}, fmt.Errorf("inspecting Claude subscription credentials for isolated launch: %w", pathInfoErr)
	}
	credentials, err := claudeaccount.ReadCredentialsAt(path)
	if errors.Is(err, claudeaccount.ErrCurrentCredentialsNoAccessToken) {
		return preservedClaudeOAuth{}, nil
	}
	if err != nil {
		return preservedClaudeOAuth{}, fmt.Errorf("reading Claude subscription credentials for isolated launch: %w", err)
	}
	if credentials.RefreshToken != "" && claudeOAuthScopes(credentials.ScopesJSON) == "" {
		return preservedClaudeOAuth{}, fmt.Errorf("claude subscription credentials include a refresh token without OAuth scopes; refusing an isolated launch that Claude Code cannot refresh safely")
	}
	return preservedClaudeOAuth{
		AccessToken: credentials.AccessToken, RefreshToken: credentials.RefreshToken, ScopesJSON: credentials.ScopesJSON,
	}, nil
}
