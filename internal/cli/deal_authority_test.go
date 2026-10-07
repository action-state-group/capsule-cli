package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authorityLine struct{ Layer, At, Text, Note string }

func authorityBlocks(t *testing.T, report map[string]any) []map[string]any {
	t.Helper()
	raw, ok := report["authority"].([]any)
	require.True(t, ok, "the report has an AUTHORITY block")
	out := make([]map[string]any, len(raw))
	for i, b := range raw {
		out[i] = b.(map[string]any)
	}
	return out
}

func authorityLines(block map[string]any) []authorityLine {
	var out []authorityLine
	for _, l := range block["layers"].([]any) {
		m := l.(map[string]any)
		note, _ := m["note"].(string)
		out = append(out, authorityLine{m["layer"].(string), m["at"].(string), m["text"].(string), note})
	}
	return out
}

// The merchant-changed counterexample, as the receipt shows it: every layer
// on its own line with its own time, in order. The platform's approval is
// its own check and never reads as the answer to the user's rules; nothing
// collapses into "you approved".
func TestAuthorityBlockListsEveryLayerWithItsTime(t *testing.T) {
	dealFixture(t)
	now := clockAt(t, time.Date(2026, 10, 6, 9, 2, 0, 0, time.UTC))
	id := openTypedFlight(t)
	*now = now.Add(11 * time.Minute) // 09:13
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	*now = now.Add(time.Minute) // 09:14
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	*now = now.Add(2 * time.Minute) // 09:16
	_, err := answerCheck(t, id, c["check_id"].(string), "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	*now = now.Add(time.Minute) // 09:17
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))
	require.Equal(t, false, paid["unchecked"])

	report := dealRun(t, "report", "--deal", id)
	blocks := authorityBlocks(t, report)
	require.Len(t, blocks, 1)
	assert.Equal(t, true, blocks[0]["covered"])
	assert.Equal(t, paid["capsule_id"], blocks[0]["step"])
	lines := authorityLines(blocks[0])
	layers := make([]string, len(lines))
	for i, l := range lines {
		layers[i] = l.Layer
	}
	assert.Equal(t, []string{"task_authority", "evaluation", "user_approval", "platform_approval", "action"}, layers)
	assert.Equal(t, "2026-10-06T09:02:00Z", lines[0].At)
	assert.Equal(t, `Your request recorded: "book the SJC to HOU flight under $600"`, lines[0].Text)
	assert.Equal(t, "2026-10-06T09:13:00Z", lines[1].At)
	assert.True(t, strings.HasPrefix(lines[1].Text, "Your rules: ASK, because "), lines[1].Text)
	assert.Contains(t, lines[1].Text, "Travel Super Discount")
	assert.Equal(t, "2026-10-06T09:16:00Z", lines[2].At)
	assert.Equal(t, `Approved by you: "yes, the new site is fine"`, lines[2].Text)
	assert.Equal(t, "On the card the check showed you.", lines[2].Note)
	assert.Equal(t, "2026-10-06T09:14:00Z", lines[3].At, "when it was observed")
	assert.Equal(t, `example-platform asked separately (native-gate): "Approve $558.80 purchase"; the reply it returned: "Approve"`, lines[3].Text)
	assert.Contains(t, lines[3].Note, "it does not answer your rules")
	assert.Equal(t, "2026-10-06T09:17:00Z", lines[4].At)
	assert.Equal(t, "Done: pay $558.80 to Travel Super Discount by card", lines[4].Text)
	for _, l := range lines {
		assert.NotContains(t, strings.ToLower(l.Text+" "+l.Note), "user approved")
		if l.Layer == "platform_approval" {
			assert.NotContains(t, l.Text, "Approved by you", "a platform's prompt is not the user's answer to their rules")
		}
	}

	// The trail says which authority the payment went ahead on.
	assert.Contains(t, report["trail"], "pay done, as checked: you approved this")
	assert.Contains(t, report["trail"], "example-platform separately asked you for approval (recorded as observed; it does not answer your rules)")
	assert.Contains(t, reportTexts(t, report, "did"), `platform_approval: example-platform separately asked you for approval: "Approve $558.80 purchase" (recorded as observed; it does not answer your rules)`)

	// The receipt page carries the same block, and still verifies.
	page := filepath.Join(t.TempDir(), "receipt.html")
	dealRun(t, "report", "--deal", id, "--html", page)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	b := embeddedBundle(t, string(raw))
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.GraphClosure.Status)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)
	ext := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	assert.Equal(t, toPlain(t, report["authority"]), toPlain(t, ext["authority"]))
	assert.Contains(t, string(raw), `host.append(el("h2", "Authority"))`)
}

