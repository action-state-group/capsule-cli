package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodedRun is a long hex or base64-alphabet run: a digest, a signature, a
// key. Random content of that kind holds any short digit string now and then
// ("99812" inside a 64-hex digest), so a check for a private value in a shared
// copy leaves these runs out; none of the private values is one.
var encodedRun = regexp.MustCompile(`[0-9A-Za-z+/_=-]{40,}`)

// assertNoPrivateValue fails when v appears in s outside a long encoded run,
// case-insensitively.
func assertNoPrivateValue(t *testing.T, s, v string, msgAndArgs ...any) {
	t.Helper()
	text := strings.ToLower(encodedRun.ReplaceAllString(s, " "))
	assert.NotContains(t, text, strings.ToLower(v), msgAndArgs...)
}

// The private values a shared copy must never carry, all present in the
// deal's baseline: a home address, a full card number, a verification code
// and the counterparty's contact details.
const (
	homeAddress = "41 Larkspur Lane, Apt 3, Springfield"
	cardNumber  = "4111 1111 1111 1111"
	verifyCode  = "739142"
	shopEmail   = "orders@stickers.example"
	shopPhone   = "+1 555 010 7788"
	shopName    = "Sticker Shop Ltd"
)

// unicodeEvasions writes the address and the code so that a byte-for-byte
// match misses them: Cyrillic а and е, fullwidth digits, a zero-width space,
// and the code inside a query string and a URL path.
const unicodeEvasions = "Ship to 41 L\u0430rkspur Lane, Springfi\u0435ld. Code \uff17\uff13\uff19\uff11\uff14\uff12, or 739\u200b142. " +
	"See ?order_ref=G739142 and track/ABCDEFGHIJ739142."

// openPrivateDeal opens a purchase whose baseline holds every prohibited
// value, then a message repeating them, a check, an approval if the check
// paused, and the payment with its card reference.
func openPrivateDeal(t *testing.T) string {
	t.Helper()
	open := map[string]any{
		"type": "purchase", "channel": "web",
		"intent": map[string]any{
			"verbatim": "Buy the sticker, ship it to " + homeAddress + ", pay with card " + cardNumber + ", the code they texted me is " + verifyCode,
			"allowed":  []string{"pay", "commit"},
		},
		"who":      map[string]any{"name": shopName, "domain": "stickers.example", "email": shopEmail, "phone": shopPhone},
		"terms":    map[string]any{"item": "sticker", "price_minor": 627, "currency": "USD", "place": homeAddress},
		"claims":   []any{map[string]any{"text": "order held under code " + verifyCode, "source": "checkout page"}},
		"recourse": map[string]any{"rail": "card", "refundable": true},
	}
	raw, err := json.Marshal(open)
	require.NoError(t, err)
	dealID := dealRun(t, "open", "--input", writeJSON(t, string(raw)))["deal_id"].(string)
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t,
		`{"from":"counterparty","channel":"web","text":"Your verification code is `+verifyCode+`. Card on file 4111-1111-1111-1111, shipping to `+homeAddress+`."}`))
	// The same values, written to slip past a pattern: inside words, split by
	// a space, and the address in pieces and in another order.
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t,
		`{"from":"counterparty","channel":"web","text":"Codes: G`+verifyCode+`, code`+verifyCode+`, 739 142. Deliver to 41 Larkspur, Springfield or Larkspur Lane 41."}`))
	// And written to slip past a byte match: lookalike letters, fullwidth
	// digits, a zero-width space, inside a URL.
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t,
		`{"from":"counterparty","channel":"web","text":"`+unicodeEvasions+`"}`))
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":627,"authorized_max_minor":627,"recourse":{"rail":"card","refundable":true}}`))
	if check["verdict"] == "pause" {
		dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed")
	}
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t,
		`{"action":"pay","amount_minor":627,"payee":"`+shopName+`","rail":"card","reference":"card `+cardNumber+`"}`))
	return dealID
}

