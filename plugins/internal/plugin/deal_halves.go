package plugin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/evidencebook"
)

// The deal reconcile (design note §2.10): each party's final close of a deal
// is its half, and the two halves are reconciled under the Deal Reconcile
// Contract, ec:deal-reconcile@1, by evidencebook's recompute. This file reads
// the halves and admits them; deal_terms_result.go is the verb.
//
// A half is the close's bundle record, the object cli.GetCapsule builds and
// every copy carries: the Capsule with its producer signature and key_id.
// Its octets are the JCS of that object and its digest their SHA-256, so
// both parties compute one digest from one record, whichever holds it.

// dealHalfFields are the fields two halves are compared on.
var dealHalfFields = []string{dealAmountField, dealCurrencyField}

// dealHalfMapping is the deal profile's half mapping (DR §4.2), which
// PROFILE.md declares. book_id is the key the record's producer signature
// verified under: both parties' books are named deal/<deal_id>, so the
// signer is the one per-party identity a half carries.
var dealHalfMapping = evidencebook.HalfMapping{Profile: cli.DealProfile, Members: map[string]string{
	evidencebook.HalfMemberDealID:             "/deal_id",
	evidencebook.HalfMemberBookID:             "/key_id",
	evidencebook.HalfMemberRecordedAt:         "/" + evidencebook.RecordHeaderMember + "/committed_at",
	evidencebook.HalfMemberCommitAlg:          "/" + evidencebook.RecordHeaderMember + "/commit_alg",
	evidencebook.HalfMemberAmountCommitment:   "/" + evidencebook.RecordHeaderMember + "/field_commitments/" + dealAmountField,
	evidencebook.HalfMemberCurrencyCommitment: "/" + evidencebook.RecordHeaderMember + "/field_commitments/" + dealCurrencyField,
}}

// dealHalf is one admitted half.
type dealHalf struct {
	recordID string
	// book is the hex key the half's producer signature verified under.
	book       string
	octets     []byte
	digest     string
	recordedAt time.Time
	header     evidencebook.Header
}

// halfOf is the half a bundle record makes.
func halfOf(record map[string]interface{}) (dealHalf, error) {
	octets, err := canonical.JCS(record)
	if err != nil {
		return dealHalf{}, err
	}
	id, _ := record["capsule_id"].(string)
	book, _ := record["key_id"].(string)
	h := dealHalf{recordID: id, book: book, octets: octets, digest: cli.BytesDigest(octets)}
	h.header, err = inlineHeader(record)
	if err != nil {
		return dealHalf{}, err
	}
	h.recordedAt, err = time.Parse(time.RFC3339, h.header.CommittedAt)
	if err != nil {
		return dealHalf{}, cli.InputError("record " + id + " carries no committed_at")
	}
	return h, nil
}

// inlineHeader is the record header a Capsule carries inline: it is a
// Capsule member, so the Capsule's signature covers it.
func inlineHeader(record map[string]interface{}) (evidencebook.Header, error) {
	var header evidencebook.Header
	raw, err := json.Marshal(record[evidencebook.RecordHeaderMember])
	if err != nil || record[evidencebook.RecordHeaderMember] == nil || json.Unmarshal(raw, &header) != nil {
		id, _ := record["capsule_id"].(string)
		return evidencebook.Header{}, cli.InputError("record " + id + " carries no record header")
	}
	return header, nil
}