// When the user's rules needed no approval, the block says DO, within your
// rules, beside a platform's own approval, rather than "you approved".
func TestAuthorityBlockDOBesideAPlatformApproval(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	c := checkPay(t, id, 600)
	platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $6.00", "600")
	require.Equal(t, false, payNow(t, id, 600)["unchecked"])

	report := dealRun(t, "report", "--deal", id)
	lines := authorityLines(authorityBlocks(t, report)[0])
	require.Len(t, lines, 4)
	assert.Equal(t, "Your rules: DO, within your rules (no approval needed)", lines[1].Text)
	assert.Equal(t, "platform_approval", lines[2].Layer)
	assert.Contains(t, lines[2].Note, "Your rules needed no approval: DO, within your rules.")
	assert.Contains(t, report["trail"], "pay done, as checked: I went ahead based on what you had already asked me to do")
	assert.Contains(t, reportTexts(t, report, "did"), "act: Did: pay $6.00 (I went ahead based on what you had already asked me to do)")
}

// A platform approval that stated another amount says so beside it.
func TestAuthorityBlockPlatformScopeMismatch(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	c := checkPay(t, id, 600)
	platformApproval(t, id, c["check_id"].(string), "Buy otter sticker for $9.99", "999")
	payNow(t, id, 600)
	lines := authorityLines(authorityBlocks(t, dealRun(t, "report", "--deal", id))[0])
	assert.Contains(t, lines[2].Note, "It stated another amount than the one done ($9.99, not $6.00).")
}

// Paying on the platform's approval alone, or on a card answered with no
// words, is not covered: it is sealed with no authority basis, and its block
// is the action and the reason it was not covered.
func TestAuthorityBlockShowsAnUncoveredAction(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	_, err := answerCheck(t, id, c["check_id"].(string), "", c["card"].(string))
	require.NoError(t, err)
	paid := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))
	require.Equal(t, true, paid["unchecked"])

	block := authorityBlocks(t, dealRun(t, "report", "--deal", id))[0]
	assert.Equal(t, false, block["covered"])
	lines := authorityLines(block)
	require.Len(t, lines, 1, "no sealed basis: nothing is re-decided for the block")
	assert.Equal(t, "action", lines[0].Layer)
	assert.True(t, strings.HasPrefix(lines[0].Text, "Done without a passing check or your approval: pay $558.80"), lines[0].Text)
	assert.Contains(t, lines[0].Note, "your words")
}

