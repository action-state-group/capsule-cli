package cli

import (
	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// commitText is the salted commitment SHA-256(JCS({"nonce","text"})).
func CommitText(nonceHex, text string) (string, error) {
	return canonical.JSONDigest(map[string]interface{}{"nonce": nonceHex, "text": text})
}
