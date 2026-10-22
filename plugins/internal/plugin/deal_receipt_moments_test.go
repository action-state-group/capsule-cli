package plugin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The receipt moments of actionstate-muse#208 that capsulectl words: the
// first receipt, a called-off receipt and "show my receipts".

const fawnPaid = `{"action":"pay","amount_minor":190,"rail":"card","reference":"card ending 4242"}`

// The first receipt in the profile adds what ours covers next to the
// merchant's and why to keep it; no later receipt says either again.
func TestTheFirstReceiptSaysWhyToKeepItOnce(t *testing.T) {
	dealFixture(t)
	keep := words("say.receipt.first.keep")
	contrast := cli.BuiltinWording().Line("say.receipt.first.contrast", cli.V{"merchant": "sticker_shop_example"})

	first := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	page := filepath.Join(t.TempDir(), "receipt.html")
	act := dealRun(t, "note", "--deal", first, "--kind", "act", "--html", page, "--input", writeJSON(t, fawnPaid))
	say := act["say"].(string)
	assert.Contains(t, say, contrast)
	assert.True(t, strings.HasSuffix(say, keep+"\n"+words("report.view_details")), say)
	plainSay(t, "first act", act)

	closed := dealRun(t, "close", "--deal", first, "--input", writeJSON(t, `{"status":"received"}`))
	assert.NotContains(t, closed["say"], keep, "the deal's second receipt moment")

	second := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	again := dealRun(t, "note", "--deal", second, "--kind", "act", "--input", writeJSON(t, fawnPaid))
	assert.NotContains(t, again["say"], keep, "a later deal's receipt")
	assert.NotContains(t, again["say"], contrast)

	rep := dealRun(t, "report", "--deal", first)
	assert.NotContains(t, rep["say"], keep, "a report asked for later")
}

// A first receipt with nothing paid says why to keep it, never what the
// merchant's receipt covers: there is none.
func TestAFirstReceiptWithNothingPaidNamesNoMerchantReceipt(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"), words("say.receipt.first.keep")), closed["say"])
}

// A deal called off after a hold says nothing was paid or shared, and its
// receipt is still offered; once details were shared, it says only that
// nothing was paid.
func TestACalledOffReceiptSaysWhatDidNotHappen(t *testing.T) {
	dealFixture(t)
	// A receipt moment elsewhere, so none below is the first.
	other := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	dealRun(t, "note", "--deal", other, "--kind", "act", "--input", writeJSON(t, fawnPaid))

	held := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	page := filepath.Join(t.TempDir(), "receipt.html")
	closed := dealRun(t, "close", "--deal", held, "--html", page, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.VoiceReport, closed["voice"])
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"), words("report.view_details")), closed["say"])
	assert.Equal(t, map[string]any{"path": page, "media_type": "text/html", "label": words("report.private")}, closed["attach"])
	rep := dealRun(t, "report", "--deal", held)
	assert.True(t, strings.HasPrefix(rep["say"].(string), cli.Lines(words("say.close.called_off"), words("say.close.nothing_done"))), rep["say"])

	shared := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", shared, "--input", writeJSON(t, fawnShare))
	_, err := answer(t, shared, check["check_id"].(string), "yes share it", shownCard(t, check["card"].(string)))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", shared, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"web","fields":[{"class":"email","value":"buyer@example.com"}]}`))
	closed = dealRun(t, "close", "--deal", shared, "--input", writeJSON(t, `{"status":"called_off"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_paid")), closed["say"])

	// A deal not chosen (another seller was) says the same.
	other2 := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	closed = dealRun(t, "close", "--deal", other2, "--input", writeJSON(t, `{"status":"not_selected"}`))
	assert.Equal(t, cli.Lines(words("say.close.called_off"), words("say.close.nothing_done")), closed["say"])
}

// "Show my receipts" reads `deal list --json`: each deal has its own line,
// with no id, plain or over a range.
func TestShowMyReceiptsSaysOneLinePerDeal(t *testing.T) {
	legacyDeals(t)
	closed, open, sale, thread := stickerStore(t)
	for _, args := range [][]string{{"--json"}, {"--json", "--since", "2000-01-01"}} {
		raw := dealListOf(t, args...)
		var deals []map[string]any
		if args[len(args)-1] == "--json" {
			require.NoError(t, json.Unmarshal([]byte(raw), &deals), raw)
		} else {
			var r struct {
				Deals []map[string]any `json:"deals"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &r), raw)
			deals = r.Deals
		}
		byID := map[string]string{}
		for _, d := range deals {
			say, _ := d["say"].(string)
			byID[d["deal_id"].(string)] = say
			plainSay(t, "listing", map[string]any{"say": say})
		}
		assert.Regexp(t, `^[^:]+: bought( for \$[0-9.,]+)?, [A-Z][a-z]{2} \d{1,2}\.$`, byID[closed], args)
		assert.Regexp(t, `^[^:]+: still open, since [A-Z][a-z]{2} \d{1,2}\.$`, byID[open], args)
		assert.Regexp(t, `^[^:]+: still open, since `, byID[thread], args)
		assert.Regexp(t, `^[^:]+: listed for sale, [A-Z][a-z]{2} \d{1,2}\.$`, byID[sale], args)
	}
}
