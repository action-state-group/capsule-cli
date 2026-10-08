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

const exampleRulesDigest = "61a3c8954f2b8e7d0c1a6b5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c"

// stubChecker writes an executable checker script into a trusted plugin root
// (the test's own) and returns its path. It saves the record it is given
// beside itself, as <path>.input, and runs body.
func stubChecker(t *testing.T, name, body string) string {
	t.Helper()
	root := os.Getenv("CAPSULECTL_PLUGIN_ROOTS")
	if root == "" {
		root = t.TempDir()
		t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	}
	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\ncat > \""+path+".input\"\n"+body+"\n"), 0o700))
	return path
}

// checkerPrints is a checker body that prints one external-check-result/v0.
func checkerPrints(verdict string, findings string) string {
	return `printf '%s' '{"schema":"external-check-result/v0","ruleset_id":"example-rules/1.0.0","definition_digest":"` + exampleRulesDigest + `","verdict":"` + verdict + `","findings":[` + findings + `]}'`
}

const (
	passFinding = `{"id":"per-purchase","check":"caps/1","verdict":"pass","limit":{"per_action_minor":2500},"value":{"field":"spend_authorized_minor","minor":480}}`
	denyFinding = `{"id":"per-purchase","check":"caps/1","verdict":"fail","reason":"over the per-purchase limit","limit":{"per_action_minor":2500},"value":{"field":"spend_authorized_minor","minor":55880}}`
)

func writePin(t *testing.T, pin map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(pin)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "rules-checker.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func pinChecker(t *testing.T, pin map[string]any) {
	t.Helper()
	path := writePin(t, pin)
	out, err := invoke(t, "", "--profile", "deal", "profile", "update", "--rules-checker", path)
	require.NoError(t, err, out)
}

func approvalText(check map[string]any) string {
	s, _ := check["approval_text"].(string)
	return s
}

// A pinned checker that allows: the check passes, and the prompt leads with
// the ruleset it ran (id and digest) and says a weekly limit was checked
// against this action alone. The checker was given one capsule with its
// disclosed record; the verdict seals the ruleset as data, and the exported
// record carries it.
func TestADealCheckRunsThePinnedRulesChecker(t *testing.T) {
	id := stickerDeal(t, "card", false)
	checker := stubChecker(t, "rules-allow", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})

	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "pass", check["verdict"])
	rules := check["rules"].(map[string]any)
	assert.Equal(t, "evaluated", rules["status"])
	assert.Equal(t, "example-rules/1.0.0", rules["ruleset_id"])
	assert.Equal(t, exampleRulesDigest, rules["definition_digest"])
	assert.Equal(t, "allow", rules["verdict"])
	assert.Equal(t, "this_action_only", rules["window"])
	text := approvalText(check)
	assert.True(t, strings.HasPrefix(text, "Your rules (example-rules/1.0.0, digest 61a3c895): allowed. Any weekly limit was checked against this action alone."), text)
	assert.Contains(t, text, "Before you go ahead: no differences.")
	assert.NotContains(t, text, "Deal check")
	assert.NotContains(t, text, "Stale after", "the stale line stays in the record, out of the prompt")

	raw, err := os.ReadFile(checker + ".input")
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(raw, &input))
	assert.Equal(t, check["snapshot_id"], input["capsule_id"], "one capsule: the record of what is about to happen")
	assert.Equal(t, "pay", input["agent_input"].(map[string]any)["body"].(map[string]any)["action"], "with its disclosed record")

	records, export := exportRecords(t, id)
	var sealed map[string]any
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			sealed = r["body"].(map[string]any)["rules"].(map[string]any)
		}
	}
	require.NotNil(t, sealed)
	assert.Equal(t, "example-rules/1.0.0", sealed["ruleset_id"])
	assert.Equal(t, exampleRulesDigest, sealed["definition_digest"])
	exported, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.Contains(t, string(exported), "example-rules/")
	assert.Contains(t, string(exported), exampleRulesDigest)
	checkProfile(t, export)
}

// A deny is a deny: no proceed option, the limit and value named, and an
// approval to proceed is refused.
func TestADenyFromTheRulesOffersNoWayToProceed(t *testing.T) {
	id := stickerDeal(t, "card", false)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-deny", checkerPrints("deny", denyFinding))}})
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "deny", check["verdict"])
	assert.Equal(t, false, check["proceed"])
	var options []string
	for _, o := range check["options"].([]any) {
		options = append(options, o.(map[string]any)["id"].(string))
	}
	assert.Equal(t, []string{"hold"}, options, "no proceed option")
	assert.Contains(t, check["card"], `Not allowed by your rules: per-purchase: over the per-purchase limit (limit {"per_action_minor":2500}; value {"field":"spend_authorized_minor","minor":55880})`)
	assert.Contains(t, approvalText(check), "Your rules (example-rules/1.0.0, digest 61a3c895): not allowed.")
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "go ahead")
	require.ErrorIs(t, err, ErrInput, "a denied check cannot be approved past")
	report := dealRun(t, "report", "--deal", id)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "Checked before paying: not allowed by your rules")
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

