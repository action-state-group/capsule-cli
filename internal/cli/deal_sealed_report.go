package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/cll-go/cll"
)

// dealReportActionID is the action_id of the capsule a deal report seals its
// copy's readable text in: an fyi capsule on the deal's own log whose input
// is the deal_report record (dealSealReport). The bundle's x-deal-v0
// extension then only points at it (dealReportPointer), so the text a page
// shows is a disclosed record payload, checked like every other: an edited
// line makes the bundle INVALID.
const dealReportActionID = "capsulectl-deal-report"

// dealReportPointer is the x-deal-v0 extension member naming the copy's
// sealed report capsule.
const dealReportPointer = "sealed_report"

// dealSealReport seals one copy's own deal text (ext: the extension that copy
// would otherwise carry, already scrubbed for a shared copy) as a deal_report
// record and appends it to the deal's own log, and returns its capsule id.
// The record holds a fresh 256-bit nonce, so its digest, which every later
// bundle carries with the record withheld, confirms no guess at the text.
// Its index row is written before the log append, as a step's is; a row
// whose capsule never reached the log is ignored.
func (s *dealSession) dealSealReport(ctx context.Context, dealID, audience string, ext map[string]interface{}) (string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// The record's bytes are its JCS form, as a step's are, read back through
	// the generic JSON shape so numbers are canonicalized as the verifier
	// reads them.
	raw, err := json.Marshal(map[string]interface{}{
		"type": "deal_report", "profile": dealProfile, "deal_id": dealID, "audience": audience,
		"nonce": hex.EncodeToString(nonce), "report": ext,
	})
	if err != nil {
		return "", err
	}
	generic, err := decodeBundleJSON(raw)
	if err != nil {
		return "", err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return "", err
	}
	request := Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: dealReportActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: dealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	record, err := seal(request, s.key)
	if err != nil {
		return "", err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO deal_reports (deal_id, capsule_id, audience, cll_sequence) VALUES (?,?,?,0)`, dealID, record.CapsuleID, audience); err != nil {
		return "", err
	}
	pub, err := s.t.publish(ctx, request, s.key)
	if err == nil && pub.CapsuleID != record.CapsuleID {
		err = ErrConflict
	}
	if err != nil {
		// Never appended: withdraw the row. Appended: keep it, so the next
		// read sets the entry aside as a report.
		if entries, scanErr := s.logEntries(ctx); scanErr == nil && !holdsEntry(entries, record.CapsuleID) {
			_, delErr := s.db.ExecContext(ctx, `DELETE FROM deal_reports WHERE deal_id=? AND capsule_id=?`, dealID, record.CapsuleID)
			err = errors.Join(err, delErr)
		}
		return "", err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE deal_reports SET cll_sequence=? WHERE deal_id=? AND capsule_id=?`, pub.Sequence, dealID, record.CapsuleID); err != nil {
		return "", err
	}
	return record.CapsuleID, nil
}

// reportIDs are the capsule ids of every report sealed for the deal.
func (s *dealSession) reportIDs(ctx context.Context, dealID string) (_ map[string]bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT capsule_id FROM deal_reports WHERE deal_id=?`, dealID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// stepsIn is how many of the deal's steps the first leaves entries of its
// log hold: the entries less its sealed reports'.
func (s *dealSession) stepsIn(ctx context.Context, dealID string, leaves uint64) (uint64, error) {
	var reports uint64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_reports WHERE deal_id=? AND cll_sequence>0 AND cll_sequence<=?`, dealID, leaves).Scan(&reports)
	if err != nil || reports > leaves {
		return 0, errors.Join(err, ErrConflict)
	}
	return leaves - reports, nil
}

func holdsEntry(entries []cll.Entry, capsuleID string) bool {
	for _, e := range entries {
		if hex.EncodeToString(e.Value) == capsuleID {
			return true
		}
	}
	return false
}

// dealReportOf is the deal text a report bundle carries: the input of the
// sealed report its x-deal-v0 extension points at, when that is disclosed;
// for a bundle written before reports were sealed, the extension itself;
// nil when the pointer names nothing disclosed.
func dealReportOf(b map[string]interface{}) map[string]interface{} {
	ext, _ := b["extensions"].(map[string]interface{})
	deal, _ := ext[dealProfile].(map[string]interface{})
	id, sealed := deal[dealReportPointer].(string)
	if !sealed {
		return deal
	}
	disclosures, _ := b["disclosures"].(map[string]interface{})
	d, _ := disclosures[id].(map[string]interface{})
	input, _ := d["agent_input"].(map[string]interface{})
	report, _ := input["report"].(map[string]interface{})
	return report
}
