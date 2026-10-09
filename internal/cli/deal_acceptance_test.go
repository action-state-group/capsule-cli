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

// A seller's agent offers, the buyer accepts that exact offer, and only then
// may the agent commit. A later offer supersedes the one before it, and a
// change of details after the acceptance leaves it behind.

const sellerTyped = `{"type": "purchase", "demo": true, "channel": "web",
 "intent": {"verbatim": "Sell my example bicycle; ask 1900", "party_role": "seller",
            "asked": {"item": "example bicycle", "quantity": 1}, "allowed": ["offer", "commit", "share_contact"]},
 "who": {"name": "Example Buyer", "domain": "buyer.example"},
 "terms": {"item": "example bicycle", "quantity": 1, "price_minor": 190000, "currency": "USD"},
 "recourse": {"rail": "card", "refundable": false}}`

func offerAt(t *testing.T, dealID, price string) string {
	t.Helper()
	c := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":`+price+`,"terms":{"item":"example bicycle","quantity":1,"price_minor":`+price+`}}`))
	return approved(t, dealID, c)
}

// approved is the check's id, answered with the user's own words on the card
// it showed when it paused.
func approved(t *testing.T, dealID string, c map[string]any) string {
	t.Helper()
	check := c["check_id"].(string)
	if c["verdict"] != "pass" {
		_, err := answerCheck(t, dealID, check, "yes, go ahead", c["card"].(string))
		require.NoError(t, err)
	}
	return check
}

func makeOffer(t *testing.T, dealID, price string) string {
	t.Helper()
	check := offerAt(t, dealID, price)
	a := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":`+price+`}`))
	require.Equal(t, false, a["unchecked"])
	return check
}

func acceptOffer(t *testing.T, dealID, check string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "acceptance",
		"--check", check, "--words", "Deal, that works for me", "--channel", "app_chat")
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

// commitNow checks and records a commit, returning the act's result.
func commitNow(t *testing.T, dealID, price string) map[string]any {
	t.Helper()
	approved(t, dealID, dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t,
		`{"action":"commit","amount_minor":`+price+`,"terms":{"item":"example bicycle","quantity":1,"price_minor":`+price+`}}`)))
	return dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":`+price+`}`))
}

func TestASellerCommitsToTheOfferTheBuyerAccepted(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	check := makeOffer(t, id, "190000")
	acc, err := acceptOffer(t, id, check)
	require.NoError(t, err)
	commit := commitNow(t, id, "190000")
	assert.Equal(t, false, commit["unchecked"])
	assert.NotEmpty(t, commit["authorized_by"])

	// The acceptance is an action-approval/v0 observation bound to the
	// offer's exact proposed action, the buyer's words by commitment only.
	_, chain := ownReportAndChain(t, id)
	assert.Contains(t, chain, `"authority":"counterparty_acceptance"`)
	assert.Contains(t, chain, `"kind":"counterparty-acceptance-observation"`)
	assert.NotContains(t, chain, "that works for me")
	var accepted map[string]any
	for _, r := range records(t, id) {
		if b, ok := r["body"].(map[string]any); ok && b["authority"] == "counterparty_acceptance" {
			accepted = b
		}
	}
	require.NotNil(t, accepted)
	assert.Equal(t, stepDigest(t, id, acc["accepts"].(string)), digestOfRef(accepted["proposed_action_ref"]))
	assert.Equal(t, "app_chat", accepted["channel"])
	checkerPasses(t, id)
}

func TestASellerCannotCommitWithoutAnAcceptedOffer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	commit := commitNow(t, id, "190000")
	assert.Equal(t, true, commit["unchecked"])
	assert.Equal(t, "no_accepted_offer", commit["rule"])

	id = openTyped(t, sellerTyped)
	makeOffer(t, id, "190000")
	commit = commitNow(t, id, "190000")
	assert.Equal(t, true, commit["unchecked"])
	assert.Equal(t, "offer_not_accepted", commit["rule"])
	checkerPasses(t, id)
}

