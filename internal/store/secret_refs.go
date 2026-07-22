package store

import "github.com/hishamkaram/claude-code-router/internal/secret"

func validateSecretRef(ref string) error {
	return secret.ValidateRef(ref)
}
