package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// `backfill` imports what an agent host already recorded in its own store
// (its tool-call, sub-task and spend-approval rows). It is the effects leg
// only: it says what the host recorded, when the host says it happened, and
// when we imported it; never that the user approved anything, and never what
// was charged. Capture is broad and disclosure narrow (the bundle verbs), in
// tiers, because the host's rows are perishable:
//
//   - Tier A: every row in the window is committed by digest in the pass's
//     import record. No field of the row is copied; the record pins the
//     window's existence, order and completeness.
//   - Tier B: a row carrying a consequential signal, and every row within
//     --promote-around of one, is retained as an ordinary Capsule in
//     provenance mode "backfilled" (AAC -05) whose payload holds the
//     allow-listed fields.
//   - Tier C (raw rows) is never taken here; raw_digest pins it for whatever
//     keeps it (an open deal's own store).
//
// Each pass is a watermark poll: per source, the rows whose monotonic cursor
// (the host's sequence id, never a timestamp) is past the highest cursor
// already digested. A new row at or below that cursor arrived late; it is
// captured and flagged as an anomaly, never skipped silently.
//
// Promotion may come after the fact: a pass re-reads earlier rows around a new
// signal. A re-read row is promoted only when its digest equals the one Tier
// A committed, so hindsight can reveal more of a window, never rewrite it.
//
// The importer is run by the agent it audits, so it cannot prevent absence,
// only make it detectable (backfillLimits).
//
// capsulectl holds no host-specific code. A reader for a host is a small
// program outside this repository that selects allow-listed columns from the
// host's store and writes backfill-source-record/v0 lines (BACKFILL.md).

const (
	recordTypeBackfillImport = "backfill_import"
	backfillSourceRefType    = "x-external-ledger-entry"
	backfillDeveloper        = "capsulectl-backfill"
	backfillPayloadSchema    = "backfill-record/v0"
	backfillImportSchema     = "backfill-import/v0"
)

// backfillScope is stated in every backfilled Capsule's payload and in every
// import record, so no rendering of either can drop it.
var backfillScope = map[string]string{
	"what_this_is": "A copy of what the agent host recorded in its own store, imported after the fact. It is not a receipt.",
	"effects":      "shown: the host recorded this action and the status it gave it",
	"amount":       "not shown: no charged amount is recorded here; an approved ceiling, when present, is the approval limit the host stored, not the amount charged",
	"authority":    "not shown: whether the user approved this action is not recorded here",
	"time":         "the time is the host's own claim (time_rung self_attested); nothing independent corroborates it",
}

// backfillLimits is the structural limit, stated in every import record.
const backfillLimits = "This importer is run by the agent it audits. The agent cannot rewrite the host's store, but it can fail to run the importer or narrow its query. What this provides is detectability of absence (an append-only book, witnessed checkpoints, and an import record naming the window and every gap), not prevention. The book is tamper-evident against ourselves and the agent once a pass is checkpointed; the host's store is not covered: there is no baseline before the first pass, it is read-only to us, and rows it reaps are gone."

// backfillCannotSee is printed with every import record.
var backfillCannotSee = []string{
	"only the rows the reader wrote to the file it was given: a table, session or sub-task the reader does not select is not seen",
	"only allow-listed fields: message text and form values are never read, so what a form submitted is not shown",
	"a row the host deleted, pruned or never wrote before this pass ran is not seen: downtime longer than the host's retention window is permanent loss, shown here at most as a horizon gap",
	"the charged amount and the user's approval are not in the host's records read here",
	"source times are the host's own claims; they are not witnessed",
}

// backfillSourceRecord is one backfill-source-record/v0 line: a row the
// host recorded ("row"), a source the reader could not read ("gap"), or the
// oldest row a source still holds ("horizon").
// Unknown fields refuse the file: there is no field for text or form values.
type backfillSourceRecord struct {
	Kind string `json:"kind"`
	// Cursor is the row's monotonic id in its source (row), or the oldest
	// cursor the source still holds (horizon).
	Cursor *int64 `json:"cursor,omitempty"`

	Source               string   `json:"source,omitempty"`
	ID                   string   `json:"id,omitempty"`
	At                   string   `json:"at,omitempty"`
	RecordKind           string   `json:"record_kind,omitempty"`
	Task                 string   `json:"task,omitempty"`
	ParentTask           string   `json:"parent_task,omitempty"`
	ToolCallID           string   `json:"tool_call_id,omitempty"`
	Tool                 string   `json:"tool,omitempty"`
	MerchantDomain       string   `json:"merchant_domain,omitempty"`
	LineItems            []string `json:"line_items,omitempty"`
	Status               string   `json:"status,omitempty"`
	ApprovedCeilingMinor *int64   `json:"approved_ceiling_minor,omitempty"`
	Currency             string   `json:"currency,omitempty"`
	// RawDigest is the reader's SHA-256 over the whole row as the host
	// stored it, every column included: it pins the raw row without
	// carrying it.
	RawDigest string   `json:"raw_digest,omitempty"`
	Signals   []string `json:"signals,omitempty"`

	Reason string `json:"reason,omitempty"`
	// Limited (horizon) says the reader's select hit its row limit: rows past
	// it wait for the next pass.
	Limited bool `json:"limited,omitempty"`
}

