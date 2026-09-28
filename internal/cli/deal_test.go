package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const jetSkiDemo = "../../skills/deal/demo/jet-ski"
const bookingFixture = "testdata/deal/booking"

// dealFixture creates a fresh deal profile named "deal" in a temp config dir.
func dealFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(dealCheckURLEnv, "")
	fixed := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	old := dealClock
	dealClock = func() time.Time { return fixed }
	t.Cleanup(func() { dealClock = old })
	dir := filepath.Join(t.TempDir(), "deal")
	out, err := invoke(t, "", "deal", "init", "--profile", "deal", "--dir", dir)
	require.NoError(t, err, out)
	return dir
}

func dealRun(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal"}, args...)...)
	require.NoError(t, err, out)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m
}

func writeJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func openJetSki(t *testing.T) string {
	t.Helper()
	opened := dealRun(t, "open", "--input", filepath.Join(jetSkiDemo, "01-open.json"))
	dealID := opened["deal_id"].(string)
	assert.Equal(t, true, opened["demo"])
	assert.Equal(t, map[string]any{"state": "signed", "checkpoint": float64(1), "witness": "not_configured"}, opened["checkpoint"])
	assert.Equal(t, "not_configured", opened["remote_warm"])
	for _, step := range []struct{ kind, file string }{
		{"message", "02-message-quote.json"}, {"message", "03-message-switch.json"},
		{"change", "04-change.json"}, {"evidence", "05-evidence-domain.json"},
	} {
		dealRun(t, "note", "--deal", dealID, "--kind", step.kind, "--input", filepath.Join(jetSkiDemo, step.file))
	}
	return dealID
}

func TestDealInitProtectsSeeds(t *testing.T) {
	dir := dealFixture(t)
	for _, name := range []string{"signing.seed", "checkpoint.seed", "deal.db"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
	_, err := invoke(t, "", "deal", "init", "--profile", "deal", "--dir", dir)
	require.Error(t, err)
}

func TestDealJetSkiPayeeSwitch(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	want, err := os.ReadFile(filepath.Join(jetSkiDemo, "expected-card.txt"))
	require.NoError(t, err)

	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	assert.Equal(t, strings.TrimSpace(string(want)), check["card"])
	assert.Equal(t, "pause", check["verdict"])
	assert.Equal(t, false, check["proceed"])
	var ids []string
	for _, o := range check["options"].([]any) {
		ids = append(ids, o.(map[string]any)["id"].(string))
	}
	assert.Equal(t, []string{"hold", "verify_contact", "proceed"}, ids)

	// The user holds; the answer is sealed and does not authorize paying.
	checkID := check["check_id"].(string)
	answer := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", checkID, "--choice", "hold", "--said", "hold on")
	assert.Equal(t, false, answer["proceed"])
	assert.Equal(t, "signed", answer["checkpoint"].(map[string]any)["state"])

	// An agent that pays anyway is recorded, and the skip is visible.
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "the check paused and the answer was hold", act["reason"])

	report := dealRun(t, "report", "--deal", dealID)
	assert.Contains(t, reportTexts(t, report, "anomalies"), "agent/unsealed_approval: Went ahead without your approval: pay $200.00 to M. Torres by Zelle (the check paused and the answer was hold)")
	assert.Contains(t, report["trail"], "⚠️ SKIPPED CHECK: pay")
	assert.Contains(t, report["trail"], "your answer: hold")

	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"pending"}`))
	assert.Equal(t, "open", closed["outcome"])
	assert.Equal(t, float64(1), closed["unchecked_actions"])
}

func TestDealAskedVsDidBooking(t *testing.T) {
	dealFixture(t)
	opened := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))
	dealID := opened["deal_id"].(string)

	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit.json"))
	assert.Equal(t, "⚠️ Not what you asked: check_out 2026-10-05 → 2026-10-07 · Over your limit of $400.00 ($760.00) · unverified: free cancellation until October 1 · [Hold] [Confirm anyway]", check["card"])

	cancel := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-cancel.json"))
	assert.Equal(t, "⚠️ You didn't ask for this: cancelling · unverified: free cancellation until October 1 · [Hold] [Cancel anyway]", cancel["card"])

	// What was asked passes quietly and authorizes the act.
	ok := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	assert.Equal(t, "pass", ok["verdict"])
	assert.Equal(t, true, ok["proceed"])
	assert.Equal(t, "", ok["card"])
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"commit","reference":"CONF-1"}`))
	assert.Equal(t, false, act["unchecked"])
	assert.Equal(t, ok["approval_id"], act["authorized_by"], "a passing check is approved by standing intent, sealed as its own step")

	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"when":"2026-10-03","price_minor":41000}}`))
	assert.Equal(t, "mismatch", closed["outcome"])
	assert.Equal(t, "Price: agreed $380.00, delivered $410.00", closed["differences"].([]any)[0].(map[string]any)["text"])
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	require.Error(t, err, "a closed deal takes no more steps")
}

func TestDealSkippedCheckIsVisible(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "no check before this action", act["reason"])
	report := dealRun(t, "report", "--deal", dealID)
	assert.Contains(t, reportTexts(t, report, "anomalies"), "agent/skipped_check: Skipped the check: pay $380.00 (no check before this action)")
	assert.Contains(t, report["trail"], "2. 2026-09-27T18:00:00Z ⚠️ SKIPPED CHECK: pay done without a passing check or your approval (no check before this action)")
	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"when":"2026-10-03"}}`))
	assert.Equal(t, "completed", closed["outcome"])
	assert.Equal(t, float64(1), closed["unchecked_actions"])
}

