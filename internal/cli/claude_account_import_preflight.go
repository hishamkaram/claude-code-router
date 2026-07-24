package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func preflightClaudeAccountImport(
	ctx context.Context,
	opts *options,
	name string,
	replace bool,
	refresh bool,
) (*store.Store, store.ClaudeAccount, bool, error) {
	dbPath, err := resolveDBPath(opts)
	if err != nil {
		return nil, store.ClaudeAccount{}, false, err
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, store.ClaudeAccount{}, false,
				validateClaudeAccountImportState(name, false, replace, refresh)
		}
		return nil, store.ClaudeAccount{}, false,
			fmt.Errorf("inspecting Claude account registry: %w", statErr)
	}

	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return nil, store.ClaudeAccount{}, false, err
	}
	existing, exists, err := inspectClaudeAccountImportState(
		ctx, s, name, replace, refresh,
	)
	if err != nil {
		closeStore(s)
		return nil, store.ClaudeAccount{}, false, err
	}
	return s, existing, exists, nil
}

func inspectClaudeAccountImportState(
	ctx context.Context,
	s *store.Store,
	name string,
	replace bool,
	refresh bool,
) (store.ClaudeAccount, bool, error) {
	existing, exists, err := findClaudeAccount(ctx, s, name)
	if err != nil {
		return store.ClaudeAccount{}, false, err
	}
	if err := validateClaudeAccountImportState(name, exists, replace, refresh); err != nil {
		return store.ClaudeAccount{}, false, err
	}
	return existing, exists, nil
}

func validateClaudeAccountImportState(name string, exists, replace, refresh bool) error {
	if refresh && !exists {
		return fmt.Errorf(
			"claude account %q is not registered; use ccr claude-account import first",
			name,
		)
	}
	if exists && !replace {
		return fmt.Errorf(
			"claude account %q already exists; use --replace or ccr claude-account refresh",
			name,
		)
	}
	return nil
}
