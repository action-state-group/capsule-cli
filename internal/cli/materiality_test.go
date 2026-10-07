package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materialityFixture is dealFixture with the profile's predicate set by args
// (none: the check fails safe).
func materialityFixture(t *testing.T, args ...string) {
	t.Helper()
	dealFixture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "deal")
	out, err := invoke(t, "", append([]string{"deal", "init", "--profile", "deal", "--dir", dir, "--no-witness"}, args...)...)
	require.NoError(t, err, out)
}

// flightCheck opens a flight purchase whose dates, place and fare the agent
// filled in (the user's terms name none of them) and checks the payment.
func flightCheck(t *testing.T) map[string]any {
	t.Helper()
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"book me a flight","allowed":["pay"]},
		"who":{"name":"Example Air","domain":"air.example"},
		"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"currency":"USD","conditions":{"fare":"Basic"}},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"conditions":{"fare":"Basic"}},"recourse":{"rail":"card","refundable":true}}`))
}

func pausedOn(check map[string]any) []string {
	var fields []string
	for _, d := range check["differences"].([]any) {
		if m := d.(map[string]any); m["rule"] == "agent_picked" {
			fields = append(fields, m["field"].(string))
		}
	}
	return fields
}

// pin sets the deal profile's materiality predicate, as the user does.
func pin(t *testing.T, path string) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "profile", "update", "--materiality", path)
	require.NoError(t, err, out)
}

func writePredicate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "predicate.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// With no predicate configured, every attribute the agent picked pauses the
// check: nothing it chose alone goes through unasked. The neutral example
// lets the same check pass, listing the picks.
func TestNoMaterialityPredicateFailsSafe(t *testing.T) {
	materialityFixture(t)
	check := flightCheck(t)
	assert.Equal(t, "pause", check["verdict"])
	assert.ElementsMatch(t, []string{"item", "when", "place", "conditions.fare"}, pausedOn(check))
	assert.Contains(t, check["card"], "I picked dates Oct 21 to Oct 24; you didn't choose it")
	assert.NotContains(t, check["card"], "picked by the agent, not by you", "no pick is set aside as minor")

	materialityFixture(t, "--materiality", neutralMateriality)
	check = flightCheck(t)
	assert.Equal(t, "pass", check["verdict"], check["card"])
	assert.Empty(t, pausedOn(check))
}

// The Oct 2 case pauses with no predicate as well as under the example: a
// digit in the user's words never settles a quantity the agent picked.
func TestADigitInADateStillPausesWithNoPredicate(t *testing.T) {
	materialityFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"get me tickets for the Oct 2 show","allowed":["pay"]},
		"who":{"name":"Example Tickets","domain":"tickets.example"},
		"terms":{"item":"show ticket","when":"Oct 2","price_minor":9000,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":9000,"terms":{"item":"show ticket","quantity":2,"when":"Oct 2","price_minor":9000},"recourse":{"rail":"card","refundable":true}}`))
	assert.Equal(t, "pause", check["verdict"])
	assert.Contains(t, pausedOn(check), "quantity")
	assert.Contains(t, check["card"], "I picked quantity 2")
}

// The predicate decides: one that names the dates pauses on the dates alone;
// one that names nothing lets every pick through, listed as the agent's.
func TestThePredicateDecidesWhichPicksPause(t *testing.T) {
	materialityFixture(t)
	pin(t, writePredicate(t, `{"type":"materiality-predicate/v0","name":"dates only","version":"1","material":[{"field":"when"}]}`))
	check := flightCheck(t)
	assert.Equal(t, []string{"when"}, pausedOn(check))
	assert.Contains(t, check["card"], "picked by the agent, not by you: item WN 1234")

	pin(t, writePredicate(t, `{"type":"materiality-predicate/v0","name":"nothing","version":"1","material":[]}`))
	check = flightCheck(t)
	assert.Equal(t, "pass", check["verdict"], check["card"])
	assert.Empty(t, pausedOn(check))

	pin(t, writePredicate(t, `{"type":"materiality-predicate/v0","name":"fare","version":"1","material":[{"field_prefix":"conditions.","name_contains_any":["FARE"]}]}`))
	assert.Equal(t, []string{"conditions.fare"}, pausedOn(flightCheck(t)), "names match case-insensitively")
}

