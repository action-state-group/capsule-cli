package cli

import (
	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// CommitText is the salted commitment SHA-256(JCS({"nonce","text"})) the
// deal profile's records use, kept here for the plugin that builds them.
func CommitText(nonceHex, text string) (string, error) {
	return canonical.JSONDigest(map[string]interface{}{"nonce": nonceHex, "text": text})
}
