package deal

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A deal's receipt still writes its page: every deal page measured is VALID,
// own copy and shared.
func TestPageGateKeepsDealReceipts(t *testing.T) {
	id := cancelAfterPurchase(t)
	page := filepath.Join(t.TempDir(), "receipt.html")
	dealRun(t, "report", "--deal", id, "--html", page)
	assert.FileExists(t, page)
	shared := filepath.Join(t.TempDir(), "shared.html")
	dealRun(t, "report", "--deal", id, "--share", "counterparty", "--to", "the shop's support desk", "--html", shared)
	assert.FileExists(t, shared)
}
