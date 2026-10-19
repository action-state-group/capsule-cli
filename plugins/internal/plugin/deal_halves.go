package plugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
)

// The deal-half reconcile (design note §2.9): each party's final close of a
// deal is its half, and two halves agree when both parties closed on the same
// money terms. It is an internal engine in PR3, reached by no command; the
// claim that cites the deal reconcile contract (AAC u436) is built on it in a
// follow-up, where it is the Result builder's recompute path.

// dealPeerHalf is the counterparty's side of the reconcile, every input
// explicit: the deal, the response this profile recorded for its request for
// the counterparty's copy (whose kept reply holds the verified copy and whose
// record holds the pinned signing key), the record in that copy taken as the
// counterparty's half, and the openings of that record's fields.
type dealPeerHalf struct {
	DealID         string
	ResponseRecord string
	RecordID       string
	Openings       map[string]evidencebook.Opening
}

// dealHalfFields are the fields two halves are compared on.
var dealHalfFields = []string{dealAmountField, dealCurrencyField}

// dealHalfCheck is OpenedFieldsAgree on the money terms, except that an
// opened empty text (a step that is not a received close) is no value: the
// pair is Undecided (INSUFFICIENT), never two empty texts agreeing.
func dealHalfCheck(a, b evidencebook.Half) evidencebook.ContentVerdict {
	for _, h := range []evidencebook.Half{a, b} {
		for _, f := range dealHalfFields {
			if text, opened := h.Opened[f]; opened && text == "" {
				return evidencebook.Undecided
			}
		}
	}
	return evidencebook.OpenedFieldsAgree(dealHalfFields...)(a, b)
}

// dealHalfCorrelation pairs the two halves of a deal in memory only: it is
// never written to a header, where it would mark which withheld step is the
// close (note §2.9).
func dealHalfCorrelation(dealID string) evidencebook.Correlation {
	return evidencebook.Correlation{ExchangeID: "deal:" + dealID + ":close"}
}

// reconcileDealHalves reconciles this profile's half of a book deal with the
// counterparty's: the session must have the deal in use (useDeal).
func (s *dealSession) reconcileDealHalves(ctx context.Context, dealID string, peer dealPeerHalf) ([]evidencebook.PairResult, evidencebook.Tallies, error) {
	if !s.onBook || s.book == nil {
		return nil, evidencebook.Tallies{}, cli.InputError("deal " + dealID + " is a legacy deal: it has no record header and no field commitments, so it has no halves")
	}
	own, err := s.ownHalf(ctx, dealID)
	if err != nil {
		return nil, evidencebook.Tallies{}, err
	}
	theirs, err := s.peerHalf(ctx, dealID, peer)
	if err != nil {
		return nil, evidencebook.Tallies{}, err
	}
	a := evidencebook.HalfSet{}
	if own != nil {
		a = evidencebook.HalfSet{Halves: []evidencebook.Half{*own}, Complete: own.Covered}
	}
	// A copy can never prove the counterparty has no close: withheld steps
	// share one header shape. So their side is never complete.
	b := evidencebook.HalfSet{Halves: []evidencebook.Half{theirs}}
	results, tallies := evidencebook.ReconcileHalvesWith(a, b, dealHalfCheck)
	return results, tallies, nil
}

