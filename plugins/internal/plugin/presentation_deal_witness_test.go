package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deal view states no verification of its own, rendered in Chrome
// (CAPSULECTL_CHROME): no witness line, no list of checks, no signature
// line, on the user's own copy and both shared copies, whether the deal is
// witnessed or not. The core's verification section, below it, is the one
// verdict on the page (Steven's banner ruling on capsule-cli#341). "verified"
// appears in the deal section only as "not verified" or "verified or not".
func TestPresentationDealStatesNoVerificationOfItsOwn(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	chrome := startHeadlessChrome(t, binary)
	check := func(t *testing.T, dealID string) {
		t.Helper()
		dir := t.TempDir()
		copies := map[string][]string{"own": nil, "counterparty": {"--share", "counterparty", "--to", "the shop's support desk"}, "adjudicator": {"--share", "adjudicator", "--to", "the shop's support desk"}}
		for name, extra := range copies {
			path := filepath.Join(dir, name+".html")
			dealRun(t, append([]string{"report", "--deal", dealID, "--html", path}, extra...)...)
			page := chrome.open(t, "file://"+path, presentationViews[1])
			var got struct {
				Own  int    `json:"own"`
				Text string `json:"text"`
			}
			page.eval(t, `(() => ({
  own: document.querySelectorAll("#deal [data-witness], #deal [data-checks], #deal .deal-verify").length,
  text: (document.getElementById("deal") || {}).textContent || "",
}))()`, &got)
			assert.Zero(t, got.Own, "%s: no witness line or check list of the deal view's own", name)
			for _, own := range []string{"Witnessed through", "Not witnessed", "Sealed by my agent", "Checked only by capsulectl verify", "signature on every record"} {
				assert.NotContains(t, got.Text, own, name)
			}
			// A withheld step on a shared copy is verified, never "not
			// verified".
			assert.NotContains(t, got.Text, "step not verified", name)
			// One line per event: an act's own step is not said again under
			// it, and an approval is listed once, under its check.
			assert.NotContains(t, got.Text, "done, as checked", name)
			assert.LessOrEqual(t, strings.Count(got.Text, "Went ahead on what you already allowed"), 1, name)
			scan := strings.NewReplacer("not verified", "", "verified or not", "").Replace(got.Text)
			assert.NotRegexp(t, `(?i)\bverified\b`, scan, name)
		}
	}
	t.Run("not witnessed", func(t *testing.T) {
		dealFixture(t)
		check(t, retailDeal(t))
	})
	t.Run("witnessed in part", func(t *testing.T) {
		public, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		endpoint, _ := countingWitness(t)
		p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
		now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
		dealID := retailDeal(t)
		require.Equal(t, "ticked", dealRun(t, "tick")["state"])
		deliver(t, p, cadenceSize(t, p), key)
		*now = now.Add(10 * time.Minute)
		dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"Your order has shipped."}`))
		check(t, dealID)
	})
}

// With JavaScript off, as a script-blocking viewer or a host's inline
// preview shows it, the deal page runs nothing and its title still says
// what that copy shows in the clear: the words asked on the user's own
// copy, whom it is for on a shared copy.
func TestPresentationDealPageWithoutJavaScript(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	chrome := startHeadlessChrome(t, binary)
	dealFixture(t)
	id := retailDeal(t)
	dir := t.TempDir()
	off := presentationViews[1]
	off.name, off.scriptsOff = "scripts off", true
	for name, c := range map[string]struct {
		extra []string
		title string
	}{
		"own":          {nil, "Deal report: “Order the cat sticker in my cart at Example Stickers”"},
		"counterparty": {[]string{"--share", "counterparty", "--to", "the shop's support desk"}, "Deal report: a shared copy for the other party"},
		"adjudicator":  {[]string{"--share", "adjudicator", "--to", "the shop's support desk"}, "Deal report: a shared copy for an adjudicator"},
	} {
		path := filepath.Join(dir, name+".html")
		dealRun(t, append([]string{"report", "--deal", id, "--html", path}, c.extra...)...)
		page := chrome.load(t, "file://"+path, off)
		var got struct {
			Title  string `json:"title"`
			Ran    bool   `json:"ran"`
			Bundle bool   `json:"bundle"`
		}
		page.eval(t, `({ title: document.title, ran: document.getElementById("deal") !== null || document.getElementById("app").childElementCount > 0, bundle: typeof window.__BUNDLE__ !== "undefined" })`, &got)
		assert.False(t, got.Ran, "%s: no script ran", name)
		assert.False(t, got.Bundle, "%s: not even the bundle slot", name)
		assert.Equal(t, c.title, got.Title, name)
	}
}
