package cli

import (
	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// CommitText is the x-deal-v0 salted commitment SHA-256(JCS({"nonce","text"})),
// dealprofile.CommitText's construction, which verify recomputes from an
// opening (see deal_profile.go).
func CommitText(nonceHex, text string) (string, error) {
	return canonical.JSONDigest(map[string]interface{}{"nonce": nonceHex, "text": text})
}