// assertCarriesNone asserts the page carries none of the prohibited values,
// as written or in an obvious encoding.
func assertCarriesNone(t *testing.T, page string) {
	t.Helper()
	low := strings.ToLower(page)
	for _, v := range []string{homeAddress, "Larkspur", "Springfield", cardNumber, "4111-1111-1111-1111", "4111111111111111", shopEmail, shopPhone, "5550107788", shopName, "texted me",
		"G" + verifyCode, "code" + verifyCode, "739 142", "739-142",
		"L\u0430rkspur", "Springfi\u0435ld", "\uff17\uff13\uff19\uff11\uff14\uff12", "739\u200b142", "order_ref=G739142", "ABCDEFGHIJ739142"} {
		for _, enc := range []string{v, base64.StdEncoding.EncodeToString([]byte(v)), base64.RawURLEncoding.EncodeToString([]byte(v)), hex.EncodeToString([]byte(v)), url.QueryEscape(v), url.PathEscape(v)} {
			if len(enc) < 6 {
				continue
			}
			assert.NotContains(t, low, strings.ToLower(enc), "the shared copy carries %q (as %q)", v, enc)
		}
	}
	// The code is six digits: hex digests and base64 signatures can hold that
	// run by chance, so it must not stand alone (bounded by non-token characters).
	code := regexp.MustCompile(`(^|[^0-9A-Za-z+/_-])` + verifyCode + `([^0-9A-Za-z+/_-]|$)`)
	assert.False(t, code.MatchString(page), "the shared copy carries the verification code")
	for _, enc := range []string{hex.EncodeToString([]byte(verifyCode)), base64.StdEncoding.EncodeToString([]byte(verifyCode))} {
		assert.NotContains(t, page, enc)
	}
}

func shareRun(t *testing.T, dealID, audience, path string) map[string]any {
	t.Helper()
	out := dealRun(t, "report", "--deal", dealID, "--html", path, "--share", audience, "--to", "the shop's support desk")
	assert.Equal(t, path, out["html"])
	assert.Equal(t, "self_attested", out["assurance"].(map[string]any)["rung"], "no witness configured: never more than sealed by my agent")
	assert.NotContains(t, out, "asked", "a share's output carries none of the local report's text")
	assert.NotContains(t, out, "trail")
	return out["share"].(map[string]any)
}

// sharedRecords reads the sealed disclosure records back from the local
// store, and the digests on the deal's disclosure log.
func sharedRecords(t *testing.T, dealID string) ([]map[string]any, []string) {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, dsn, err := sqliteConnection(p)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(`SELECT record_digest, record FROM deal_disclosures WHERE deal_id=? ORDER BY n`, dealID)
	require.NoError(t, err)
	defer rows.Close()
	var records []map[string]any
	for rows.Next() {
		var digest, raw string
		require.NoError(t, rows.Scan(&digest, &raw))
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &record))
		again, err := canonical.JSONDigest(record)
		require.NoError(t, err)
		assert.Equal(t, digest, again, "the stored record is the one whose digest was sealed")
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, nil
	}
	log, err := openLog(context.Background(), p, dsn, dealDisclosureLogID(dealID))
	require.NoError(t, err)
	defer log.Close()
	entries, err := log.ScanEntries(context.Background(), 0, 100)
	require.NoError(t, err)
	var onLog []string
	for _, e := range entries {
		onLog = append(onLog, hex.EncodeToString(e.Value))
	}
	return records, onLog
}

