package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// words is the built-in pack's entry at key.
func words(key string) string { return cli.BuiltinWording().Entries[key] }

// plainSay fails when a spoken line carries what a user is never read: an
// id, a digest, a rule id, a flag or a UTC time.
func plainSay(t *testing.T, where string, out map[string]any) {
	t.Helper()
	say, _ := out["say"].(string)
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`[0-9a-f]{16,}`), regexp.MustCompile(`\bdeal-[0-9a-z]+`), regexp.MustCompile(`\b[rR][0-9]{2}\b`),
		regexp.MustCompile(`--[a-z]`), regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`), regexp.MustCompile(`\bUTC\b`),
	} {
		assert.NotRegexp(t, bad, say, where)
	}
}

// fawnOpen is a synthetic purchase with the shape of the 2026-10-20 one: a
// $1.75 sticker plus tax under the user's $2, from a seller the profile has
// never dealt with, sharing details before paying.
const fawnOpen = `{"type":"purchase","channel":"web",
	"intent":{"verbatim":"get me a cheap fawn sticker, no more than $2","asked":{"item":"Fawn Sticker"},"max_total_minor":200,"allowed":["pay","share_contact"]},
	"who":{"name":"sticker_shop_example","domain":"marketplace.example"},
	"terms":{"item":"Fawn Sticker","price_minor":175,"currency":"USD"},
	"recourse":{"rail":"card","refundable":true}}`

const fawnShare = `{"action":"share_contact","disclosing_to":"counterparty","description":"your details to ship the sticker","disclosing":["name","email","address","phone"]}`

const fawnPay = `{"action":"pay","amount_minor":190,"authorized_max_minor":200,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`

// transcript is what the agent says over a conversation: each step's say,
// and on a card the buttons after it.
type transcript struct{ lines []string }

func (tr *transcript) add(step string, out map[string]any) {
	voice, _ := out["voice"].(string)
	if voice == cli.VoiceSilent {
		tr.lines = append(tr.lines, "["+step+": silent]")
		return
	}
	tr.lines = append(tr.lines, "["+step+": "+voice+"]", out["say"].(string))
	if voice == cli.VoiceAsk {
		var labels []string
		if ask, ok := out["ask"].(map[string]any); ok {
			for _, o := range ask["options"].([]any) {
				labels = append(labels, "<"+o.(map[string]any)["label"].(string)+">")
			}
		} else {
			for _, o := range out["options"].([]any) {
				labels = append(labels, "<"+o.(map[string]any)["label"].(string)+">")
			}
		}
		tr.lines = append(tr.lines, strings.Join(labels, " "))
	}
	if a, ok := out["attach"].(map[string]any); ok {
		tr.lines = append(tr.lines, "(attached: "+a["media_type"].(string)+")")
	}
}

// The whole purchase, replayed from what the commands say: the agent's side
// of the conversation is these lines and nothing it made up.
func TestTheFawnPurchaseReplaysFromSayAlone(t *testing.T) {
	dealFixture(t)
	var tr transcript
	opened := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))
	tr.add("open", opened)
	id := opened["deal_id"].(string)

	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	tr.add("check share", share)
	ok, err := answer(t, id, share["check_id"].(string), "yes share it", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	tr.add("approval", ok)
	told := dealRun(t, "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
		`{"channel":"web","fields":[{"class":"name","value":"A. Buyer"},{"class":"email","value":"buyer@example.com"},{"class":"address","value":"1 Example St"},{"class":"phone","value":"555-0100"}]}`))
	tr.add("disclosure", told)

	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	tr.add("check pay", pay)
	if pay["verdict"] != "pass" {
		paid, err := answer(t, id, pay["check_id"].(string), "ok pay", shownCard(t, pay["card"].(string)))
		require.NoError(t, err)
		tr.add("approval", paid)
	}
	page := filepath.Join(t.TempDir(), "receipt.html")
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--html", page, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"rail":"card","reference":"card ending 4242"}`))
	tr.add("act", act)
	require.FileExists(t, page)
	closed := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
	tr.add("close", closed)

	for _, out := range []map[string]any{opened, ok, told, act, closed} {
		plainSay(t, "replay", out)
	}
	got := strings.Join(tr.lines, "\n") + "\n"
	golden := filepath.Join("testdata", "narration", "fawn-replay.txt")
	if os.Getenv("UPDATE_NARRATION_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

// Each command speaks with its voice: the matrix of capsule-cli#345.
func TestEveryDealCommandSpeaks(t *testing.T) {
	dealFixture(t)
	first := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))
	assert.Equal(t, cli.VoiceLine, first["voice"], "the profile's first deal")
	assert.Equal(t, words("say.open.first"), first["say"])
	second := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))
	assert.Equal(t, cli.VoiceSilent, second["voice"], "a later deal says nothing")
	id := first["deal_id"].(string)

	msg := dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"agent","text":"Asked about shipping"}`))
	assert.Equal(t, cli.VoiceSilent, msg["voice"])
	assert.NotContains(t, msg, "say")

	// A pause asks with the card; a hold is one line.
	share := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	require.Equal(t, "pause", share["verdict"])
	assert.Equal(t, cli.VoiceAsk, share["voice"])
	assert.Equal(t, share["card"], share["say"], "the card is what is said")
	held := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", share["check_id"].(string), "--choice", "hold", "--said", "not yet", "--shown-card", shownCard(t, share["card"].(string)))
	assert.Equal(t, cli.VoiceLine, held["voice"])
	assert.Equal(t, words("say.approval.hold"), held["say"])
	// An answer that is not applied never says "you OK'd".
	again, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	assert.Equal(t, false, again["proceed"])
	assert.Equal(t, words("say.approval.not_applied.answered"), again["say"])

	share = dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnShare))
	ok, err := answer(t, id, share["check_id"].(string), "yes", shownCard(t, share["card"].(string)))
	require.NoError(t, err)
	assert.Equal(t, words("say.approval.proceed.share_contact"), ok["say"])

	// Asking for a higher limit is a card with no button that says yes.
	raise := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"actually up to $5","max_total_minor":500,"allowed":["pay","share_contact"]}`))
	assert.Equal(t, cli.VoiceAsk, raise["voice"])
	ask := raise["ask"].(map[string]any)
	assert.Equal(t, words("card.tag.ask"), ask["tag"])
	assert.Equal(t, "Raise your limit from $2 to $5?", ask["question"])
	assert.Equal(t, "words", ask["reply"])
	assert.Equal(t, []any{map[string]any{"label": "Hold", "choice": "hold"}}, ask["options"])
	assert.Equal(t, words("card.tag.ask")+"\nRaise your limit from $2 to $5?\n"+words("card.reply.words"), raise["say"])
	limits := dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", raise["capsule_id"].(string), "--choice", "confirm_limits", "--said", "yes up to five")
	assert.Equal(t, "Got it: your limit is now $5.", limits["say"])
	within := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"no more sharing, just pay","max_total_minor":500,"allowed":["pay"]}`))
	assert.Equal(t, cli.VoiceSilent, within["voice"])

	// A pass the share's card promised to ask about still asks
	// (TestAPromisedPaymentIsAskedEvenWhenItPasses); the act is the receipt.
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))}})
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, fawnPay))
	if pay["verdict"] == "pass" {
		assert.Equal(t, cli.VoiceAsk, pay["voice"], "the share's card said it would ask before paying")
	} else {
		_, err = answer(t, id, pay["check_id"].(string), "ok", shownCard(t, pay["card"].(string)))
		require.NoError(t, err)
	}
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"rail":"card"}`))
	assert.Equal(t, cli.VoiceReport, act["voice"])
	assert.NotContains(t, act, "attach", "no page without --html")
	assert.NotContains(t, act["say"], words("report.view_details"))
	assert.True(t, strings.HasPrefix(act["say"].(string), "Done: Fawn Sticker, $1.90, within your $5 limit."), act["say"])
	plainSay(t, "act", act)

	// A report with a page says View details and attaches it.
	page := filepath.Join(t.TempDir(), "receipt.html")
	rep := dealRun(t, "report", "--deal", id, "--html", page)
	assert.Equal(t, cli.VoiceReport, rep["voice"])
	assert.Equal(t, map[string]any{"path": page, "media_type": "text/html", "label": words("report.private")}, rep["attach"])
	assert.True(t, strings.HasSuffix(rep["say"].(string), "\n"+words("report.view_details")))
	plainSay(t, "report", rep)

	// A pending close stays a line; a final close is the final receipt.
	pending := dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"pending"}`))
	assert.Equal(t, cli.VoiceLine, pending["voice"])
	assert.Equal(t, words("say.close.open"), pending["say"])
	final := filepath.Join(t.TempDir(), "final.html")
	closed := dealRun(t, "close", "--deal", id, "--html", final, "--input", writeJSON(t, `{"status":"received"}`))
	assert.Equal(t, cli.VoiceReport, closed["voice"])
	assert.Equal(t, words("say.close.received")+"\n"+words("report.view_details"), closed["say"])
	require.FileExists(t, final)

	// Silent commands.
	for _, args := range [][]string{
		{"export", "--deal", id, "--output", filepath.Join(t.TempDir(), "export.json")},
		{"checkpoint", "status", "--deal", id},
		{"tick"},
	} {
		out := dealRun(t, args...)
		assert.Equal(t, cli.VoiceSilent, out["voice"], args[0])
		assert.NotContains(t, out, "say", args[0])
	}
	for _, args := range [][]string{{"--profile", "deal", "profile", "show", "deal"}, {"plugin", "ls"}} {
		out, err := invoke(t, "", args...)
		require.NoError(t, err, out)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &m), out)
		assert.Equal(t, cli.VoiceSilent, m["voice"], args)
	}

	// Reports across deals.
	deadlines := dealRun(t, "deadlines")
	assert.Equal(t, cli.VoiceReport, deadlines["voice"])
	assert.Equal(t, words("say.deadlines.none"), deadlines["say"])
	history := dealRun(t, "history", "--deal", id, "--to", "a reader", "--out", filepath.Join(t.TempDir(), "history.json"))
	assert.Equal(t, cli.VoiceReport, history["voice"])
	assert.Equal(t, words("say.history.one"), history["say"])
}

// A step's page that cannot be written is said apart: the step itself is
// sealed and the command succeeds.
func TestAReceiptPageThatFailsIsNotAFailedStep(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	taken := filepath.Join(t.TempDir(), "taken.html")
	require.NoError(t, os.WriteFile(taken, []byte("x"), 0o600))
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--html", taken, "--input", writeJSON(t, `{"action":"pay","amount_minor":190,"rail":"card"}`))
	assert.NotEmpty(t, act["step"], "the act is sealed")
	assert.NotEmpty(t, act["attach_error"])
	assert.NotContains(t, act, "attach")
	assert.True(t, strings.HasSuffix(act["say"].(string), words("report.attach_failed")))

	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "message", "--html", taken, "--input", writeJSON(t, `{"from":"agent","text":"x"}`))
	assert.ErrorContains(t, err, "--html writes the updated receipt page")
}

func TestASaleListingSaysWhatIsListed(t *testing.T) {
	dealFixture(t)
	out := dealRun(t, "sale", "new", "--input", repFile("sale.json"))
	assert.Equal(t, cli.VoiceLine, out["voice"])
	assert.Equal(t, "Listed: example bicycle at $1,900; I'll keep each buyer in their own thread.", out["say"])
}

func TestPaymentAndDeliveryNotesSayOneLine(t *testing.T) {
	id, dueStep := sellerDealWithDueDelivery(t)
	buyer := newBuyerSide(t)
	terms, _ := buyer.terms()
	paid := notePaymentReceived(t, id, terms, x402Settle(true))
	assert.Equal(t, cli.VoiceLine, paid["voice"])
	assert.Equal(t, "Payment received: 1.50 USDC.", paid["say"], "the amount, with the asset's name, never its id")
	assert.Equal(t, "", receivedAmount([]byte(`{"received":{"value":"150","assetCode":"eip155:1/erc20:0xabc","assetScale":2}}`)), "an unknown asset is not read out")
	assert.Equal(t, "", receivedAmount([]byte(`{}`)))
	delivered := dealRun(t, "note", "--deal", id, "--kind", "delivered", "--terms", terms,
		"--content", writeFile(t, "content.bin", deliveredContent), "--resolves", dueStep)
	assert.Equal(t, cli.VoiceLine, delivered["voice"])
	assert.Equal(t, words("say.delivered"), delivered["say"])

	out, err := invoke(t, "", "--profile", "deal", "deal", "settlement", "--deal", id, "--leg", terms)
	require.NoError(t, err, out)
	var settled map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &settled))
	assert.Equal(t, cli.VoiceReport, settled["voice"])
	assert.Equal(t, words("say.settlement.one_side"), settled["say"])
}

func TestAFailedDealStepSaysWhetherItHappened(t *testing.T) {
	deal := &cobra.Command{Use: "deal"}
	note := func(kind string) *cobra.Command {
		c := &cobra.Command{Use: "note"}
		c.Flags().String("kind", kind, "")
		deal.AddCommand(c)
		return c
	}
	under := func(name string) *cobra.Command {
		c := &cobra.Command{Use: name}
		deal.AddCommand(c)
		return c
	}
	sale := &cobra.Command{Use: "sale"}
	saleClose := &cobra.Command{Use: "close"}
	sale.AddCommand(saleClose)
	deal.AddCommand(sale)
	assert.Equal(t, "other", dealErrorPhase(saleClose), "a sale's close records no step of the user's own")
	for _, tc := range []struct {
		cmd   *cobra.Command
		phase string
	}{
		{under("check"), "before"},
		{under("open"), "before"},
		{note("approval"), "before"},
		{note("intent"), "before"},
		{note("disclosure"), "before"},
		{note("act"), "after"},
		{note("evidence"), "after"},
		{note(noteKindPaymentReceived), "after"},
		{note(noteKindDelivered), "after"},
		{under("close"), "after"},
		{note("message"), "other"},
		{under("report"), "other"},
	} {
		assert.Equal(t, tc.phase, dealErrorPhase(tc.cmd), tc.cmd.Name())
	}
}

// A note asking for several changes names them all: confirming it applies
// them all.
func TestALimitRaiseCardNamesEveryChange(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	raise := dealRun(t, "note", "--deal", id, "--kind", "intent", "--input", writeJSON(t, `{"verbatim":"up to $5, and you can give them my login","max_total_minor":500,"allowed":["pay","share_contact","share_credentials"]}`))
	ask := raise["ask"].(map[string]any)
	assert.Equal(t, "Raise your limit from $2 to $5 and let me also share a login or code on this errand?", ask["question"])
	assert.Equal(t, words("say.approval.limit_hold"), ask["hold_say"])
}

// The receipt says what each OK'd payment was, and never adds money of two
// currencies.
func TestTheReceiptSaysWhatWasOKd(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	pay := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":250,"authorized_max_minor":250,"terms":{"item":"Fawn Sticker","price_minor":175},"recourse":{"rail":"card","refundable":true}}`))
	require.Equal(t, "pause", pay["verdict"])
	_, err := answer(t, id, pay["check_id"].(string), "ok pay it", shownCard(t, pay["card"].(string)))
	require.NoError(t, err)
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":250,"rail":"card"}`))
	lines := strings.Split(act["say"].(string), "\n")
	assert.Equal(t, "Done: Fawn Sticker, $2.50.", lines[0], "over the limit: not said to be within it")
	assert.Equal(t, "You OK'd 1 thing on the way: paying $2.50.", lines[1])

	other := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":100,"currency":"EUR","rail":"card"}`))
	assert.True(t, strings.HasPrefix(other["say"].(string), "Done: Fawn Sticker, $2.50."), "the euro payment is not added to the dollars: %s", other["say"])
}

