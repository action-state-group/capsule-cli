package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two rulesets' definition digests a profile pins side by side, and a third
// it does not.
const (
	firstRulesDigest  = "8a8e0a7e1ca4319b6e45f2037f4af49e6592919f373e59874ce1ead7249b073d"
	secondRulesDigest = "f0068688317b0cae3c40e839d7cff0137991aca466a878f9b558356f1808321a"
	otherRulesDigest  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// roleChecker is a checker whose ruleset follows the deal's party_role: the
// second ruleset on a deal whose party_role is seller, the first otherwise.
// It allows.
func roleChecker(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	path := filepath.Join(root, "role-rules")
	script := `#!/bin/sh
input=$(cat)
case "$input" in
*'"party_role":"seller"'*) id=rules-b/1; digest=` + secondRulesDigest + ` ;;
*) id=rules-a/1; digest=` + firstRulesDigest + ` ;;
esac
printf '{"schema":"external-check-result/v0","ruleset_id":"%s","definition_digest":"%s","verdict":"allow","findings":[]}' "$id" "$digest"
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path
}

// pinSetDeals opens a deal with no party_role and one whose party_role is
// seller, on one profile, and pins roleChecker to digests.
func pinSetDeals(t *testing.T, digests ...string) (buyer, seller string) {
	t.Helper()
	buyer = stickerDeal(t, "card", false)
	seller = dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
	pinChecker(t, map[string]any{"command": []string{roleChecker(t)}, "definition_digests": digests})
	return buyer, seller
}

func payRulesCheck(t *testing.T, id string) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))["rules"].(map[string]any)
}

func commitRulesCheck(t *testing.T, id string) map[string]any {
	t.Helper()
	return dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"commit","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000}}`))["rules"].(map[string]any)
}

// One profile pinned to two rulesets evaluates a check under either: each
// check's ruleset is one of the set.
func TestAProfilePinnedToASetEvaluatesEachRulesetInIt(t *testing.T) {
	buyer, seller := pinSetDeals(t, firstRulesDigest, secondRulesDigest)

	rules := payRulesCheck(t, buyer)
	assert.Equal(t, "evaluated", rules["status"], rules)
	assert.Equal(t, firstRulesDigest, rules["definition_digest"])
	rules = commitRulesCheck(t, seller)
	assert.Equal(t, "evaluated", rules["status"], rules)
	assert.Equal(t, secondRulesDigest, rules["definition_digest"])
}

// A ruleset outside the pinned set is still a changed ruleset: the check
// is not evaluated, while the ruleset in the set still is.
func TestARulesetOutsideThePinnedSetHasChanged(t *testing.T) {
	buyer, seller := pinSetDeals(t, firstRulesDigest, otherRulesDigest)

	assert.Equal(t, "evaluated", payRulesCheck(t, buyer)["status"])
	rules := commitRulesCheck(t, seller)
	assert.Equal(t, "not_evaluated", rules["status"], rules)
	assert.Equal(t, "ruleset_changed", rules["cause"])
}

// The pinned set prints in full: profile show and doctor name every digest.
func TestEveryPinnedDigestIsShown(t *testing.T) {
	pinSetDeals(t, firstRulesDigest, secondRulesDigest, firstRulesDigest)
	want := []any{firstRulesDigest, secondRulesDigest}

	out, err := invoke(t, "", "profile", "show", "deal")
	require.NoError(t, err, out)
	var shown map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	checker := shown["RulesChecker"].(map[string]any)
	assert.Equal(t, want, checker["DefinitionDigests"], "the set, once each, in the order given")
	assert.Equal(t, "", checker["DefinitionDigest"])

	out, err = invoke(t, "", "--profile", "deal", "doctor")
	require.NoError(t, err, out)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, want, report["profile"].(map[string]any)["rules_checker"].(map[string]any)["definition_digests"])
}

// A single digest, given either way, pins as before: definition_digest
// alone, with no set beside it.
func TestOneDigestPinsAsBefore(t *testing.T) {
	for name, pin := range map[string]map[string]any{
		"definition_digest":  {"definition_digest": firstRulesDigest},
		"definition_digests": {"definition_digests": []string{firstRulesDigest}},
		"both, the same":     {"definition_digest": firstRulesDigest, "definition_digests": []string{firstRulesDigest}},
	} {
		t.Run(name, func(t *testing.T) {
			stickerDeal(t, "card", false)
			pin["command"] = []string{roleChecker(t)}
			pinChecker(t, pin)
			p, err := loadProfile("deal")
			require.NoError(t, err)
			assert.Equal(t, firstRulesDigest, p.RulesChecker.DefinitionDigest)
			assert.Nil(t, p.RulesChecker.DefinitionDigests)
		})
	}
}

// A profile pinned to one digest before a set could be pinned reads, saves
// and shows byte for byte as it did: the fixture and its profile show are
// what the previous capsulectl wrote.
func TestAProfilePinnedToOneDigestIsUnchanged(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fixture, err := os.ReadFile("testdata/rules-checker/single-pin.yaml")
	require.NoError(t, err)
	dir, err := profilesDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "single-pin.yaml")
	require.NoError(t, os.WriteFile(path, fixture, 0o600))

	out, err := invoke(t, "", "profile", "show", "single-pin")
	require.NoError(t, err, out)
	golden, err := os.ReadFile("testdata/rules-checker/single-pin.show.json")
	require.NoError(t, err)
	assert.Equal(t, string(golden), out)

	out, err = invoke(t, "", "--profile", "single-pin", "profile", "update")
	require.NoError(t, err, out)
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(fixture), string(saved))
}

// A profile file holding both shapes, or a malformed digest in the set, is
// refused when it is read.
func TestAMalformedPinnedSetIsRefused(t *testing.T) {
	raw, err := os.ReadFile("testdata/rules-checker/single-pin.yaml")
	require.NoError(t, err)
	fixture, single := string(raw), "    definition_digest: "+firstRulesDigest+"\n"
	for name, body := range map[string]string{
		"both shapes":     fixture + "    definition_digests:\n        - " + secondRulesDigest + "\n",
		"a malformed one": strings.Replace(fixture, single, "    definition_digests:\n        - "+firstRulesDigest+"\n        - NOT-HEX\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, fixture, body)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir, err := profilesDir()
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "single-pin.yaml"), []byte(body), 0o600))
			_, err = loadProfile("single-pin")
			assert.ErrorIs(t, err, ErrInput)
		})
	}
}

// A malformed digest in a pin file's set is refused when it is pinned.
func TestAMalformedDigestInThePinFileIsRefused(t *testing.T) {
	stickerDeal(t, "card", false)
	path := writePin(t, map[string]any{"command": []string{roleChecker(t)}, "definition_digests": []string{firstRulesDigest, "ABC"}})
	_, err := invoke(t, "", "--profile", "deal", "profile", "update", "--rules-checker", path)
	assert.ErrorIs(t, err, ErrInput)
}
