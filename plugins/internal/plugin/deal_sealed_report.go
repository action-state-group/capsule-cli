package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
)

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
		"type": "deal_report", "profile": cli.DealProfile, "deal_id": dealID, "audience": audience,
		"nonce": hex.EncodeToString(nonce), "report": ext,
	})
	if err != nil {
		return "", err
	}
	generic, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return "", err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return "", err
	}
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: cli.DealReportActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: cli.DealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	record, err := cli.Seal(request, s.key)
	if err != nil {
		return "", err
	}
	if err = s.index.prepareReport(ctx, dealID, record.CapsuleID, audience); err != nil {
		return "", err
	}
	pub, err := s.t.Publish(ctx, request, s.key)
	if err == nil && pub.CapsuleID != record.CapsuleID {
		err = cli.ErrConflict
	}
	if err != nil {
		// Never appended: withdraw the row. Appended: keep it, so the next
		// read sets the entry aside as a report.
		if entries, scanErr := s.logEntries(ctx); scanErr == nil && !holdsEntry(entries, record.CapsuleID) {
			err = errors.Join(err, s.index.withdrawReport(ctx, dealID, record.CapsuleID))
		}
		return "", err
	}
	if err = s.index.publishedReport(ctx, dealID, record.CapsuleID, pub.Sequence); err != nil {
		return "", err
	}
	return record.CapsuleID, nil
}

// reportIDs are the capsule ids of every report sealed for the deal.
func (s *dealSession) reportIDs(ctx context.Context, dealID string) (map[string]bool, error) {
	return s.index.reportIDs(ctx, dealID)
}

// stepsIn is how many of the deal's steps the first leaves entries of its
// log hold: the entries less its sealed reports'.
func (s *dealSession) stepsIn(ctx context.Context, dealID string, leaves uint64) (uint64, error) {
	n, err := s.index.reportsThrough(ctx, dealID, leaves)
	reports := uint64(n)
	if err != nil || reports > leaves {
		return 0, errors.Join(err, cli.ErrConflict)
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
	deal, _ := ext[cli.DealProfile].(map[string]interface{})
	id, sealed := deal[cli.DealReportPointer].(string)
	if !sealed {
		return deal
	}
	disclosures, _ := b["disclosures"].(map[string]interface{})
	d, _ := disclosures[id].(map[string]interface{})
	input, _ := d["agent_input"].(map[string]interface{})
	report, _ := input["report"].(map[string]interface{})
	return report
}