// No value is spoken empty: a missing amount, price or name picks the line
// that does without it.
func TestNoLineIsSpokenHalfFilled(t *testing.T) {
	w := cli.BuiltinWording()
	assert.Equal(t, words("say.approval.proceed.other"), approvalNarration(w, dealApproval{Choice: "proceed"}, nil, dealState{}).Say)
	assert.Equal(t, "Listed: bike; I'll keep each buyer in their own thread.", saleLine(w, dealTerms{Item: "bike"}).Say)
	price := int64(55000)
	assert.Equal(t, "Listed: bike at $550; I'll keep each buyer in their own thread.", saleLine(w, dealTerms{Item: "bike", PriceMinor: &price, Currency: "USD"}).Say)
	assert.Equal(t, "Countersigned.", countersignLine(w, dealCountersignView{Rung: "countersigned"}).Say)
	assert.Equal(t, "The countersignature did not check out.", countersignLine(w, dealCountersignView{Finding: "unverified"}).Say)
	named := dealCountersignView{Entries: []cli.CountersignatureReport{{SignerName: "Example Countersigner"}}}
	assert.Equal(t, "The countersignature from Example Countersigner did not check out.", countersignLine(w, named).Say)
	assert.Equal(t, "shop.example's email checks out: signed by shop.example.", verifyEmailLine(w, "", "shop.example", true, "shop.example").Say)
	assert.Equal(t, "Shop's email did not check out, so I'm not relying on it.", verifyEmailLine(w, "Shop", "shop.example", false, "").Say)
}

