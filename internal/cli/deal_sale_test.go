package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One item sold to one of several buyers: a sale holds the one task
// authority, and each buyer's negotiation is a thread opened under it.

const saleInput = `{"type": "purchase", "demo": true,
 "intent": {"verbatim": "Sell my example bicycle; ask 1900, not under 1700", "min_total_minor": 170000,
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit"]},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`

const offerInput = `{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`

func newSale(t *testing.T) (string, string) {
	t.Helper()
	out := dealRun(t, "sale", "new", "--input", writeJSON(t, saleInput))
	return out["sale_id"].(string), out["task_authority_digest"].(string)
}

func buyerThread(t *testing.T, saleID, domain string) string {
	t.Helper()
	return dealRun(t, "open", "--sale", saleID, "--input", writeJSON(t,
		`{"channel": "marketplace", "who": {"name": "Example Buyer", "domain": "`+domain+`"}, "terms": {"price_minor": 190000}}`))["deal_id"].(string)
}

// saleItemRef is the sale's item reference, from this device's store.
func saleItemRef(t *testing.T, saleID string) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	sale, _, err := s.saleOf(t.Context(), saleID)
	require.NoError(t, err)
	return sale.ItemRef
}

func exportSale(t *testing.T, saleID string) ([]map[string]any, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sale.json")
	dealRun(t, "sale", "export", "--sale", saleID, "--output", path)
	var recs []map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &recs))
	return recs, path
}

// A sale's own log is its root and its one task authority. The root names
// no buyer, and carries the item reference only as a commitment.
func TestASaleHoldsItsOneTaskAuthority(t *testing.T) {
	dealFixture(t)
	saleID, authority := newSale(t)
	item := saleItemRef(t, saleID)
	require.Regexp(t, `^[0-9a-f]{64}$`, item)

	recs, path := exportSale(t, saleID)
	require.Len(t, recs, 2)
	root := recs[0]["x-deal-v0"].(map[string]any)
	assert.Equal(t, "sale", root["record_type"])
	assert.Nil(t, root["counterparty"])
	assert.Equal(t, "task-authority/v0", recs[1]["type"])
	assert.Equal(t, authority, recordDigest(t, recs[1]))
	raw := string(mustRead(t, path))
	assert.NotContains(t, raw, item)
	assert.NotContains(t, raw, "170000", "the floor is a commitment here too")
	assert.Contains(t, raw, `"item_ref_commitment"`)
	checkProfile(t, path)
}

// Each buyer's thread is under the sale's one task authority, and each of
// its checks seals the item reference: both salted per step, so equal
// across threads only to whoever holds the openings. The profile's own
// rules checker is given the plain reference, the same on every thread.
func TestEveryThreadOfASaleCitesItsOneAuthority(t *testing.T) {
	dealFixture(t)
	saleID, authority := newSale(t)
	item := saleItemRef(t, saleID)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")

	var commitments, authorities []string
	for _, id := range []string{a, b} {
		sealed, _, input := ruleInputs(t, id, offerInput)
		assert.Equal(t, item, input["item_ref"], "the local checker is given the plain reference")
		commitments = append(commitments, sealed["item_ref_commitment"].(string))
		recs := records(t, id)
		assert.Equal(t, "task-authority/v0", recs[1]["type"])
		sealedAuthority := recs[1]["body"].(map[string]any)["sale_authority_commitment"].(string)
		authorities = append(authorities, sealedAuthority)
		// It opens, with the step's own nonce, to the sale's authority digest.
		opened, err := commitText(dealNonceOf(t, id, chainSteps(t, id)[1].CapsuleID, "sale_authority"), authority)
		require.NoError(t, err)
		assert.Equal(t, sealedAuthority, opened)
		raw := mustJSONString(t, recs)
		assert.NotContains(t, raw, item, "no record carries the plain reference")
		assert.NotContains(t, raw, authority, "nor the sale's authority digest")
	}
	assert.NotEqual(t, commitments[0], commitments[1], "salted per check: nothing equal across threads")
	assert.NotEqual(t, authorities[0], authorities[1], "salted per thread")

	// The commitment opens with the check's own nonce.
	var snapshot string
	for _, se := range chainSteps(t, a) {
		if se.Event.Kind == "snapshot" {
			snapshot = se.CapsuleID
		}
	}
	opened, err := commitText(dealNonceOf(t, a, snapshot, "item_ref"), item)
	require.NoError(t, err)
	assert.Equal(t, commitments[0], opened)
}

// recordsByType groups records by type (an x-deal-v0 record by its
// record_type).
func recordsByType(recs []map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, r := range recs {
		name, _ := r["type"].(string)
		if blk, ok := r["x-deal-v0"].(map[string]any); ok {
			name = blk["record_type"].(string)
		}
		out[name] = append(out[name], r)
	}
	return out
}

// hexValues are the 64-hex strings in a text: digests and commitments.
func hexValues(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`[0-9a-f]{64}`).FindAllString(text, -1) {
		out[m] = true
	}
	return out
}

