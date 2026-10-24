package deal

import (
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proposal an offer answers (capsule-cli#364, capsule-engine#193): a
// buyer's message named by answering, sealed as the check's source ref and
// told to the rules checker as proposal_at; or, with no buyer message sealed
// yet, the rule input no_proposal.

const firstOffer = `{"action":"offer","amount_minor":185000,"terms":{"item":"example bicycle","quantity":1,"price_minor":185000}}`

func answeringOffer(message string) string {
	return `{"action":"offer","amount_minor":185000,"terms":{"item":"example bicycle","quantity":1,"price_minor":185000},"answering":"` + message + `"}`
}

// noteMessage seals a message and returns its capsule id and sealed at.
func noteMessage(t *testing.T, id, body string) (string, string) {
	t.Helper()
	out := dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, body))
	records, _ := exportRecords(t, id)
	last := records[len(records)-1]["x-deal-v0"].(map[string]any)
	require.Equal(t, "message", last["record_type"])
	return out["capsule_id"].(string), last["at"].(string)
}

// latestCheck is the refs and body of the deal's latest typed check.
func latestCheck(t *testing.T, id string) (refs []any, body map[string]any) {
	t.Helper()
	records, _ := exportRecords(t, id)
	for _, r := range records {
		if typeOf(r) == cli.TypeProposedAction {
			refs, _ = r["refs"].([]any)
			body = r["body"].(map[string]any)
		}
	}
	require.NotNil(t, body, "the deal has a check")
	return refs, body
}

// refTo is the digest the one ref of rel names, or empty.
func refTo(refs []any, rel string) string {
	for _, x := range refs {
		r := x.(map[string]any)
		if r["rel"] == rel {
			return r["digest"].(string)
		}
	}
	return ""
}

func TestAnOfferAnsweringABuyersMessageCarriesItsTime(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	message, noted := noteMessage(t, id, `{"from":"counterparty","text":"Would you take 1850?"}`)
	laterClock(t, 2*time.Hour)
	sealed, _, input := ruleInputs(t, id, answeringOffer(message))
	record := input["record"].(map[string]any)
	assert.Equal(t, noted, record["proposal_at"], "the message's sealed at: when it was noted")
	assert.NotContains(t, sealed, "no_proposal")
	refs, _ := latestCheck(t, id)
	records, _ := exportRecords(t, id)
	var digest string
	for _, r := range records {
		if b, ok := r["x-deal-v0"].(map[string]any); ok && b["record_type"] == "message" {
			digest = recordDigest(t, r)
		}
	}
	assert.Equal(t, digest, refTo(refs, "source"), "the typed check's source ref names the message")
	assert.Empty(t, refTo(refs, "supersedes"), "the first offer supersedes none")
}

// The platform's own time, when the agent noted it, is the proposal's time:
// not when the message was noted.
func TestAnOfferIsHeldToTheMessagesSentAt(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	sent := cli.DealClock().Add(-30 * time.Hour).In(time.FixedZone("PDT", -7*3600)).Format(time.RFC3339)
	message, _ := noteMessage(t, id, `{"from":"counterparty","text":"Would you take 1850?","sent_at":"`+sent+`"}`)
	records, _ := exportRecords(t, id)
	body := records[len(records)-1]["body"].(map[string]any)
	want := cli.DealClock().Add(-30 * time.Hour).UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	assert.Equal(t, want, body["sent_at"], "sealed beside the note, in UTC")
	_, _, input := ruleInputs(t, id, answeringOffer(message))
	assert.Equal(t, want, input["record"].(map[string]any)["proposal_at"])
}

