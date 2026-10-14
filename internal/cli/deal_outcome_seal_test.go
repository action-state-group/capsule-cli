package cli

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealsOf is every outcome_seal record of the deal, in order, as exported.
func sealsOf(t *testing.T, id string) []map[string]any {
	t.Helper()
	recs, _ := exportRecords(t, id)
	var seals []map[string]any
	for _, r := range recs {
		if block, ok := r[dealProfile].(map[string]any); ok && block["record_type"] == "outcome_seal" {
			seals = append(seals, r)
		}
	}
	return seals
}

// actsOf is the capsule id of each act of the deal sealed as an action, in
// order.
func actsOf(t *testing.T, id string) []string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, err := sql.Open("sqlite", p.Connection.Database)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := db.Query(`SELECT capsule_id FROM deal_steps WHERE deal_id=? AND kind='act' AND json_extract(local, '$.act.unchecked') IS NOT 1 ORDER BY n`, id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		ids = append(ids, c)
	}
	require.NoError(t, rows.Err())
	return ids
}

// A close seals the producer's own statement of how the deal's last act came
// out, just before the close: exactly the act's capsule id, the outcome and
// the act's class. The profile checker (the Python implementation) accepts
// the deal with it.
func TestDealCloseSealsTheOutcome(t *testing.T) {
	for _, c := range []struct{ name, input, outcome string }{
		{"received as agreed", `{"status":"received","delivered":{"item":"cat sticker"}}`, "delivered"},
		{"not received", `{"status":"not_received","reason":"user_closed"}`, "not_delivered"},
		{"received, with a difference", `{"status":"received","delivered":{"item":"dog sticker"}}`, "unknown"},
		{"still pending", `{"status":"pending"}`, "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dealFixture(t)
			id := retailDeal(t)
			closed := closeDealWith(t, id, c.input)
			acts := actsOf(t, id)
			require.Len(t, acts, 1)
			seal := closed["outcome_seal"].(map[string]any)
			assert.Equal(t, c.outcome, seal["outcome"])
			assert.Equal(t, acts[0], seal["step"])

			recs, path := exportRecords(t, id)
			seals := sealsOf(t, id)
			require.Len(t, seals, 1)
			body := seals[0]["body"].(map[string]any)
			assert.Equal(t, map[string]any{"step": acts[0], "outcome": c.outcome, "action_class": classMoneyPurchase}, body, "three fields, nothing else")
			assert.Equal(t, "outcome_seal", recs[len(recs)-2][dealProfile].(map[string]any)["record_type"], "sealed just before the close")
			for _, k := range []string{"counterparty", "channel", "refs"} {
				assert.NotContains(t, seals[0][dealProfile], k)
			}
			checkProfile(t, path)
		})
	}
}

// A deal with no act sealed as an action gets no seal: there is no step for
// it to name.
func TestDealCloseWithNoActSealsNoOutcome(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, stickerOpen))["deal_id"].(string)
	closed := closeDealWith(t, id, `{"status":"not_received","reason":"user_closed"}`)
	assert.NotContains(t, closed, "outcome_seal")
	assert.Empty(t, sealsOf(t, id))
}

// A cancel changes the deal's outcome, so it is sealed at once: refunded when
// it returns the payment (money.refund), cancelled when it returns none.
func TestDealCancelSealsTheOutcome(t *testing.T) {
	for _, c := range []struct {
		name           string
		cancelled      int
		outcome, class string
	}{
		{"returning the payment", 55880, "refunded", classMoneyRefund},
		{"returning nothing", 0, "cancelled", classExternalCommitOther},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := cancelAfterPurchaseOf(t, c.cancelled)
			acts := actsOf(t, id)
			require.Len(t, acts, 2)
			seals := sealsOf(t, id)
			require.Len(t, seals, 1, "the pay changes no outcome; the cancel does")
			assert.Equal(t, map[string]any{"step": acts[1], "outcome": c.outcome, "action_class": c.class}, seals[0]["body"])
			_, path := exportRecords(t, id)
			checkProfile(t, path)
		})
	}
}

