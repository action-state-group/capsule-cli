package plugin

import (
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sale closes once (capsule-cli#271): no buyer's thread opens on it
// afterwards, a thread already open goes on, and its copy still writes,
// verifies and says so. `deal list` shows it closed.
func TestASaleCloses(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	a := buyerThread(t, saleID, "buyer-a.example")
	closed := dealRun(t, "sale", "close", "--sale", saleID)
	assert.Equal(t, saleID, closed["sale_id"])
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close"}, saleRecordTypes(exportedSale(t, saleID)))

	_, err := invoke(t, "", "--profile", "deal", "deal", "sale", "close", "--sale", saleID)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "already closed")
	_, err = invoke(t, "", "--profile", "deal", "deal", "open", "--sale", saleID, "--input", writeJSON(t,
		`{"channel": "marketplace", "who": {"name": "Another Buyer", "domain": "buyer-b.example"}, "terms": {"price_minor": 190000}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "is closed: no buyer's thread opens on it")

	// The open thread goes on: an offer, then its close.
	dealRun(t, "check", "--deal", a, "--input", writeJSON(t, offerInput))
	dealRun(t, "close", "--deal", a, "--input", writeJSON(t, received))

	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.Equal(t, true, res["closed"])
	verdict, findings := verifySale(t, bundle)
	assert.Equal(t, "VALID", verdict, "core verify --bundle reads a closed sale: %v", findings)
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close", "sale_cut"}, saleRecordTypes(exportedSale(t, saleID)), "only a cut follows the close")
	checkSaleProfile(t, saleID)

	out, err := invoke(t, "", "--profile", "deal", "deal", "list", "--json")
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	states := map[string]any{}
	for _, l := range list {
		states[l["deal_id"].(string)] = l["state"]
	}
	assert.Equal(t, "closed", states[saleID])
	assert.Equal(t, "closed", states[a])
}

// The close first seals the opening a crash left out, so nothing but a cut
// ever follows it: interrupted open, the thread closed, then sale close,
// then a bundle; the log passes the profile checker.
func TestASaleCloseSealsAnOpeningACrashLeftOut(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "authority")
	require.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread"}, saleRecordTypes(exportedSale(t, saleID)))
	dealRun(t, "close", "--deal", registeredThreads(t, saleID)[0], "--input", writeJSON(t, received))
	dealRun(t, "sale", "close", "--sale", saleID)
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close"}, saleRecordTypes(exportedSale(t, saleID)))
	_, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close", "sale_cut"}, saleRecordTypes(exportedSale(t, saleID)))
	checkSaleProfile(t, saleID)
}

// The refusal comes before a registration is reused: an open stopped
// after its key row leaves a registration whose thread never opened, and
// after the close a retry is refused, seals nothing, and leaves that
// registration never opened.
func TestAClosedSaleRefusesToReuseARegistration(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "keyed")
	dealRun(t, "sale", "close", "--sale", saleID)
	before := saleRecordTypes(exportedSale(t, saleID))
	require.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "sale_close"}, before)
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--sale", saleID, "--input", writeJSON(t,
		`{"channel": "marketplace", "who": {"name": "Example Buyer", "domain": "buyer-a.example"}, "terms": {"price_minor": 190000}}`))
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "is closed: no buyer's thread opens on it")
	assert.Equal(t, before, saleRecordTypes(exportedSale(t, saleID)), "nothing sealed")
	bundle, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	entries, _ := saleSection(bundle)
	require.Len(t, entries, 1)
	assert.Equal(t, cli.SaleThreadNeverOpened, entries[0].(map[string]any)["member"])
}

// registeredThreads are the thread ids a sale registered, from this
// device's store.
func registeredThreads(t *testing.T, saleID string) []string {
	t.Helper()
	var ids []string
	withSession(t, func(s *dealSession) {
		steps, err := s.index.steps(t.Context(), stepFilter{DealID: saleID, Kinds: []string{"thread"}})
		require.NoError(t, err)
		for _, st := range steps {
			var ev dealEvent
			require.NoError(t, json.Unmarshal([]byte(st.Local), &ev))
			ids = append(ids, ev.Thread.ThreadID)
		}
	})
	require.NotEmpty(t, ids)
	return ids
}

// checkSaleProfile runs the profile checker on a sale's exported log.
func checkSaleProfile(t *testing.T, saleID string) {
	t.Helper()
	_, exported := exportSale(t, saleID)
	if python := profilePython(t); python != "" {
		out, err := exec.Command(python, dealProfileDir+"/check_profile.py", exported).CombinedOutput()
		require.NoError(t, err, "the profile checker takes the sale's log: %s", out)
	}
}

// A closed sale leaves the tick once a witnessed tick covers its log; an
// open one never does. A receipt for a tick that held the sale before a
// later append does not take it out, and a bundle after it has left, which
// seals a late report on its closed thread and a cut on the sale, brings
// both back until the next witnessed tick. Each receipt is reported in a fresh session
// (deliver), so settleEnded settles the sale.
func TestAClosedSaleLeavesTheTick(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "book", true: "legacy"}[legacy], func(t *testing.T) {
			p, _, witnessKey := rc16Store(t)
			if legacy {
				legacyDeals(t)
			}
			at := time.Date(2026, 10, 19, 9, 28, 0, 0, time.UTC)
			tick := func() uint64 {
				at = at.Add(2 * time.Minute)
				cli.DealClock = func() time.Time { return at }
				out := dealRun(t, "tick")
				require.Equal(t, "ticked", out["state"])
				return uint64(out["checkpoint"].(float64))
			}
			cli.DealClock = func() time.Time { return at }
			dealRun(t, "tick")
			saleID, _ := newSale(t)
			log := cli.DealLogID(saleID)
			thread := buyerThread(t, saleID, "buyer-a.example")
			dealRun(t, "close", "--deal", thread, "--input", writeJSON(t, received))
			deliver(t, p, tick(), witnessKey)
			require.Contains(t, liveLogs(t, p), log, "an open sale stays in the tick")

			// A tick cut while the sale was open, whose receipt comes after
			// the close: it covers less than the closed sale holds.
			beforeClose := tick()
			dealRun(t, "sale", "close", "--sale", saleID)
			deliver(t, p, beforeClose, witnessKey)
			assert.Contains(t, liveLogs(t, p), log, "a receipt for a tick cut before the close")
			assert.Contains(t, liveLogs(t, p), log, "closed, not yet witnessed")
			pending := tick()
			assert.Contains(t, liveLogs(t, p), log, "a pending tick keeps it")

			// The pending tick holds the closed sale; a bundle then appends
			// a cut to it (and a report to its thread) before that tick's
			// receipt comes: the receipt covers less than the sale holds,
			// so it does not take the sale out.
			saleBundle(t, saleID)
			deliver(t, p, pending, witnessKey)
			assert.Contains(t, liveLogs(t, p), log, "a receipt for a tick cut before the append")
			assert.Contains(t, liveLogs(t, p), cli.DealLogID(thread))
			deliver(t, p, tick(), witnessKey)
			assert.NotContains(t, liveLogs(t, p), log, "closed and witnessed whole: it leaves")
			assert.NotContains(t, liveLogs(t, p), cli.DealLogID(thread), "its closed thread left too")

			// A later bundle brings both back until the next witnessed tick.
			_, res := saleBundle(t, saleID)
			assert.Equal(t, "VALID", res["verdict"])
			require.Contains(t, liveLogs(t, p), log, "an append brings it back")
			require.Contains(t, liveLogs(t, p), cli.DealLogID(thread), "the late report brings the thread back")
			deliver(t, p, tick(), witnessKey)
			assert.NotContains(t, liveLogs(t, p), log)
			assert.NotContains(t, liveLogs(t, p), cli.DealLogID(thread))
		})
	}
}
