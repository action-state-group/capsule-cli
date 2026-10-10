package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deal view opens with one line saying what the witness covers, read
// from the witness block only: through which step and when, with the later
// steps pending the next tick, never the whole deal; "Not witnessed" when
// there is no receipt. Rendered in Chrome (CAPSULECTL_CHROME), on the
// user's own copy and on both shared copies.
func TestPresentationDealWitnessLine(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	chrome := startHeadlessChrome(t, binary)
	line := func(t *testing.T, path string) (string, string) {
		t.Helper()
		p := chrome.open(t, "file://"+path, presentationViews[1])
		var got struct {
			State string `json:"state"`
			Text  string `json:"text"`
		}
		p.eval(t, `(() => { const e = document.querySelector("#deal [data-witness]"); return e ? { state: e.dataset.witness, text: e.textContent } : { state: "", text: "" }; })()`, &got)
		return got.State, got.Text
	}
	pages := func(t *testing.T, dealID string) map[string]string {
		t.Helper()
		dir := t.TempDir()
		out := map[string]string{"own": filepath.Join(dir, "own.html")}
		dealRun(t, "report", "--deal", dealID, "--html", out["own"])
		for _, audience := range []string{"counterparty", "adjudicator"} {
			out[audience] = filepath.Join(dir, audience+".html")
			dealRun(t, "report", "--deal", dealID, "--html", out[audience], "--share", audience, "--to", "the shop's support desk")
		}
		return out
	}

	t.Run("not witnessed", func(t *testing.T) {
		dealFixture(t)
		for name, path := range pages(t, retailDeal(t)) {
			state, text := line(t, path)
			assert.Equal(t, "none", state, name)
			assert.Equal(t, "Not witnessed.", text, name)
		}
	})

	t.Run("pending, then witnessed, then witnessed in part", func(t *testing.T) {
		public, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		endpoint, _ := countingWitness(t)
		p := cadenceFixture(t, endpoint, public, "1h", "0s", 0)
		now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
		dealID := retailDeal(t)

		state, text := line(t, pages(t, dealID)["counterparty"])
		assert.Contains(t, []string{"scheduled", "pending"}, state)
		assert.Equal(t, "Not witnessed yet: witness receipt pending the next tick.", text)

		require.Equal(t, "ticked", dealRun(t, "tick")["state"])
		deliver(t, p, cadenceSize(t, p), key)
		for name, path := range pages(t, dealID) {
			state, text := line(t, path)
			assert.Equal(t, "all", state, name)
			assert.Equal(t, "Witnessed through the last step this copy holds at 2026-10-04T09:00:00Z (receipt attached).", text, name)
		}

		*now = now.Add(10 * time.Minute)
		dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"Your order has shipped."}`))
		assurance := dealRun(t, "report", "--deal", dealID)["assurance"].(map[string]any)
		k, n := int(assurance["steps_witnessed"].(float64)), int(assurance["steps"].(float64))
		require.Equal(t, k+1, n)
		for name, path := range pages(t, dealID) {
			state, text := line(t, path)
			assert.Equal(t, "part", state, name)
			assert.Equal(t, fmt.Sprintf("Witnessed through step %d of %d at 2026-10-04T09:00:00Z (receipt attached); later steps: witness receipt pending the next tick.", k, n), text, name)
		}
	})
}

// The deal view says which checks ran on the page and which only
// `capsulectl verify` runs, rendered in Chrome on a deal shaped like a
// shared receipt whose witness covers part of it (an earlier checkpoint, a
// receipt, a consistency proof), on every copy. No "verified" appears in
// the deal section except as "not verified" or "verified or not".
func TestPresentationDealSaysWhichChecksRan(t *testing.T) {
	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	chrome := startHeadlessChrome(t, binary)
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

	dir := t.TempDir()
	copies := map[string][]string{"own": nil, "counterparty": {"--share", "counterparty", "--to", "the shop's support desk"}, "adjudicator": {"--share", "adjudicator", "--to", "the shop's support desk"}}
	for name, extra := range copies {
		path := filepath.Join(dir, name+".html")
		dealRun(t, append([]string{"report", "--deal", dealID, "--html", path}, extra...)...)
		page := chrome.open(t, "file://"+path, presentationViews[1])
		var got struct {
			Page    []string `json:"page"`
			CLI     []string `json:"cli"`
			CLIText []string `json:"cliText"`
			Command string   `json:"command"`
			Text    string   `json:"text"`
		}
		page.eval(t, `(() => {
  const ids = (w) => [...document.querySelectorAll('#deal [data-checks="' + w + '"] li')].map((li) => li.dataset.check);
  return {
    page: ids("page"), cli: ids("cli"),
    cliText: [...document.querySelectorAll('#deal [data-checks="cli"] li')].map((li) => li.textContent),
    command: (document.querySelector("#deal .deal-verify") || {}).textContent || "",
    text: document.getElementById("deal").textContent,
  };
})()`, &got)
		assert.Equal(t, []string{"digests", "membership", "range", "disclosures"}, got.Page, name)
		assert.Equal(t, []string{"checkpoint-signature", "producer-signatures", "witness-receipt", "consistency"}, got.CLI, name)
		assert.Contains(t, got.CLIText[0], "This page cannot check it, and it shows the bundle as passing without it.", name)
		assert.Regexp(t, `^capsulectl verify --bundle \S+ --witness-directory DIRECTORY\.json$`, got.Command, name)
		scan := strings.NewReplacer("not verified", "", "verified or not", "").Replace(got.Text)
		assert.NotRegexp(t, `(?i)\bverified\b`, scan, "%s: \"verified\" only for a check the page ran", name)
		assert.NotContains(t, got.Text, "all records", name)
	}
}
