package cli

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withBoundsNonce is events with every bounds nonce replaced by nonce, or
// removed when nonce is empty: the opening as a damaged store would hold it.
func withBoundsNonce(events []sealedEvent, nonce string) []sealedEvent {
	out := make([]sealedEvent, len(events))
	for i, se := range events {
		out[i] = se
		if _, ok := se.Event.Nonces["bounds"]; !ok {
			continue
		}
		nonces := map[string]string{}
		for k, v := range se.Event.Nonces {
			nonces[k] = v
		}
		if nonce == "" {
			delete(nonces, "bounds")
		} else {
			nonces["bounds"] = nonce
		}
		out[i].Event.Nonces = nonces
	}
	return out
}

// A floor in force goes to the rules checker opened, or not at all: each way
// its opening cannot be built refuses the check, naming the cause and never
// the floor. With no floor in force nothing is needed.
func TestAFloorWhoseOpeningCannotBeBuiltRefusesTheCheck(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	events := chainSteps(t, id)
	opening, err := boundsOpeningInForce(events)
	require.NoError(t, err)
	require.NotNil(t, opening, "a floor in force opens")

	// Limits confirmed with a commitment no opening on this device
	// recomputes to.
	floor := int64(170000)
	confirmed := append(append([]sealedEvent{}, events...), sealedEvent{Event: dealEvent{Kind: "approval", Approval: &dealApproval{
		Choice: "confirm_limits", Approver: "user", Proceed: true,
		Limits: &dealLimits{New: dealLimitSet{MinTotalMinor: &floor, BoundsCommitment: strings.Repeat("ab", 32)}},
	}}})
	for name, damaged := range map[string][]sealedEvent{
		"no opening":                         withBoundsNonce(events, ""),
		"an opening that does not recompute": confirmed,
		"limits that cannot be read":         events[1:],
	} {
		t.Run(name, func(t *testing.T) {
			opening, err := boundsOpeningInForce(damaged)
			require.Error(t, err)
			assert.Nil(t, opening)
			assert.Contains(t, err.Error(), "refusing to check")
			for _, floor := range []string{"170000", "1700.00", "1700"} {
				assert.NotContains(t, err.Error(), floor, "the error never names the floor")
			}
		})
	}

	other := stickerDeal(t, "card", false)
	none, err := boundsOpeningInForce(chainSteps(t, other))
	require.NoError(t, err, "no floor in force: nothing to open")
	assert.Nil(t, none)
}

// The rules checker's input is never built without the opening: rulesInput
// refuses, so the checker is not run on the check.
func TestTheCheckerInputIsNotBuiltWithoutTheFloorsOpening(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	_, _, input := ruleInputs(t, id, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`)
	require.Contains(t, input, "commercial_bounds_opening", "a normal floor is sent opened")

	events := chainSteps(t, id)
	var check string
	for _, se := range events {
		if se.Event.Kind == "snapshot" {
			check = se.CapsuleID
		}
	}
	require.NotEmpty(t, check)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), id, false))
	raw, _, err := s.rulesInput(t.Context(), check, withBoundsNonce(events, ""))
	require.Error(t, err)
	assert.Nil(t, raw, "no input to send")
	_, _, err = s.rulesInput(t.Context(), check, events)
	require.NoError(t, err)
}

// opensTo mirrors, with this CLI's own construction, whether an opening
// opens to a sealed bounds_commitment: a commercial-bounds/v0 document
// stating one safe integer floor and nothing else, and SHA-256 over
// JCS({nonce, text}) of it equal to both the opening's stated commitment and
// the sealed one.
func opensTo(opening map[string]any, sealed string) (int64, bool) {
	doc, _ := opening["document"].(map[string]any)
	nonce, _ := opening["nonce"].(string)
	min, ok := doc["min_total_minor"].(float64)
	if !ok || len(doc) != 2 || doc["type"] != commercialBoundsKind || nonce == "" || min < 0 || min > 9007199254740991 || min != float64(int64(min)) {
		return 0, false
	}
	c, err := commitText(nonce, commercialBoundsText(int64(min)))
	return int64(min), err == nil && c == opening["bounds_commitment"] && c == sealed
}

var lowerHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// capsule-engine's price_floor/2.0.0, over its vectors, and this CLI's own
// construction agree on every opening: the engine evaluates the floor
// exactly when the opening opens to the sealed commitment here, and fails
// it as a mismatch exactly when it does not. And the checker input this CLI
// sends for a floor in force opens.
func TestTheEngineAgreesOnTheFloorsOpening(t *testing.T) {
	var vectors struct {
		PriceFloor []struct {
			Name   string `json:"name"`
			Action struct {
				ActionClass string `json:"action_class"`
				AmountMinor int64  `json:"amount_minor"`
			} `json:"action"`
			Record  map[string]any `json:"task_authority_record"`
			Opening map[string]any `json:"commercial_bounds_opening"`
			Expect  struct {
				Result string `json:"result"`
				Reason string `json:"reason"`
			} `json:"expect"`
		} `json:"price_floor"`
		ItemRef []struct {
			Name    string `json:"name"`
			ItemRef any    `json:"item_ref"`
			Expect  struct {
				ActionItemRef any `json:"action_item_ref"`
			} `json:"expect"`
		} `json:"item_ref"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join("testdata", "commercial-bounds", "engine-price-floor-vectors.json")), &vectors))
	evaluated, mismatched := 0, 0
	for _, c := range vectors.PriceFloor {
		sealed, _ := c.Record["body"].(map[string]any)["bounds_commitment"].(string)
		if c.Opening == nil || sealed == "" || c.Expect.Result == "n/a" {
			assert.Equal(t, "n/a", c.Expect.Result, "%s: nothing to open, or not in scope", c.Name)
			continue
		}
		if !lowerHex64.MatchString(sealed) {
			assert.Equal(t, "fail", c.Expect.Result, "%s: a malformed sealed commitment fails", c.Name)
			continue
		}
		min, opens := opensTo(c.Opening, sealed)
		mismatch := strings.Contains(c.Expect.Reason, "does not open")
		assert.Equal(t, !opens, mismatch, "%s: the engine and this CLI agree whether the opening opens (%s)", c.Name, c.Expect.Reason)
		if opens {
			evaluated++
			want := "fail"
			if c.Action.AmountMinor >= min {
				want = "pass"
			}
			assert.Equal(t, want, c.Expect.Result, c.Name)
		} else {
			mismatched++
		}
	}
	assert.Positive(t, evaluated)
	assert.Positive(t, mismatched)

	// This CLI's own checker input for a floor in force opens to the
	// commitment its task authority seals.
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	_, _, input := ruleInputs(t, id, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`)
	record, ok := input["task_authority_record"].(map[string]any)
	require.True(t, ok, "a typed seller deal sends its task authority")
	sealed := record["body"].(map[string]any)["bounds_commitment"].(string)
	min, opens := opensTo(input["commercial_bounds_opening"].(map[string]any), sealed)
	assert.True(t, opens, "the opening this CLI sends opens")
	assert.Equal(t, int64(170000), min)

	// item_ref: the engine keys only a 64-lowercase-hex reference, the only
	// shape this CLI sends.
	for _, c := range vectors.ItemRef {
		s, _ := c.ItemRef.(string)
		assert.Equal(t, c.Expect.ActionItemRef != nil, lowerHex64.MatchString(s), c.Name)
	}
	saleID, _ := newSale(t)
	_, _, thread := ruleInputs(t, buyerThread(t, saleID, "buyer-a.example"), offerInput)
	assert.Regexp(t, lowerHex64, thread["item_ref"])
}