// Two answers that read the same are said once on the receipt.
func TestTheReceiptSaysEachOKOnce(t *testing.T) {
	approval := func(check string) sealedEvent {
		return sealedEvent{Event: dealEvent{Kind: "approval", Approval: &dealApproval{Check: check, Choice: "proceed", Proceed: true, Approver: "user"}}}
	}
	check := func(id, action string) sealedEvent {
		return sealedEvent{CapsuleID: id, Event: dealEvent{Kind: "check", Check: &dealCheckResult{Action: action}}}
	}
	open := sealedEvent{Event: dealEvent{Kind: "open", Open: &dealOpen{Terms: dealTerms{Item: "lease", Currency: "USD"}}}}
	lines := receiptLines(cli.BuiltinWording(), []sealedEvent{open, check("c1", "sign"), approval("c1"), check("c2", "sign"), approval("c2")})
	assert.Contains(t, lines, "You OK'd 1 thing on the way: going ahead.")
}

// --html never goes unsaid: on a deferred email the result says why there
// is no page, and a pending close still writes it.
func TestHTMLIsNeverDroppedSilently(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnOpen))["deal_id"].(string)
	page := filepath.Join(t.TempDir(), "pending.html")
	pending := dealRun(t, "close", "--deal", id, "--html", page, "--input", writeJSON(t, `{"status":"pending"}`))
	assert.Equal(t, cli.VoiceLine, pending["voice"])
	assert.Equal(t, page, pending["attach"].(map[string]any)["path"])
	require.FileExists(t, page)
}