// In typed records, a deny is the evaluation's DENY, and the evaluation
// names the ruleset it ran.
func TestATypedDenyIsDENYAndNamesTheRuleset(t *testing.T) {
	id := stickerDeal(t, "card", true)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-deny", checkerPrints("deny", denyFinding))}})
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "DENY", check["check_response"].(map[string]any)["disposition"])
	_, records := recordsOf(t, id)
	var evaluation map[string]any
	for _, r := range records {
		if typeOf(r) == typeActionEvaluation {
			evaluation = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, evaluation)
	assert.Equal(t, "DENY", evaluation["disposition"])
	assert.Equal(t, []any{map[string]any{"ruleset_id": "example-rules/1.0.0", "definition_digest": exampleRulesDigest, "verdict": "deny"}}, evaluation["rules_checks"])
	_, export := exportRecords(t, id)
	checkProfile(t, export)
}

// An escalation pauses, and proceeding over it is an override: "Pay anyway".
func TestAnEscalationPausesWithPayAnyway(t *testing.T) {
	id := stickerDeal(t, "card", false)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-escalate", checkerPrints("escalate", denyFinding))}})
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "pause", check["verdict"])
	assert.Contains(t, check["card"], "Your rules ask for approval: per-purchase")
	assert.Contains(t, check["card"], "[Pay anyway]")
}

// A configured checker that cannot answer pauses the check, saying why, and
// proceeding over it is "Pay", not "Pay anyway": no rule found anything.
func TestARulesCheckerThatCannotAnswerPauses(t *testing.T) {
	cases := map[string]struct {
		body string
		pin  map[string]any
		want string
	}{
		"a refusal":       {"echo 'unknown field: shoe_size' >&2; exit 1", nil, "the rules checker refused the record: unknown field: shoe_size"},
		"a timeout":       {"sleep 5", map[string]any{"timeout": "1s"}, "the rules checker did not answer within 1s"},
		"bad output":      {"echo 'Decision: allow'", nil, "the rules checker's answer could not be read"},
		"another schema":  {`printf '%s' '{"schema":"something-else/v1"}'`, nil, "the rules checker's answer could not be read: its output is not external-check-result/v0"},
		"another ruleset": {checkerPrints("allow", passFinding), map[string]any{"definition_digest": strings.Repeat("0", 64)}, "the ruleset changed since it was pinned"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			id := stickerDeal(t, "card", false)
			pin := map[string]any{"command": []string{stubChecker(t, "rules", tc.body)}}
			for k, v := range tc.pin {
				pin[k] = v
			}
			pinChecker(t, pin)
			check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
			assert.Equal(t, "pause", check["verdict"])
			assert.Equal(t, "not_evaluated", check["rules"].(map[string]any)["status"])
			assert.Contains(t, check["card"], "Your rules were not checked: "+tc.want)
			assert.Contains(t, approvalText(check), "Your rules were not checked: "+tc.want)
			assert.Contains(t, check["card"], "[Pay]", "no rule found anything: not an override")
			assert.NotContains(t, check["card"], "anyway")
		})
	}
}

// A checker swapped after it was pinned is not run.
func TestASwappedRulesCheckerIsNotRun(t *testing.T) {
	id := stickerDeal(t, "card", false)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	require.NoError(t, os.WriteFile(checker, []byte("#!/bin/sh\n"+checkerPrints("allow", passFinding)+"\n# swapped\n"), 0o700))
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "pause", check["verdict"])
	assert.Contains(t, check["card"], "Your rules were not checked: the rules checker changed since it was pinned")
	_, err := os.Stat(checker + ".input")
	assert.True(t, os.IsNotExist(err), "the swapped checker never ran")
}

// No checker configured: the check does not pause for it, but the prompt
// and the sealed verdict say the rules were not checked.
func TestNoRulesCheckerIsSaidAndSealed(t *testing.T) {
	id := stickerDeal(t, "card", false)
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Equal(t, "pass", check["verdict"])
	assert.Equal(t, map[string]any{"status": "not_configured"}, check["rules"])
	assert.True(t, strings.HasPrefix(approvalText(check), "Your rules were not checked: no rules checker configured."), approvalText(check))
	records, export := exportRecords(t, id)
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			assert.Equal(t, map[string]any{"status": "not_configured"}, r["body"].(map[string]any)["rules"])
		}
	}
	checkProfile(t, export)
}

// Pinning is checked: an absolute path under a trusted plugin root.
func TestARulesCheckerMustBePinnedFromATrustedRoot(t *testing.T) {
	stickerDeal(t, "card", false)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", t.TempDir())
	elsewhere := filepath.Join(t.TempDir(), "rules")
	require.NoError(t, os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o700))
	for name, command := range map[string][]string{"a relative path": {"rules"}, "outside the trusted roots": {elsewhere}, "no command": {}} {
		path := writePin(t, map[string]any{"command": command})
		_, err := invoke(t, "", "--profile", "deal", "profile", "update", "--rules-checker", path)
		assert.ErrorIs(t, err, ErrInput, name)
	}
}

