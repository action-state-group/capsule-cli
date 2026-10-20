package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// standaloneOf is a copy's x-deal-report/v0 section.
func standaloneOf(t *testing.T, b map[string]any) map[string]any {
	t.Helper()
	x, ok := b["extensions"].(map[string]any)[cli.DealReportExtension].(map[string]any)
	require.True(t, ok, "the copy carries a standalone report")
	return x
}

// deckCheckpoints are the checkpoints a deal's book has issued, read without
// opening the deal (which would recover an uncovered step).
func deckCheckpoints(t *testing.T, dealID string) int {
	t.Helper()
	n := 0
	withSession(t, func(s *dealSession) {
		book, err := s.shelf.Open(t.Context(), cli.DealLogID(dealID))
		require.NoError(t, err)
		cps, err := book.Checkpoints(t.Context())
		require.NoError(t, err)
		n = len(cps)
	})
	return n
}

// verdictOf verifies a copy and returns its verdict and the x-deal-v0
// entry's findings.
func verdictOf(t *testing.T, b map[string]any) (string, []any) {
	t.Helper()
	result, _ := verifyBundleOutput(t, writeBundle(t, b))
	for _, raw := range result["extensions"].([]any) {
		if e := raw.(map[string]any); e["kind"] == cli.DealProfile {
			findings, _ := e["findings"].([]any)
			return result["verdict"].(string), findings
		}
	}
	return result["verdict"].(string), nil
}

// A deal on the shelf seals each copy's report off its log (capsule-cli
// #294): two copies leave the log, and its checkpoints, as they were; each
// copy carries its own report and no record of another; each verifies.
func TestACopyCarriesItsOwnReportOnly(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	records, checkpoints := len(bookRecords(t, id)), deckCheckpoints(t, id)
	first, firstRaw := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	second, secondRaw := sharedCopy(t, id, cli.DealAudienceAdjudicator, "the platform")
	assert.Len(t, bookRecords(t, id), records, "no copy adds a record to the deal's log")
	assert.Equal(t, checkpoints, deckCheckpoints(t, id), "no copy cuts a checkpoint")
	firstID := standaloneOf(t, first)["record"].(map[string]any)["capsule_id"].(string)
	for _, c := range []struct {
		b   map[string]any
		raw string
	}{{first, firstRaw}, {second, secondRaw}} {
		for _, r := range c.b["records"].([]any) {
			assert.NotEqual(t, cli.DealReportActionID, r.(map[string]any)["action_id"], "the log holds steps only")
		}
		verdict, findings := verdictOf(t, c.b)
		assert.Equal(t, "VALID", verdict, "%v", findings)
	}
	assert.NotContains(t, secondRaw, firstID, "a copy carries nothing of another copy's report")
	assert.Equal(t, first["checkpoint"], second["checkpoint"], "both copies are at the one checkpoint")
}

// resealed is a copy whose standalone report is sealed again over its payload
// as mutate leaves it, with key, and named by the pointer.
func resealed(t *testing.T, b map[string]any, key ed25519.PrivateKey, mutate func(input map[string]any)) map[string]any {
	t.Helper()
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	out, err := cli.DecodeBundleJSON(raw)
	require.NoError(t, err)
	x := out["extensions"].(map[string]any)[cli.DealReportExtension].(map[string]any)
	input := x["agent_input"].(map[string]any)
	mutate(input)
	payload, err := canonical.JCS(input)
	require.NoError(t, err)
	record, err := cli.Seal(cli.Request{Version: "capsule-seal-request/v1", Capsule: emit.Input{
		ActionID: cli.DealReportActionID, ActionType: emit.ActionTypeFYI, Operator: "deal", Developer: "capsulectl-deal",
		Timestamp: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	}, Payload: payload}, key)
	require.NoError(t, err)
	capsule, err := cli.DecodeBundleJSON(record.Capsule)
	require.NoError(t, err)
	capsule["signature"] = hex.EncodeToString(record.ProducerEnvelope)
	capsule["key_id"] = hex.EncodeToString(key.Public().(ed25519.PublicKey))
	generic, err := cli.DecodeBundleJSON(payload)
	require.NoError(t, err)
	x["record"], x["agent_input"] = capsule, generic
	out["extensions"].(map[string]any)[cli.DealProfile] = map[string]any{cli.DealReportPointer: record.CapsuleID}
	return out
}