// ownHalf is this profile's final close as its half, or nil when the deal
// has none.
func (s *dealSession) ownHalf(ctx context.Context, dealID string) (*evidencebook.Half, error) {
	events, err := s.loadBook(ctx, dealID)
	if err != nil {
		return nil, err
	}
	i := finalClose(events)
	if i < 0 {
		return nil, nil
	}
	x := s.index.(shelfDealIndex)
	records, err := x.records(ctx, dealID)
	if err != nil {
		return nil, err
	}
	var record *evidencebook.ShelfRecord
	for j := range records {
		if records[j].RecordID == events[i].CapsuleID {
			record = &records[j]
		}
	}
	if record == nil {
		return nil, cli.ErrConflict
	}
	checkpoints, err := s.book.Checkpoints(ctx)
	if err != nil {
		return nil, err
	}
	covered := false
	for _, cp := range checkpoints {
		covered = covered || cp.Entries >= record.Seq
	}
	openings := map[string]evidencebook.Opening{}
	for _, f := range dealHalfFields {
		o, err := x.openingOf(ctx, *record, f)
		if err != nil {
			return nil, err
		}
		openings[f] = o
	}
	// loadBook re-verified the close's Capsule under the profile's keys.
	half := evidencebook.OpenHalf(evidencebook.Half{
		RecordID: record.RecordID, Seq: record.Seq, Correlation: dealHalfCorrelation(dealID),
		PayloadCommitments: record.Header.PayloadCommitments, FieldCommitments: record.Header.FieldCommitments,
		Covered: covered, Trusted: true,
	}, openings)
	return &half, nil
}

// peerHalf is the counterparty's half: the selected record of the copy this
// profile recorded, checked against every peer input (note §2.9).
func (s *dealSession) peerHalf(ctx context.Context, dealID string, peer dealPeerHalf) (evidencebook.Half, error) {
	if peer.DealID != dealID {
		return evidencebook.Half{}, cli.InputError("the counterparty's half is of deal " + peer.DealID + ", not " + dealID)
	}
	rows, err := s.exchangeRows(ctx, `kind=? AND record_digest=?`, dealExchangeResponse, peer.ResponseRecord)
	if err != nil {
		return evidencebook.Half{}, err
	}
	if len(rows) != 1 || rows[0].dealID != dealID {
		return evidencebook.Half{}, cli.InputError(peer.ResponseRecord + " is no response this profile recorded for deal " + dealID + ": record the counterparty's copy with `deal request --response`")
	}
	row := rows[0]
	if row.record["outcome"] != evidencebook.OutcomeArtifact {
		return evidencebook.Half{}, cli.InputError("response " + peer.ResponseRecord + " is not a verified copy (outcome " + strconv.Quote(fmtAny(row.record["outcome"])) + ")")
	}
	raw, held, err := s.keptReply(ctx, &row)
	if err != nil {
		return evidencebook.Half{}, err
	}
	if !held {
		return evidencebook.Half{}, cli.InputError("response " + peer.ResponseRecord + "'s octets are not in the artifact store")
	}
	var resp dealResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Artifact == nil {
		return evidencebook.Half{}, cli.ErrConflict
	}
	bundle, err := cli.DecodeBundleJSON(resp.Artifact.Artifact)
	if err != nil {
		return evidencebook.Half{}, cli.ErrConflict
	}
	own, err := s.index.(shelfDealIndex).records(ctx, dealID)
	if err != nil {
		return evidencebook.Half{}, err
	}
	for _, r := range own {
		if r.RecordID == peer.RecordID {
			return evidencebook.Half{}, cli.InputError("record " + peer.RecordID + " is on this profile's own book: the counterparty's half is a record of their copy")
		}
	}
	capsule := copyRecord(bundle, peer.RecordID)
	if capsule == nil {
		return evidencebook.Half{}, cli.InputError("the recorded copy holds no record " + peer.RecordID)
	}
	var header evidencebook.Header
	rawHeader, _ := json.Marshal(capsule[evidencebook.RecordHeaderMember])
	if json.Unmarshal(rawHeader, &header) != nil || header.BookID != cli.DealLogID(dealID) {
		return evidencebook.Half{}, cli.InputError("record " + peer.RecordID + " carries no record header of deal " + dealID)
	}
	for _, f := range dealHalfFields {
		if _, ok := header.FieldCommitments[f]; !ok {
			return evidencebook.Half{}, cli.InputError("record " + peer.RecordID + " commits no " + f)
		}
	}
	checkpoint, _ := bundle["checkpoint"].(map[string]interface{})
	size, _ := cli.JSONUint(checkpoint["mmr_size"])
	half := evidencebook.Half{
		RecordID: peer.RecordID, Seq: header.Seq, Correlation: dealHalfCorrelation(dealID),
		PayloadCommitments: header.PayloadCommitments, FieldCommitments: header.FieldCommitments,
		Covered: header.Seq >= 1 && header.Seq <= mmrLeafCount(size),
		Trusted: signedBy(capsule, peer.RecordID, fmtAny(row.record["key_id"])),
	}
	half = evidencebook.OpenHalf(half, peer.Openings)
	if half.Trusted && asserts(half) && !disclosedCloseAgrees(bundle, peer.RecordID, half) {
		half.Trusted = false
	}
	return half, nil
}