func TestASentAtInTheFutureOrUnreadableIsRefused(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	for _, at := range []string{cli.DealClock().Add(time.Hour).UTC().Format(time.RFC3339), "yesterday", "2026-10-06 09:14"} {
		err := dealRunErr(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"hi","sent_at":"`+at+`"}`))
		require.Error(t, err, at)
		assert.Contains(t, err.Error(), "`sent_at` must be when the platform shows the message was sent")
	}
}

// A first offer on a thread no buyer message reached yet answers no
// proposal: the check seals no_proposal, with no source and no proposal_at.
func TestAFirstOfferWithNoBuyerMessageCarriesNoProposal(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	noteMessage(t, id, `{"from":"agent","text":"Still available, 1900."}`)
	noteMessage(t, id, `{"from":"user","text":"Tell them it is still for sale."}`)
	sealed, given, input := ruleInputs(t, id, firstOffer)
	assert.Equal(t, true, sealed["no_proposal"])
	assert.Equal(t, true, given["no_proposal"])
	assert.NotContains(t, input["record"], "proposal_at")
	refs, _ := latestCheck(t, id)
	assert.Empty(t, refTo(refs, "source"))
}

// no_proposal is decided by capsulectl from its own log, never taken from
// the input file: an offer on a thread with a buyer message that names none
// carries neither, and still fails closed on the proposal's age.
func TestAnOfferNamingNoMessageOnAThreadWithOneCarriesNeither(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	noteMessage(t, id, `{"from":"counterparty","text":"Would you take 1850?"}`)
	sealed, _, input := ruleInputs(t, id, `{"action":"offer","amount_minor":185000,"no_proposal":true,"terms":{"item":"example bicycle","quantity":1,"price_minor":185000}}`)
	assert.NotContains(t, sealed, "no_proposal")
	assert.NotContains(t, input["record"], "proposal_at")
	refs, _ := latestCheck(t, id)
	assert.Empty(t, refTo(refs, "source"))
}

// A later offer answering a later message supersedes the one before and
// names the message it answers.
func TestALaterOfferAnswersALaterMessage(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	makeOffer(t, id, "190000")
	_, body := latestCheck(t, id)
	assert.Equal(t, true, body["no_proposal"], "the first offer, before any buyer message")
	message, noted := noteMessage(t, id, `{"from":"counterparty","text":"1850 and I pick it up today"}`)
	_, _, input := ruleInputs(t, id, answeringOffer(message))
	assert.Equal(t, noted, input["record"].(map[string]any)["proposal_at"])
	refs, body := latestCheck(t, id)
	assert.NotEmpty(t, refTo(refs, "supersedes"))
	assert.NotEmpty(t, refTo(refs, "source"))
	assert.NotContains(t, body, "no_proposal")
}

func TestAnsweringIsRefusedUnlessItNamesABuyersMessageOnAnOffer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	agent, _ := noteMessage(t, id, `{"from":"agent","text":"Still available, 1900."}`)
	user, _ := noteMessage(t, id, `{"from":"user","text":"Tell them it is still for sale."}`)
	buyer, _ := noteMessage(t, id, `{"from":"counterparty","text":"Would you take 1850?"}`)
	other := openTyped(t, sellerTyped)
	elsewhere, _ := noteMessage(t, other, `{"from":"counterparty","text":"Is it still for sale?"}`)
	for name, answering := range map[string]string{
		"the agent's message": agent, "the user's message": user, "a message in another deal": elsewhere,
		"no message sealed yet": "0000000000000000000000000000000000000000000000000000000000000000",
	} {
		err := dealRunErr(t, "check", "--deal", id, "--input", writeJSON(t, answeringOffer(answering)))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "`answering` names no message the buyer sent in this deal", name)
	}
	for name, check := range map[string]string{
		"a commit": `{"action":"commit","amount_minor":185000,"terms":{"item":"example bicycle","quantity":1,"price_minor":185000},"answering":"` + buyer + `"}`,
		"a share":  `{"action":"share_contact","disclosing":["email"],"disclosing_to":"counterparty","answering":"` + buyer + `"}`,
	} {
		err := dealRunErr(t, "check", "--deal", id, "--input", writeJSON(t, check))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "`answering` goes only with an offer", name)
	}
}

// A commit keeps its own path: the accepted offer's time, never a message's,
// and it carries neither source nor no_proposal.
func TestACommitStillRestsOnTheAcceptedOffer(t *testing.T) {
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	message, _ := noteMessage(t, id, `{"from":"counterparty","text":"Would you take 1900?"}`)
	check := approved(t, id, dealRun(t, "check", "--deal", id, "--input", writeJSON(t,
		`{"action":"offer","amount_minor":190000,"terms":{"item":"example bicycle","quantity":1,"price_minor":190000},"answering":"`+message+`"}`)))
	dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"offer","amount_minor":190000}`))
	offered := offerRecordAt(t, id)
	laterClock(t, 3*time.Hour)
	_, err := acceptOffer(t, id, check)
	require.NoError(t, err)
	sealed, _, input := ruleInputs(t, id, sellerCommit)
	assert.Equal(t, offered, input["record"].(map[string]any)["proposal_at"])
	assert.NotContains(t, sealed, "no_proposal")
	refs, _ := latestCheck(t, id)
	assert.Empty(t, refTo(refs, "source"))
	act := dealRun(t, "note", "--deal", id, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","amount_minor":190000}`))
	assert.Equal(t, false, act["unchecked"])
}