// The block is the sealed record: each covered action's layers are its
// action-record/v0's authority_basis refs, in order, with the evaluation it
// relied on (evaluation_ref) after the task authority, and a scope mismatch
// exactly where the basis states one.
func TestAuthorityBlockIsTheSealedBasis(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	platformApproval(t, id, c["check_id"].(string), "Approve $600.00 purchase", "60000")
	_, err := answerCheck(t, id, c["check_id"].(string), "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))
	sticker := openTypedSticker(t)
	checkPay(t, sticker, 600)
	payNow(t, sticker, 600)

	for _, deal := range []string{id, sticker} {
		report := dealRun(t, "report", "--deal", deal)
		assert.Equal(t, dealAuthorityOrder, report["authority_order"])
		records := chainRecords(t, deal)
		digest := map[string]string{}
		for _, se := range chainSteps(t, deal) {
			digest[se.CapsuleID] = se.Digest
		}
		for _, b := range authorityBlocks(t, report) {
			body := bodyOf(records[b["step"].(string)])
			var want []string
			for j, e := range body["authority_basis"].([]any) {
				entry := e.(map[string]any)
				want = append(want, entry["type"].(string)+"@"+digestOfRef(entry["ref"]))
				if entry["scope"] == "mismatch" {
					want[len(want)-1] += "/mismatch"
				}
				if j == 0 {
					want = append(want, "evaluation@"+digestOfRef(body["evaluation_ref"]))
				}
			}
			want = append(want, "action@"+digest[b["step"].(string)])
			var got []string
			for _, l := range b["layers"].([]any) {
				m := l.(map[string]any)
				line := m["layer"].(string) + "@" + digest[m["step"].(string)]
				if note, _ := m["note"].(string); strings.Contains(note, "another amount") {
					line += "/mismatch"
				}
				got = append(got, line)
			}
			assert.Equal(t, want, got, "the rendered layers are the sealed basis")
		}
	}
}

// A deal sealed in x-deal-v0 records keeps its report as it was: no block.
func TestNoAuthorityBlockForXDealV0Deals(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	report := dealRun(t, "report", "--deal", id)
	assert.NotContains(t, report, "authority")
}

// The block is the user's own copy: a counterparty's shared copy carries
// neither it nor the user's words or limit.
func TestAuthorityBlockStaysInTheUsersCopy(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	_, err := answerCheck(t, id, c["check_id"].(string), "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))

	b, raw := sharedCopy(t, id, dealAudienceCounterparty, "x")
	ext := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	assert.NotContains(t, ext, "authority")
	for _, private := range []string{"book the SJC to HOU flight", "yes, the new site is fine", "Approve $558.80 purchase", "max_total_minor", "$600.00"} {
		assert.NotContains(t, raw, private)
	}
}

// The merchant's email is compared with what went ahead: on a DO, the amount
// checked against what the user had already asked for, never their limit.
func TestApprovedAmountOnATypedDO(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	checkPay(t, id, 600)
	payNow(t, id, 600)
	events := chainSteps(t, id)
	state, err := foldDeal(events)
	require.NoError(t, err)
	amount, _, basis, _, limit := approvedAmount(events, state)
	require.NotNil(t, amount)
	assert.EqualValues(t, 600, *amount)
	assert.False(t, limit)
	assert.Equal(t, "the amount checked against what you had already asked for", basis)
}

// The rule still asks when the user named this very purchase over their
// limit, and the card says why it is an exception; naming the item without
// its price is not that.
func TestOverLimitExceptionWording(t *testing.T) {
	dealFixture(t)
	open := func(asked string) string {
		return dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
			"intent":{"verbatim":"get me the Otterly Chaos poster","max_total_minor":2500,"allowed":["pay"],"asked":`+asked+`},
			"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
			"terms":{"item":"Otterly Chaos poster","price_minor":6000,"currency":"USD"},
			"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	}
	check := `{"action":"pay","amount_minor":6000,"terms":{"item":"Otterly Chaos poster","price_minor":6000}}`
	c := dealRun(t, "check", "--deal", open(`{"item":"Otterly Chaos poster","price_minor":6000}`), "--input", writeJSON(t, check))
	require.Equal(t, "pause", c["verdict"], "the rule still asks")
	assert.Contains(t, c["card"], "Your rules normally ask above $25.00. You asked for this $60.00 purchase specifically. Approve this exception?")
	assert.NotContains(t, c["card"], "Over your limit")

	c = dealRun(t, "check", "--deal", open(`{"item":"Otterly Chaos poster"}`), "--input", writeJSON(t, check))
	assert.Contains(t, c["card"], "Over your limit of $25.00 ($60.00)")
	assert.NotContains(t, c["card"], "exception")
}

// The emailed receipt carries the same block, in both bodies.
func TestAuthorityBlockInTheEmailedReceipt(t *testing.T) {
	dealFixture(t)
	id := openTypedFlight(t)
	c := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, merchantCheck))
	platformApproval(t, id, c["check_id"].(string), "Approve $558.80 purchase", "55880")
	_, err := answerCheck(t, id, c["check_id"].(string), "yes, the new site is fine", c["card"].(string))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, merchantPay))
	eml := filepath.Join(t.TempDir(), "receipt.eml")
	dealRun(t, "report", "--deal", id, "--email", eml)
	raw, err := os.ReadFile(eml)
	require.NoError(t, err)
	_, bodies, _ := emailParts(t, raw)
	for _, body := range []string{bodies["text/plain"], bodies["text/html"]} {
		assert.Contains(t, body, "Authority")
		assert.Contains(t, body, "Approved by you: ")
		assert.Contains(t, body, "it does not answer your rules")
		assert.Contains(t, body, "Done: pay $558.80 to Travel Super Discount by card")
		assert.NotContains(t, strings.ToLower(body), "verified")
	}
}
