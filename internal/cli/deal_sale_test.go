package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// Each buyer's thread is under the sale's one task authority (by digest),
// and each of its checks seals the item reference salted per check: equal
// across threads only to whoever holds the openings. The profile's own
// rules checker is given the plain reference, the same on every thread.
func TestEveryThreadOfASaleCitesItsOneAuthority(t *testing.T) {
	dealFixture(t)
	saleID, authority := newSale(t)
	item := saleItemRef(t, saleID)
	a := buyerThread(t, saleID, "buyer-a.example")
	b := buyerThread(t, saleID, "buyer-b.example")

	var commitments []string
	for _, id := range []string{a, b} {
		sealed, _, input := ruleInputs(t, id, offerInput)
		assert.Equal(t, item, input["item_ref"], "the local checker is given the plain reference")
		commitments = append(commitments, sealed["item_ref_commitment"].(string))
		recs := records(t, id)
		assert.Equal(t, "task-authority/v0", recs[1]["type"])
		ref := recs[1]["body"].(map[string]any)["sale_authority_ref"].(map[string]any)
		assert.Equal(t, authority, ref["digest"])
		assert.NotContains(t, mustJSONString(t, recs), item, "no record carries the plain reference")
	}
	assert.NotEqual(t, commitments[0], commitments[1], "salted per check: nothing equal across threads")

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

// A buyer's copy carries neither the reference nor the sale's authority
// digest, the one value equal on every thread.
func TestABuyersCopyDoesNotLinkTheSalesThreads(t *testing.T) {
	dealFixture(t)
	saleID, authority := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	_, err := acceptOffer(t, a, makeOffer(t, a, "190000"))
	require.NoError(t, err)
	_, shared := sharedCopy(t, a, dealAudienceCounterparty, "the buyer")
	assert.NotContains(t, shared, saleItemRef(t, saleID))
	assert.NotContains(t, shared, authority)
	assert.NotContains(t, shared, "sale_authority_ref")
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
			r["body"].(map[string]any)["sale_authority_ref"] = map[string]any{"type": "record", "digest_alg": "SHA-256", "digest": recordDigest(t, recs[0])}
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

// The sale's authority digest is the one value equal on every thread: a
// record that carries it is never in a buyer's copy, whatever else it holds.
func TestTheSalesAuthorityDigestIsNeverTheBuyers(t *testing.T) {
	record := map[string]any{"body": map[string]any{"sale_authority_ref": map[string]any{
		"type": "record", "digest_alg": "SHA-256", "digest": "0c5151aa765aa415630a06b4bd5d09a2fc56b24d44a5883684e3f63e20b7b5c9"}}}
	assert.False(t, dealRecordShareable(record, "", dealAudienceCounterparty, dealPrivate{}))
	assert.True(t, dealRecordShareable(record, "", dealAudienceAdjudicator, dealPrivate{}))
}