// A predicate that cannot be read is refused when the user pins it, and
// never pinned: it never falls back to anything.
func TestAMalformedPredicateCannotBePinned(t *testing.T) {
	materialityFixture(t)
	for name, body := range map[string]string{
		"unknown member":            `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[],"extra":1}`,
		"other type":                `{"type":"materiality-predicate/v1","name":"x","version":"1","material":[]}`,
		"no material":               `{"type":"materiality-predicate/v0","name":"x","version":"1"}`,
		"no name":                   `{"type":"materiality-predicate/v0","version":"1","material":[]}`,
		"both field kinds":          `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[{"field":"when","field_prefix":"conditions."}]}`,
		"neither field kind":        `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[{"except_values":["1"]}]}`,
		"words with an exact field": `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[{"field":"when","name_contains_any":["a"]}]}`,
		"an empty word":             `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[{"field_prefix":"conditions.","name_contains_any":[" "]}]}`,
		"two documents":             `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[]} {}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "profile", "update", "--materiality", writePredicate(t, body))
		assert.ErrorIs(t, err, ErrInput, name)
	}
	assert.Equal(t, "pause", flightCheck(t)["verdict"], "the profile still pins none: every pick pauses")

	_, err := invoke(t, "", "deal", "init", "--profile", "other", "--dir", filepath.Join(t.TempDir(), "other"), "--no-witness",
		"--materiality", writePredicate(t, `{"type":"materiality-predicate/v0"}`))
	assert.ErrorIs(t, err, ErrInput, "deal init refuses a predicate it cannot read")
}

// Whoever runs a check cannot choose the predicate: there is no per-check
// override, so an agent cannot hand the check an empty predicate to silence
// every pause. Only the profile's pinned predicate applies.
func TestAnAgentCannotSilenceThePausesWithItsOwnPredicate(t *testing.T) {
	materialityFixture(t)
	empty := writePredicate(t, `{"type":"materiality-predicate/v0","name":"nothing","version":"1","material":[]}`)
	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"), "--materiality", empty)
	require.Error(t, err, "deal check takes no predicate of its own")
	assert.Contains(t, err.Error(), "has no flag --materiality")
	check := flightCheck(t)
	assert.Equal(t, "pause", check["verdict"], "the profile pins none: every pick still pauses")
	assert.Equal(t, "none", check["materiality"].(map[string]any)["digest"])
}

// A pinned predicate that changed on disk refuses the check, which seals
// nothing; so does a predicate path with no pinned digest (a profile written
// before predicates were pinned). Re-pinning is the user's profile update.
func TestAChangedOrUnpinnedPredicateRefusesTheCheck(t *testing.T) {
	materialityFixture(t)
	path := writePredicate(t, `{"type":"materiality-predicate/v0","name":"dates only","version":"1","material":[{"field":"when"}]}`)
	pin(t, path)
	assert.Equal(t, []string{"when"}, pausedOn(flightCheck(t)))

	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"materiality-predicate/v0","name":"dates only","version":"1","material":[]}`), 0o600))
	trail := func() string { return dealRun(t, "report", "--deal", id)["trail"].(string) }
	before := trail()
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "materiality predicate changed since it was pinned")
	assert.Contains(t, err.Error(), "profile update --materiality")
	assert.Equal(t, before, trail(), "a refused check seals nothing")

	pin(t, path)
	assert.Equal(t, "pass", flightCheck(t)["verdict"], "re-pinned by the user, the new predicate applies")

	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Materiality.Digest = ""
	require.NoError(t, saveProfile(p, true))
	_, err = invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "not pinned")

	pin(t, "")
	assert.Equal(t, "pause", flightCheck(t)["verdict"], "removing the predicate falls back to every pick pausing")
}