// verify --bundle fails a standalone report that breaks any of its checks,
// and a copy stripped of it.
func TestAStandaloneReportIsCheckedWhole(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	b, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	var key ed25519.PrivateKey
	withSession(t, func(s *dealSession) { key = s.key })
	same := resealed(t, b, key, func(map[string]any) {})
	verdict, findings := verdictOf(t, same)
	require.Equal(t, "VALID", verdict, "resealed unchanged with the deal's key: %v", findings)

	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	edit := func(f func(b map[string]any)) map[string]any {
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		c, err := cli.DecodeBundleJSON(raw)
		require.NoError(t, err)
		f(c)
		return c
	}
	sectionOf := func(c map[string]any) map[string]any {
		return c["extensions"].(map[string]any)[cli.DealReportExtension].(map[string]any)
	}
	for name, c := range map[string]map[string]any{
		"1: a signature that does not verify": edit(func(c map[string]any) {
			r := sectionOf(c)["record"].(map[string]any)
			sig := r["signature"].(string)
			r["signature"] = sig[:len(sig)-2] + map[bool]string{true: "00", false: "01"}[!strings.HasSuffix(sig, "00")]
		}),
		"2: signed by another key": resealed(t, b, other, func(map[string]any) {}),
		"3: an edited report": edit(func(c map[string]any) {
			sectionOf(c)["agent_input"].(map[string]any)["report"].(map[string]any)["scope"] = "edited"
		}),
		"4: another deal":            resealed(t, b, key, func(in map[string]any) { in["deal_id"] = "deal-0000000000000000" }),
		"4: not a copy audience":     resealed(t, b, key, func(in map[string]any) { in["audience"] = cli.DealAudienceHistory }),
		"5: another checkpoint root": resealed(t, b, key, func(in map[string]any) { in["deal_checkpoint"].(map[string]any)["root"] = strings.Repeat("0", 64) }),
		"5: another checkpoint size": resealed(t, b, key, func(in map[string]any) { in["deal_checkpoint"].(map[string]any)["mmr_size"] = json.Number("1") }),
		"5: another log": resealed(t, b, key, func(in map[string]any) {
			in["deal_checkpoint"].(map[string]any)["log_id"] = "deal/deal-0000000000000000"
		}),
		"6: another member": edit(func(c map[string]any) { sectionOf(c)["extra"] = true }),
		"7: another as_of": resealed(t, b, key, func(in map[string]any) {
			in["report"].(map[string]any)["lifecycle"].(map[string]any)["as_of"] = "2020-01-01T00:00:00Z"
		}),
		"no pointer": edit(func(c map[string]any) { c["extensions"].(map[string]any)[cli.DealProfile] = map[string]any{} }),
		"stripped to inline text": edit(func(c map[string]any) {
			exts := c["extensions"].(map[string]any)
			exts[cli.DealProfile] = sectionOf(c)["agent_input"].(map[string]any)["report"]
			delete(exts, cli.DealReportExtension)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			verdict, findings := verdictOf(t, c)
			assert.Equal(t, "INVALID", verdict, "%v", findings)
		})
	}
}