// The phase is read on the tree the plugin really runs (capsulectl-deal),
// not only under a parent named deal.
func TestTheRealDealTreeKnowsWhetherAStepHappened(t *testing.T) {
	root := cli.NewPluginCommand(DealMetadata, DealCommands()...)
	find := func(args ...string) *cobra.Command {
		c, _, err := root.Find(args)
		require.NoError(t, err)
		return c
	}
	note := find("note")
	require.NoError(t, note.Flags().Set("kind", "act"))
	assert.Equal(t, "after", dealErrorPhase(note))
	assert.Equal(t, "after", dealErrorPhase(find("close")))
	assert.Equal(t, "before", dealErrorPhase(find("check")))
	assert.Equal(t, "other", dealErrorPhase(find("sale", "close")))
}

// Dropping what the buyer must be told is on the card beside the floor.
func TestALimitCardSaysWhatTheBuyerIsNoLongerTold(t *testing.T) {
	w := cli.BuiltinWording()
	floor, lower := int64(170000), int64(160000)
	cond := []string{"condition"}
	none := []string{}
	n := intentAsk(w, dealLimitSet{MinTotalMinor: &floor, OrderedRepresentations: &cond}, dealLimitSet{MinTotalMinor: &lower, OrderedRepresentations: &none}, "USD")
	assert.Equal(t, "Lower the lowest price you'll take from $1,700 to $1,600 and stop telling them about the condition?", n.Ask.Question)
}

func TestAmountsAreSaidAsPeopleSayThem(t *testing.T) {
	assert.Equal(t, "$1,700", spokenMoney(170000, "USD"))
	assert.Equal(t, "$1.90", spokenMoney(190, "USD"))
	assert.Equal(t, "$1,234,567.05", spokenMoney(123456705, "usd"))
	assert.Equal(t, "-$5", spokenMoney(-500, "USD"))
	assert.Equal(t, "¥1,500", spokenMoney(1500, "JPY"))
	assert.Equal(t, "12.50 CHF", spokenMoney(1250, "CHF"))
}
