package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/verify"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The synthetic store: a parent agent starts a sub-task, which fills a form,
// raises a spend approval up to a ceiling, and hands off; plus two rows far
// from any signal, and each source's horizon (its oldest row still held).
const backfillFixture = "testdata/backfill/window.jsonl"

func backfillPinClock(t *testing.T, at string) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	require.NoError(t, err)
	previous := backfillClock
	backfillClock = func() time.Time { return now }
	t.Cleanup(func() { backfillClock = previous })
}

func backfillProfile(t *testing.T) Profile {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := bookProfile(t, "effects")
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	return p
}

func runBackfillCmd(t *testing.T, p Profile, source string, args ...string) (backfillResult, error) {
	// args: extra flags for backfill run.
	t.Helper()
	out, err := invoke(t, "", append([]string{"backfill", "run", "--profile", p.Name, "--source", source}, args...)...)
	var result backfillResult
	if out != "" && strings.HasPrefix(out, "{") {
		require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	}
	return result, err
}

func writeSource(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func rawDigest(id string) string {
	sum := sha256.Sum256([]byte("raw row " + id))
	return hex.EncodeToString(sum[:])
}

// storedCapsule reads a retained Capsule and its payload back from the store.
func storedCapsule(t *testing.T, p Profile, capsuleID string) ([]byte, map[string]any) {
	t.Helper()
	target, err := openTarget(t.Context(), p, useArtifacts)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	record, err := target.artifacts.Get(t.Context(), capsuleID)
	require.NoError(t, err)
	var payload map[string]any
	for _, a := range record.Artifacts {
		if a.Name == "payload" {
			d := json.NewDecoder(bytes.NewReader(a.Content))
			d.UseNumber()
			require.NoError(t, d.Decode(&payload))
		}
	}
	require.NotNil(t, payload, "a Tier B Capsule retains its payload")
	return record.Capsule, payload
}

func publishedCount(t *testing.T, p Profile) int {
	t.Helper()
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	records, err := opened.book.Query(t.Context(), evidencebook.Filter{RecordType: recordTypePublished})
	require.NoError(t, err)
	return len(records)
}

// toolRow is a synthetic tool_calls row with no signal.
func toolRow(id string, cursor int, at, status string) string {
	return fmt.Sprintf(`{"kind":"row","source":"tool_calls","cursor":%d,"id":"%s","at":"%s","record_kind":"tool_call","task":"agent-main","tool":"search","status":"%s","raw_digest":"%s"}`, cursor, id, at, status, rawDigest(id))
}

// TestBackfillPassAcceptance: one watermark pass digests every row it reads,
// retains the rows around each signal as backfilled Capsules that pass Class
// 1 verification with every provenance_mode field and time_rung
// self_attested, and commits an import record naming the window (per-source
// cursor range) and each source's horizon.
func TestBackfillPassAcceptance(t *testing.T) {
	p := backfillProfile(t)
	backfillPinClock(t, "2026-09-27T19:05:00Z")
	result, err := runBackfillCmd(t, p, backfillFixture)
	require.NoError(t, err)

	assert.True(t, result.FirstPass)
	nineteen, one := int64(19), int64(1)
	assert.Equal(t, map[string]backfillCursor{"tool_calls": {Through: &nineteen}, "subtasks": {Through: &one}, "spend_approvals": {Through: &one}}, result.Window)
	assert.Equal(t, &backfillSpan{Earliest: "2026-09-27T17:00:00Z", Latest: "2026-09-27T18:50:00Z"}, result.SourceTimeSpan)
	assert.Equal(t, backfillHorizon{Cursor: 4, At: "2026-09-20T00:00:00Z"}, result.Horizons["tool_calls"])
	assert.Equal(t, 7, result.RowsRead)
	assert.Empty(t, result.Gaps)
	assert.Empty(t, result.Refused)
	assert.Empty(t, result.Anomalies)
	assert.NotEmpty(t, result.ImportRecordID)
	assert.Contains(t, result.Summary, "not what was charged and not who approved it")
	assert.Contains(t, result.Summary, "Detectability of absence, not prevention.")
	assert.Equal(t, backfillLimits, result.Limits)
	assert.Contains(t, result.Limits, "run by the agent it audits")
	assert.Equal(t, backfillScope, result.Scope)

	// Tier A: every row read, digests only.
	var tierA []string
	for _, e := range result.TierA {
		tierA = append(tierA, e.ID)
		assert.Len(t, e.Digest, 64)
	}
	assert.Equal(t, []string{"sa-1", "st-1", "tc-0", "tc-1", "tc-2", "tc-3", "tc-9"}, tierA)

	// Tier B: within 30m of the spend approval or the hand-off; tc-0 and tc-9 are not.
	var retained []string
	for _, r := range result.Retained {
		retained = append(retained, r.ActionID)
		assert.False(t, r.Retrospective)
		assert.Equal(t, result.ImportBatch, r.PinnedBy)
	}
	assert.Equal(t, []string{"tool_calls/tc-1", "subtasks/st-1", "tool_calls/tc-2", "spend_approvals/sa-1", "tool_calls/tc-3"}, retained)

	for _, r := range result.Retained {
		raw, payload := storedCapsule(t, p, r.CapsuleID)
		capsule, err := verify.DecodeCapsuleJSON(raw)
		require.NoError(t, err)
		v := verify.Verify(capsule, nil, nil)
		require.True(t, v.OK, "Class 1: %v", v.Findings)
		assert.Equal(t, "backfilled", v.Assurance["provenance_mode"])
		assert.Equal(t, "self_attested", v.Assurance["provenance_time_rung"])

		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		pm := fields["provenance_mode"].(map[string]any)
		assert.Equal(t, "backfilled", pm["mode"])
		assert.Equal(t, "self_attested", pm["time_rung"])
		assert.Equal(t, result.ImportBatch, pm["import_batch"])
		assert.Equal(t, "2026-09-27T19:05:00Z", pm["imported_at"])
		assert.NotEqual(t, pm["imported_at"], pm["source_asserted_at"])
		ref := pm["source_ref"].(map[string]any)
		assert.Equal(t, map[string]any{"type": "x-external-ledger-entry", "digest_alg": "SHA-256", "digest": r.SourceDigest}, ref)

		// The source_ref recomputes from the retained payload.
		host, err := canonical.JCS(payload["host_record"])
		require.NoError(t, err)
		sum := sha256.Sum256(host)
		assert.Equal(t, r.SourceDigest, hex.EncodeToString(sum[:]))
		assert.Equal(t, pm["source_asserted_at"], payload["host_record"].(map[string]any)["at"])
		assert.Equal(t, "B", payload["tier"])
		assert.Equal(t, map[string]any{
			"what_this_is": backfillScope["what_this_is"], "effects": backfillScope["effects"],
			"amount": backfillScope["amount"], "authority": backfillScope["authority"], "time": backfillScope["time"],
		}, payload["scope"])
		if r.ActionID == "spend_approvals/sa-1" {
			// What the host recorded, in its terms, and never as a charge.
			assert.Equal(t, "The host recorded: spend approval, on www.stickers.example, approved ceiling 11.00 USD (a limit, not the charge), status closed.", payload["recorded"])
			assert.NotContains(t, string(raw), "amount_minor")
		}
	}

	status, err := invoke(t, "", "backfill", "status", "--profile", p.Name)
	require.NoError(t, err)
	assert.Contains(t, status, `"cursors":{"spend_approvals":1,"subtasks":1,"tool_calls":19}`)
}

// TestBackfillRerunNoDuplicates: the same rows again append no Capsule.
func TestBackfillRerunNoDuplicates(t *testing.T) {
	p := backfillProfile(t)
	backfillPinClock(t, "2026-09-27T19:05:00Z")
	first, err := runBackfillCmd(t, p, backfillFixture)
	require.NoError(t, err)
	require.Len(t, first.Retained, 5)
	before := publishedCount(t, p)

	// The same second as the first pass: the passes still stay apart.
	again, err := runBackfillCmd(t, p, backfillFixture)
	require.NoError(t, err)
	assert.Empty(t, again.Retained)
	assert.Empty(t, again.TierA, "every row already has its Tier A entry")
	assert.Empty(t, again.Anomalies, "a re-read row is not a late arrival")
	assert.Equal(t, 7, again.RereadVerified)
	assert.Equal(t, 5, again.AlreadyRetained)
	assert.Empty(t, again.Refused)
	assert.Equal(t, before, publishedCount(t, p))
	assert.NotEqual(t, first.ImportBatch, again.ImportBatch)
}

// TestBackfillRetrospectivePromotion: a pass that saw no signal digests its
// rows only. A later signal promotes the earlier row near it: the re-read row
// is retained only when its digest equals what Tier A committed. A row the
// host changed since is refused, with both digests.
func TestBackfillRetrospectivePromotion(t *testing.T) {
	p := backfillProfile(t)
	quiet := toolRow("tc-9", 9, "2026-09-27T18:50:00Z", "succeeded")
	other := toolRow("tc-8", 8, "2026-09-27T18:40:00Z", "succeeded")
	backfillPinClock(t, "2026-09-27T19:00:00Z")
	first, err := runBackfillCmd(t, p, writeSource(t, quiet, other))
	require.NoError(t, err)
	require.Len(t, first.TierA, 2)
	require.Empty(t, first.Retained, "no signal, nothing retained")

	signal := `{"kind":"row","source":"spend_approvals","cursor":1,"id":"sa-2","at":"2026-09-27T19:10:00Z","record_kind":"spend_approval","status":"pending","raw_digest":"` + rawDigest("sa-2") + `","signals":["spend_request"]}`
	changed := toolRow("tc-8", 8, "2026-09-27T18:40:00Z", "failed")
	next := toolRow("tc-10", 10, "2026-09-27T19:30:00Z", "succeeded")
	backfillPinClock(t, "2026-09-27T20:00:00Z")
	second, err := runBackfillCmd(t, p, writeSource(t, quiet, changed, next, signal))
	require.ErrorIs(t, err, ErrPartial, "a refused row exits 3")
	nine, ten := int64(9), int64(10)
	assert.Equal(t, backfillCursor{After: &nine, Through: &ten}, second.Window["tool_calls"])
	assert.Empty(t, second.Gaps)
	assert.Empty(t, second.Anomalies)

	byID := map[string]backfillRetained{}
	for _, r := range second.Retained {
		byID[r.ActionID] = r
	}
	require.Len(t, byID, 3)
	promoted := byID["tool_calls/tc-9"]
	assert.True(t, promoted.Retrospective)
	assert.Equal(t, first.ImportBatch, promoted.PinnedBy)
	_, payload := storedCapsule(t, p, promoted.CapsuleID)
	assert.Equal(t, first.ImportBatch, payload["pinned_by"])
	assert.False(t, byID["spend_approvals/sa-2"].Retrospective)
	assert.False(t, byID["tool_calls/tc-10"].Retrospective)

	require.Len(t, second.Refused, 1)
	refused := second.Refused[0]
	assert.Equal(t, "tc-8", refused.ID)
	var pinned string
	for _, e := range first.TierA {
		if e.ID == "tc-8" {
			pinned = e.Digest
		}
	}
	assert.Equal(t, pinned, refused.TierADigest)
	assert.NotEqual(t, refused.TierADigest, refused.Digest)
	assert.Contains(t, refused.Reason, "differs from the digest Tier A committed")
	assert.NotContains(t, byID, "tool_calls/tc-8")
}

// TestBackfillFlagsRowBelowPassedCursor: the cursor is a monotonic id, never
// a timestamp, and a new row appearing at or below a cursor already passed is
// captured and flagged, never skipped silently.
func TestBackfillFlagsRowBelowPassedCursor(t *testing.T) {
	p := backfillProfile(t)
	backfillPinClock(t, "2026-09-27T19:00:00Z")
	_, err := runBackfillCmd(t, p, writeSource(t,
		toolRow("tc-1", 1, "2026-09-27T18:00:00Z", "succeeded"),
		toolRow("tc-2", 2, "2026-09-27T18:01:00Z", "succeeded"),
		toolRow("tc-5", 5, "2026-09-27T18:05:00Z", "succeeded")))
	require.NoError(t, err)

	backfillPinClock(t, "2026-09-27T19:05:00Z")
	// tc-3 committed late: its created_at is older than rows already passed,
	// which is exactly the row a timestamp watermark would skip.
	result, err := runBackfillCmd(t, p, writeSource(t,
		toolRow("tc-3", 3, "2026-09-27T18:02:00Z", "succeeded"),
		toolRow("tc-6", 6, "2026-09-27T18:58:00Z", "succeeded")))
	require.ErrorIs(t, err, ErrPartial, "an anomaly exits 3")
	require.Len(t, result.Anomalies, 1)
	assert.Equal(t, backfillAnomaly{Source: "tool_calls", ID: "tc-3", Cursor: 3, PassedCursor: 5, Reason: result.Anomalies[0].Reason}, result.Anomalies[0])
	assert.Contains(t, result.Anomalies[0].Reason, "below a cursor an earlier pass had already passed")
	var tierA []string
	for _, e := range result.TierA {
		tierA = append(tierA, e.ID)
	}
	assert.Equal(t, []string{"tc-3", "tc-6"}, tierA, "the late row is still captured")
}

// TestBackfillDeclaresGaps: cursors gone before a pass reached them (the
// horizon moved past the watermark), and a source the reader could not read,
// are both declared; a late pass says so.
func TestBackfillDeclaresGaps(t *testing.T) {
	p := backfillProfile(t)
	backfillPinClock(t, "2026-09-27T19:00:00Z")
	_, err := runBackfillCmd(t, p, writeSource(t, toolRow("tc-5", 5, "2026-09-27T18:05:00Z", "succeeded")))
	require.NoError(t, err)

	backfillPinClock(t, "2026-09-28T02:00:00Z")
	result, err := runBackfillCmd(t, p, writeSource(t,
		`{"kind":"horizon","source":"tool_calls","cursor":9,"at":"2026-09-27T20:00:00Z","limited":true}`,
		toolRow("tc-9", 9, "2026-09-27T20:00:00Z", "succeeded"),
		`{"kind":"gap","source":"spend_approvals","reason":"the select timed out for this source"}`), "--expect-every", "1h")
	require.ErrorIs(t, err, ErrPartial)
	six, eight := int64(6), int64(8)
	require.Len(t, result.Gaps, 2)
	assert.Equal(t, "tool_calls", result.Gaps[0].Source)
	assert.Equal(t, &six, result.Gaps[0].FromCursor)
	assert.Equal(t, &eight, result.Gaps[0].ToCursor)
	assert.Contains(t, result.Gaps[0].Reason, "permanent loss")
	assert.Equal(t, backfillGap{Source: "spend_approvals", Reason: "declared by the reader: the select timed out for this source"}, result.Gaps[1])
	assert.Contains(t, result.Late, "more than twice the expected 1h0m0s")
	assert.Contains(t, strings.Join(result.Notes, "\n"), "hit its row limit for tool_calls")
	require.NotNil(t, result.Previous)
}

// TestBackfillTimeRungOverclaimFails: claiming time_rung "witnessed" with no
// reference citing corroborates_source_time cannot be sealed, and a Capsule
// edited to claim it fails Class 1 as provenance_time_rung_overclaim.
func TestBackfillTimeRungOverclaimFails(t *testing.T) {
	p := backfillProfile(t)
	rows, _, _, err := readBackfillSource(strings.NewReader(toolRow("tc-1", 1, "2026-09-27T18:00:00Z", "succeeded")))
	require.NoError(t, err)
	digest, err := rows[0].sourceDigest()
	require.NoError(t, err)
	at := time.Date(2026, 9, 27, 19, 0, 0, 0, time.UTC)
	_, key := profileFixture(t)

	witnessed, err := backfillRequest(p, rows[0], digest, "b", "b", at, "witnessed")
	require.NoError(t, err)
	_, err = seal(witnessed, key)
	require.ErrorIs(t, err, ErrInput, "the producer refuses the overclaim")

	honest, err := backfillRequest(p, rows[0], digest, "b", "b", at, "self_attested")
	require.NoError(t, err)
	record, err := seal(honest, key)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &fields))
	fields["provenance_mode"].(map[string]any)["time_rung"] = "witnessed"
	delete(fields, "capsule_id")
	id, err := canonical.ComputeCapsuleID(fields)
	require.NoError(t, err)
	fields["capsule_id"] = id
	edited, err := canonical.JCS(fields)
	require.NoError(t, err)
	capsule, err := verify.DecodeCapsuleJSON(edited)
	require.NoError(t, err)
	v := verify.Verify(capsule, nil, nil)
	assert.False(t, v.OK)
	var codes []string
	for _, f := range v.Findings {
		if f.Check != nil && *f.Check == 9 {
			codes = append(codes, f.Code)
		}
	}
	assert.Equal(t, []string{"provenance_time_rung_overclaim"}, codes)
	assert.Equal(t, emit.SpecVersion, fields["spec_version"])
}