// The check says which predicate decided, in its output and in its sealed
// verdict, so a verifier knows what applied: name, version and digest, or
// digest "none" when every pick paused.
func TestTheSealedCheckCarriesThePredicate(t *testing.T) {
	materialityFixture(t, "--materiality", neutralMateriality)
	raw, err := os.ReadFile(neutralMateriality)
	require.NoError(t, err)
	example, err := parseMaterialityPredicate("", raw)
	require.NoError(t, err)

	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	assert.Equal(t, map[string]any{"name": example.Name, "version": example.Version, "digest": example.Digest}, check["materiality"])
	pin(t, "")
	dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))

	records, export := exportRecords(t, id)
	var sealed []any
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			sealed = append(sealed, r["body"].(map[string]any)["materiality"])
		}
	}
	require.Len(t, sealed, 2)
	assert.Equal(t, map[string]any{"name": example.Name, "version": example.Version, "digest": example.Digest}, sealed[0])
	assert.Equal(t, map[string]any{"digest": "none"}, sealed[1])
	checkProfile(t, export)
}

// The predicate's identity is the SHA-256 of its JCS bytes: key order and
// whitespace do not change it, content does.
func TestThePredicateDigestIsItsJCSDigest(t *testing.T) {
	a := `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[{"field":"when"}]}`
	b := "{\n  \"material\": [ {\"field\": \"when\"} ],\n  \"version\": \"1\", \"name\": \"x\",\n  \"type\": \"materiality-predicate/v0\"\n}"
	pa, err := parseMaterialityPredicate("a", []byte(a))
	require.NoError(t, err)
	pb, err := parseMaterialityPredicate("b", []byte(b))
	require.NoError(t, err)
	var generic any
	require.NoError(t, json.Unmarshal([]byte(a), &generic))
	want, err := canonical.JSONDigest(generic)
	require.NoError(t, err)
	assert.Equal(t, want, pa.Digest)
	assert.Equal(t, pa.Digest, pb.Digest)
	pc, err := parseMaterialityPredicate("c", []byte(strings.Replace(a, "when", "place", 1)))
	require.NoError(t, err)
	assert.NotEqual(t, pa.Digest, pc.Digest)
}

// No materiality policy is compiled in: the words the old regex held live
// only in the example predicate.
func TestNoMaterialityPolicyInTheCheck(t *testing.T) {
	src, err := os.ReadFile("deal_rules.go")
	require.NoError(t, err)
	for _, word := range []string{"materialCondition", "materialAttribute", "variant", "shipping", "delivery"} {
		assert.NotContains(t, string(src), word)
	}
	example, err := os.ReadFile(neutralMateriality)
	require.NoError(t, err)
	assert.Contains(t, string(example), "example, not policy")
	_, err = parseMaterialityPredicate("the example", example)
	require.NoError(t, err)
}

// doctor says when a deal profile has no predicate (a notice: checks fail
// safe) and names the one it has.
func TestDoctorReportsTheMaterialityPredicate(t *testing.T) {
	t.Run("changed since pinned", func(t *testing.T) {
		materialityFixture(t)
		path := writePredicate(t, `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[]}`)
		pin(t, path)
		require.NoError(t, os.WriteFile(path, []byte(`{"type":"materiality-predicate/v0","name":"x","version":"2","material":[]}`), 0o600))
		out, err := invoke(t, "", "doctor", "--profile", "deal")
		require.ErrorIs(t, err, ErrPartial)
		assert.Contains(t, out, "changed since it was pinned")
	})
	materialityFixture(t)
	report := func() map[string]any {
		out, err := invoke(t, "", "doctor", "--profile", "deal")
		require.NoError(t, err, out)
		var r map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		return r["profile"].(map[string]any)["materiality"].(map[string]any)
	}
	none := report()
	assert.Equal(t, false, none["configured"])
	assert.Contains(t, none["notice"], "every attribute the agent picks pauses the check")

	materialityFixture(t, "--materiality", neutralMateriality)
	set := report()
	assert.Equal(t, true, set["ok"])
	example, err := os.ReadFile(neutralMateriality)
	require.NoError(t, err)
	p, err := parseMaterialityPredicate("", example)
	require.NoError(t, err)
	assert.Equal(t, p.Digest, set["digest"])
}

