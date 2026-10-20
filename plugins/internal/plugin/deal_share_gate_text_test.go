package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gates read a page's text and data, never its code (capsule-cli#247):
// a refreshed runtime, module or stylesheet whose own strings hold the
// spending limit as money ("$8.00") does not trip the share, and the same
// money in the page's data, or a card number in its markup, still does.
func TestTheShareGateReadsTextNotCode(t *testing.T) {
	dealFixture(t)
	id := openSticker(t, true)
	path := filepath.Join(t.TempDir(), "shared.html")
	dealRun(t, "report", "--deal", id, "--html", path, "--share", "counterparty", "--to", "the shop")
	page, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(page, cli.EvidenceGraphIIFE), "the page inlines the runtime")
	events := chainSteps(t, id)
	require.NoError(t, dealShareGate(page, events, cli.DealAudienceCounterparty), "the page as shared passes again")

	refreshed := bytes.Replace(page, cli.EvidenceGraphIIFE, append(append([]byte(nil), cli.EvidenceGraphIIFE...), []byte(`;const sample="Pay $8.00 now";`)...), 1)
	refreshed = bytes.Replace(refreshed, []byte(cli.DealViewJS), []byte(cli.DealViewJS+`;const fee="$8.00";`), 1)
	refreshed = bytes.Replace(refreshed, []byte("</style>"), []byte(`.price::after{content:"$8.00"}</style>`), 1)
	require.False(t, bytes.Contains(refreshed, cli.EvidenceGraphIIFE) && bytes.Contains(refreshed, []byte(cli.DealViewJS+"</script>")), "the vendored bytes no longer match byte for byte")
	assert.NoError(t, dealShareGate(refreshed, events, cli.DealAudienceCounterparty), "code and style are not read")

	inData := bytes.Replace(refreshed, []byte("window.__BUNDLE__ = {"), []byte(`window.__BUNDLE__ = {"note":"my limit is $8.00",`), 1)
	require.NotEqual(t, refreshed, inData)
	assert.ErrorContains(t, dealShareGate(inData, events, cli.DealAudienceCounterparty), "your spending limit", "the bundle is data")
	inPageData := bytes.Replace(refreshed, []byte("globalThis.capsulectlDealPage = Object.freeze({"), []byte(`globalThis.capsulectlDealPage = Object.freeze({"note":"my limit is $8.00",`), 1)
	require.NotEqual(t, refreshed, inPageData)
	assert.ErrorContains(t, dealShareGate(inPageData, events, cli.DealAudienceCounterparty), "your spending limit", "the page's data is data")
	inMarkup := bytes.Replace(refreshed, []byte(`<div id="app"></div>`), []byte(`<div id="app">Paid with 4242 4242 4242 4242</div>`), 1)
	require.NotEqual(t, refreshed, inMarkup)
	assert.ErrorContains(t, dealShareGate(inMarkup, events, cli.DealAudienceCounterparty), "card number", "the markup is text")
}

// A page whose bundle script is not found is read whole: the strings of an
// unknown script are then read too, so the gate never stops reading data it
// cannot place. A data script that does not decode is read whole as well.
func TestTheShareGateReadsAnUnplacedPageWhole(t *testing.T) {
	limit := int64(800)
	events := []sealedEvent{{Event: dealEvent{Kind: "open", DealID: "deal-0123456789abcdef", Open: &dealOpen{
		Intent: dealIntent{MaxTotalMinor: &limit}, Terms: dealTerms{Currency: "USD"}}}}}
	code := `<script>const fee="$8.00";</script>`
	assert.NoError(t, dealShareGate([]byte(`<!doctype html><script>window.__BUNDLE__ = {"line":"ok"};</script>`+code), events, cli.DealAudienceCounterparty))
	assert.Error(t, dealShareGate([]byte(`<!doctype html><script>window.data = {"line":"ok"};</script>`+code), events, cli.DealAudienceCounterparty), "no bundle script: read whole")
	assert.Error(t, dealShareGate([]byte(`<!doctype html><script>window.__BUNDLE__ = {"line":"my limit is $8.00"</script>`), events, cli.DealAudienceCounterparty), "a bundle that does not decode is read whole")
	assert.True(t, strings.Contains(string(dealGateText([]byte(`<p>side</p>`))), "side"), "a copy that is not a page is read whole")
}