// The neutral result's schema ships with the skill, and a checker's output
// of that shape is what the check reads.
func TestTheExternalCheckResultSchemaShips(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(dealProfileDir, "external-check-result-v0.schema.json"))
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, "external-check-result/v0", schema["properties"].(map[string]any)["schema"].(map[string]any)["const"])
	_, err = parseExternalCheck([]byte(`{"schema":"external-check-result/v0","ruleset_id":"example-rules/1.0.0","definition_digest":"` + exampleRulesDigest + `","verdict":"allow","findings":[` + passFinding + `]}`))
	require.NoError(t, err)
}

// The shipped stub checker (testdata), run as a profile pins it: it allows
// under its limit and denies over it, naming the limit and the value.
func TestTheStubCheckerAllowsAndDenies(t *testing.T) {
	stub, err := os.ReadFile("testdata/rules-checker/stub-checker.sh")
	require.NoError(t, err)
	for limit, want := range map[string]string{"1000": "pass", "100": "deny"} {
		t.Run(limit, func(t *testing.T) {
			id := stickerDeal(t, "card", false)
			root := t.TempDir()
			t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
			path := filepath.Join(root, "stub-checker")
			require.NoError(t, os.WriteFile(path, stub, 0o700))
			pinChecker(t, map[string]any{"command": []string{path, limit}, "definition_digest": strings.Repeat("0", 63) + "1"})
			check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
			assert.Equal(t, want, check["verdict"])
			assert.Equal(t, "stub-rules/0.1.0", check["rules"].(map[string]any)["ruleset_id"])
			if want == "deny" {
				assert.Contains(t, check["card"], `Not allowed by your rules: per-action: over the per-action limit (limit {"per_action_minor":100}; value {"minor":480})`)
			}
		})
	}
}

// The schema a checker vendors is the one the check validates against.
func TestTheExternalCheckResultSchemaIsTheEmbeddedOne(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join(dealProfileDir, "external-check-result-v0.schema.json"))
	require.NoError(t, err)
	assert.Equal(t, string(shipped), string(externalCheckSchemaJSON))
	for name, out := range map[string]string{
		"an unknown member":  `{"schema":"external-check-result/v0","ruleset_id":"r","definition_digest":"` + exampleRulesDigest + `","verdict":"allow","findings":[],"pack":{}}`,
		"an unknown verdict": `{"schema":"external-check-result/v0","ruleset_id":"r","definition_digest":"` + exampleRulesDigest + `","verdict":"maybe","findings":[]}`,
		"two values":         `{"schema":"external-check-result/v0","ruleset_id":"r","definition_digest":"` + exampleRulesDigest + `","verdict":"allow","findings":[]} {}`,
		"no findings":        `{"schema":"external-check-result/v0","ruleset_id":"r","definition_digest":"` + exampleRulesDigest + `","verdict":"allow"}`,
	} {
		_, err := parseExternalCheck([]byte(out))
		assert.Error(t, err, name)
	}
}

// The ruleset a user runs and its limits are the user's policy: a shared copy
// never carries them (like the materiality predicate's name and the user's
// spending limit). The user's own copy carries them.
func TestASharedCopyCarriesNoneOfTheUsersRules(t *testing.T) {
	id := stickerDeal(t, "card", false)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules-escalate", checkerPrints("escalate", denyFinding))}})
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		_, raw := sharedCopy(t, id, audience, "the shop's support desk")
		assert.NotContains(t, raw, "example-rules/", audience)
		assert.NotContains(t, raw, "per_action_minor", audience)
		assert.NotContains(t, raw, "over the per-purchase limit", audience)
	}
	own := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", id, "--bundle", own)
	raw, err := os.ReadFile(own)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "example-rules/1.0.0")
}

// The checker's own words reach the prompt, never the record in the clear:
// a refusal seals its cause as a token, and a finding its data.
func TestNoCheckerWordsAreSealedInTheClear(t *testing.T) {
	id := stickerDeal(t, "card", false)
	pinChecker(t, map[string]any{"command": []string{stubChecker(t, "rules", "echo 'risk score too high for shoe_size' >&2; exit 1")}})
	check := dealRun(t, "check", "--deal", id, "--input", writeJSON(t, payCheck("card", 454, 480)))
	assert.Contains(t, check["card"], "shoe_size")
	records, export := exportRecords(t, id)
	for _, r := range records {
		if r["x-deal-v0"].(map[string]any)["record_type"] == "verdict" {
			assert.Equal(t, "refused", r["body"].(map[string]any)["rules"].(map[string]any)["cause"])
		}
	}
	raw, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "shoe_size")
	checkProfile(t, export)
}
