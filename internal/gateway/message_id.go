package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const gatewayMessageIDRandomBytes = 16

// newGatewayMessageID creates an Anthropic-compatible message identity for a
// single translated response. It must be request-scoped: Claude Code uses
// message IDs when preserving conversation groups during compaction.
func newGatewayMessageID() (string, error) {
	raw := make([]byte, gatewayMessageIDRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read message identifier randomness: %w", err)
	}
	return "msg_ccr_" + hex.EncodeToString(raw), nil
}