// The deal's opening seals the predicate pinned then, from the profile and
// never from the input. A check under another one (a re-pin mid-deal) is
// flagged on its card and pauses; one under the same predicate is not.
func TestARepinMidDealIsFlagged(t *testing.T) {
	materialityFixture(t, "--materiality", neutralMateriality)
	raw, err := os.ReadFile(filepath.Join(otterFixture, "open.json"))
	require.NoError(t, err)
	var open map[string]any
	require.NoError(t, json.Unmarshal(raw, &open))
	open["materiality"] = map[string]any{"digest": "none"}
	claimed, err := json.Marshal(open)
	require.NoError(t, err)
	id := dealRun(t, "open", "--input", writeJSON(t, string(claimed)))["deal_id"].(string)

	example, err := os.ReadFile(neutralMateriality)
	require.NoError(t, err)
	p, err := parseMaterialityPredicate("", example)
	require.NoError(t, err)
	records, _ := exportRecords(t, id)
	assert.Equal(t, map[string]any{"name": p.Name, "version": p.Version, "digest": p.Digest}, records[0]["body"].(map[string]any)["materiality"], "the profile's predicate, not the input's claim")

	rules := func(check map[string]any) []string {
		var out []string
		for _, d := range check["differences"].([]any) {
			out = append(out, d.(map[string]any)["rule"].(string))
		}
		return out
	}
	same := dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	assert.NotContains(t, rules(same), "materiality_changed")

	pin(t, writePredicate(t, `{"type":"materiality-predicate/v0","name":"nothing","version":"2","material":[]}`))
	changed := dealRun(t, "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	assert.Equal(t, "pause", changed["verdict"])
	assert.Contains(t, rules(changed), "materiality_changed")
	assert.Contains(t, changed["card"], "The rule for which of the agent's picks need your answer changed since this deal opened ("+p.Name+" "+p.Version+" → nothing 2)")
	assert.Empty(t, pausedOn(changed), "the new predicate itself pauses on nothing: only the change is flagged")
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

// A pinned predicate whose file is gone refuses with a message that says so,
// at a check and at a deal's opening.
func TestAMissingPredicateFileRefusesWithItsOwnMessage(t *testing.T) {
	materialityFixture(t)
	path := writePredicate(t, `{"type":"materiality-predicate/v0","name":"x","version":"1","material":[]}`)
	pin(t, path)
	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	require.NoError(t, os.Remove(path))
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "materiality predicate file missing")
	assert.Contains(t, err.Error(), "profile update --materiality")
	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--input", filepath.Join(otterFixture, "open.json"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "materiality predicate file missing")
}

// A shared copy carries the materiality the opening and every check
// recorded: the opening and check records are not withheld for it.
func TestASharedCopyKeepsTheMaterialityRecords(t *testing.T) {
	materialityFixture(t, "--materiality", neutralMateriality)
	id := openCeilingDeal(t, false)
	b, _ := sharedCopy(t, id, dealAudienceCounterparty, "x")
	for _, s := range b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["steps"].([]any) {
		step := s.(map[string]any)
		if step["kind"] == "open" || step["kind"] == "check" {
			assert.Equal(t, false, step["withheld"], "step %v (%v)", step["n"], step["kind"])
		}
	}
	for _, bad := range []any{
		map[string]any{"digest": "abc"},
		map[string]any{"digest": "none", "name": "x"},
		map[string]any{"digest": "none", "name": "x", "version": "1", "extra": "y"},
		"none",
	} {
		assert.False(t, shareableMateriality(bad), "%v", bad)
	}
}
