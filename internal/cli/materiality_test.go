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
func flightCheck(t *testing.T, extra ...string) map[string]any {
	t.Helper()
	id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"book me a flight","allowed":["pay"]},
		"who":{"name":"Example Air","domain":"air.example"},
		"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"currency":"USD","conditions":{"fare":"Basic"}},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	args := append([]string{"check", "--deal", id, "--input", writeJSON(t, `{"action":"pay","amount_minor":55880,"terms":{"item":"WN 1234","when":"Oct 21 to Oct 24","place":"HOU/SJC","price_minor":55880,"conditions":{"fare":"Basic"}},"recourse":{"rail":"card","refundable":true}}`)}, extra...)
	return dealRun(t, args...)
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
	dates := writePredicate(t, `{"type":"materiality-predicate/v0","name":"dates only","version":"1","material":[{"field":"when"}]}`)
	check := flightCheck(t, "--materiality", dates)
	assert.Equal(t, []string{"when"}, pausedOn(check))
	assert.Contains(t, check["card"], "picked by the agent, not by you: item WN 1234")

	nothing := writePredicate(t, `{"type":"materiality-predicate/v0","name":"nothing","version":"1","material":[]}`)
	check = flightCheck(t, "--materiality", nothing)
	assert.Equal(t, "pass", check["verdict"], check["card"])
	assert.Empty(t, pausedOn(check))

	sizeOnly := writePredicate(t, `{"type":"materiality-predicate/v0","name":"fare","version":"1","material":[{"field_prefix":"conditions.","name_contains_any":["FARE"]}]}`)
	assert.Equal(t, []string{"conditions.fare"}, pausedOn(flightCheck(t, "--materiality", sizeOnly)), "names match case-insensitively")
}

// A predicate that cannot be read refuses the check and seals nothing: it
// never falls back to anything.
func TestAMalformedPredicateRefusesTheCheck(t *testing.T) {
	materialityFixture(t)
	id := dealRun(t, "open", "--input", filepath.Join(otterFixture, "open.json"))["deal_id"].(string)
	steps := func() string { return dealRun(t, "report", "--deal", id)["trail"].(string) }
	before := steps()
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
		_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", filepath.Join(otterFixture, "check-pay.json"), "--materiality", writePredicate(t, body))
		assert.ErrorIs(t, err, ErrInput, name)
	}
	assert.Equal(t, before, steps(), "a refused check seals nothing")

	_, err := invoke(t, "", "deal", "init", "--profile", "other", "--dir", filepath.Join(t.TempDir(), "other"), "--no-witness",
		"--materiality", writePredicate(t, `{"type":"materiality-predicate/v0"}`))
	assert.ErrorIs(t, err, ErrInput, "deal init refuses a predicate it cannot read")
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
