package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A seller owes the buyer a delivery by a date: a due date, recorded, listed,
// emitted for the calendar and carried at a close, like a cancel-by date.
func TestASellersDeliveryIsADueDate(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-28T10:00:00Z")
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)

	sealed := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--input", writeJSON(t, `{
		"about": "the sale", "source": "agent",
		"obligation": {"kind": "deliver_by", "due_by": "2026-10-03"}}`))
	deadline := sealed["deadline"].(map[string]any)
	assert.Equal(t, "2026-10-03", deadline["due_by"])
	assert.Nil(t, deadline["cancel_by"])
	assert.Equal(t, "delivery is due by 2026-10-03", deadline["text"])

	ics := filepath.Join(t.TempDir(), "deadlines.ics")
	list := dealRun(t, "deadlines", "--deal", id, "--ics", ics)
	ds := list["deadlines"].([]any)
	require.Len(t, ds, 1)
	d := ds[0].(map[string]any)
	assert.Equal(t, "2026-10-03", d["due_by"])
	assert.Equal(t, "open", d["status"])
	assert.Equal(t, float64(5), d["days_left"])
	assert.Equal(t, "It is due by 2026-10-03; nothing resolving it is sealed on this deal yet.", d["holds"])
	cal, err := os.ReadFile(ics)
	require.NoError(t, err)
	assert.Contains(t, string(cal), "SUMMARY:Due ("+id+"): delivery is due by 2026-10-03")
	assert.Contains(t, string(cal), "DTSTART;VALUE=DATE:20261003")

	// Carried at a close, with its due date.
	dealRun(t, "close", "--deal", id, "--carry-open-obligations", "--input", writeJSON(t, `{"status":"received"}`))
	assert.Contains(t, dealOwnBundle(t, id), `"carried_obligations":[{"due_by":"2026-10-03"`)
}

func TestADueDateIsNotACancelByDate(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
	for _, bad := range []string{
		`{"kind": "deliver_by", "cancel_by": "2026-10-03"}`,
		`{"kind": "perform_by", "due_by": "2026-10-03", "cancel_by": "2026-10-02"}`,
		`{"kind": "trial_conversion", "due_by": "2026-10-03"}`,
	} {
		_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "evidence",
			"--input", writeJSON(t, `{"about": "the sale", "source": "agent", "obligation": `+bad+`}`))
		require.ErrorIs(t, err, ErrInput, bad)
	}
}

// A sealed cancel ends a cancel-by obligation, never a due one: the delivery
// is still owed. Evidence that resolves it does end it.
func TestACancelDoesNotEndADueDate(t *testing.T) {
	open := &dealOpen{Terms: dealTerms{Currency: "USD"}}
	at := func(s string) string { return s }
	events := []sealedEvent{
		{CapsuleID: "c0", Event: dealEvent{Kind: "open", Open: open, At: at("2026-09-28T10:00:00Z")}},
		{CapsuleID: "c1", Event: dealEvent{Kind: "evidence", N: 2, At: at("2026-09-28T11:00:00Z"),
			Evidence: &dealEvidence{About: "the sale", Source: "agent", Obligation: &dealObligation{Kind: "deliver_by", DueBy: "2026-10-03"}}}},
		{CapsuleID: "c2", Event: dealEvent{Kind: "act", N: 3, At: at("2026-09-29T09:00:00Z"), Act: &dealAct{Action: "cancel"}}},
	}
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	ds := dealDeadlines(events, now, 2)
	require.Len(t, ds, 1)
	assert.Equal(t, "open", ds[0].Status, "a cancel does not end a delivery the user owes")
	assert.Empty(t, dealCancellations(events)[0].CancelBy, "a cancel is not paired with a due date")

	events = append(events, sealedEvent{CapsuleID: "c3", Event: dealEvent{Kind: "evidence", N: 4, At: at("2026-10-02T09:00:00Z"),
		Evidence: &dealEvidence{About: "the sale", Source: "agent", Resolves: "c1"}}})
	ds = dealDeadlines(events, now, 2)
	assert.Equal(t, "resolved", ds[0].Status)
	assert.Equal(t, "c3", ds[0].ResolvedBy)
}