func TestDealApprovalGoesStaleAfterChange(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "change", "--input", writeJSON(t, `{"source":"seller message","terms":{"deposit_minor":30000}}`))
	answer := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed")
	assert.Equal(t, false, answer["proceed"])
	assert.Equal(t, "details changed after this check; check again", answer["reason"])

	// A current proceed authorizes the act; paying a different payee does not.
	again := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	assert.Contains(t, again["card"], "Deposit changed since it was agreed ($200.00 → $300.00)")
	yes := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", again["check_id"].(string), "--choice", "proceed", "--said", "pay it")
	assert.Equal(t, true, yes["proceed"])
	wrong := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","payee":"Someone Else"}`))
	assert.Equal(t, "the payee differs from the one checked", wrong["reason"])
	right := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","payee":"M. Torres","amount_minor":20000,"rail":"zelle"}`))
	assert.Equal(t, false, right["unchecked"])
	assert.Equal(t, yes["capsule_id"], right["authorized_by"])

	// Answers must name a real option of a real check.
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "approval", "--check", again["check_id"].(string), "--choice", "whatever")
	require.Error(t, err)
	_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "approval", "--check", again["snapshot_id"].(string), "--choice", "proceed")
	require.Error(t, err)
}

func TestDealRejectsUnknownActionAndBadInput(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"cancel"}`))
	require.Error(t, err, "cancel is not a point of no return for a rental")
	_, err = invoke(t, "", "--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","unchecked":false,"authorized_by":"x"}`))
	require.Error(t, err, "authorization cannot be supplied by the caller")
	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, `{"type":"rental","intent":{"verbatim":""},"who":{"name":"x"},"terms":{},"recourse":{"rail":"card","refundable":true}}`))
	require.Error(t, err, "the user's verbatim words are required")
	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--input", writeJSON(t, `{"type":"rental","intent":{"verbatim":"x"},"who":{"name":"x"},"terms":{},"claims":[{"text":"has skis","source":""}],"recourse":{"rail":"card","refundable":true}}`))
	require.Error(t, err, "every claim needs its source")
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", "deal-0000000000000000")
	require.Error(t, err)
}

