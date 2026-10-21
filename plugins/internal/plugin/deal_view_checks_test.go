package plugin

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deal view states no verification of its own: no list of what was
// checked, no signature line and no witness line. The core's lists and its
// one verdict stand alone on the page (Steven's banner ruling, 2026-10-21,
// on capsule-cli#341).
func TestTheDealViewStatesNoVerificationOfItsOwn(t *testing.T) {
	script := cli.DealViewJS
	for _, own := range []string{"Checked on this page", "Checked only by capsulectl verify", "The agent's signature on every record", "Witnessed through", "Sealed by my agent", "Not witnessed"} {
		assert.NotContains(t, script, own)
	}
}

// A deal page in a viewer without WebCrypto (a non-secure or opaque origin):
// scripts/check-viewer-without-webcrypto.mjs runs the page's own scripts with
// no crypto global. With the runtime's plain SHA-256 all ten checks pass;
// with it switched off, the page's own bootstrap draws "unable to verify in
// this viewer" with the verify command, and never the failure banner.
func TestADealPageWithoutWebCrypto(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, err := filepath.Abs("../../../scripts/check-viewer-without-webcrypto.mjs")
	require.NoError(t, err)
	page := filepath.Join(presentationDir, "2-unilateral-receipt", "page.html")
	for _, args := range [][]string{
		{"--crypto=none", "--expect=verified"},
		{"--crypto=none", "--fallback=off", "--expect=unable"},
	} {
		out, err := exec.Command(node, append([]string{script, page}, args...)...).CombinedOutput()
		assert.NoError(t, err, "%v:\n%s", args, out)
	}
}