// A copy's report is as of the deal's checkpoint, the deal's last step: a
// copy made earlier still verifies after later steps, and states the
// earlier time; a share neither cuts a checkpoint nor changes the time.
func TestAStandaloneReportIsAsOfItsCheckpoint(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-27T18:00:00Z")
	id := retailDeal(t)
	setDealClock(t, "2026-09-28T09:30:00Z")
	early, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	asOf := standaloneOf(t, early)["agent_input"].(map[string]any)["report"].(map[string]any)["lifecycle"].(map[string]any)["as_of"]
	assert.Equal(t, "2026-09-27T18:00:00Z", asOf, "the last step's time, not the copy's")
	assert.Contains(t, early["checkpoint"].(map[string]any)["timestamp"], "2026-09-27T18:00:00", "the checkpoint is the step's")

	setDealClock(t, "2026-09-29T12:00:00Z")
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Ships today", "source_kind": "merchant", "source": "listing_page"}`))
	later, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	assert.Equal(t, "2026-09-29T12:00:00Z", standaloneOf(t, later)["agent_input"].(map[string]any)["report"].(map[string]any)["lifecycle"].(map[string]any)["as_of"])
	verdict, findings := verdictOf(t, early)
	assert.Equal(t, "VALID", verdict, "an earlier copy still verifies: %v", findings)
}

// A crash between a step and its checkpoint is finished by the deal's next
// command, at the step's own time, before a tick could cut the step at the
// tick's minute; History refuses until then; recovering twice cuts once.
func TestACrashBeforeAStepsCutIsRecoveredAtTheStepsTime(t *testing.T) {
	p, _, _ := rc16Store(t)
	setDealClock(t, "2026-10-19T09:28:13Z")
	dealRun(t, "tick")
	id := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	before := deckCheckpoints(t, id)
	saved := dealStepCutStops
	dealStepCutStops = func() error { return errors.New("stopped before the cut") }
	setDealClock(t, "2026-10-19T09:29:07Z")
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Room confirmed", "source_kind": "merchant", "source": "listing_page"}`))
	dealStepCutStops = saved
	require.Error(t, err)
	assert.Equal(t, before, deckCheckpoints(t, id), "the step is on the book without its cut")

	_, err = invoke(t, "", "--profile", "deal", "deal", "history", "--deal", id, "--to", "a friend", "--out", filepath.Join(t.TempDir(), "h.json"))
	require.Error(t, err, "History writes nothing, so it refuses over the uncovered step")
	assert.Contains(t, err.Error(), "does not cover yet")

	setDealClock(t, "2026-10-19T09:31:00Z")
	dealRun(t, "tick")
	b, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the hotel")
	assert.Contains(t, b["checkpoint"].(map[string]any)["timestamp"], "2026-10-19T09:29:07", "recovered at the step's time, not the tick's minute")
	after := deckCheckpoints(t, id)
	dealRun(t, "report", "--deal", id)
	assert.Equal(t, after, deckCheckpoints(t, id), "a second recovery cuts nothing")
	_ = p
}

// After the signing key changes, a deal whose last step is under the earlier
// key is refused a copy, with a message, on every path; a legitimate later
// record under the new key lets it be copied again.
func TestAKeyChangeAfterTheLastStepRefusesACopy(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
	rotateSigningKey(t)
	path := filepath.Join(t.TempDir(), "shared.json")
	_, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", cli.DealAudienceCounterparty, "--to", "the shop", "--out", path)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "signed with an earlier key")
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "nothing written")

	dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t, `{"about": "the merchant's refund notice", "source": "agent"}`))
	b, err := cli.DecodeBundleJSON([]byte(dealOwnBundle(t, id)))
	require.NoError(t, err)
	verdict, findings := verdictOf(t, b)
	assert.Equal(t, "VALID", verdict, "late evidence under the new key: %v", findings)
}

// The share record lists the standalone report as revealed.
func TestTheShareRecordListsTheStandaloneReport(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	_, raw := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	b, err := cli.DecodeBundleJSON([]byte(raw))
	require.NoError(t, err)
	reportID := standaloneOf(t, b)["record"].(map[string]any)["capsule_id"].(string)
	record, err := cli.DisclosureRecord(b)
	require.NoError(t, err)
	assert.Contains(t, record["revealed"], reportID)
}