func TestDealDetectsIndexTampering(t *testing.T) {
	dir := dealFixture(t)
	dealID := openJetSki(t)
	var p Profile
	p.Connection.Database = filepath.Join(dir, "deal.db")
	db, _, err := sqliteConnection(p)
	require.NoError(t, err)
	// Dropping a step (here: the payee change) breaks the prev chain.
	_, err = db.Exec(`DELETE FROM deal_steps WHERE deal_id=? AND kind='change'`, dealID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE deal_steps SET n=n-1 WHERE deal_id=? AND n>4`, dealID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID)
	require.ErrorIs(t, err, ErrConflict)
}

func TestDealRemoteChecker(t *testing.T) {
	dealFixture(t)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(b))
		if r.URL.Path == "/v1/check" {
			_, _ = w.Write([]byte(`{"verdict":"pause","differences":[{"rule":"extra","text":"Second reader: deposit is unusually high"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv(dealCheckURLEnv, srv.URL)
	opened := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))
	assert.Equal(t, "sent", opened["remote_warm"])
	dealID := opened["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	assert.Equal(t, "used", check["remote"])
	assert.Equal(t, "pause", check["verdict"], "a remote pause is added to a local pass")
	assert.Contains(t, check["card"], "Second reader: deposit is unusually high")
	require.Len(t, bodies, 2)
	assert.Contains(t, bodies[1], `"action":"commit"`)
	for _, private := range []string{"Tokyo", "Example Hotel", "free cancellation", "Book me"} {
		assert.NotContains(t, bodies[1], private, "only minimal fields leave the machine")
	}

	// Unreachable: the local rules decide alone.
	srv.Close()
	local := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	assert.Equal(t, "unavailable", local["remote"])
	assert.Equal(t, "pass", local["verdict"])

	t.Setenv(dealCheckURLEnv, "http://checker.example")
	_, err := invoke(t, "", "--profile", "deal", "deal", "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	require.Error(t, err, "a non-loopback checker must use https")
}

func TestDealNeedsSQLiteProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Name = "deal"
	require.NoError(t, saveProfile(p, false))
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--input", filepath.Join(bookingFixture, "open.json"))
	require.ErrorIs(t, err, ErrInput)
}

func TestDealConcurrentWritersChainInOrder(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	msg := writeJSON(t, `{"from":"counterparty","text":"still available"}`)
	const writers = 8
	errs := make(chan error, writers)
	for range writers {
		go func() {
			c := NewCommand()
			c.SetOut(io.Discard)
			c.SetArgs([]string{"--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "message", "--input", msg})
			errs <- c.ExecuteContext(t.Context())
		}()
	}
	for range writers {
		require.NoError(t, <-errs)
	}
	report := dealRun(t, "report", "--deal", dealID)
	assert.Len(t, strings.Split(report["trail"].(string), "\n"), writers+1)
}

// B1: "show me options, don't book" is an empty allowed list, and booking
// anyway (non-refundable) must pause.
func TestDealOptionsOnlyIntentPausesABooking(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", writeJSON(t, `{"type":"booking","channel":"web",
		"intent":{"verbatim":"find me some hotel options in Tokyo for October 3, dont book yet","allowed":[]},
		"who":{"name":"Example Hotel Shinjuku","domain":"hotel.example"},
		"terms":{"item":"double room","price_minor":38000,"currency":"USD","when":"2026-10-03"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"commit","recourse":{"refundable":false}}`))
	assert.Equal(t, "pause", check["verdict"])
	assert.Equal(t, false, check["proceed"])
	assert.Contains(t, check["card"], "You didn't ask for this: confirming a commitment")
	assert.Contains(t, check["card"], "No longer refundable")
	assert.Nil(t, check["approval_id"], "nothing is approved by standing intent")
}

// B1: a change of refundability from what was agreed is a difference.
func TestDealRefundabilityChangePauses(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"commit","terms":{"when":"2026-10-03","conditions":{"check_out":"2026-10-05"},"price_minor":38000},"recourse":{"refundable":false}}`))
	assert.Equal(t, "pause", check["verdict"])
	assert.Equal(t, "⚠️ No longer refundable (agreed as refundable) · unverified: free cancellation until October 1 · [Hold] [Confirm anyway]", check["card"])
}

// B2: a payee change carried in a counterparty message, after the check,
// makes the user's approval of that check stale; the act must be refused
// and a new check shows the change.
func TestDealPayeeChangeInAMessageMakesTheApprovalStale(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(jetSkiDemo, "01-open.json"))["deal_id"].(string)
	check := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"recourse":{"rail":"zelle"}}`))
	require.Equal(t, "pause", check["verdict"])
	require.NotContains(t, check["card"], "Payee changed")
	dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"send it to M. Torres","who":{"payee":"M. Torres"}}`))
	answer := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "ok pay")
	assert.Equal(t, false, answer["proceed"], "the answer is to a check that no longer shows who is being paid")
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "details changed after the last check", act["reason"])
	again := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"recourse":{"rail":"zelle"}}`))
	assert.Contains(t, again["card"], "Payee changed since first contact (Coastal Jet Rentals LLC → M. Torres, Zelle)")
}

// B2: the same after a passing check approved by standing intent, and for
// evidence carrying who.
func TestDealIdentityEvidenceAfterAPassingCheckRefusesTheAct(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	ok := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	require.Equal(t, "pass", ok["verdict"])
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--input", writeJSON(t, `{"about":"booking site","source":"domain_lookup","who":{"domain":"hotel-bookings-secure.example"}}`))
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"commit"}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "details changed after the last check", act["reason"])
}

// B3: deleting the LAST index row (here a skipped check) must not make the
// step disappear: the deal's log still has it, so report and close refuse.
func TestDealDroppedLastStepIsAConflict(t *testing.T) {
	dir := dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":38000}`))
	require.Equal(t, true, act["unchecked"])
	var p Profile
	p.Connection.Database = filepath.Join(dir, "deal.db")
	db, _, err := sqliteConnection(p)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM deal_steps WHERE deal_id=? AND n=2`, dealID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID)
	require.ErrorIs(t, err, ErrConflict)
	_, err = invoke(t, "", "--profile", "deal", "deal", "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received"}`))
	require.ErrorIs(t, err, ErrConflict)
}