func TestACounterofferSupersedesTheOfferBeforeIt(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	first := makeOffer(t, id, "190000")
	_, err := acceptOffer(t, id, first)
	require.NoError(t, err)
	second := makeOffer(t, id, "185000")

	// The second offer's proposed action names the first's as superseded.
	var refs []any
	for _, r := range records(t, id) {
		if b, ok := r["body"].(map[string]any); ok && r["type"] == "proposed-action/v0" && b["action"] == "offer" {
			refs, _ = r["refs"].([]any)
		}
	}
	require.Len(t, refs, 1)
	assert.Equal(t, "supersedes", refs[0].(map[string]any)["rel"])

	// The first offer's acceptance does not carry over, and the first offer
	// can no longer be accepted.
	assert.Equal(t, "offer_not_accepted", commitNow(t, id, "185000")["rule"])
	_, err = acceptOffer(t, id, first)
	require.ErrorIs(t, err, ErrInput)
	_, err = acceptOffer(t, id, second)
	require.NoError(t, err)
	assert.Equal(t, false, commitNow(t, id, "185000")["unchecked"])
	checkerPasses(t, id)
}

func TestAnOfferIsAcceptedOnlyOnceItWasMade(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := acceptOffer(t, id, offerAt(t, id, "190000"))
	require.ErrorIs(t, err, ErrInput, "checked, never made")
}

func TestAChangeAfterTheAcceptanceLeavesItBehind(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	_, err := acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t,
		`{"source":"buyer message","who":{"name":"Example Buyer","domain":"buyer-two.example"}}`))
	assert.Equal(t, "changed_after_acceptance", commitNow(t, id, "190000")["rule"])

	_, err = acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	assert.Equal(t, false, commitNow(t, id, "190000")["unchecked"])
	checkerPasses(t, id)
}

func TestOnlyASellerMakesAnOffer(t *testing.T) {
	dealFixture(t)
	id := openTypedSticker(t)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":600,"terms":{"item":"otter sticker","price_minor":600}}`))
	require.ErrorIs(t, err, ErrInput)
}

// A seller gives out where the item is only for an accepted offer, as a
// commit is.
func TestASellerSharesAnAddressOnlyForAnAcceptedOffer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	share := func() (map[string]any, error) {
		approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
			`{"action":"share_contact","disclosing_to":"counterparty","description":"where to pick it up","disclosing":["pickup_location"]}`)))
		out, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "disclosure", "--input", writeJSON(t,
			`{"channel":"app_chat","fields":[{"class":"pickup_location","value":"Example Street 1"}]}`))
		if err != nil {
			return nil, err
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &m), out)
		return m, nil
	}
	// Its approval covers no address before an acceptance, so the first
	// telling of the pickup location is held and nothing is sealed.
	_, err := share()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs the user's approval")
	_, err = acceptOffer(t, id, makeOffer(t, id, "190000"))
	require.NoError(t, err)
	after, err := share()
	require.NoError(t, err)
	assert.Equal(t, true, after["approved"])
	checkerPasses(t, id)
}

func records(t *testing.T, dealID string) []map[string]any {
	t.Helper()
	recs, _ := exportRecords(t, dealID)
	return recs
}