// The buyer sees the offer they accepted, and the task authority it was
// made under, in full; and nothing in two buyers' copies of one sale's
// threads is equal that is not equal between any two deals of the profile.
func TestABuyersCopyShowsTheOfferAndDoesNotLinkTheSalesThreads(t *testing.T) {
	dealFixture(t)
	saleID, authority := newSale(t)
	threads := []string{buyerThread(t, saleID, "buyer-a.example"), buyerThread(t, saleID, "buyer-b.example")}
	copies := []string{}
	for _, id := range threads {
		_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
		require.NoError(t, err)
		b, shared := sharedCopy(t, id, dealAudienceCounterparty, "the buyer")
		copies = append(copies, shared)
		recs := recordsByType(disclosedRecords(b))
		var offer map[string]any
		for _, r := range recs["proposed-action/v0"] {
			if body := r["body"].(map[string]any); body["action"] == "offer" {
				offer = body
			}
		}
		require.NotNil(t, offer, "the buyer's copy carries the offer they accepted")
		assert.Equal(t, float64(190000), offer["amount_minor"])
		assert.Equal(t, "USD", offer["currency"])
		assert.Equal(t, "example bicycle", offer["terms"].(map[string]any)["item"])
		assert.NotEmpty(t, offer["item_ref_commitment"])
		require.Len(t, recs["task-authority/v0"], 1, "and the task authority, in full")
		assert.NotEmpty(t, recs["task-authority/v0"][0]["body"].(map[string]any)["sale_authority_commitment"])
		assert.Len(t, recs["counterparty-acceptance-observation"], 0)
		assert.NotEmpty(t, recs["action-approval/v0"], "and the acceptance")
		assert.NotContains(t, shared, saleItemRef(t, saleID))
		assert.NotContains(t, shared, authority)
		assert.NotContains(t, shared, "sale_authority_opening")
	}
	// An unrelated deal of the same profile: what it shares with a thread
	// (the profile's materiality predicate, the ruleset) links nothing.
	other := openTyped(t, sellerTyped)
	_, err := acceptOffer(t, other, makeOffer(t, other, "190000"))
	require.NoError(t, err)
	_, unrelated := sharedCopy(t, other, dealAudienceCounterparty, "the buyer")
	profileWide := hexValues(unrelated)
	a, b := hexValues(copies[0]), hexValues(copies[1])
	for v := range a {
		if b[v] && !profileWide[v] {
			t.Errorf("%s is in both buyers' copies of one sale's threads, and in no unrelated deal's", v)
		}
	}

	// The adjudicator holds the opening that ties the thread to the sale.
	adjudicator, _ := sharedCopy(t, threads[0], dealAudienceAdjudicator, "an adjudicator")
	opening := dealReportOf(adjudicator)["sale_authority_opening"].(map[string]any)
	assert.Equal(t, authority, opening["text"])
}

func TestAThreadIsUnderTheSale(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	refused := func(args ...string) {
		t.Helper()
		_, err := invoke(t, "", append([]string{"--profile", "deal", "deal"}, args...)...)
		require.ErrorIs(t, err, ErrInput, args)
	}
	thread := func(body string) []string {
		return []string{"open", "--sale", saleID, "--input", writeJSON(t, body)}
	}
	refused(thread(`{"who": {"name": "B"}, "intent": {"verbatim": "Sell it for 1500", "party_role": "seller"}}`)...)
	refused(thread(`{"who": {"name": "B"}, "terms": {"item": "another bicycle"}}`)...)
	refused(thread(`{"who": {"name": "B"}, "terms": {"currency": "EUR"}}`)...)
	refused("open", "--sale", "sale-0000000000000000", "--input", writeJSON(t, `{"who": {"name": "B"}}`))

	// Its request and limits do not change per buyer.
	a := buyerThread(t, saleID, "buyer-a.example")
	refused("note", "--deal", a, "--kind", "intent", "--input", writeJSON(t, `{"verbatim": "1600 is ok", "min_total_minor": 160000}`))
	// A sale's log is not a deal.
	refused("check", "--deal", saleID, "--input", writeJSON(t, offerInput))
	refused("sale", "new", "--input", writeJSON(t, `{"type": "purchase", "who": {"name": "B"}, "intent": {"verbatim": "sell"}, "terms": {"item": "x", "currency": "USD"}}`))
	refused("sale", "new", "--input", writeJSON(t, `{"type": "purchase", "intent": {"verbatim": "buy", "party_role": "buyer"}, "terms": {"item": "x", "currency": "USD"}}`))

	// The deal-wide listings read past the sale's own log.
	dealRun(t, "deadlines")
}

// A check on a thread is refused when the sale's task authority is no
// longer the one the thread was opened under.
func TestAThreadRefusesASaleThatChanged(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE deal_steps SET record_digest=? WHERE deal_id=? AND n=2`, "00"+saleID[5:]+saleID[5:]+saleID[5:]+saleID[5:][:14], saleID)
	require.NoError(t, err)
	require.NoError(t, s.close())
	_, err = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", a, "--input", writeJSON(t, offerInput))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "not the one this thread was opened under")
}

// The profile checker holds a sale's log to its root and one task authority.
func TestTheProfileCheckerHoldsASalesLog(t *testing.T) {
	python := profilePython(t)
	if python == "" {
		return
	}
	dealFixture(t)
	saleID, _ := newSale(t)
	recs, _ := exportSale(t, saleID)
	for name, c := range map[string]struct {
		chain []map[string]any
		want  string
	}{
		"the task authority dropped": {recs[:1], "its root and its one task authority"},
		"a second task authority":    {append(append([]map[string]any{}, recs...), recs[1]), "its root and its one task authority"},
		"the authority names a sale": {relinked(t, recs, nil, map[int]func(map[string]any){1: func(r map[string]any) {
			r["body"].(map[string]any)["sale_authority_commitment"] = recordDigest(t, recs[0])
		}}), "names no other sale"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sale.json")
			raw, err := json.Marshal(c.chain)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			out, err := exec.Command(python, dealProfileDir+"/check_profile.py", path).CombinedOutput()
			require.Error(t, err, string(out))
			assert.Contains(t, string(out), c.want)
		})
	}
}
