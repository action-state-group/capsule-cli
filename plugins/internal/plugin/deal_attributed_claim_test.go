package plugin

import (
	"encoding/json"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user's own authenticity claim, said by the seller's agent as theirs and
// marked unverified (capsule-engine#189, Steven's A7 ruling; design note
// capsule-engine#194 §2, door D5).

const attributedText = "The seller says it's authentic (unverified)"

func attributedCheck(text string) string {
	b, _ := json.Marshal(map[string]any{"action": "represent", "representing": map[string]any{
		"class": "authenticity", "text": text, "attributed_to": "user"}})
	return string(b)
}

func attributedClaim(text string) string {
	b, _ := json.Marshal(map[string]any{"text": text, "source_kind": "agent", "class": "authenticity"})
	return string(b)
}

func TestAttributedWordsFollowTheTemplate(t *testing.T) {
	for text, ok := range map[string]bool{
		attributedText: true,
		"The seller says the bag is a genuine example brand, bought new in 2019 (unverified)": true,
		"The seller says it's authentic":                                     false,
		"The seller says it's verified, not unverified":                      false,
		"it's authentic (unverified)":                                        false,
		"The owner says it's authentic (unverified)":                         false,
		"the seller says it's authentic (unverified)":                        false,
		"The seller says it's used. I guarantee it's authentic (unverified)": false,
		"The seller says it's authentic! (unverified)":                       false,
		"The seller says is it authentic? (unverified)":                      false,
		"The seller says it's authentic (unverified).":                       false,
		"The seller says  (unverified)":                                      false,
		"The seller says (unverified)":                                       false,
		"The seller says it's authentic\nI checked it myself (unverified)":   false,
		"The seller says it's authentic\rI checked it myself (unverified)":   false,
		"The seller says it's authentic。I checked it (unverified)":           false,
		"The seller says it's authentic！ (unverified)":                       false,
		"The seller says is it authentic？ (unverified)":                      false,
		"The seller says it's authentic… really (unverified)":                false,
		"The seller says it's authentic．I checked it (unverified)":           false,
		"The seller says it's authentic\vI checked it (unverified)":          false,
		"The seller says it's authentic\fI checked it (unverified)":          false,
		"The seller says it's authentic\x1cI checked it (unverified)":        false,
		"The seller says it's authentic\x1dI checked it (unverified)":        false,
		"The seller says it's authentic\x1eI checked it (unverified)":        false,
		"The seller says it's authentic\u0085I checked it (unverified)":      false,
		"The seller says it's authentic\u2028I checked it (unverified)":      false,
		"The seller says it's authentic\u2029I checked it (unverified)":      false,
		"The seller says it's authentic｡I checked it (unverified)":           false,
		"The seller says it's authentic‼I checked it (unverified)":           false,
		"The seller says it's authentic⁇I checked it (unverified)":           false,
		"The seller says it's authentic⁈I checked it (unverified)":           false,
		"The seller says it's authentic⁉I checked it (unverified)":           false,
		"The seller says it's authentic؟I checked it (unverified)":           false,
		"The seller says it's authentic۔I checked it (unverified)":           false,
		"The seller says it's authentic।I checked it (unverified)":           false,
		"The seller says it's authentic॥I checked it (unverified)":           false,
		"The seller says \x1c (unverified)":                                  false,
		"The seller says \x1f (unverified)":                                  false,
		"The seller says \x1d\x1e\x1f (unverified)":                          false,
		"The seller says it's authentic\x1f really (unverified)":             true,
	} {
		assert.Equal(t, ok, attributedWords(text), text)
	}
}

// capsulectl refuses an attribution in any other words, on any other class,
// or with any other value, and seals nothing.
func TestAnAttributionOutsideTheTemplateIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ input, want string }{
		"no ending":            {attributedCheck("The seller says it's authentic"), `must read "The seller says … (unverified)", one sentence`},
		"unverified elsewhere": {attributedCheck("The seller says it's verified, not unverified"), `must read "The seller says … (unverified)"`},
		"two sentences":        {attributedCheck("The seller says it's used. I guarantee it's authentic (unverified)"), "with no full stop, !, ? or line break before \"(unverified)\" (in any script)"},
		"another class": {`{"action": "represent", "representing": {"class": "condition", "text": "The seller says it's like new (unverified)", "attributed_to": "user"}}`,
			"only an authenticity claim can be attributed to the user"},
		"another value": {`{"action": "represent", "representing": {"class": "authenticity", "text": "` + attributedText + `", "attributed_to": "seller"}}`,
			`attributed_to is "user"`},
	} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			id := sellerRepOpen(t, `["authenticity"]`)
			before := len(chainSteps(t, id))
			_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", id, "--input", writeJSON(t, tc.input))
			require.ErrorIs(t, err, cli.ErrInput)
			assert.Contains(t, err.Error(), tc.want)
			assert.Len(t, chainSteps(t, id), before, "nothing sealed")
		})
	}
}