// B3: a step whose index row was written but whose Capsule never reached the
// log (a crash in between) is recovered on the next read, not lost.
func TestDealRecoversAStepThatNeverReachedTheLog(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	events, err := s.load(t.Context(), dealID)
	require.NoError(t, err)
	act := &dealAct{Action: "pay", AmountMinor: ptr(int64(38000))}
	act.AuthorizedBy, act.Reason, act.Rule = authorizeAct(events, *act)
	act.Unchecked = true
	_, _, err = s.prepareStep(t.Context(), dealID, events, dealEvent{Kind: "act", Act: act})
	require.NoError(t, err)
	require.NoError(t, s.close())

	report := dealRun(t, "report", "--deal", dealID)
	assert.Contains(t, reportTexts(t, report, "anomalies"), "agent/skipped_check: Skipped the check: pay $380.00 (no check before this action)")
}

func ptr[T any](v T) *T { return &v }

// SF1: accepting an offer is a point of no return for a purchase or rental.
func TestDealCommitIsCheckedForPurchasesAndRentals(t *testing.T) {
	dealFixture(t)
	for _, dealType := range []string{"purchase", "rental"} {
		opened := dealRun(t, "open", "--input", writeJSON(t, `{"type":"`+dealType+`","intent":{"verbatim":"buy the bike if it is still $300"},
			"who":{"name":"A Seller"},"terms":{"item":"bike","price_minor":30000,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`))
		assert.Contains(t, opened["points_of_no_return"], "commit", dealType)
		check := dealRun(t, "check", "--deal", opened["deal_id"].(string), "--input", writeJSON(t, `{"action":"commit","terms":{"price_minor":30000}}`))
		assert.Equal(t, "pass", check["verdict"], dealType)
	}
}

