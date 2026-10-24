package deal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/cli"
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
	at := cli.DealClock().UTC().Truncate(time.Second)
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: cli.DealReportActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: at,
		},
		Payload: payload,
	}
	if s.onBook {
		if s.book == nil {
			return "", cli.ErrReadOnlyCLL
		}
		return s.sealBookReport(ctx, at, request)
	}
	record, err := cli.Seal(request, s.key)
	if err != nil {
		return "", err
	}
	if err = s.index.prepareReport(ctx, dealID, record.CapsuleID, audience); err != nil {
		return "", err
	}
	pub, err := s.appendLegacy(ctx, func() (cli.Publication, error) { return s.t.Publish(ctx, request, s.key) })
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

// dealReportOf is the deal text a report bundle carries: the input of its
// sealed report, in the log or standalone (cli.DealSealedReport); for a
// bundle written before reports were sealed, the extension itself; nil when
// the pointer names nothing the copy carries.
func dealReportOf(b map[string]interface{}) map[string]interface{} {
	report, _, _ := cli.DealSealedReport(b)
	return report
}

// dealStandaloneReport seals one copy's own deal text as a deal_report
// Capsule that no log holds (capsule-cli#294), bound to the deal checkpoint
// b is assembled at: so a deal's log, its copies and its History never show
// when or how often it was shared, and the copy carries this report only.
// It returns the report's capsule id and the copy's x-deal-report/v0
// section. Nothing is stored: a later copy seals its own.
func (s *dealSession) dealStandaloneReport(dealID, audience string, ext, b map[string]interface{}) (string, map[string]interface{}, error) {
	bound, err := dealBoundCheckpoint(b)
	if err != nil {
		return "", nil, err
	}
	// The report is signed by the key that signed the deal's last step, or
	// a copy made now would not verify (DealReportEntry check 2).
	switch last := cli.DealLastStepKey(b); {
	case last == "":
		return "", nil, cli.Hint(cli.ErrConflict, "the copy's last step does not verify, so no report can be bound to it")
	case last != hex.EncodeToString(s.key.Public().(ed25519.PublicKey)):
		return "", nil, errDealKeyChanged
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(map[string]interface{}{
		"type": "deal_report", "profile": cli.DealProfile, "deal_id": dealID, "audience": audience,
		"nonce": hex.EncodeToString(nonce), "report": ext, "deal_checkpoint": bound,
	})
	if err != nil {
		return "", nil, err
	}
	generic, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return "", nil, err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return "", nil, err
	}
	record, err := cli.Seal(cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: cli.DealReportActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: cli.DealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}, s.key)
	if err != nil {
		return "", nil, err
	}
	capsule, err := cli.DecodeBundleJSON(record.Capsule)
	if err != nil {
		return "", nil, err
	}
	capsule["signature"] = hex.EncodeToString(record.ProducerEnvelope)
	capsule["key_id"] = hex.EncodeToString(s.key.Public().(ed25519.PublicKey))
	input, err := cli.DecodeBundleJSON(payload)
	if err != nil {
		return "", nil, err
	}
	return record.CapsuleID, map[string]interface{}{"record": capsule, "agent_input": input}, nil
}

// checkProducedReport runs a produced copy's standalone report checks over
// its JSON form, as a reader decodes it: a copy that would not verify is
// refused, never written.
func checkProducedReport(b map[string]interface{}) error {
	// Only the standalone report the producer sealed: a bundle with none
	// (the terminal-assurance bundle carries its text inline, and is never
	// a copy) has nothing here to check.
	if exts, _ := b["extensions"].(map[string]interface{}); exts[cli.DealReportExtension] == nil {
		return nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	decoded, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return err
	}
	if err = cli.CheckDealStandaloneReport(decoded); err != nil {
		return cli.Hint(cli.ErrConflict, "the copy's report does not verify against its own checkpoint, so no copy is written: "+err.Error())
	}
	return nil
}

// errDealKeyChanged refuses a copy of a deal whose last step was signed
// with a key the profile no longer signs with: its standalone report could
// not be signed by that step's key.
var errDealKeyChanged = cli.InputError("this deal's last step was signed with an earlier key, so a copy made now would not verify; share it from the store that holds that key")

// dealBoundCheckpoint is the deal checkpoint a bundle is assembled at, as a
// standalone report binds it: the certificate's log id, and the
// checkpoint's MMR size and root.
func dealBoundCheckpoint(b map[string]interface{}) (map[string]interface{}, error) {
	cp, _ := b["checkpoint"].(map[string]interface{})
	cert, _ := b["completeness_certificate"].(map[string]interface{})
	logID, _ := cert["log_id"].(string)
	root, _ := cp["root"].(string)
	size, err := cli.JSONUint(cp["mmr_size"])
	if err != nil || logID == "" || root == "" {
		return nil, errors.Join(cli.ErrConflict, err)
	}
	return map[string]interface{}{"log_id": logID, "mmr_size": cli.Integer(size), "root": root}, nil
}

// bundleCheckpointTime is the signed time of the checkpoint a bundle is
// assembled at.
func bundleCheckpointTime(b map[string]interface{}) (time.Time, error) {
	cp, _ := b["checkpoint"].(map[string]interface{})
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(cp["timestamp"]))
	if err != nil {
		return time.Time{}, errors.Join(cli.ErrConflict, err)
	}
	return at.UTC(), nil
}