// ownDealHalf is this profile's final close of a book deal as its half, with
// its openings from the artifact store, or nil when the deal has no final
// close. The session must have the deal in use (useDeal).
func (s *dealSession) ownDealHalf(ctx context.Context, dealID string) (*dealHalf, map[string]evidencebook.Opening, error) {
	if !s.onBook {
		return nil, nil, cli.InputError("deal " + dealID + " is a legacy deal: it has no record header and no field commitments, so it has no halves")
	}
	if s.book == nil {
		return nil, nil, cli.ErrReadOnlyCLL
	}
	// loadBook verifies every step's Capsule under the profile's keys.
	events, err := s.loadBook(ctx, dealID)
	if err != nil {
		return nil, nil, err
	}
	i := finalClose(events)
	if i < 0 {
		return nil, nil, nil
	}
	x := s.index.(shelfDealIndex)
	records, err := x.records(ctx, dealID)
	if err != nil {
		return nil, nil, err
	}
	var record *evidencebook.ShelfRecord
	for j := range records {
		if records[j].RecordID == events[i].CapsuleID {
			record = &records[j]
		}
	}
	if record == nil {
		return nil, nil, cli.ErrConflict
	}
	checkpoints, err := s.book.Checkpoints(ctx)
	if err != nil {
		return nil, nil, err
	}
	covered := false
	for _, cp := range checkpoints {
		covered = covered || cp.Entries >= record.Seq
	}
	if !covered {
		return nil, nil, cli.InputError("the deal's final close is not under a checkpoint yet: run `deal checkpoint` first")
	}
	capsule, err := cli.GetCapsule(ctx, s.t.Artifacts, record.RecordID)
	if err != nil {
		return nil, nil, err
	}
	half, err := halfOf(capsule)
	if err != nil {
		return nil, nil, err
	}
	if half.book != hex.EncodeToString(s.key.Public().(ed25519.PublicKey)) {
		return nil, nil, cli.Hint(cli.ErrConflict, "the deal's final close is not signed under this profile's signing key")
	}
	if half.header.RecordType != dealStepRecordType || half.header.BookID != cli.DealLogID(dealID) {
		return nil, nil, cli.ErrConflict
	}
	openings := map[string]evidencebook.Opening{}
	for _, f := range dealHalfFields {
		if openings[f], err = x.openingOf(ctx, *record, f); err != nil {
			return nil, nil, err
		}
	}
	return &half, openings, nil
}

// dealPeerCopy is the counterparty's copy of a deal as this profile recorded
// it (`deal request --response`, outcome artifact), with the responder key
// that response pinned.
type dealPeerCopy struct {
	bundle map[string]interface{}
	pinned string
}

// peerCopy reads the verified copy recorded for a response of the deal.
func (s *dealSession) peerCopy(ctx context.Context, dealID, response string) (dealPeerCopy, error) {
	rows, err := s.exchangeRows(ctx, `kind=? AND record_digest=?`, dealExchangeResponse, response)
	if err != nil {
		return dealPeerCopy{}, err
	}
	if len(rows) != 1 || rows[0].dealID != dealID {
		return dealPeerCopy{}, cli.InputError(response + " is no response this profile recorded for deal " + dealID + ": record the counterparty's copy with `deal request --response`")
	}
	row := rows[0]
	if row.record["outcome"] != evidencebook.OutcomeArtifact {
		return dealPeerCopy{}, cli.InputError("response " + response + " is not a verified copy (outcome " + strconv.Quote(fmtAny(row.record["outcome"])) + ")")
	}
	raw, held, err := s.keptReply(ctx, &row)
	if err != nil {
		return dealPeerCopy{}, err
	}
	if !held {
		return dealPeerCopy{}, cli.InputError("response " + response + "'s octets are not in the artifact store")
	}
	var resp dealResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Artifact == nil {
		return dealPeerCopy{}, cli.ErrConflict
	}
	bundle, err := cli.DecodeBundleJSON(resp.Artifact.Artifact)
	if err != nil {
		return dealPeerCopy{}, cli.ErrConflict
	}
	return dealPeerCopy{bundle: bundle, pinned: fmtAny(row.record["key_id"])}, nil
}

// copyHalves are a copy's records as halves, by half digest. A record that
// carries no readable header is left out here: the finality check refuses
// a copy where one follows the candidate.
func copyHalves(bundle map[string]interface{}) map[string]dealHalf {
	out := map[string]dealHalf{}
	records, _ := bundle["records"].([]interface{})
	for _, r := range records {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if h, err := halfOf(m); err == nil {
			out[h.digest] = h
		}
	}
	return out
}