// The user answers "pay anyway", then the counterparty changes the terms
// again before the agent pays: the approval no longer covers the payment,
// a second "pay anyway" on the same check is refused, and only a new check,
// which shows the new change, can be approved.
func TestDealPayAnywayThenTermsChangeAgain(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	pay := filepath.Join(jetSkiDemo, "06-check-pay.json")
	check := dealRun(t, "check", "--deal", dealID, "--input", pay)
	require.Equal(t, "pause", check["verdict"])
	yes := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "pay anyway")
	require.Equal(t, true, yes["proceed"])

	dealRun(t, "note", "--deal", dealID, "--kind", "change", "--input", filepath.Join("testdata/deal/stale-approval", "change-again.json"))

	act := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":20000,"payee":"M. Torres","rail":"zelle"}`))
	assert.Equal(t, true, act["unchecked"])
	assert.Equal(t, "details changed after the last check", act["reason"])
	again := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "proceed", "--said", "pay anyway")
	assert.Equal(t, false, again["proceed"])
	assert.NotEmpty(t, again["reason"])

	recheck := dealRun(t, "check", "--deal", dealID, "--input", writeJSON(t, `{"action":"pay","amount_minor":30000,"who":{"payee":"M. Torres"},"recourse":{"rail":"zelle","refundable":false}}`))
	assert.Equal(t, "pause", recheck["verdict"])
	assert.Contains(t, recheck["card"], "Deposit changed since it was agreed ($200.00 → $300.00)")
	ok := dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", recheck["check_id"].(string), "--choice", "proceed", "--said", "pay the new deposit")
	require.Equal(t, true, ok["proceed"])
	paid := dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", writeJSON(t, `{"action":"pay","amount_minor":30000,"payee":"M. Torres","rail":"zelle"}`))
	assert.Equal(t, false, paid["unchecked"])
	assert.Equal(t, ok["capsule_id"], paid["authorized_by"])
}

// The demo script compares the card with expected-card.txt ignoring trailing
// whitespace: a file saved with an extra newline, trailing spaces or CRLF
// line endings is the same card.
func TestDealDemoScriptIgnoresTrailingWhitespace(t *testing.T) {
	if testing.Short() {
		t.Skip("builds capsulectl and runs the demo script")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	work := t.TempDir()
	bin := filepath.Join(work, "capsulectl")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/capsulectl")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	skill := filepath.Join(work, "deal")
	for _, dir := range []string{"scripts", "demo/jet-ski", "profile"} {
		require.NoError(t, os.CopyFS(filepath.Join(skill, dir), os.DirFS(filepath.Join("../../skills/deal", dir))))
	}
	want, err := os.ReadFile(filepath.Join(jetSkiDemo, "expected-card.txt"))
	require.NoError(t, err)
	padded := strings.TrimRight(string(want), "\n") + "  \r\n\n"
	require.NoError(t, os.WriteFile(filepath.Join(skill, "demo/jet-ski/expected-card.txt"), []byte(padded), 0o600))

	demo := func() (string, error) {
		run := exec.Command("bash", filepath.Join(skill, "scripts/run-demo.sh"))
		run.Env = append(os.Environ(), "CAPSULECTL="+bin)
		out, err := run.CombinedOutput()
		return string(out), err
	}
	got, err := demo()
	require.NoError(t, err, got)
	assert.Contains(t, got, strings.TrimSpace(string(want)))

	// A card that differs in anything but trailing whitespace still fails.
	other := strings.Replace(padded, "3 weeks ago", "4 weeks ago", 1)
	require.NoError(t, os.WriteFile(filepath.Join(skill, "demo/jet-ski/expected-card.txt"), []byte(other), 0o600))
	got, err = demo()
	require.Error(t, err, got)
	assert.Contains(t, got, "unexpected card")
}