// TestBackfillRefusesPersonalData: there is no field for text or form
// values, and an allow-listed field holding an email address, phone number,
// street address or card number refuses the whole file, naming the line and
// field and never the value. Nothing is written.
func TestBackfillRefusesPersonalData(t *testing.T) {
	const email, phone, street, card = "jane.doe@example.com", "+1 555 010 0123", "12 Main Street, Springfield", "4111 1111 1111 1111"
	base := func(extra string) string {
		return `{"kind":"row","source":"tool_calls","cursor":2,"id":"tc-2","at":"2026-09-27T18:00:20Z","record_kind":"tool_call","tool":"browser_fill","raw_digest":"` + rawDigest("tc-2") + `"` + extra + `}`
	}
	cases := map[string]string{
		"text field":         base(`,"text_content":"filled ` + email + ` ` + phone + ` ` + street + `"`),
		"form values field":  base(`,"form_values":{"address":"` + street + `"}`),
		"email in task":      base(`,"task":"` + email + `"`),
		"phone in task":      base(`,"task":"` + strings.ReplaceAll(phone, " ", "") + `"`),
		"email in line item": base(`,"line_items":["` + email + `"]`),
		"phone in line item": base(`,"line_items":["` + phone + `"]`),
		"card in id":         strings.Replace(base(""), `"id":"tc-2"`, `"id":"`+strings.ReplaceAll(card, " ", "")+`"`, 1),
		"street in reason":   `{"kind":"gap","source":"tool_calls","reason":"ship to ` + street + ` ` + phone + `"}`,
	}
	p := backfillProfile(t)
	backfillPinClock(t, "2026-09-27T19:05:00Z")
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := invoke(t, "", "backfill", "run", "--profile", p.Name, "--source", writeSource(t, base(""), line))
			require.ErrorIs(t, err, ErrInput)
			msg := SafeError(err)
			assert.Contains(t, msg, "backfill source line 2")
			for _, v := range []string{email, phone, street, card} {
				assert.NotContains(t, msg+out, v)
			}
		})
	}
	assert.Equal(t, 0, publishedCount(t, p))
	status, err := invoke(t, "", "backfill", "status", "--profile", p.Name)
	require.NoError(t, err)
	assert.Contains(t, status, `"passes":0`)
}