// A cancel done without a passing check or the user's approval is sealed as
// an outcome, never as an action: it is no step a seal names, and it seals
// nothing (a seal would name the pay before it).
func TestDealUncheckedCancelSealsNoOutcome(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"cancel","amount_minor":0,"currency":"USD","rail":"card"}`))
	require.Equal(t, true, act["unchecked"])
	assert.NotContains(t, act, "outcome_seal")
	assert.Empty(t, sealsOf(t, id))
}

// openedBeforeSeals makes id a deal opened by a build before outcome seals:
// no stored step carries the mark (a record never did, so nothing sealed
// changes).
func openedBeforeSeals(t *testing.T, id string) {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, err := sql.Open("sqlite", p.Connection.Database)
	require.NoError(t, err)
	res, err := db.Exec(`UPDATE deal_steps SET local=json_remove(local, '$.outcome_sealing') WHERE deal_id=?`, id)
	require.NoError(t, err)
	changed, err := res.RowsAffected()
	require.NoError(t, err)
	require.NotZero(t, changed)
	require.NoError(t, db.Close())
}

// A deal opened before outcome seals is never given one, closed or
// cancelled after; in a History its outcome stays not_proven, beside a new
// deal's seal.
func TestDealOpenedBeforeSealsStaysNotProven(t *testing.T) {
	dealFixture(t)
	old := retailDeal(t)
	openedBeforeSeals(t, old)
	closed := closeDealWith(t, old, `{"status":"received","delivered":{"item":"cat sticker"}}`)
	assert.NotContains(t, closed, "outcome_seal")
	assert.Empty(t, sealsOf(t, old))

	fresh := retailDeal(t)
	closeDealWith(t, fresh, `{"status":"received","delivered":{"item":"cat sticker"}}`)
	path, _ := writeHistory(t, []string{old, fresh}, sentTo, old, fresh)
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
	outcomes := map[string]any{}
	for _, raw := range entry["deals"].([]any) {
		m := raw.(map[string]any)
		outcomes[m["deal_id"].(string)] = m["outcome"]
		if m["deal_id"] == old {
			assert.NotContains(t, m, "outcome_seals")
		}
	}
	assert.Equal(t, map[string]any{old: "not_proven", fresh: "delivered"}, outcomes)
	assert.Equal(t, 1.0, entry["outcome_counts"].(map[string]any)["not_proven"])
	assert.Equal(t, 1.0, entry["outcome_counts"].(map[string]any)["delivered"])
}

// sealedHistory is a History of one deal, paid then refunded then closed
// not received, with every step under a checkpoint: two seals, refunded then
// not_delivered.
func sealedHistory(t *testing.T) (string, string) {
	t.Helper()
	id := cancelAfterPurchase(t)
	closeDealWith(t, id, `{"status":"not_received","reason":"user_closed"}`)
	path, _ := writeHistory(t, []string{id}, sentTo, id)
	return id, path
}

// A History discloses each seal whole and nothing else, and verify reads
// them: the latest is the deal's outcome, every one is listed oldest first
// with its act's time and its own time apart, and the act's class is never
// said to match (its payload is withheld).
func TestDealHistoryRecountsOutcomesFromTheSeals(t *testing.T) {
	id, path := sealedHistory(t)
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
	assert.Empty(t, findings)
	assert.Equal(t, []any{"outcome", "action_class"}, entry["not_proven"], "a seal is the producer's word, not proof")
	member := entry["deals"].([]any)[0].(map[string]any)
	assert.Equal(t, "not_delivered", member["outcome"])
	seals := member["outcome_seals"].([]any)
	require.Len(t, seals, 2)
	acts := actsOf(t, id)
	for i, want := range []map[string]any{
		{"outcome": "refunded", "action_class": classMoneyRefund, "step": acts[1]},
		{"outcome": "not_delivered", "action_class": classMoneyRefund, "step": acts[1]},
	} {
		s := seals[i].(map[string]any)
		for k, v := range want {
			assert.Equal(t, v, s[k], "seal %d %s", i, k)
		}
		assert.Equal(t, "unchecked", s["action_class_match"], "the act's payload is withheld: never passed silently")
		assert.NotEmpty(t, s["step_at"])
		assert.NotEmpty(t, s["sealed_at"])
		assert.NotContains(t, s, "witnessed_at", "no witnessed time is in the copy (a checkpoint and a receipt carry none)")
	}
	counts := entry["outcome_counts"].(map[string]any)
	assert.Len(t, counts, len(outcomeSealValues)+1, "every value, zeros too, and not_proven")
	assert.Equal(t, 1.0, counts["not_delivered"], "a deal counts once, by its latest seal")
	assert.Equal(t, 0.0, counts["refunded"])
}

// historySealEdit rewrites the History at path with edit applied to the
// disclosed payload of the deal's last seal, and returns verify's findings.
func historySealEdit(t *testing.T, id, path string, edit func(record map[string]any)) (string, []string) {
	t.Helper()
	b := readHistory(t, path)
	tb := historyExt(b)["deals"].(map[string]any)[id].(map[string]any)
	var last map[string]any
	var lastSeq float64
	for _, raw := range tb["disclosures"].(map[string]any) {
		d := raw.(map[string]any)
		if len(d) == 0 {
			continue
		}
		record := d["agent_input"].(map[string]any)
		if seq := record[dealProfile].(map[string]any)["seq"].(float64); seq > lastSeq {
			last, lastSeq = record, seq
		}
	}
	require.NotNil(t, last)
	edit(last)
	verdict, _, findings, err := verifyHistory(t, rewrite(t, b))
	require.Error(t, err)
	return verdict, findings
}

// An outcome seal with anything beyond its three fields, in its body or on
// its block, or an outcome outside the set, fails verify, and names the deal.
// (The edit also breaks the disclosure's match to the record: deal_invalid.)
func TestDealHistoryFailsAnOutcomeSealWithMore(t *testing.T) {
	id, path := sealedHistory(t)
	for name, edit := range map[string]func(map[string]any){
		"a fourth body field": func(r map[string]any) { r["body"].(map[string]any)["item"] = "Southwest WN 1234" },
		"a counterparty on its block": func(r map[string]any) {
			r[dealProfile].(map[string]any)["counterparty"] = map[string]any{"fp_alg": "hmac-sha256-deal-key", "ids": map[string]any{}}
		},
		"an outcome outside the set": func(r map[string]any) { r["body"].(map[string]any)["outcome"] = "completed" },
		"a step the copy does not carry": func(r map[string]any) {
			r["body"].(map[string]any)["step"] = "00000000000000000000000000000000000000000000000000000000000000aa"
		},
		"a field beside the record": func(r map[string]any) { r["note"] = "fine" },
		"a body field missing":      func(r map[string]any) { delete(r["body"].(map[string]any), "action_class") },
	} {
		t.Run(name, func(t *testing.T) {
			verdict, findings := historySealEdit(t, id, path, edit)
			assert.Equal(t, "INVALID", verdict)
			assert.Contains(t, findings, "outcome_seal_malformed:"+id)
			assert.Contains(t, findings, "deal_invalid:"+id)
		})
	}
}

// The extra-field rule on its own, without the disclosure check: a seal the
// producer had sealed with a fourth field (the schema refuses to seal one, so
// it is built here) is malformed, and its three-field twin is read.
func TestHistoryOutcomeSealsReadsExactlyThreeFields(t *testing.T) {
	const step = "1111111111111111111111111111111111111111111111111111111111111111"
	const id = "deal-0123456789abcdef"
	copyWith := func(body map[string]any) map[string]any {
		return map[string]any{
			"records": []any{
				map[string]any{"capsule_id": step, "timestamp": "2026-10-13T10:00:00Z"},
				map[string]any{"capsule_id": "2222222222222222222222222222222222222222222222222222222222222222", "timestamp": "2026-10-13T11:00:00Z"},
			},
			"disclosures": map[string]any{
				"2222222222222222222222222222222222222222222222222222222222222222": map[string]any{"agent_input": map[string]any{
					dealProfile: map[string]any{"profile": dealProfile, "canonicalization": "jcs", "deal_id": id, "seq": json.Number("3"), "at": "2026-10-13T11:00:00Z", "record_type": "outcome_seal"},
					"body":      body,
				}},
			},
		}
	}
	three := map[string]any{"step": step, "outcome": "delivered", "action_class": classMoneyPurchase}
	member := map[string]any{}
	assert.Empty(t, historyOutcomeSeals(id, copyWith(three), member))
	assert.Equal(t, "delivered", member["outcome"])
	assert.Equal(t, "2026-10-13T10:00:00Z", member["outcome_seals"].([]map[string]any)[0]["step_at"])
	assert.Equal(t, "2026-10-13T11:00:00Z", member["outcome_seals"].([]map[string]any)[0]["sealed_at"])

	four := map[string]any{"step": step, "outcome": "delivered", "action_class": classMoneyPurchase, "item": "cat sticker"}
	member = map[string]any{}
	assert.Equal(t, []string{"outcome_seal_malformed:" + id}, historyOutcomeSeals(id, copyWith(four), member))
	assert.Equal(t, "not_proven", member["outcome"], "a malformed seal is not read")
	assert.NotContains(t, member, "outcome_seals")
}

// The History sent to buyer B carries the closed deal's seal in clear, and
// still nothing of buyer A (TestDealHistoryForAnotherBuyerCarriesNothingOfTheFirst
// scans that file whole); here: the seal's own bytes name nothing of anyone.
func TestDealHistorySealNamesNoOne(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	b := readHistory(t, h.closed)
	tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
	disclosed := 0
	for _, raw := range tb["disclosures"].(map[string]any) {
		dd := raw.(map[string]any)
		if len(dd) == 0 {
			continue
		}
		disclosed++
		record := dd["agent_input"].(map[string]any)
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		raw := string(encoded)
		for _, v := range []string{"cat sticker", "sticker", homeAddress, shopEmail, shopPhone, "Larkspur", "payee", "counterparty", "channel"} {
			assert.NotContains(t, raw, v)
		}
	}
	assert.Equal(t, 1, disclosed, "the closed deal's one seal")
}

// In a typed deal an approved disclosure is an action-record/v0 too, but it
// is not an act: the seal names the pay before it, with the pay's class, and
// the profile checker agrees.
func TestDealTypedSealSkipsAnApprovedDisclosure(t *testing.T) {
	dealFixture(t)
	open := `{"type":"purchase","channel":"marketplace",
		"intent":{"verbatim":"buy the bike for $120, you can give them my number","allowed":["pay","share_contact"]},
		"who":{"name":"` + sellerName + `","phone":"` + sellerPhone + `"},
		"terms":{"item":"bike","price_minor":12000,"currency":"USD"},
		"recourse":{"rail":"cash","refundable":false}}`
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, open))["deal_id"].(string)
	answer := func(check map[string]any, said string) {
		if check["verdict"] != "pass" {
			_, err := answerCheck(t, id, check["check_id"].(string), said, check["card"].(string))
			require.NoError(t, err)
		}
	}
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":12000,"terms":{"item":"bike","price_minor":12000},"recourse":{"rail":"cash","refundable":false}}`)), "yes, pay")
	pay := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":12000,"currency":"USD","rail":"cash"}`))
	require.Equal(t, false, pay["unchecked"])
	answer(dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"share_contact","disclosing_to":"counterparty","description":"give the seller my number","disclosing":["phone"]}`)), "yes, give them my number")
	told := dealRun(t, "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t, `{"channel":"marketplace","fields":[{"class":"phone","value":"`+userPhone+`"}]}`))
	require.Equal(t, true, told["approved"], "an approved disclosure: typed as an action-record/v0")
	closed := closeDealWith(t, id, `{"status":"received","delivered":{"item":"bike"}}`)
	seal := closed["outcome_seal"].(map[string]any)
	assert.Equal(t, pay["capsule_id"], seal["step"])
	body := sealsOf(t, id)[0]["body"].(map[string]any)
	assert.Equal(t, classMoneyPurchase, body["action_class"], "the pay's class, not the disclosure's")
	_, path := exportRecords(t, id)
	checkProfile(t, path)
}