// copyRecord is the capsule a copy carries for id.
func copyRecord(bundle map[string]interface{}, id string) map[string]interface{} {
	records, _ := bundle["records"].([]interface{})
	for _, r := range records {
		if m, ok := r.(map[string]interface{}); ok && m["capsule_id"] == id {
			return m
		}
	}
	return nil
}

// signedBy is whether a capsule's producer signature verifies under the
// pinned key (hex): verifyDealCopy pins only the checkpoint key, and the
// bundle check verifies each capsule against the key it declares itself.
func signedBy(capsule map[string]interface{}, id, pinned string) bool {
	sig, _ := capsule["signature"].(string)
	key, _ := capsule["key_id"].(string)
	envelopeBytes, err := hex.DecodeString(sig)
	if err != nil || key != pinned {
		return false
	}
	keyBytes, err := hex.DecodeString(pinned)
	if err != nil {
		return false
	}
	result := envelope.Verify(id, envelopeBytes)
	return result.OK && bytes.Equal(result.PublicKey, keyBytes)
}

// asserts is whether a half opened a value: a non-empty verified opening,
// which only a received close carries.
func asserts(h evidencebook.Half) bool {
	for _, f := range dealHalfFields {
		if h.Opened[f] != "" {
			return true
		}
	}
	return false
}

// disclosedCloseAgrees cross-checks a half that asserts a value against
// what the copy discloses, and only that (note §2.9): a disclosed record
// must be a close whose outcome is completed or mismatch; the outcome record
// it refs, found by digest among the disclosed payloads, must have status
// received; and its delivered price and currency, where it states them, must
// equal the verified opened texts. Anything withheld is no finding.
func disclosedCloseAgrees(bundle map[string]interface{}, id string, h evidencebook.Half) bool {
	disclosed := disclosedPayloads(bundle)
	closeRecord, ok := disclosed[id]
	if !ok {
		return true
	}
	block, _ := closeRecord[cli.DealProfile].(map[string]interface{})
	body, _ := closeRecord["body"].(map[string]interface{})
	if block["record_type"] != "close" || (body["outcome"] != "completed" && body["outcome"] != "mismatch") {
		return false
	}
	refs, _ := block["refs"].([]interface{})
	for _, r := range refs {
		ref, _ := r.(map[string]interface{})
		if ref["rel"] != "outcome" {
			continue
		}
		digest, _ := ref["digest"].(string)
		for _, record := range disclosed {
			if d, err := canonical.JSONDigest(record); err != nil || d != digest {
				continue
			}
			outcome, _ := record["body"].(map[string]interface{})
			if outcome["status"] != "received" {
				return false
			}
			delivered, _ := outcome["delivered"].(map[string]interface{})
			if price, ok := delivered["price_minor"]; ok {
				if text, opened := h.Opened[dealAmountField]; opened && fmtAny(price) != text {
					return false
				}
			}
			if currency, ok := delivered["currency"].(string); ok {
				if text, opened := h.Opened[dealCurrencyField]; opened && currency != text {
					return false
				}
			}
		}
	}
	return true
}

// disclosedPayloads are the payloads a copy discloses, by capsule id.
func disclosedPayloads(bundle map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	disclosures, _ := bundle["disclosures"].(map[string]interface{})
	for id, d := range disclosures {
		m, _ := d.(map[string]interface{})
		if input, ok := m["agent_input"].(map[string]interface{}); ok {
			out[id] = input
		}
	}
	return out
}

// fmtAny is a JSON value as text: a number as its decimal digits.
func fmtAny(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