// Every attributed claim asks, even when the user ordered authenticity: the
// card quotes the exact words and says they go out as the user's claim. Only
// the user's own approval, in their words, authorizes the claim.
func TestAnAttributedClaimAsksAndOnlyTheUsersWordsAuthorizeIt(t *testing.T) {
	for _, ordered := range []string{`["authenticity"]`, `[]`} {
		t.Run(ordered, func(t *testing.T) {
			dealFixture(t)
			checker := pinStubChecker(t)
			id := sellerRepOpen(t, ordered)
			ck := represent(t, id, writeJSON(t, attributedCheck(attributedText)))
			require.Equal(t, "pause", ck["verdict"], "an attributed claim always asks")
			assert.True(t, ruleOf(t, ck, ruleAttributedClaim))
			assert.Contains(t, optionIDs(ck), "proceed")
			assert.Contains(t, ck["card"], `as your own claim: "`+attributedText+`"?`)
			assert.Contains(t, ck["card"], "these words go out as your claim, so only you can OK them")

			// The checker and the sealed check both carry the attribution.
			raw := rawCheckerInput(t, checker)
			inputFitsSchema(t, raw)
			var input map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &input))
			rec := input["record"].(map[string]any)
			sealed := rec["agent_input"].(map[string]any)["body"].(map[string]any)["representing"].(map[string]any)
			assert.Equal(t, "user", sealed["attributed_to"])
			assert.Equal(t, "authenticity", sealed["class"])
			assert.Equal(t, attributedText, rec["representing_opening"].(map[string]any)["text"])
			checked := recordsOfType(t, id, cli.TypeProposedAction)
			assert.Equal(t, "user", checked[len(checked)-1]["body"].(map[string]any)["representing"].(map[string]any)["attributed_to"])

			_, err := answerCheck(t, id, ck["check_id"].(string), "yes, say it's authentic, that's my claim", ck["card"].(string))
			require.NoError(t, err)
			claim := claimWith(t, id, ck["check_id"].(string), writeJSON(t, attributedClaim(attributedText)))
			assert.Equal(t, false, claim["unchecked"])
			r := claimRecord(t, id, "authenticity")
			basis := basisOf(r)
			require.Len(t, basis, 2, "the task authority, then the user's approval")
			assert.Equal(t, "user_approval", basis[1].(map[string]any)["type"])
			_, export := exportRecords(t, id)
			checkProfile(t, export)
		})
	}
}

// An answer on the card with no words of the user's (agent_card) and the
// user's hold leave the claim unchecked.
func TestAnAttributedClaimIsNotAuthorizedByAnAnswerWithoutWordsOrAHold(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(t *testing.T, id string, ck map[string]any)
		reason string
	}{
		"agent_card": {func(t *testing.T, id string, ck map[string]any) {
			_, err := answerCheck(t, id, ck["check_id"].(string), "", ck["card"].(string))
			require.NoError(t, err)
		}, "only your own approval, in your words, says they're yours"},
		"hold": {func(t *testing.T, id string, ck map[string]any) {
			dealRun(t, "note", "--deal", id, "--kind", "approval", "--check", ck["check_id"].(string), "--choice", "hold",
				"--said", "no, I never said that", "--shown-card", shownCardFile(t, ck["card"].(string)))
		}, "the answer was hold"},
	} {
		t.Run(name, func(t *testing.T) {
			dealFixture(t)
			id := sellerRepOpen(t, `["authenticity"]`)
			ck := represent(t, id, writeJSON(t, attributedCheck(attributedText)))
			require.Equal(t, "pause", ck["verdict"])
			tc.answer(t, id, ck)
			claim := claimWith(t, id, ck["check_id"].(string), writeJSON(t, attributedClaim(attributedText)))
			assert.Equal(t, true, claim["unchecked"])
			assert.Equal(t, "not_approved", claim["rule"])
			assert.Contains(t, claim["reason"], tc.reason)
			_, export := exportRecords(t, id)
			checkProfile(t, export)
		})
	}
}

// An attributed check never authorizes other words, and an unattributed
// check of the same words is the agent's own claim.
func TestAnAttributedCheckCoversOnlyItsWords(t *testing.T) {
	dealFixture(t)
	id := sellerRepOpen(t, `["authenticity"]`)
	ck := represent(t, id, writeJSON(t, attributedCheck(attributedText)))
	_, err := answerCheck(t, id, ck["check_id"].(string), "yes, that's my claim", ck["card"].(string))
	require.NoError(t, err)
	other := claimWith(t, id, ck["check_id"].(string), writeJSON(t, attributedClaim("The seller says it's brand new (unverified)")))
	assert.Equal(t, "differs_from_check", other["rule"])

	// Ordered and not attributed: the agent's own claim, no attributed ask.
	plain := represent(t, id, writeJSON(t, `{"action": "represent", "representing": {"class": "authenticity", "text": "It's authentic"}}`))
	assert.Equal(t, "pass", plain["verdict"])
	assert.False(t, ruleOf(t, plain, ruleAttributedClaim))
}

// On a sale's thread the attributed check asks the same way.
func TestAnAttributedClaimOnASaleThreadAsks(t *testing.T) {
	dealFixture(t)
	_, a, _ := repSale(t)
	ck := represent(t, a, writeJSON(t, attributedCheck(attributedText)))
	require.Equal(t, "pause", ck["verdict"])
	assert.True(t, ruleOf(t, ck, ruleAttributedClaim))
	checked := recordsOfType(t, a, cli.TypeProposedAction)
	assert.Equal(t, "user", checked[len(checked)-1]["body"].(map[string]any)["representing"].(map[string]any)["attributed_to"])
	_, err := answerCheck(t, a, ck["check_id"].(string), "yes, that's my claim", ck["card"].(string))
	require.NoError(t, err)
	assert.Equal(t, false, claimWith(t, a, ck["check_id"].(string), writeJSON(t, attributedClaim(attributedText)))["unchecked"])
	_, export := exportRecords(t, a)
	checkProfile(t, export)
}