func TestDealShareCounterpartyCarriesNoPrivateValues(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	dir := t.TempDir()

	// The user's own copy keeps everything: the baseline really holds the address.
	keep := filepath.Join(dir, "keep.html")
	keepOut := dealRun(t, "report", "--deal", dealID, "--html", keep)
	assert.Equal(t, "self_attested", keepOut["assurance"].(map[string]any)["rung"], "no witness configured: never more than sealed by my agent")
	raw, err := os.ReadFile(keep)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "Larkspur Lane", "the baseline record holds the home address")
	keepExt := embeddedBundle(t, string(raw))["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	assert.Equal(t, "keep", keepExt["audience"])
	assertScope(t, string(raw), keepExt)
	kept, _ := sharedRecords(t, dealID)
	assert.Empty(t, kept, "keeping your own copy is not a share")

	page := filepath.Join(dir, "receipt.html")
	share := shareRun(t, dealID, "counterparty", page)
	raw, err = os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	assertCarriesNone(t, html)
	assert.NotContains(t, html, "(from checkout page)", "claim sources are for an adjudicator only")
	assert.NotContains(t, html, "Your verification code is", "message text is for an adjudicator only")

	// It still verifies offline, from the file alone.
	b := embeddedBundle(t, html)
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.GraphClosure.Status)
	assert.Equal(t, "pass", v.IntervalCoverage.Status, v.IntervalCoverage.Findings)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)
	statuses := map[string]int{}
	for _, d := range v.Disclosures {
		statuses[string(d.Status)]++
	}
	assert.Positive(t, statuses["withheld"], "records carrying private values are withheld")
	assert.Positive(t, statuses["disclosure_match"], "records with only amounts, rails, times and digests are disclosed")
	assert.Equal(t, len(v.Disclosures), statuses["withheld"]+statuses["disclosure_match"])
	out, err := invoke(t, "", "verify", "--bundle", page)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"verdict":"VALID"`)

	ext := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	assert.Equal(t, "counterparty", ext["audience"])
	assertScope(t, html, ext)
	assert.Contains(t, html, "What the agent did is the agent's own report", "what was done is not independently confirmed")
	assert.Contains(t, html, "sealed on the agent's device, where they happened", "the conversation is rightly self-attested")
	assert.Equal(t, "capsulectl verify --bundle receipt.html", ext["verify_command"])
	assert.NotContains(t, ext, "asked_opening")
	assert.Contains(t, html, "What this does not claim")
	assert.Contains(t, html, "Sealed by my agent")
	assert.Contains(t, html, "tamper-evident, not non-repudiation")
	assert.Contains(t, html, "does not prove the merchant shipped")
	assert.Contains(t, html, "Did: pay $6.27 by card", "amounts and rails stay")

	// The share is on record: stored, sealed on the disclosure log, and
	// naming what was shared, with whom, in what mode and what was withheld.
	records, onLog := sharedRecords(t, dealID)
	require.Len(t, records, 1)
	r := records[0]
	assert.Equal(t, []string{share["disclosure_record"].(string)}, onLog)
	assert.Equal(t, "disclosure_record", r["type"])
	assert.Equal(t, dealID, r["deal_id"])
	assert.Equal(t, "counterparty", r["audience"])
	assert.Equal(t, "the shop's support desk", r["recipient"])
	assert.Equal(t, "selected", r["payloads_mode"])
	assert.NotEmpty(t, r["withheld_records"])
	assert.Contains(t, r["suppressed_receipt_fields"], "home address")
	assert.Contains(t, r["suppressed_receipt_fields"], "verification codes")
	assert.Contains(t, r["suppressed_receipt_fields"], "card and payment identifiers")
	sum := sha256.Sum256(raw)
	assert.Equal(t, hex.EncodeToString(sum[:]), r["receipt_sha256"], "the record names the exact page shared")
	assert.Equal(t, float64(1), share["sequence"])
}

func TestDealShareAdjudicatorAddsMessagesAndSources(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	dir := t.TempDir()
	shareRun(t, dealID, "counterparty", filepath.Join(dir, "counterparty.html"))
	page := filepath.Join(dir, "adjudicator.html")
	share := shareRun(t, dealID, "adjudicator", page)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	assertCarriesNone(t, html)
	assert.Contains(t, html, "Your verification code is [withheld]", "message text, with codes, cards and addresses withheld")
	assert.Contains(t, html, "Ship to [withheld] [withheld] Lane, [withheld]. Code [withheld], or [withheld]. See ?order_ref=[withheld] and track/[withheld].",
		"lookalike letters, fullwidth digits, zero-width spaces and codes in URLs are withheld")
	assert.Contains(t, html, "Codes: [withheld], [withheld], [withheld]. Deliver to [withheld] [withheld], [withheld] or [withheld] Lane [withheld].",
		"codes inside words and split by a space, and the address in pieces, are withheld")
	assert.Contains(t, html, "(from checkout page)", "claim sources")

	b := embeddedBundle(t, html)
	assertScope(t, html, b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any))
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.IntervalCoverage.Status)
	assert.Equal(t, "pass", v.PerRecordMembership.Status)
	out, err := invoke(t, "", "verify", "--bundle", page)
	require.NoError(t, err, out)

	// A sealed disclosure record for every share.
	records, onLog := sharedRecords(t, dealID)
	require.Len(t, records, 2)
	assert.Len(t, onLog, 2)
	assert.Equal(t, share["disclosure_record"], onLog[1])
	assert.Equal(t, "adjudicator", records[1]["audience"])
	assert.NotContains(t, records[1]["suppressed_receipt_fields"], "message text")
}

func TestDealShareNeedsAnAudienceAndARecipient(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	page := filepath.Join(t.TempDir(), "r.html")
	for _, args := range [][]string{
		{"--html", page, "--share", "counterparty"},
		{"--share", "counterparty", "--to", "x"},
		{"--html", page, "--to", "x"},
		{"--html", page, "--share", "everyone", "--to", "x"},
		// The email and the bundle file are the user's own copy.
		{"--html", page, "--share", "counterparty", "--to", "x", "--email", page + ".eml"},
		{"--html", page, "--share", "adjudicator", "--to", "x", "--bundle", page + ".json"},
	} {
		out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "report", "--deal", dealID}, args...)...)
		assert.Error(t, err, out)
	}
	_, err := os.Stat(page)
	assert.True(t, os.IsNotExist(err), "nothing written")
}

func TestDealShareScrub(t *testing.T) {
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Who:    dealWho{Payee: "M. Torres"},
		Terms:  dealTerms{Place: homeAddress},
		Intent: dealIntent{Verbatim: "the code is " + verifyCode},
	}}}}
	p := dealPrivateValues(events)
	for in, want := range map[string]string{
		"your code is 4821":                         "your code is [withheld]",
		"pay $1200.00 by card on 2026-10-03":        "pay $1200.00 by card on 2026-10-03",
		"card 4111 1111 1111 1111 please":           "card [withheld] please",
		"ship to 12 Elm St, Apt 4B today":           "ship to [withheld] today",
		"mail me at a@b.example or 555 010 2044":    "mail me at [withheld] or [withheld]",
		"pay m. torres by Zelle":                    "pay [withheld] by Zelle",
		"G739142 then code739142 then 739 142":      "[withheld] then [withheld] then [withheld]",
		"G-7391 or 7391x":                           "[withheld] or [withheld]",
		"41 Larkspur, Springfield":                  "[withheld] [withheld], [withheld]",
		"Larkspur Lane 41":                          "[withheld] Lane [withheld]",
		"rent 2 jet skis for $6.27":                 "rent 2 jet skis for $6.27",
		"41 L\u0430rkspur, Springfi\u0435ld":        "[withheld] [withheld], [withheld]",
		"code \uff17\uff13\uff19\uff11\uff14\uff12": "code [withheld]",
		"code 739\u200b142 or 73\u00ad9142":         "code [withheld] or [withheld]",
		"?order_ref=G739142":                        "?order_ref=[withheld]",
		"track/ABCDEFGHIJ739142":                    "track/[withheld]",
		"\u041f\u0440\u0438\u0432\u0435\u0442":      "\u041f\u0440\u0438\u0432\u0435\u0442", // Russian proper is left as written
	} {
		assert.Equal(t, want, p.scrub(in), in)
	}
}

// The gate reads the final page bytes with its own detectors.
func TestDealSharePageGate(t *testing.T) {
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Who:    dealWho{Name: shopName, Email: shopEmail},
		Terms:  dealTerms{Place: homeAddress},
		Intent: dealIntent{Verbatim: "pay with card " + cardNumber + ", the code is " + verifyCode},
	}}}}
	digest := "aa" + verifyCode + strings.Repeat("b", 56) // a code-shaped run inside a digest, by chance
	page := func(text string) []byte {
		return []byte("<!doctype html><script>" + string(evidenceGraphIIFE) + "</script><script>window.__BUNDLE__ = " +
			`{"line":"` + text + `","sig":"` + digest + `","amount_minor":120000}` + ";</script><script>" + dealViewJS + "</script>")
	}
	assert.NoError(t, dealPageGate(page("Did: pay $1200.00 by card · code [withheld]"), events))
	for _, leak := range []string{
		homeAddress, "Springfield", "LARKSPUR", base64.StdEncoding.EncodeToString([]byte(homeAddress)),
		base64.RawURLEncoding.EncodeToString([]byte(homeAddress)), hex.EncodeToString([]byte("Larkspur")),
		url.QueryEscape(homeAddress), `41 Larkspur Lane, Apt 3, Springfield \u003cb\u003e`,
		"G" + verifyCode, "code" + verifyCode, "739 142", "739-142", "x " + verifyCode,
		cardNumber, "4111-1111-1111-1111", "4111111111111111", base64.StdEncoding.EncodeToString([]byte(cardNumber)),
		shopEmail, shopName,
		"4242 4242 4242 4242", // any Luhn-valid card number, not only this deal's
		"your PIN is 5521", "OTP: 88213",
		"41 L\u0430rkspur Lane, Springfi\u0435ld", "Springfi\u00adeld", "\uff17\uff13\uff19\uff11\uff14\uff12", "739\u200b142",
		"?order_ref=G" + verifyCode, "track/ABCDEFGHIJ" + verifyCode,
	} {
		assert.Error(t, dealPageGate(page(leak), events), leak)
	}
}

// assertScope checks a receipt states its scope on its face, and never
// implies it is complete.
func assertScope(t *testing.T, html string, ext map[string]any) {
	t.Helper()
	const scope = "This receipt covers this one deal. It is not a record of everything the agent did."
	assert.Equal(t, scope, ext["scope"])
	assert.Contains(t, html, scope)
	for _, claim := range []string{"all activity", "complete history", "complete record", "full history"} {
		assert.NotContains(t, strings.ToLower(html), claim)
	}
}

func TestDealReportStatesItsScope(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	out := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, "This receipt covers this one deal. It is not a record of everything the agent did.", out["scope"])
}

// The gate derives its own needles: it must not call the scrubber.
func TestDealSharePageGateIsIndependent(t *testing.T) {
	src, err := os.ReadFile("deal_share_gate.go")
	require.NoError(t, err)
	for _, name := range []string{"dealLocalData", "addressFragments", "dealPrivate", "scrub(", "codeLocs", "phoneLocs", "shareNumberish", "shareAddress"} {
		assert.NotContains(t, string(src), name)
	}
}

// A merchant's own confirmation email in a shared copy. The counterparty's
// copy carries the order number, because the merchant's signature checks out
// against the deal's own counterparty and the id is an order number
// (shareableOrderID). It carries nothing else from the email: not the
// customer's name or address, not the signing domain. The adjudicator's copy
// carries no order number at all.
func TestDealShareMerchantEmail(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	dir := t.TempDir()
	private := []string{"Sam Customer", "sam.customer@mail.example", "orders@shop.example", "shop.example", "Shop Example", "99812", "card ending 4242"}

	page := filepath.Join(dir, "counterparty.html")
	shareRun(t, dealID, "counterparty", page)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	for _, v := range private {
		assertNoPrivateValue(t, html, v)
	}
	ext := embeddedBundle(t, html)["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	rows := ext["merchant"].([]any)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	assert.Equal(t, "SE-104233", row["order_id"], "a merchant-confirmed order number is shared with the counterparty")
	assert.Equal(t, true, row["verified"])
	assert.Equal(t, "$64.00", row["charged"])
	assert.NotContains(t, row, "items")
	assert.NotContains(t, row, "domains")
	out, err := invoke(t, "", "verify", "--bundle", page)
	require.NoError(t, err, out)

	page = filepath.Join(dir, "adjudicator.html")
	shareRun(t, dealID, "adjudicator", page)
	raw, err = os.ReadFile(page)
	require.NoError(t, err)
	for _, v := range append(private, "SE-104233", "104233") {
		assertNoPrivateValue(t, string(raw), v)
	}
	ext = embeddedBundle(t, string(raw))["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
	assert.NotContains(t, ext["merchant"].([]any)[0], "order_id", "no order number in the adjudicator's copy")
}

// The gate takes the shareable order number out before it checks, and only
// that: the same page with the customer's address still refuses.
func TestDealSharePageGateAllowsOnlyTheShareableOrderID(t *testing.T) {
	events := plantedEvents()
	events[0].Event.Open.Intent.Verbatim += " order SE-104233"
	assert.Error(t, dealPageGate(gatePage(t, "Order SE-104233"), events))
	assert.NoError(t, dealPageGate(gatePage(t, "Order SE-104233"), events, "SE-104233"))
	assert.Error(t, dealPageGate(gatePage(t, "Order SE-104233, "+homeAddress), events, "SE-104233"))
}

// The customer's own name, as the merchant's email addresses them (To: Sam
// Customer <...>), is private: withheld from a shared copy as written, in
// lookalike letters or spelled out, and refused by the gate on its own.
func TestDealShareWithholdsTheCustomersNameFromTheMerchantEmail(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	names := []string{"Sam Customer", "Sаm Custоmer", "S a m  C u s t o m e r", "SAM CUSTOMER"}
	msg, err := json.Marshal(map[string]any{"from": "counterparty", "text": "Shipping to " + strings.Join(names, ", ") + " today"})
	require.NoError(t, err)
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, string(msg)))

	page := filepath.Join(t.TempDir(), "adjudicator.html")
	shareRun(t, dealID, "adjudicator", page)
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	for _, n := range append(names, "Customer") {
		assert.NotContains(t, strings.ToLower(string(raw)), strings.ToLower(n))
	}
	assert.Contains(t, string(raw), "Shipping to [withheld], [withheld], [withheld], [withheld] today")

	// The gate alone, as if the scrubber had missed it.
	eml, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	events := plantedEvents()
	events = append(events, sealedEvent{Event: dealEvent{Kind: "evidence", Evidence: &dealEvidence{Email: &merchantEmail{Raw: eml}}}})
	for _, n := range names {
		assert.Error(t, dealPageGate(gatePage(t, "Shipping to "+n), events), n)
	}
	assert.NoError(t, dealPageGate(gatePage(t, "Shipping today"), events))
}

// Display names from the merchant email's recipient headers: a role
// ("Customer Support") is not a name; a one-word name is withheld only as a
// whole word, so it never takes a bite out of a longer one; a name of
// several words is withheld as before.
func TestDealShareDisplayNamesAreWholeWordsAndNotRoles(t *testing.T) {
	eml := []byte("From: Shop Example <orders@shop.example>\r\n" +
		"To: Grace <grace@mail.example>, Chris <chris@mail.example>, Customer Support <cs@mail.example>, Sam Customer <sam@mail.example>\r\n" +
		"Subject: Your order\r\n\r\nThanks for your order.\r\n")
	events := plantedEvents()
	events = append(events, sealedEvent{Event: dealEvent{Kind: "evidence", Evidence: &dealEvidence{Email: &merchantEmail{Raw: eml}}}})
	p := dealPrivateValues(events)

	for _, kept := range []string{
		"what a disgrace, just before Christmas",
		"Dear customer, the support team is here to help",
		"Graceful service from Christopher",
	} {
		assert.Equal(t, kept, p.scrub(kept))
		assert.NoError(t, dealPageGate(gatePage(t, kept), events), kept)
	}
	for in, want := range map[string]string{
		"Thanks Grace!":        "Thanks [withheld]!",
		"for G r a c e, today": "for [withheld], today",
		"to Grаce (lookalike)": "to [withheld] (lookalike)",
		"ecarG backwards":      "[withheld] backwards",
		"Hi Chris.":            "Hi [withheld].",
		"ship to Sam Customer": "ship to [withheld]",
	} {
		assert.Equal(t, want, p.scrub(in), in)
		assert.Error(t, dealPageGate(gatePage(t, in), events), in)
	}
}

// The floor for a one-word name is three letters. "Amy" is withheld, as a
// whole word only ("Amygdala" stays); "Al" is never matched, so a page that
// mentions AL (Alabama) or "Al" is neither scrubbed nor refused.
func TestDealShareOneWordNameFloorIsThreeLetters(t *testing.T) {
	eml := []byte("From: Shop Example <orders@shop.example>\r\n" +
		"To: Amy <amy@mail.example>, Al <al@mail.example>\r\n" +
		"Subject: Your order\r\n\r\nThanks for your order.\r\n")
	events := plantedEvents()
	events = append(events, sealedEvent{Event: dealEvent{Kind: "evidence", Evidence: &dealEvidence{Email: &merchantEmail{Raw: eml}}}})
	p := dealPrivateValues(events)
	_, words, _ := gateSecrets(events)
	assert.Equal(t, []string{"Amy"}, words, "only a name of three letters or more is a word needle")

	for _, kept := range []string{"the amygdala", "Al said hi", "Montgomery, AL", "an alpaca"} {
		assert.Equal(t, kept, p.scrub(kept))
		assert.NoError(t, dealPageGate(gatePage(t, kept), events), kept)
	}
	for _, in := range []string{"Thanks Amy!", "for A m y", "ymA"} {
		assert.Contains(t, p.scrub(in), "[withheld]", in)
		assert.NotContains(t, strings.ToLower(p.scrub(in)), "amy", in)
		assert.Error(t, dealPageGate(gatePage(t, in), events), in)
	}
}
