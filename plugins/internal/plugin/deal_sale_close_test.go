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
	_, exported := exportSale(t, saleID)
	if python := profilePython(t); python != "" {
		out, err := exec.Command(python, dealProfileDir+"/check_profile.py", exported).CombinedOutput()
		require.NoError(t, err, "the profile checker takes a closed sale's log: %s", out)
	}

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
// ever follows it: interrupted open, then sale close, then a bundle.
func TestASaleCloseSealsAnOpeningACrashLeftOut(t *testing.T) {
	dealFixture(t)
	saleID, _ := newSale(t)
	openThreadStoppingAt(t, saleID, "authority")
	require.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread"}, saleRecordTypes(exportedSale(t, saleID)))
	dealRun(t, "sale", "close", "--sale", saleID)
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close"}, saleRecordTypes(exportedSale(t, saleID)))
	_, res := saleBundle(t, saleID)
	assert.Equal(t, "VALID", res["verdict"])
	assert.Equal(t, []string{"sale", cli.TypeTaskAuthority, "thread", "thread_opened", "sale_close", "sale_cut"}, saleRecordTypes(exportedSale(t, saleID)))
}

// A closed sale leaves the tick once a witnessed tick covers its log; an
// open one never does. A receipt for a tick cut before a later append
// does not take it out, and a bundle after it has left, which seals a late
// report on its closed thread and a cut on the sale, brings both back until
// the next witnessed tick. Each receipt is reported in a fresh session
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

			dealRun(t, "sale", "close", "--sale", saleID)
			assert.Contains(t, liveLogs(t, p), log, "closed, not yet witnessed")
			pending := tick()
			assert.Contains(t, liveLogs(t, p), log, "a pending tick keeps it")
			deliver(t, p, pending, witnessKey)
			assert.NotContains(t, liveLogs(t, p), log, "closed and witnessed: it leaves")
			assert.NotContains(t, liveLogs(t, p), cli.DealLogID(thread), "its closed thread left too")

			// A bundle appends a cut: the sale is back until a later
			// witnessed tick covers it, and a receipt for an older tick
			// does not take it out.
			older := tick()
			saleBundle(t, saleID)
			require.Contains(t, liveLogs(t, p), log, "an append brings it back")
			require.Contains(t, liveLogs(t, p), cli.DealLogID(thread), "the late report brings the thread back")
			deliver(t, p, older, witnessKey)
			assert.Contains(t, liveLogs(t, p), log, "a receipt for a tick cut before the append")
			deliver(t, p, tick(), witnessKey)
			assert.NotContains(t, liveLogs(t, p), log)
			assert.NotContains(t, liveLogs(t, p), cli.DealLogID(thread))
		})
	}
}