// The email of a copy whose report is standalone says, dated, where the deal
// stood when it was made, apart from the report, and points to the page.
func TestTheEmailOfAStandaloneReportCarriesAComposedNote(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-27T18:00:00Z")
	id := retailDeal(t)
	setDealClock(t, "2026-10-30T08:00:00Z")
	out := dealRun(t, "report", "--deal", id, "--email", filepath.Join(t.TempDir(), "r.eml"))
	text := out["email"].(map[string]any)["text"].(string)
	assert.Contains(t, text, "Made by capsulectl at 2026-10-30T08:00:00Z")
	assert.Contains(t, text, "as of 2026-09-27T18:00:00Z; open the attached page")
}

// deal respond answers a time floor from the deal's checkpoint, whose time is
// its last step's: a floor later than that cannot be met, since a copy never
// cuts a checkpoint, and is refused before a copy is made; an earlier one is
// served.
func TestARespondTimeFloorPastTheLastStepIsRefused(t *testing.T) {
	setDealClock(t, "2026-09-27T18:00:00Z")
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	seller := thisDealParty(t)
	setDealClock(t, "2026-09-28T09:00:00Z")
	late := askFor(t, seller, buyer, id, map[string]any{"min_freshness": map[string]any{"time": "2026-09-28T08:00:00Z"}})
	assert.Equal(t, "refusal", late.outcome, "later than the buyer's last step")
	early := askFor(t, seller, buyer, id, map[string]any{"min_freshness": map[string]any{"time": "2026-09-27T17:00:00Z"}})
	assert.Equal(t, "artifact", early.outcome)
}

// crashBeforeTheCut seals a claim on the deal and stops before its cut, as a
// crash there would.
func crashBeforeTheCut(t *testing.T, id string) {
	t.Helper()
	saved := dealStepCutStops
	dealStepCutStops = func() error { return errors.New("stopped before the cut") }
	defer func() { dealStepCutStops = saved }()
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Room confirmed", "source_kind": "merchant", "source": "listing_page"}`))
	require.Error(t, err)
}

// Every writer recovers a cut a crash left out, before it reads or extends
// the deal: a share with no tick between is at the step's time; the next
// step first covers the earlier one; a recovery that fails aborts the
// command, which then writes nothing.
func TestAWriterRecoversACutBeforeItUsesTheDeal(t *testing.T) {
	t.Run("a share, with no tick between", func(t *testing.T) {
		dealFixture(t)
		setDealClock(t, "2026-10-19T09:28:13Z")
		id := retailDeal(t)
		setDealClock(t, "2026-10-19T09:29:07Z")
		crashBeforeTheCut(t, id)
		setDealClock(t, "2026-10-19T10:00:00Z")
		b, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
		assert.Contains(t, b["checkpoint"].(map[string]any)["timestamp"], "2026-10-19T09:29:07")
		verdict, findings := verdictOf(t, b)
		assert.Equal(t, "VALID", verdict, "%v", findings)
	})
	t.Run("the next step", func(t *testing.T) {
		dealFixture(t)
		setDealClock(t, "2026-10-19T09:28:13Z")
		id := retailDeal(t)
		before := deckCheckpoints(t, id)
		setDealClock(t, "2026-10-19T09:29:07Z")
		crashBeforeTheCut(t, id)
		setDealClock(t, "2026-10-19T09:40:00Z")
		dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Late checkout", "source_kind": "merchant", "source": "listing_page"}`))
		assert.Equal(t, before+2, deckCheckpoints(t, id), "the crashed step's cut, then the new step's")
	})
	t.Run("a recovery that fails", func(t *testing.T) {
		dealFixture(t)
		id := retailDeal(t)
		crashBeforeTheCut(t, id)
		saved := dealCoverStops
		dealCoverStops = func() error { return errors.New("recovery failed") }
		defer func() { dealCoverStops = saved }()
		path := filepath.Join(t.TempDir(), "shared.json")
		_, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", id, "--share", cli.DealAudienceCounterparty, "--to", "the shop", "--out", path)
		require.Error(t, err)
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "no copy over an uncovered step")
	})
}