var (
	backfillToken  = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	backfillIdent  = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]{1,200}$`)
	backfillHost   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	backfillCode   = regexp.MustCompile(`^[A-Z]{3}$`)
	backfillReason = regexp.MustCompile(`^[A-Za-z0-9 ,.;:()_/'-]{1,200}$`)
	backfillItem   = regexp.MustCompile(`^[\p{L}\p{N} ,.&'()/#+_-]{1,120}$`)
	// Shapes no allow-listed field ever legitimately holds.
	backfillEmail = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[A-Za-z]{2,}`)
	backfillPAN   = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)
	backfillPhone = regexp.MustCompile(`\+?\d(?:[\s().-]*\d){6,}`)
)

var backfillRecordKinds = map[string]bool{"tool_call": true, "subtask": true, "spend_approval": true}

// backfillSignals are the consequential signals a reader may mark on a row,
// mapped mechanically from the row alone. A marked row is retained (Tier B).
var backfillSignals = map[string]bool{"spend_request": true, "handoff": true, "payment_navigation": true, "approval_ref": true}

var backfillHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// personalShape reports whether v looks like an email address, a card
// number or a phone number.
func personalShape(v string) bool {
	return backfillEmail.MatchString(v) || backfillPAN.MatchString(v) || backfillPhone.MatchString(v)
}

func backfillLineError(line int, field, why string) error {
	// Names the line and the field, never the value.
	return hint(ErrInput, fmt.Sprintf("backfill source line %d: %s %s", line, field, why))
}

func parseBackfillTime(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, v)
	return t.UTC(), err == nil
}

// validate checks one decoded line and normalizes its times to UTC.
func (r *backfillSourceRecord) validate(line int) error {
	if !backfillToken.MatchString(r.Source) {
		return backfillLineError(line, "source", "must be a lowercase token")
	}
	cursorOK := r.Cursor != nil && *r.Cursor >= 0 && *r.Cursor <= 1<<53
	rowFields := r.ID != "" || r.RecordKind != "" || r.Task != "" || r.ParentTask != "" || r.ToolCallID != "" || r.Tool != "" ||
		r.MerchantDomain != "" || r.LineItems != nil || r.Status != "" || r.ApprovedCeilingMinor != nil || r.Currency != "" || r.RawDigest != "" || r.Signals != nil
	switch r.Kind {
	case "gap":
		if rowFields || r.Cursor != nil || r.At != "" || r.Limited {
			return backfillLineError(line, "gap", "carries a field other than source and reason")
		}
		if !backfillReason.MatchString(r.Reason) || personalShape(r.Reason) {
			return backfillLineError(line, "reason", "must be a short plain sentence")
		}
		return nil
	case "horizon":
		if rowFields || r.Reason != "" {
			return backfillLineError(line, "horizon", "carries a field other than source, cursor, at and limited")
		}
		if !cursorOK {
			return backfillLineError(line, "cursor", "must be the oldest cursor the source holds, a non-negative integer")
		}
		at, ok := parseBackfillTime(r.At)
		if !ok {
			return backfillLineError(line, "at", "must be an RFC 3339 time")
		}
		r.At = at.Format(time.RFC3339Nano)
		return nil
	case "row":
	default:
		return backfillLineError(line, "kind", "must be row, gap or horizon")
	}
	if r.Reason != "" || r.Limited {
		return backfillLineError(line, "row", "carries a gap or horizon field")
	}
	if !cursorOK {
		return backfillLineError(line, "cursor", "must be the row's monotonic id in its source, a non-negative integer")
	}
	if !backfillRecordKinds[r.RecordKind] {
		return backfillLineError(line, "record_kind", "must be tool_call, subtask or spend_approval")
	}
	at, ok := parseBackfillTime(r.At)
	if !ok {
		return backfillLineError(line, "at", "must be an RFC 3339 time")
	}
	r.At = at.Format(time.RFC3339Nano)
	for _, f := range []struct {
		name, value string
		required    bool
	}{{"id", r.ID, true}, {"task", r.Task, false}, {"parent_task", r.ParentTask, false}, {"tool_call_id", r.ToolCallID, false}, {"tool", r.Tool, false}} {
		if f.value == "" && !f.required {
			continue
		}
		if !backfillIdent.MatchString(f.value) || backfillEmail.MatchString(f.value) || backfillPAN.MatchString(f.value) || strings.HasPrefix(f.value, "+") {
			return backfillLineError(line, f.name, "must look like an identifier")
		}
	}
	if r.RecordKind == "tool_call" && r.Tool == "" {
		return backfillLineError(line, "tool", "is required for a tool_call")
	}
	if r.MerchantDomain != "" && !backfillHost.MatchString(r.MerchantDomain) {
		return backfillLineError(line, "merchant_domain", "must be a lowercase host name")
	}
	if r.Status != "" && !backfillToken.MatchString(r.Status) {
		return backfillLineError(line, "status", "must be a lowercase token")
	}
	if len(r.LineItems) > 50 {
		return backfillLineError(line, "line_items", "holds more than 50 names")
	}
	for i, item := range r.LineItems {
		if !backfillItem.MatchString(item) || personalShape(item) {
			return backfillLineError(line, fmt.Sprintf("line_items[%d]", i), "must be a short product name")
		}
	}
	if !backfillHex64.MatchString(r.RawDigest) {
		return backfillLineError(line, "raw_digest", "must be 64 lowercase hex (SHA-256 of the whole row)")
	}
	for i, sig := range r.Signals {
		if !backfillSignals[sig] {
			return backfillLineError(line, fmt.Sprintf("signals[%d]", i), "must be spend_request, handoff, payment_navigation or approval_ref")
		}
	}
	if r.ApprovedCeilingMinor != nil || r.Currency != "" {
		if r.RecordKind != "spend_approval" {
			return backfillLineError(line, "approved_ceiling_minor", "belongs only on a spend_approval")
		}
		if r.ApprovedCeilingMinor == nil || *r.ApprovedCeilingMinor < 0 || *r.ApprovedCeilingMinor > 1<<53 || !backfillCode.MatchString(r.Currency) {
			return backfillLineError(line, "approved_ceiling_minor/currency", "must be a non-negative integer with a 3-letter upper-case code")
		}
	}
	return nil
}

// readBackfillSource reads a backfill-source-record/v0 JSONL stream. Any bad
// line refuses the whole file: nothing is imported from a file the reader
// got wrong, and the next pass starts from the same place.
func readBackfillSource(in io.Reader) (rows, gaps, horizons []backfillSourceRecord, err error) {
	s := bufio.NewScanner(io.LimitReader(in, maxInput+1))
	s.Buffer(make([]byte, 64<<10), 1<<20)
	read, line := 0, 0
	for s.Scan() {
		line++
		raw := s.Bytes()
		read += len(raw) + 1
		if read > maxInput {
			return nil, nil, nil, inputError("backfill source exceeds size limit")
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var r backfillSourceRecord
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&r) != nil || d.More() {
			return nil, nil, nil, backfillLineError(line, "line", "is not one JSON object of known fields")
		}
		if err := r.validate(line); err != nil {
			return nil, nil, nil, err
		}
		switch r.Kind {
		case "gap":
			gaps = append(gaps, r)
		case "horizon":
			horizons = append(horizons, r)
		default:
			rows = append(rows, r)
		}
	}
	if err := s.Err(); err != nil {
		return nil, nil, nil, inputError("backfill source could not be read as lines")
	}
	return rows, gaps, horizons, nil
}

// hostRecord is the row as it is committed: the source line minus kind.
func (r backfillSourceRecord) hostRecord() map[string]any {
	m := map[string]any{"source": r.Source, "cursor": *r.Cursor, "id": r.ID, "at": r.At, "record_kind": r.RecordKind, "raw_digest": r.RawDigest}
	for k, v := range map[string]string{"task": r.Task, "parent_task": r.ParentTask, "tool_call_id": r.ToolCallID, "tool": r.Tool, "merchant_domain": r.MerchantDomain, "status": r.Status, "currency": r.Currency} {
		if v != "" {
			m[k] = v
		}
	}
	if len(r.LineItems) > 0 {
		m["line_items"] = r.LineItems
	}
	if r.ApprovedCeilingMinor != nil {
		m["approved_ceiling_minor"] = *r.ApprovedCeilingMinor
	}
	if len(r.Signals) > 0 {
		m["signals"] = r.Signals
	}
	return m
}

// sourceDigest is SHA-256 over the JCS form of the host record. It is the
// row's Tier A digest and its Tier B Capsule's source_ref: whoever holds the
// payload can recompute it, and whoever holds the host's store can re-run
// the reader and compare. raw_digest inside it pins the raw row too.
func (r backfillSourceRecord) sourceDigest() (string, error) {
	// Through JSON first, so JCS sees plain JSON values.
	raw, err := json.Marshal(r.hostRecord())
	if err != nil {
		return "", err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err = d.Decode(&value); err != nil {
		return "", err
	}
	b, err := canonical.JCS(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// backfillRecorded is the one plain line a Capsule's payload gives for what
// the host recorded, in the host's own terms.
func (r backfillSourceRecord) recorded() string {
	var parts []string
	switch r.RecordKind {
	case "tool_call":
		parts = append(parts, "tool call "+r.Tool)
	case "subtask":
		parts = append(parts, "sub-task")
	case "spend_approval":
		parts = append(parts, "spend approval")
	}
	if r.MerchantDomain != "" {
		parts = append(parts, "on "+r.MerchantDomain)
	}
	if r.ApprovedCeilingMinor != nil {
		parts = append(parts, fmt.Sprintf("approved ceiling %d.%02d %s (a limit, not the charge)", *r.ApprovedCeilingMinor/100, *r.ApprovedCeilingMinor%100, r.Currency))
	}
	if r.Status != "" {
		parts = append(parts, "status "+r.Status)
	}
	return "The host recorded: " + strings.Join(parts, ", ") + "."
}

// backfillRequest is the seal request for one row. timeRung is always
// "self_attested" from the command; it is a parameter so a test can show
// what a "witnessed" claim with no corroborating reference does.
// pinnedBy is the import batch whose Tier A entry committed the row.
func backfillRequest(p Profile, r backfillSourceRecord, digest, batch, pinnedBy string, importedAt time.Time, timeRung string) (Request, error) {
	payload, err := json.Marshal(map[string]any{
		"schema":      backfillPayloadSchema,
		"tier":        "B",
		"pinned_by":   pinnedBy,
		"recorded":    r.recorded(),
		"host_record": r.hostRecord(),
		"scope":       backfillScope,
	})
	if err != nil {
		return Request{}, err
	}
	return Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: r.Source + "/" + r.ID, ActionType: emit.ActionTypeFYI,
			Operator: p.Operator, Developer: backfillDeveloper, Timestamp: importedAt,
			ProvenanceMode: &emit.ProvenanceMode{
				Mode:             "backfilled",
				SourceRef:        &emit.Reference{Type: backfillSourceRefType, DigestAlg: "SHA-256", Digest: digest},
				SourceAssertedAt: r.At,
				ImportBatch:      batch,
				ImportedAt:       importedAt.Format(time.RFC3339Nano),
				TimeRung:         emit.TimeRung(timeRung),
			},
		},
		Payload: payload,
	}, nil
}

// backfillGap is something no pass read: a source the reader could not read,
// or cursors that were gone before a pass reached them.
type backfillGap struct {
	Source     string `json:"source"`
	FromCursor *int64 `json:"from_cursor,omitempty"`
	ToCursor   *int64 `json:"to_cursor,omitempty"`
	Reason     string `json:"reason"`
}

// backfillTierA is one row's Tier A entry: its key, its time, its digest.
type backfillTierA struct {
	Source string `json:"source"`
	Cursor int64  `json:"cursor"`
	ID     string `json:"id"`
	At     string `json:"at"`
	Digest string `json:"digest"`
}

// backfillRetained is a row retained this pass as a Tier B Capsule.
type backfillRetained struct {
	CapsuleID    string `json:"capsule_id"`
	SourceDigest string `json:"source_digest"`
	ActionID     string `json:"action_id"`
	PinnedBy     string `json:"pinned_by"`
	// Retrospective is true for a row an earlier pass digested.
	Retrospective bool `json:"retrospective"`
}

// backfillRefused is a re-read row that was not promoted, and why.
type backfillRefused struct {
	Source      string `json:"source"`
	ID          string `json:"id"`
	Digest      string `json:"digest"`
	TierADigest string `json:"tier_a_digest"`
	Reason      string `json:"reason"`
}

// backfillAnomaly is a new row that appeared at or below a cursor an earlier
// pass had already passed: it arrived late. It is digested (and may be
// retained) like any new row, and listed here so it is never silent.
type backfillAnomaly struct {
	Source       string `json:"source"`
	ID           string `json:"id"`
	Cursor       int64  `json:"cursor"`
	PassedCursor int64  `json:"passed_cursor"`
	Reason       string `json:"reason"`
}

// backfillCursor is one source's watermark: after (exclusive) is the highest
// cursor digested before this pass, through the highest after it.
type backfillCursor struct {
	After   *int64 `json:"after"`
	Through *int64 `json:"through"`
}

// backfillHorizon is the oldest row a source still held when read.
type backfillHorizon struct {
	Cursor  int64  `json:"oldest_cursor"`
	At      string `json:"oldest_at"`
	Limited bool   `json:"limited"`
}

type backfillPrevious struct {
	ImportBatch string `json:"import_batch"`
	ImportedAt  string `json:"imported_at"`
}

type backfillSpan struct {
	Earliest string `json:"earliest"`
	Latest   string `json:"latest"`
}

// backfillImport is the import record: one per pass, committed to the book,
// whatever the pass found. The window is the per-source cursor range; gaps
// is always present, and empty only when nothing was found unread.
type backfillImport struct {
	Schema          string                     `json:"schema"`
	ImportBatch     string                     `json:"import_batch"`
	ImportedAt      string                     `json:"imported_at"`
	FirstPass       bool                       `json:"first_pass"`
	Previous        *backfillPrevious          `json:"previous_pass"`
	Late            string                     `json:"late,omitempty"`
	Window          map[string]backfillCursor  `json:"window"`
	SourceTimeSpan  *backfillSpan              `json:"source_time_span"`
	Horizons        map[string]backfillHorizon `json:"horizons"`
	PromoteAround   string                     `json:"promote_around"`
	RowsRead        int                        `json:"rows_read"`
	RereadVerified  int                        `json:"reread_verified"`
	AlreadyRetained int                        `json:"already_retained"`
	TierA           []backfillTierA            `json:"tier_a"`
	Retained        []backfillRetained         `json:"retained"`
	Refused         []backfillRefused          `json:"refused"`
	Anomalies       []backfillAnomaly          `json:"anomalies"`
	Gaps            []backfillGap              `json:"gaps"`
	Notes           []string                   `json:"notes"`
	Scope           map[string]string          `json:"scope"`
	Limits          string                     `json:"limits"`
	CannotSee       []string                   `json:"cannot_see"`
}

type backfillResult struct {
	Summary        string `json:"summary"`
	ImportRecordID string `json:"import_record_id"`
	backfillImport
}

// backfillImports returns the book's import records in log order.
func backfillImports(ctx context.Context, book *evidencebook.Book) ([]backfillImport, error) {
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: recordTypeBackfillImport})
	if err != nil {
		return nil, err
	}
	out := make([]backfillImport, 0, len(records))
	for _, r := range records {
		var st backfillImport
		if err = json.Unmarshal(r.Header.Statement, &st); err != nil {
			return nil, fmt.Errorf("backfill import record %s: %w", r.RecordID, err)
		}
		out = append(out, st)
	}
	return out, nil
}

// importedSources is every source digest a backfilled Capsule in the book
// already carries, read from the Capsules themselves (not from import
// records), so a pass interrupted before its import record is still seen.
func importedSources(ctx context.Context, t *target) (map[string]string, error) {
	records, err := t.book.book.Query(ctx, evidencebook.Filter{RecordType: recordTypePublished})
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	for _, r := range records {
		record, err := t.artifacts.Get(ctx, r.Header.SubjectRef)
		if err != nil {
			return nil, err
		}
		var capsule struct {
			Developer      string `json:"developer"`
			ProvenanceMode *struct {
				Mode      string `json:"mode"`
				SourceRef struct {
					Type   string `json:"type"`
					Digest string `json:"digest"`
				} `json:"source_ref"`
			} `json:"provenance_mode"`
		}
		if err = json.Unmarshal(record.Capsule, &capsule); err != nil {
			return nil, err
		}
		pm := capsule.ProvenanceMode
		if capsule.Developer == backfillDeveloper && pm != nil && pm.Mode == "backfilled" && pm.SourceRef.Type == backfillSourceRefType {
			seen[pm.SourceRef.Digest] = record.CapsuleID
		}
	}
	return seen, nil
}

type backfillOptions struct {
	expectEvery   time.Duration
	promoteAround time.Duration
	now           time.Time
}

func backfillKey(source, id string) string { return source + "\x00" + id }

type tierAPin struct {
	digest, batch string
}

// candidate is a row eligible for Tier B: new in this pass, or re-read with
// the digest Tier A committed.
type backfillCandidate struct {
	row           backfillSourceRecord
	digest        string
	pinnedBy      string
	retrospective bool
}

// backfillState is what the book's earlier import records establish.
type backfillState struct {
	pins   map[string]tierAPin
	passed map[string]int64
	last   *backfillImport
}

func loadBackfillState(imports []backfillImport) backfillState {
	st := backfillState{pins: map[string]tierAPin{}, passed: map[string]int64{}}
	for i, im := range imports {
		for _, e := range im.TierA {
			st.pins[backfillKey(e.Source, e.ID)] = tierAPin{digest: e.Digest, batch: im.ImportBatch}
			if c, ok := st.passed[e.Source]; !ok || e.Cursor > c {
				st.passed[e.Source] = e.Cursor
			}
		}
		st.last = &imports[i]
	}
	return st
}

func int64Ptr(v int64) *int64 { return &v }

// runBackfill commits one pass: Tier A digests for every new row, Tier B
// Capsules for signaled rows and their surroundings, and the import record
// that says all of it.
func runBackfill(ctx context.Context, t *target, rows, declared, horizons []backfillSourceRecord, opt backfillOptions) (backfillResult, error) {
	imports, err := backfillImports(ctx, t.book.book)
	if err != nil {
		return backfillResult{}, err
	}
	prior := loadBackfillState(imports)
	importedAt := opt.now.UTC().Truncate(time.Second)
	st := backfillImport{Schema: backfillImportSchema, PromoteAround: opt.promoteAround.String(),
		Window: map[string]backfillCursor{}, Horizons: map[string]backfillHorizon{},
		TierA: []backfillTierA{}, Retained: []backfillRetained{}, Refused: []backfillRefused{}, Anomalies: []backfillAnomaly{},
		Gaps: []backfillGap{}, Notes: []string{}, Scope: backfillScope, Limits: backfillLimits, CannotSee: backfillCannotSee}
	st.ImportedAt = importedAt.Format(time.RFC3339Nano)
	// The pass number keeps two passes in one second apart.
	st.ImportBatch = fmt.Sprintf("backfill:%s#%d", importedAt.Format(time.RFC3339), len(imports)+1)
	if prior.last != nil {
		st.Previous = &backfillPrevious{ImportBatch: prior.last.ImportBatch, ImportedAt: prior.last.ImportedAt}
		if at, ok := parseBackfillTime(prior.last.ImportedAt); ok && opt.expectEvery > 0 && importedAt.Sub(at) > 2*opt.expectEvery {
			st.Late = fmt.Sprintf("this pass ran %s after the previous one, more than twice the expected %s: the scheduler was absent for a while, and rows the host expired meanwhile are lost", importedAt.Sub(at).Round(time.Second), opt.expectEvery)
		}
	} else {
		st.FirstPass = true
	}
	for source, c := range prior.passed {
		st.Window[source] = backfillCursor{After: int64Ptr(c)}
	}

	for _, h := range horizons {
		st.Horizons[h.Source] = backfillHorizon{Cursor: *h.Cursor, At: h.At, Limited: h.Limited}
		if h.Limited {
			st.Notes = append(st.Notes, fmt.Sprintf("the reader hit its row limit for %s: the rest waits for the next pass; if this repeats, run passes more often", h.Source))
		}
		// Retention: cursors between the watermark and the oldest row still
		// held were gone before this pass reached them.
		if passed, ok := prior.passed[h.Source]; ok && *h.Cursor > passed+1 {
			st.Gaps = append(st.Gaps, backfillGap{Source: h.Source, FromCursor: int64Ptr(passed + 1), ToCursor: int64Ptr(*h.Cursor - 1),
				Reason: "these cursors were no longer readable when this pass ran: rows the host expired before any pass read them (permanent loss), or ids the host never used"})
		}
	}
	for _, g := range declared {
		st.Gaps = append(st.Gaps, backfillGap{Source: g.Source, Reason: "declared by the reader: " + g.Reason})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return *rows[i].Cursor < *rows[j].Cursor
	})
	st.RowsRead = len(rows)
	var candidates []backfillCandidate
	inFile := map[string]string{}
	for _, r := range rows {
		digest, err := r.sourceDigest()
		if err != nil {
			return backfillResult{}, err
		}
		key := backfillKey(r.Source, r.ID)
		if inFile[key] == digest {
			continue // the same row twice in this file
		}
		inFile[key] = digest
		if pin, ok := prior.pins[key]; ok {
			if pin.digest != digest {
				// Promotion reveals; it never rewrites. The row as read now is
				// not the row Tier A committed, so it is not promoted.
				st.Refused = append(st.Refused, backfillRefused{Source: r.Source, ID: r.ID, Digest: digest, TierADigest: pin.digest, Reason: "differs from the digest Tier A committed for this row: the host changed or replaced it after it was digested"})
				continue
			}
			st.RereadVerified++
			candidates = append(candidates, backfillCandidate{row: r, digest: digest, pinnedBy: pin.batch, retrospective: true})
			continue
		}
		if seen, ok := inFile[key+"\x00tier_a"]; ok && seen != digest {
			st.Refused = append(st.Refused, backfillRefused{Source: r.Source, ID: r.ID, Digest: digest, TierADigest: seen, Reason: "the same id appears twice in this pass with different contents"})
			continue
		}
		inFile[key+"\x00tier_a"] = digest
		if passed, ok := prior.passed[r.Source]; ok && *r.Cursor <= passed {
			st.Anomalies = append(st.Anomalies, backfillAnomaly{Source: r.Source, ID: r.ID, Cursor: *r.Cursor, PassedCursor: passed,
				Reason: "a new row at or below a cursor an earlier pass had already passed: it arrived late, or the reader's earlier query missed it"})
		}
		st.TierA = append(st.TierA, backfillTierA{Source: r.Source, Cursor: *r.Cursor, ID: r.ID, At: r.At, Digest: digest})
		w := st.Window[r.Source]
		if w.Through == nil || *r.Cursor > *w.Through {
			w.Through = int64Ptr(*r.Cursor)
		}
		st.Window[r.Source] = w
		if st.SourceTimeSpan == nil {
			st.SourceTimeSpan = &backfillSpan{Earliest: r.At, Latest: r.At}
		}
		at, _ := parseBackfillTime(r.At)
		if e, _ := parseBackfillTime(st.SourceTimeSpan.Earliest); at.Before(e) {
			st.SourceTimeSpan.Earliest = r.At
		}
		if l, _ := parseBackfillTime(st.SourceTimeSpan.Latest); at.After(l) {
			st.SourceTimeSpan.Latest = r.At
		}
		candidates = append(candidates, backfillCandidate{row: r, digest: digest, pinnedBy: st.ImportBatch})
	}

	// Tier B: every candidate within promote_around of a signaled candidate.
	var signalAt []time.Time
	for _, c := range candidates {
		if len(c.row.Signals) > 0 {
			at, _ := parseBackfillTime(c.row.At)
			signalAt = append(signalAt, at)
		}
	}
	near := func(at time.Time) bool {
		for _, s := range signalAt {
			if d := at.Sub(s); d <= opt.promoteAround && d >= -opt.promoteAround {
				return true
			}
		}
		return false
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].row.At < candidates[j].row.At })
	retained, err := importedSources(ctx, t)
	if err != nil {
		return backfillResult{}, err
	}
	private, err := privateKey(t.profile.Signing)
	if err != nil {
		return backfillResult{}, err
	}
	for _, c := range candidates {
		at, _ := parseBackfillTime(c.row.At)
		if !near(at) {
			continue
		}
		if !at.Before(importedAt) {
			// imported_at must follow source_asserted_at; a row stamped at or
			// after our clock is digested but cannot be retained honestly.
			st.Notes = append(st.Notes, fmt.Sprintf("%s/%s is stamped at or after this pass's clock and was not retained", c.row.Source, c.row.ID))
			continue
		}
		if _, ok := retained[c.digest]; ok {
			st.AlreadyRetained++
			continue
		}
		request, err := backfillRequest(t.profile, c.row, c.digest, st.ImportBatch, c.pinnedBy, importedAt, "self_attested")
		if err != nil {
			return backfillResult{}, err
		}
		published, err := t.publish(ctx, request, private)
		if err != nil {
			return backfillResult{}, err
		}
		retained[c.digest] = published.CapsuleID
		st.Retained = append(st.Retained, backfillRetained{CapsuleID: published.CapsuleID, SourceDigest: c.digest, ActionID: request.Capsule.ActionID, PinnedBy: c.pinnedBy, Retrospective: c.retrospective})
	}
	body, err := json.Marshal(st)
	if err != nil {
		return backfillResult{}, err
	}
	record, err := t.book.book.Append(ctx, evidencebook.Entry{RecordType: recordTypeBackfillImport, EpistemicType: evidencebook.ProducerClaim, Statement: body})
	if err != nil {
		return backfillResult{}, err
	}
	summary := fmt.Sprintf("Digested %d new host records (Tier A) and retained %d as backfilled Capsules (Tier B; %d already retained, %d re-read rows verified); %d refused, %d anomalies, %d gaps declared. Effects only: these say what the host recorded, not what was charged and not who approved it. Detectability of absence, not prevention.",
		len(st.TierA), len(st.Retained), st.AlreadyRetained, st.RereadVerified, len(st.Refused), len(st.Anomalies), len(st.Gaps))
	return backfillResult{Summary: summary, ImportRecordID: record.RecordID, backfillImport: st}, nil
}

// backfillClock is the importer's clock (imported_at); tests pin it.
var backfillClock = time.Now

func openBackfillTarget(c *cobra.Command, use targetUse) (*target, error) {
	p, err := selected(c)
	if err != nil {
		return nil, err
	}
	if p.Type != "jsonl" {
		return nil, inputError("backfill needs a jsonl profile: its import records live in the evidence book")
	}
	if use == usePublication && p.ReadOnly {
		return nil, ErrReadOnlyCLL
	}
	t, err := openTarget(c.Context(), p, use)
	if err != nil {
		return nil, err
	}
	if t.book == nil {
		return nil, errors.Join(inputError("backfill needs the profile's evidence book: run store init"), t.close())
	}
	return t, nil
}

func backfillCommands() *cobra.Command {
	root := &cobra.Command{Use: "backfill", Short: "Import an agent host's own records as backfilled Capsules (effects only)"}
	run := &cobra.Command{Use: "run", Short: "Import one watermark pass of backfill-source-record/v0 lines and commit an import record (signs)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		var opt backfillOptions
		opt.expectEvery, _ = c.Flags().GetDuration("expect-every")
		opt.promoteAround, _ = c.Flags().GetDuration("promote-around")
		opt.now = backfillClock()
		path, _ := c.Flags().GetString("source")
		var in io.Reader = c.InOrStdin()
		if path != "-" {
			f, err := os.Open(path)
			if err != nil {
				return errors.Join(ErrInput, &inputFileError{path: path, notFound: errors.Is(err, os.ErrNotExist)})
			}
			defer f.Close()
			in = f
		}
		rows, gaps, horizons, err := readBackfillSource(in)
		if err != nil {
			return err
		}
		t, err := openBackfillTarget(c, usePublication)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, t.close()) }()
		result, err := runBackfill(c.Context(), t, rows, gaps, horizons, opt)
		if err != nil {
			return err
		}
		if err = output(c, result); err != nil {
			return err
		}
		if len(result.Gaps) > 0 || len(result.Refused) > 0 || len(result.Anomalies) > 0 {
			// Exit 3: the import record is committed, and it declares a gap,
			// a refused row or an anomaly.
			return ErrPartial
		}
		return nil
	}}
	run.Flags().String("source", "-", "backfill-source-record/v0 JSONL file, or - for stdin")
	run.Flags().Duration("expect-every", 0, "How often the scheduler should run this; a pass more than twice as late says so")
	run.Flags().Duration("promote-around", 30*time.Minute, "Retain (Tier B) every row within this long of a row carrying a consequential signal")
	status := &cobra.Command{Use: "status", Short: "List the import records and where the next pass starts", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		t, err := openBackfillTarget(c, usePublication)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, t.close()) }()
		imports, err := backfillImports(c.Context(), t.book.book)
		if err != nil {
			return err
		}
		// cursors is where the next pass starts: per source, rows past this.
		prior := loadBackfillState(imports)
		out := map[string]any{"passes": len(imports), "cursors": prior.passed, "last_pass": nil, "limits": backfillLimits}
		if prior.last != nil {
			gaps, anomalies := 0, 0
			for _, im := range imports {
				gaps += len(im.Gaps)
				anomalies += len(im.Anomalies)
			}
			last := prior.last
			out["last_pass"] = map[string]any{"import_batch": last.ImportBatch, "imported_at": last.ImportedAt, "window": last.Window, "horizons": last.Horizons,
				"tier_a": len(last.TierA), "retained": len(last.Retained), "refused": len(last.Refused), "anomalies": len(last.Anomalies), "gaps": last.Gaps}
			out["gaps_declared"], out["anomalies_declared"] = gaps, anomalies
		}
		return output(c, out)
	}}
	root.AddCommand(run, status)
	return root
}