// admitPeerHalf admits the record of the counterparty's copy whose half
// digest the counterparty's openings name (note §2.10 Decision 2, peer
// checks 1 to 5). Any failed check refuses the reconcile.
func admitPeerHalf(dealID string, peer dealPeerCopy, digest string, openings map[string]evidencebook.Opening) (dealHalf, error) {
	half, ok := copyHalves(peer.bundle)[digest]
	if !ok {
		return dealHalf{}, cli.InputError("the counterparty's openings name " + digest + ", which is no record of the copy recorded for the response")
	}
	record := copyRecord(peer.bundle, half.recordID)
	// 1. Signed under the responder key the response pinned.
	if half.book != peer.pinned || !signedBy(record, half.recordID, peer.pinned) {
		return dealHalf{}, cli.InputError("record " + half.recordID + " is not signed under the responder key pinned for the response")
	}
	// 2. A step of this deal's book.
	if half.header.RecordType != dealStepRecordType || half.header.BookID != cli.DealLogID(dealID) {
		return dealHalf{}, cli.InputError("record " + half.recordID + " is not a step of deal " + dealID)
	}
	// 3. Under the copy's verified checkpoint, at the position the copy's
	// verified membership gives it.
	checkpoint, _ := peer.bundle["checkpoint"].(map[string]interface{})
	size, _ := cli.JSONUint(checkpoint["mmr_size"])
	leaves := mmrLeafCount(size)
	if !boundSeq(peer.bundle, half.recordID, half.header) || half.header.Seq < 1 || half.header.Seq > leaves {
		return dealHalf{}, cli.InputError("record " + half.recordID + " is not under the copy's checkpoint")
	}
	// 4. The final close as far as the copy shows.
	if err := lastStep(peer.bundle, half, leaves); err != nil {
		return dealHalf{}, err
	}
	// 5. What the copy discloses of it, against the openings that verify.
	if err := disclosedCloseHolds(peer.bundle, half, openings); err != nil {
		return dealHalf{}, err
	}
	return half, nil
}

// lastStep is check 4, read from the Capsules and their inline headers,
// which every copy carries whether or not a payload is withheld: the
// candidate chains follows, and every later deal_step under the checkpoint
// chains confirms to it. A finally closed deal takes later evidence only,
// linked to the close (deal.go, `deal note`), so the final close is its
// last step chained follows. Sealed reports carry no link and are passed
// over; any other later record fails the check. A copy carries every record
// of the log up to its checkpoint, so one missing after the candidate fails
// it too: the missing record could be a later step.
func lastStep(bundle map[string]interface{}, half dealHalf, leaves uint64) error {
	refuse := cli.InputError("record " + half.recordID + " is not the counterparty's final close as their copy shows it: name the close the deal ended with")
	if relation, _ := chainOf(copyRecord(bundle, half.recordID)); relation != "follows" {
		return refuse
	}
	records, _ := bundle["records"].([]interface{})
	held := map[uint64]bool{}
	for _, r := range records {
		m, _ := r.(map[string]interface{})
		if m == nil || m["capsule_id"] == half.recordID {
			continue
		}
		header, err := inlineHeader(m)
		id, _ := m["capsule_id"].(string)
		if err != nil || !boundSeq(bundle, id, header) {
			return refuse
		}
		if header.Seq <= half.header.Seq || header.Seq > leaves {
			continue
		}
		held[header.Seq] = true
		switch header.RecordType {
		case dealReportRecordType:
		case dealStepRecordType:
			if relation, parent := chainOf(m); relation != "confirms" || parent != half.recordID {
				return refuse
			}
		default:
			return refuse
		}
	}
	for seq := half.header.Seq + 1; seq <= leaves; seq++ {
		if !held[seq] {
			return refuse
		}
	}
	return nil
}

// boundSeq is whether a record's inline header names the log position the
// copy's verified membership gives it: the bundle verifier checks each
// record's log_coordinates against the checkpoint, never the header's seq,
// so a seq is read only once it is bound to them.
func boundSeq(bundle map[string]interface{}, id string, header evidencebook.Header) bool {
	certificate, _ := bundle["completeness_certificate"].(map[string]interface{})
	memberships, _ := certificate["memberships"].(map[string]interface{})
	membership, _ := memberships[id].(map[string]interface{})
	coordinates, _ := membership["log_coordinates"].(map[string]interface{})
	seq, err := cli.JSONUint(coordinates["seq"])
	return err == nil && seq == header.Seq
}

// chainOf is a Capsule's chain relation and parent.
func chainOf(record map[string]interface{}) (relation, parent string) {
	chain, _ := record["chain"].(map[string]interface{})
	relation, _ = chain["relation"].(string)
	parent, _ = chain["parent_capsule_id"].(string)
	return relation, parent
}