// A copy whose standalone report does not verify is refused for
// countersigning, as verify --bundle refuses it.
func TestCountersignRefusesAnEditedStandaloneReport(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	path := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", id, "--bundle", path)
	b, err := cli.DecodeBundleJSON(mustRead(t, path))
	require.NoError(t, err)
	b["extensions"].(map[string]any)[cli.DealReportExtension].(map[string]any)["agent_input"].(map[string]any)["report"].(map[string]any)["scope"] = "edited"
	edited := writeBundle(t, b)
	out, err := invoke(t, "", "--profile", "deal", "deal", "report", "--deal", id, "--from-bundle", edited)
	require.ErrorIs(t, err, cli.ErrInput, out)
	assert.Contains(t, err.Error(), "its report does not verify")
}

// An email made from an earlier bundle shows that report's own lifecycle,
// as of its checkpoint, not one rebuilt from the deal's later steps: where
// the deal stands now is the composed note's.
func TestAnEmailFromAnEarlierBundleShowsItsReportsLifecycle(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-27T18:00:00Z")
	id := retailDeal(t)
	path := filepath.Join(t.TempDir(), "own.json")
	dealRun(t, "report", "--deal", id, "--bundle", path)
	setDealClock(t, "2026-09-28T10:00:00Z")
	dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received"}`))
	out := dealRun(t, "report", "--deal", id, "--from-bundle", path, "--email", filepath.Join(t.TempDir(), "r.eml"))
	text := out["email"].(map[string]any)["text"].(string)
	stands := text[strings.Index(text, "Where this deal stands:"):]
	assert.True(t, strings.HasPrefix(stands, "Where this deal stands:\nOpen"), "the report's own state: %.120s", stands)
	assert.Contains(t, text, "when the deal stood so: Closed", "now is the composed note's")
}

// verify --bundle says when a standalone report's state is from, and what
// backs it: the deal's last step's time, and whether a witness saw it.
func TestVerifySaysWhenAStandaloneReportIsAsOf(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-27T18:00:00Z")
	id := retailDeal(t)
	b, _ := sharedCopy(t, id, cli.DealAudienceCounterparty, "the shop")
	result, _ := verifyBundleOutput(t, writeBundle(t, b))
	for _, raw := range result["extensions"].([]any) {
		if e := raw.(map[string]any); e["kind"] == cli.DealProfile {
			assert.Equal(t, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); not witnessed", e["as_of"])
			return
		}
	}
	t.Fatal("no x-deal-v0 entry")
}

// deal respond, for a deal whose reports are off its log: a pin past the
// log, or a size floor one past what it holds, is refused; and after the
// signing key changes, so is any copy.
func TestRespondRefusesWhatAStandaloneReportCannotMeet(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, crossPartyBuyerOpen))["deal_id"].(string)
	buyer := thisDealParty(t)
	dealFixture(t)
	dealRun(t, "open", "--records", "typed", "--deal-id", id, "--input", writeJSON(t, sellerTyped))
	seller := thisDealParty(t)
	first, pin := pinnedCopy(t, seller, buyer, id)
	require.Equal(t, "artifact", first.outcome)
	party(t, buyer.config)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Ships today", "source_kind": "merchant", "source": "listing_page"}`))
	assert.Equal(t, "refusal", askFor(t, seller, buyer, id, map[string]any{"expected_pin": pin}).outcome, "the log has moved past the pin")
	size := float64(len(bookRecords(t, id)) + 1)
	party(t, buyer.config)
	assert.Equal(t, "refusal", askFor(t, seller, buyer, id, map[string]any{"min_freshness": map[string]any{"size": size}}).outcome, "a copy adds nothing to the log")
	party(t, buyer.config)
	buyer.signer = rotateSigningKey(t)
	assert.Equal(t, "refusal", askFor(t, seller, buyer, id, map[string]any{"min_freshness": map[string]any{"size": 1}}).outcome, "the last step is under the earlier key")
}