// The profile checker refuses a seller's chain that commits without the
// acceptance, accepts a superseded offer, or commits after a change. Each
// chain is a valid one with records dropped or edited and re-linked.
func TestTheProfileCheckerHoldsTheAcceptanceRules(t *testing.T) {
	python := profilePython(t)
	if python == "" {
		return
	}
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	first := makeOffer(t, id, "190000")
	_, err := acceptOffer(t, id, first)
	require.NoError(t, err)
	dealRun(t, "note", "--deal", id, "--kind", "change", "--input", writeJSON(t,
		`{"source":"buyer message","who":{"name":"Example Buyer","domain":"buyer-two.example"}}`))
	second := makeOffer(t, id, "185000")
	_, err = acceptOffer(t, id, second)
	require.NoError(t, err)
	commitNow(t, id, "185000")
	recs := records(t, id)
	checkerPasses(t, id)

	at := map[string]int{}
	for i, r := range recs {
		b, _ := r["body"].(map[string]any)
		switch {
		case r["type"] == "proposed-action/v0" && b["action"] == "offer":
			at["offer"+itoa(len(keysWith(at, "offer")))] = i
		case r["type"] == "action-approval/v0" && b["authority"] == "counterparty_acceptance":
			at["accept"+itoa(len(keysWith(at, "accept")))] = i
		case r["type"] == "action-record/v0" && b["action"] == "offer":
			at["made"+itoa(len(keysWith(at, "made")))] = i
		}
	}
	// An offer is its proposed action, evaluation (and answer, if it paused)
	// and action; then its acceptance.
	o2, a2 := at["offer1"], at["accept1"]
	cases := []struct {
		name, want string
		chain      []map[string]any
	}{
		{"the latest offer's acceptance dropped", "the latest offer has no recorded acceptance",
			relinked(t, recs, map[int]bool{a2: true}, nil)},
		{"the second offer dropped: the commit rests on an acceptance a change left behind", "details changed after the other party accepted",
			relinked(t, recs, span(o2, a2), nil)},
		{"the offer's action dropped", "never made",
			relinked(t, recs, map[int]bool{at["made1"]: true}, nil)},
		{"the acceptance names the superseded offer", "only the latest offer can be accepted",
			relinked(t, recs, nil, map[int]func(map[string]any){a2: func(r map[string]any) {
				r["body"].(map[string]any)["proposed_action_ref"].(map[string]any)["digest"] = recordDigest(t, recs[at["offer0"]])
			}})},
		{"the counteroffer names no superseded offer", "carries exactly one 'supersedes'",
			relinked(t, recs, nil, map[int]func(map[string]any){o2: func(r map[string]any) { delete(r, "refs") }})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chain.json")
			raw, err := json.Marshal(c.chain)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			out, err := exec.Command(python, dealProfileDir+"/check_profile.py", path).CombinedOutput()
			require.Error(t, err, string(out))
			assert.Contains(t, string(out), c.want)
		})
	}
}

// span is the indexes from..to, inclusive.
func span(from, to int) map[int]bool {
	out := map[int]bool{}
	for i := from; i <= to; i++ {
		out[i] = true
	}
	return out
}

func keysWith(m map[string]int, prefix string) []string {
	var out []string
	for k := range m {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	return out
}

func itoa(n int) string { return string(rune('0' + n)) }

// relinked is recs with the records at drop removed and edit applied, every
// later record re-linked: seq renumbered, and every digest of an earlier
// record (prev, refs, any record ref) replaced by its new digest.
func relinked(t *testing.T, recs []map[string]any, drop map[int]bool, edit map[int]func(map[string]any)) []map[string]any {
	t.Helper()
	moved := map[string]string{}
	var out []map[string]any
	var remap func(v any) any
	remap = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				x[k] = remap(c)
			}
		case []any:
			for i, c := range x {
				x[i] = remap(c)
			}
		case string:
			if n, ok := moved[x]; ok {
				return n
			}
		}
		return v
	}
	for i, r := range recs {
		old := recordDigest(t, r)
		if drop[i] {
			continue
		}
		var c map[string]any
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &c))
		if f := edit[i]; f != nil {
			f(c)
		}
		remap(c)
		block := c
		if _, typed := c["type"]; !typed {
			block = c["x-deal-v0"].(map[string]any)
		}
		block["seq"] = float64(len(out) + 1)
		if len(out) > 0 {
			block["prev"].(map[string]any)["digest"] = recordDigest(t, out[len(out)-1])
		}
		out = append(out, c)
		moved[old] = recordDigest(t, c)
	}
	return out
}

// An offer's amount is what the buyer would pay: it is never sealed as the
// user's spend.
func TestAnOfferIsNotSpend(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	makeOffer(t, id, "190000")
	for _, r := range records(t, id) {
		if b, ok := r["body"].(map[string]any); ok && b["action"] == offerAction {
			assert.Equal(t, float64(0), b["spend_minor"], r["type"])
		}
	}
}