// disclosedCloseHolds is check 5. A disclosed payload must be a close whose
// outcome is not open. Its money terms are compared only with the fields
// whose opening was carried and verifies against the half's own commitment:
// a missing or failed opening is left to the contract's rules 3 and 4. A
// withheld payload is no finding.
func disclosedCloseHolds(bundle map[string]interface{}, half dealHalf, openings map[string]evidencebook.Opening) error {
	disclosed, ok := disclosedPayloads(bundle)[half.recordID]
	if !ok {
		return nil
	}
	block, _ := disclosed[cli.DealProfile].(map[string]interface{})
	body, _ := disclosed["body"].(map[string]interface{})
	if block["record_type"] != "close" || body["outcome"] == "open" {
		return cli.InputError("record " + half.recordID + " is not a final close: the copy discloses it")
	}
	verified := evidencebook.Half{Opened: map[string]string{}}
	if half.header.CommitAlg == evidencebook.CommitAlgNonce256 {
		for f, o := range openings {
			if c, ok := half.header.FieldCommitments[f]; ok && evidencebook.VerifyOpening(c, o) {
				verified.Opened[f] = o.Text
			}
		}
	}
	if asserts(verified) && !disclosedCloseAgrees(bundle, half.recordID, verified) {
		return cli.InputError("the counterparty's opened terms contradict the close their copy discloses")
	}
	return nil
}

// dealOpeningsFile is the field_openings a `disclose --share counterparty`
// file carries (note §2.10 Decision 3), by half digest. Only they are read
// from it: nothing else in the file is trusted.
func dealOpeningsFile(raw []byte) (map[string]map[string]evidencebook.Opening, error) {
	var file struct {
		FieldOpenings map[string]map[string]evidencebook.Opening `json:"field_openings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&file); err != nil {
		return nil, cli.InputError("the openings file is not a deal copy: " + err.Error())
	}
	return file.FieldOpenings, nil
}

// soleDigest is the one half a set of field openings names, or "" when it
// names none; more than one is refused, since at most one record can be the
// final close.
func soleDigest(openings map[string]map[string]evidencebook.Opening) (string, error) {
	digests := make([]string, 0, len(openings))
	for d := range openings {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	switch len(digests) {
	case 0:
		return "", nil
	case 1:
		return digests[0], nil
	}
	return "", cli.InputError("the openings name more than one record; a deal copy carries openings for its final close only")
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
			// The ref can name an unchecked act instead (deal_profile.go:
			// 1155-1158): only an outcome record, x-deal-v0 or typed, states
			// a status and delivered terms to check.
			if !isOutcomeRecord(record) {
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

// isOutcomeRecord is whether a deal record is an outcome: an x-deal-v0
// record of type outcome, or the typed action-outcome record it maps to.
func isOutcomeRecord(record map[string]interface{}) bool {
	block, _ := record[cli.DealProfile].(map[string]interface{})
	return block["record_type"] == "outcome" || record["type"] == cli.TypeActionOutcome
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

// closeOpenings are the field_openings a `disclose --share counterparty`
// copy carries (note §2.10 Decision 3): the openings of the deal's final
// close, under its half digest in the copy, when the close commits non-empty
// money terms and the amount written as money passes the counterparty's
// ceiling gate. When it carries none, why says so, for the sharer: the
// counterparty's reconcile is then not evaluable.
func (s *dealSession) closeOpenings(ctx context.Context, dealID string, events []sealedEvent, b map[string]interface{}) (openings map[string]map[string]evidencebook.Opening, why string, err error) {
	if !s.onBook {
		return nil, "none: a deal opened before the shelf has no openings", nil
	}
	own, held, err := s.ownDealHalf(ctx, dealID)
	switch {
	case errors.Is(err, cli.ErrInput):
		return nil, "none: " + cli.SafeError(err), nil
	case err != nil:
		return nil, "", err
	case own == nil:
		return nil, "none: the deal has no final close yet", nil
	}
	amount, currency := held[dealAmountField].Text, held[dealCurrencyField].Text
	if amount == "" || currency == "" {
		return nil, "none: the final close commits no money terms (it was not received, or names no price or currency)", nil
	}
	minor, err := strconv.ParseInt(amount, 10, 64)
	if err != nil {
		return nil, "", cli.ErrConflict
	}
	if dealCeilingGate([]byte(formatMoney(minor, currency)), events, cli.DealAudienceCounterparty) != nil {
		return nil, "none: the amount, written as money, is one the counterparty share keeps from them (a limit or floor of yours), so the counterparty cannot reconcile the terms", nil
	}
	record := copyRecord(b, own.recordID)
	if record == nil {
		return nil, "", cli.ErrConflict
	}
	half, err := halfOf(record)
	if err != nil {
		return nil, "", err
	}
	if half.digest != own.digest {
		return nil, "", cli.Hint(cli.ErrConflict, "the copy's record of the final close is not the one the store holds")
	}
	return map[string]map[string]evidencebook.Opening{half.digest: held}, "carried: the final close's amount and currency openings", nil
}
