package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An act record names no counterparty, so a rule keying earlier acts on who
// they were with had no target on an act. Each history act now carries, beside
// its capsule, the counterparty its check sealed: per deal, the same key the
// deal's checks carry.

// sealedCheckCounterparty is the counterparty a checked record seals, where its
// record set seals it: an x-deal-v0 check's header, a typed check's body.
func sealedCheckCounterparty(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	input := record["agent_input"].(map[string]any)
	if block, ok := input["x-deal-v0"].(map[string]any); ok {
		return block["counterparty"].(map[string]any)
	}
	return input["body"].(map[string]any)["counterparty"].(map[string]any)
}

func checkerInputFits(t *testing.T, checker string) {
	t.Helper()
	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	schema, err := os.ReadFile(filepath.Join(dealProfileDir, "external-check-input-v0.schema.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("input.json", doc))
	compiled, err := c.Compile("input.json")
	require.NoError(t, err)
	given, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	require.NoError(t, err)
	assert.NoError(t, compiled.Validate(given), "what the checker is given fits external-check-input/v0")
}

// A repeated booking in one deal: the earlier act, as the checker is given it,
// names the same counterparty as the new check, so a dedupe rule keyed on the
// counterparty sees a repeat on acts as on checks.
func TestARepeatedBookingsActNamesTheChecksCounterparty(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		id := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		first := book(t, id)
		dealRun(t, "check", "--deal", id, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)
		checkerInputFits(t, checker)

		now := sealedCheckCounterparty(t, input["record"].(map[string]any))
		act := historyByID(input)[first["capsule_id"].(string)]
		require.NotNil(t, act)
		assert.Equal(t, now, act["counterparty"], "the act names who its check named, keyed per deal")
		assert.NotContains(t, act["agent_input"], "counterparty", "supplied beside the act record, never part of it")
		ids := act["counterparty"].(map[string]any)["ids"].(map[string]any)
		assert.Contains(t, ids, "payee", "the payee a dedupe rule compares")
		want := "hmac-sha256-deal-key"
		if typed {
			want = typedFPAlg
		}
		assert.Equal(t, want, act["counterparty"].(map[string]any)["fp_alg"])
	})
}

// A merchant booked in one deal, checked in another: the earlier act names
// its own deal's counterparty (keyed per deal, so not equal to the new
// check's) and the profile value that is (the cross-deal key a rule on
// earlier dealings reads).
func TestABookedMerchantsActInAnotherDealCarriesBothKeys(t *testing.T) {
	eachRecordSet(t, func(t *testing.T, typed bool) {
		checker := pinStubChecker(t)
		first := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		booked := book(t, first)
		again := openHotel(t, typed, "Example Hotel Shinjuku", "hotel.example")
		dealRun(t, "check", "--deal", again, "--input", writeJSON(t, hotelCommit))
		input := checkerInput(t, checker)
		checkerInputFits(t, checker)

		record := input["record"].(map[string]any)
		act := historyByID(input)[booked["capsule_id"].(string)]
		require.NotNil(t, act)
		require.Contains(t, act, "counterparty")
		assert.NotEqual(t, sealedCheckCounterparty(t, record)["ids"], act["counterparty"].(map[string]any)["ids"], "per-deal keys never match across deals")
		assert.Equal(t, record["counterparty_profile"], act["counterparty_profile"], "the profile value does: the booked merchant is not new")
	})
}

// An act with no check behind it names no counterparty.
func TestAnUncheckedActNamesNoCounterparty(t *testing.T) {
	dealFixture(t)
	checker := pinStubChecker(t)
	id := openHotel(t, false, "Example Hotel Shinjuku", "hotel.example")
	unchecked := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":38000}`))
	require.Equal(t, true, unchecked["unchecked"])
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, hotelCommit))
	act := historyByID(checkerInput(t, checker))[unchecked["capsule_id"].(string)]
	require.NotNil(t, act)
	assert.NotContains(t, act, "counterparty")
}
