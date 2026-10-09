package cli

import (
	"encoding/json"
	"errors"
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
	// Limits confirmed with a floor but no commitment to it.
	uncommitted := append(append([]sealedEvent{}, events...), sealedEvent{Event: dealEvent{Kind: "approval", Approval: &dealApproval{
		Choice: "confirm_limits", Approver: "user", Proceed: true,
		Limits: &dealLimits{New: dealLimitSet{MinTotalMinor: &floor}},
	}}})
	for name, damaged := range map[string][]sealedEvent{
		"a floor with no commitment":         uncommitted,
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
	otherEvents := chainSteps(t, other)
	none, err := boundsOpeningInForce(otherEvents)
	require.NoError(t, err, "no floor in force: nothing to open")
	assert.Nil(t, none)
	// No floor: limits that cannot be read change nothing, as before.
	none, err = boundsOpeningInForce(otherEvents[1:])
	require.NoError(t, err, "no step set a floor")
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

// capsule-engine's price_floor/2.0.1, over its vectors, and this CLI's own
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

// An opening is a commercial-bounds document, or the check is refused.
func TestABoundsOpeningIsACommercialBoundsDocument(t *testing.T) {
	nonce := strings.Repeat("ab", 32)
	for name, text := range map[string]string{"not JSON": "1700", "another type": `{"type":"commercial-bounds/v1","min_total_minor":170000}`, "no type": `{"min_total_minor":170000}`} {
		opening, err := boundsOpening(text, nonce, nonce)
		require.Error(t, err, name)
		assert.Nil(t, opening, name)
		assert.Contains(t, err.Error(), "refusing to check", name)
	}
	opening, err := boundsOpening(commercialBoundsText(170000), nonce, nonce)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"type": commercialBoundsKind, "min_total_minor": json.Number("170000")}, normalizedNumbers(opening["document"]))
}

func normalizedNumbers(v any) any {
	raw, _ := json.Marshal(v)
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out any
	_ = dec.Decode(&out)
	return out
}

// deal check with a floor in force whose opening cannot be built exits
// non-zero, runs no checker and seals nothing: the store is byte for byte
// as it was.
func TestADealCheckWithAnUnbuildableFloorOpeningSealsNothing(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	old := dealBoundsOpening
	dealBoundsOpening = func([]sealedEvent) (map[string]interface{}, error) {
		return nil, errors.New("refusing to check: a floor is in force but no opening on this device recomputes to its bounds_commitment, so the rules checker cannot be given it")
	}
	defer func() { dealBoundsOpening = old }()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	steps := len(chainSteps(t, id))
	_, err = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`))
	require.Error(t, err, "the check exits non-zero")
	assert.Contains(t, err.Error(), "refusing to check")
	assert.Equal(t, before, storeFiles(t, p), "nothing sealed: the store is unchanged")
	assert.Len(t, chainSteps(t, id), steps)
	assert.NoFileExists(t, checker+".input", "the checker was not run")
}

// price_floor/2.0.1's scope: a task authority committing to no floor puts
// the action out of scope; a floor with no opening is not applicable, in
// scope, naming the missing opening; an opening is evaluated only when it
// opens to the sealed commitment. This CLI never sends the second case: it
// refuses the check instead.
func TestTheEngineScopesTheFloorAsThisCLIDoes(t *testing.T) {
	var scope struct {
		WicketID string `json:"wicket_id"`
		Cases    []struct {
			Name   string `json:"name"`
			Action struct {
				AmountMinor int64 `json:"amount_minor"`
			} `json:"action"`
			Record  map[string]any `json:"task_authority_record"`
			Opening map[string]any `json:"commercial_bounds_opening"`
			Expect  struct {
				Result   string         `json:"result"`
				Evidence map[string]any `json:"evidence"`
			} `json:"expect"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join("testdata", "commercial-bounds", "engine-price-floor-2.0.1-scope.json")), &scope))
	require.Equal(t, "price_floor/2.0.1", scope.WicketID)
	require.NotEmpty(t, scope.Cases)
	for _, c := range scope.Cases {
		sealed, _ := c.Record["body"].(map[string]any)["bounds_commitment"].(string)
		switch {
		case sealed == "":
			assert.Equal(t, "n/a", c.Expect.Result, c.Name)
			assert.Equal(t, false, c.Expect.Evidence["in_scope"], "%s: no floor, out of scope", c.Name)
		case c.Opening == nil:
			assert.Equal(t, "n/a", c.Expect.Result, c.Name)
			assert.Equal(t, "commercial_bounds_opening", c.Expect.Evidence["missing_field"], c.Name)
		default:
			min, opens := opensTo(c.Opening, sealed)
			require.True(t, opens, c.Name)
			want := "fail"
			if c.Action.AmountMinor >= min {
				want = "pass"
			}
			assert.Equal(t, want, c.Expect.Result, c.Name)
		}
	}

	var whole struct {
		Cases []struct {
			Name     string         `json:"name"`
			TypedRef map[string]any `json:"typed_ref"`
			Expect   struct {
				TaskAuthorityRef any `json:"task_authority_ref"`
			} `json:"expect"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join("testdata", "commercial-bounds", "engine-typed-ref-whole-value.json")), &whole))
	for _, c := range whole.Cases {
		d, _ := c.TypedRef["digest"].(string)
		assert.Equal(t, c.Expect.TaskAuthorityRef != nil, lowerHex64.MatchString(d), "%s: a digest is the whole value or nothing", c.Name)
	}
}

// Every digest, commitment and nonce the checker input carries is exactly 64
// lowercase hex: no whitespace, nothing trailing.
func TestEveryDigestTheCheckerIsGivenIsWhole(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	_, _, input := ruleInputs(t, id, `{"action":"commit","terms":{"item":"example bicycle","price_minor":175000}}`)
	checked := 0
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(k, c)
			}
		case []any:
			for _, c := range x {
				walk(key, c)
			}
		case string:
			if key == "digest" || key == "nonce" || strings.HasSuffix(key, "_commitment") || strings.HasSuffix(key, "_digest") {
				checked++
				assert.Regexp(t, lowerHex64, x, key)
				assert.Equal(t, strings.TrimSpace(x), x, key)
			}
		}
	}
	walk("", input)
	assert.Positive(t, checked)
}
