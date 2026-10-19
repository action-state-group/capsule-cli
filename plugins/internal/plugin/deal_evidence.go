package plugin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// A deal's shared copy over the evidence-request carrier. On a deal profile
// (sqlite), `request` asks the other party for its copy of a deal both hold,
// `respond` answers with the same audience-withheld bundle `disclose --deal
// --share counterparty` makes, and `request --response` records what came
// back. `compose` builds composed/v1 from recorded answers only.
//
// The envelope is evidencebook's, unchanged: the same canonical request
// bytes and digest, the same signed artifact response and refusal, checked
// with evidencebook's own SigningBody and Verify. evidencebook serves only
// its own book, so the deal path is beside it: what each side records goes
// in the deal store, its digest on a per-deal log, as a disclosure does.
//
// Two wire choices are open with the spec (u817 DESIGN.md §1-2); each is
// held in one place here:
//   - dealSubjectKind: a deal is named by a correlation subject whose value
//     is the deal id both parties hold (`deal open --deal-id`).
//   - dealPin: coverage.expected_pin is {root, mmr_size}.

// dealSubjectKind is the subject form that names a deal: its value is the
// deal id.
const dealSubjectKind = evidencebook.SubjectCorrelation

// dealCopyAudience is the audience a served copy is cut for: the other party.
const dealCopyAudience = cli.DealAudienceCounterparty

// Record types on a deal's requests log.
const (
	dealExchangeRequest  = "evidence_request"
	dealExchangeResponse = "evidence_response"
	dealExchangeAbsence  = "evidence_absence"
	dealExchangeAnswered = "evidence_request_answered"
)

// dealRequestsLogID names the log a deal's request records go on. An answer
// to a request that names no deal this profile holds goes on dealUnroutedLog.
func dealRequestsLogID(dealID string) string { return cli.DealLogID(dealID) + "/requests" }

const dealUnroutedLog = "deal/requests"

// dealCompositionsLogID names the log a composer's composition records go on.
func dealCompositionsLogID(dealID string) string { return cli.DealLogID(dealID) + "/compositions" }

// dealCompositionActionID is the action_id of the capsule `compose` seals as
// the composed bundle's root: the composer's own record of the set.
const dealCompositionActionID = "capsulectl-deal-composition"

const dealRequestsSchema = `CREATE TABLE IF NOT EXISTS deal_requests (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	kind TEXT NOT NULL,
	record_digest TEXT NOT NULL,
	cites TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	record TEXT NOT NULL,
	PRIMARY KEY (deal_id, n)
)`

// dealSubject reports whether a subject names a deal: dealSubjectKind with
// a deal id as its value, and nothing else.
func dealSubject(s dealSubjectRef) bool {
	return s.Kind == dealSubjectKind && cli.DealIDPattern.MatchString(s.Value) && s.Digest == "" && s.First == 0 && s.Last == 0
}

// parseDealRequest reads request bytes as evidencebook does: unknown fields
// refused, so a field this path would ignore cannot change what is served.
func parseDealRequest(raw []byte) (dealEvidenceRequest, error) {
	var req dealEvidenceRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	if decoder.More() {
		return req, errors.New("trailing data after the request")
	}
	return req, nil
}

// dealCanonicalRequest is the request's transmitted bytes: the JCS form
// evidencebook.Book.Request writes, so a deal request and a book request
// with the same fields have the same digest.
func dealCanonicalRequest(req dealEvidenceRequest) ([]byte, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	generic, err := cli.DecodeBundleJSON(encoded)
	if err != nil {
		return nil, err
	}
	return canonical.JCS(generic)
}

// dealExchangeRow is one record on a requests log, as the store holds it.
type dealExchangeRow struct {
	dealID, kind, digest, cites string
	record                      map[string]interface{}
}

// recordExchange keeps one request-log record in the store and appends its
// digest to the deal's requests log under a fresh checkpoint, as recordShare
// does for a disclosure. It returns the record's id (its JCS digest).
func (s *dealSession) recordExchange(ctx context.Context, dealID, kind, cites string, record map[string]interface{}) (string, error) {
	record["type"] = kind
	record["deal_id"] = dealID
	record["at"] = cli.DealClock().UTC().Format(time.RFC3339Nano)
	// The record as the store reads it back: plain JSON values.
	record, err := genericJSON(record)
	if err != nil {
		return "", err
	}
	digest, err := canonical.JSONDigest(record)
	if err != nil {
		return "", err
	}
	local, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	lp := s.p
	lp.LogID = dealUnroutedLog
	if dealID != "" {
		lp.LogID = dealRequestsLogID(dealID)
	}
	lp.Namespace = ""
	t, err := cli.OpenTarget(ctx, lp, cli.UseInitialization)
	if err != nil {
		return "", err
	}
	defer func() { _ = t.Close() }()
	var n int64
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_requests WHERE deal_id=?`, dealID).Scan(&n); err != nil {
		return "", err
	}
	n++
	if _, err = s.db.ExecContext(ctx, `INSERT INTO deal_requests (deal_id, n, kind, record_digest, cites, cll_sequence, record) VALUES (?,?,?,?,?,0,?)`, dealID, n, kind, digest, cites, string(local)); err != nil {
		return "", err
	}
	entry, err := cli.AppendRecordDigest(ctx, t.Log, record)
	if err != nil {
		_, delErr := s.db.ExecContext(ctx, `DELETE FROM deal_requests WHERE deal_id=? AND n=?`, dealID, n)
		return "", errors.Join(err, delErr)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE deal_requests SET cll_sequence=? WHERE deal_id=? AND n=?`, entry.Seq, dealID, n); err != nil {
		return "", err
	}
	if _, err = cli.CutCheckpoint(ctx, lp, t.Log); err != nil {
		return "", err
	}
	return digest, nil
}

// exchangeRows returns the store's request-log rows matching one condition.
func (s *dealSession) exchangeRows(ctx context.Context, where string, args ...any) (_ []dealExchangeRow, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, kind, record_digest, cites, record FROM deal_requests WHERE cll_sequence>0 AND `+where+` ORDER BY deal_id, n`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []dealExchangeRow
	for rows.Next() {
		var r dealExchangeRow
		var raw string
		if err = rows.Scan(&r.dealID, &r.kind, &r.digest, &r.cites, &raw); err != nil {
			return nil, err
		}
		if r.record, err = cli.DecodeBundleJSON([]byte(raw)); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// dealSentRequest is a request this profile recorded, read back from its
// record: the exact bytes sent, their digest, and the request they parse to.
type dealSentRequest struct {
	row     dealExchangeRow
	bytes   []byte
	digest  string
	request dealEvidenceRequest
}

func (s *dealSession) sentRequest(ctx context.Context, recordID string) (dealSentRequest, error) {
	rows, err := s.exchangeRows(ctx, `kind=? AND record_digest=?`, dealExchangeRequest, recordID)
	if err != nil {
		return dealSentRequest{}, err
	}
	if len(rows) != 1 {
		return dealSentRequest{}, cli.InputError(recordID + " is not a request this profile recorded")
	}
	out := dealSentRequest{row: rows[0]}
	text, _ := rows[0].record["request"].(string)
	out.bytes = []byte(text)
	out.digest = cli.BytesDigest(out.bytes)
	if out.digest != rows[0].record["request_digest"] {
		return dealSentRequest{}, cli.ErrConflict
	}
	if out.request, err = parseDealRequest(out.bytes); err != nil {
		return dealSentRequest{}, errors.Join(cli.ErrConflict, err)
	}
	return out, nil
}

// outcomeOf is the one recorded outcome (a response or an absence) of a
// request record, or nil while it is pending.
func (s *dealSession) outcomeOf(ctx context.Context, recordID string) (*dealExchangeRow, error) {
	rows, err := s.exchangeRows(ctx, `kind IN (?,?) AND cites=?`, dealExchangeResponse, dealExchangeAbsence, recordID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, cli.ErrConflict
	}
	return &rows[0], nil
}

// -- asking ----------------------------------------------------------------

// dealAsk records that this profile is asking for a deal copy and writes the
// canonical request bytes to send. The asker must hold the deal: the record
// goes on that deal's requests log.
func dealAsk(c *cobra.Command, p cli.Profile, raw []byte, responder, path string) (err error) {
	req, e := parseDealRequest(raw)
	if e != nil {
		return cli.Hint(cli.ErrInput, "--request: "+e.Error())
	}
	if !dealSubject(req.Subject) {
		return cli.InputError(`a deal profile asks for a deal's copy: the subject is {"kind":"` + string(dealSubjectKind) + `","value":"<deal id>"}, the deal id both parties hold`)
	}
	if (req.Coverage.ExpectedPin == nil) == (req.Coverage.MinFreshness == nil) {
		return cli.InputError("the request needs exactly one coverage member: expected_pin or min_freshness")
	}
	if req.Deadline != "" {
		if _, e = time.Parse(time.RFC3339Nano, req.Deadline); e != nil {
			return cli.InputError("deadline must be an RFC 3339 time")
		}
	}
	if req.Nonce == "" {
		nonce := make([]byte, 16)
		if _, e = rand.Read(nonce); e != nil {
			return e
		}
		req.Nonce = hex.EncodeToString(nonce)
	}
	ctx := c.Context()
	s, e := openDealSession(ctx, p)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, s.close()) }()
	dealID := req.Subject.Value
	if e = s.useDeal(ctx, dealID, false); e != nil {
		return e
	}
	// A unique nonce gives each request its own digest, so an answer to an
	// earlier request can never be recorded as the answer to this one.
	prior, e := s.exchangeRows(ctx, `kind=?`, dealExchangeRequest)
	if e != nil {
		return e
	}
	for _, r := range prior {
		text, _ := r.record["request"].(string)
		if earlier, parseErr := parseDealRequest([]byte(text)); parseErr == nil && earlier.Nonce == req.Nonce {
			return cli.InputError("nonce already used by request " + r.digest)
		}
	}
	data, e := dealCanonicalRequest(req)
	if e != nil {
		return e
	}
	digest := cli.BytesDigest(data)
	id, e := s.recordExchange(ctx, dealID, dealExchangeRequest, "", map[string]interface{}{
		"request": string(data), "request_digest": digest, "responder": responder,
	})
	if e != nil {
		return e
	}
	result := requestResult{RecordID: id, RequestDigest: digest, Output: path}
	if e = cli.AtomicFile(path, data, false); e != nil {
		result.Output, result.Request = "", data
		return errors.Join(e, cli.Output(c, result))
	}
	return cli.Output(c, result)
}

// dealRecordResponse verifies and records what came back for a recorded
// deal request, as evidencebook's RecordResponse does for a book: a response
// that is not for this request under the pinned responder key is refused
// and never recorded; a genuinely signed artifact that does not hold up is
// recorded as artifact_failed_verification, never as a grant.
func dealRecordResponse(c *cobra.Command, p cli.Profile, forID string, raw []byte, keys evidencebook.ResponderKeys, directory *witness.Directory) (err error) {
	var resp dealResponse
	if e := cli.DecodeJSONAs("--response", raw, &resp); e != nil {
		return e
	}
	ctx := c.Context()
	s, e := openDealSession(ctx, p)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, s.close()) }()
	sent, e := s.sentRequest(ctx, forID)
	if e != nil {
		return e
	}
	if outcome, e := s.outcomeOf(ctx, forID); e != nil || outcome != nil {
		return errors.Join(e, cli.InputError("an outcome is already recorded for request "+forID))
	}
	record := map[string]interface{}{
		"request_record": forID, "request_digest": sent.digest, "key_id": keys.Signer, "checkpoint_key_id": keys.Checkpoint,
		"response_digest": cli.BytesDigest(raw),
	}
	switch {
	case resp.Refusal != nil && resp.Artifact == nil:
		r := resp.Refusal
		if r.RequestDigest != sent.digest || r.KeyID != keys.Signer {
			return cli.InputError("the refusal is not for this request under --responder-key")
		}
		if e = r.Verify(); e != nil {
			return bookError(e)
		}
		record["outcome"], record["reason"] = evidencebook.OutcomeRefusal, r.Reason
	case resp.Artifact != nil && resp.Refusal == nil:
		a := resp.Artifact
		if a.RequestDigest != sent.digest || a.KeyID != keys.Signer {
			return cli.InputError("the artifact is not for this request under --responder-key")
		}
		if e = a.Verify(); e != nil {
			return bookError(e)
		}
		record["outcome"], record["artifact_kind"], record["artifact_digest"], record["anchor"] = evidencebook.OutcomeArtifact, a.ArtifactKind, a.ArtifactDigest, a.Anchor
		if failure := verifyDealCopy(sent.request, *a, keys.Checkpoint, directory); failure != "" {
			record["outcome"], record["failure"] = dealOutcomeArtifactFailed, failure
		}
	default:
		return cli.InputError("a response carries exactly one of artifact or refusal")
	}
	id, e := s.recordExchange(ctx, sent.row.dealID, dealExchangeResponse, forID, record)
	if e != nil {
		return e
	}
	if e = s.keepReply(ctx, id, raw); e != nil {
		return errors.Join(cli.Hint(cli.ErrPending, "the response is recorded as "+id+", but its octets are not yet in the artifact store; compose takes them as REQUEST_RECORD_ID=RESPONSE_FILE meanwhile"), e)
	}
	reason, _ := record["reason"].(string)
	return cli.Output(c, requestResult{RecordID: id, RequestDigest: sent.digest, Outcome: record["outcome"].(string), Reason: reason})
}

// dealReplyActionID is the action_id of the capsule that keeps a recorded
// reply's octets: the response record that observed them, by id.
func dealReplyActionID(responseRecord string) string {
	return "capsulectl-deal-reply:" + responseRecord
}

// replyCapsule is the capsule under which the profile's artifact store keeps
// a recorded reply's octets, exactly as received. Its payload is the
// response record that observed them (so its agent_input_digest is that
// record's id), and the reply is an unbound original whose content digest
// is the record's response_digest. It is sealed from the response record
// and the profile's key alone, so compose finds it again with nothing else
// on record. octets nil seals it without the reply, which names the same
// capsule.
func (s *dealSession) replyCapsule(responseRecord string, record map[string]interface{}, octets []byte) (artifact.Record, error) {
	at, _ := record["at"].(string)
	when, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return artifact.Record{}, fmt.Errorf("response record %s: at: %w", responseRecord, err)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return artifact.Record{}, err
	}
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: dealReplyActionID(responseRecord), ActionType: emit.ActionTypeFYI,
			Operator: hex.EncodeToString(s.key.Public().(ed25519.PublicKey)), Developer: "capsulectl-deal",
			Timestamp: when.UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	if octets != nil {
		request.Artifacts = []artifact.Artifact{{Name: "reply", State: artifact.Present, Content: octets}}
	}
	return cli.Seal(request, s.key)
}

// replyStore is the deal profile's artifact store, in the deal store's own
// database.
func (s *dealSession) replyStore(ctx context.Context) (cli.ArtifactStore, error) {
	store, err := cli.NewArtifactStore(s.p, s.db, s.keys)
	if err != nil {
		return nil, err
	}
	return store, store.Init(ctx)
}

// keepReply puts a recorded reply's octets in the artifact store, under the
// response record that observed them (replyCapsule).
func (s *dealSession) keepReply(ctx context.Context, responseRecord string, octets []byte) error {
	rows, err := s.exchangeRows(ctx, `record_digest=?`, responseRecord)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return fmt.Errorf("response record %s is not on record once", responseRecord)
	}
	record, err := s.replyCapsule(responseRecord, rows[0].record, octets)
	if err != nil {
		return err
	}
	store, err := s.replyStore(ctx)
	if err != nil {
		return err
	}
	return store.Put(ctx, record)
}

// keptReply reads a recorded reply's octets back from the artifact store.
// held is false for a reply recorded before replies were kept, whose bytes
// the caller must supply.
func (s *dealSession) keptReply(ctx context.Context, response *dealExchangeRow) (octets []byte, held bool, err error) {
	record, err := s.replyCapsule(response.digest, response.record, nil)
	if err != nil {
		return nil, false, err
	}
	store, err := s.replyStore(ctx)
	if err != nil {
		return nil, false, err
	}
	stored, err := store.Get(ctx, record.CapsuleID)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, a := range stored.Artifacts {
		if a.Name == "reply" && a.State == artifact.Present && cli.BytesDigest(a.Content) == response.record["response_digest"] {
			return a.Content, true, nil
		}
	}
	return nil, false, cli.Hint(cli.ErrConflict, "the artifact store holds the reply capsule of response "+response.digest+" without the octets it recorded")
}

// verifyDealCopy checks that a responder-signed artifact is the copy asked
// for: an Evidence Bundle that `verify --bundle` finds VALID, whose
// checkpoint is signed by the pinned checkpoint key, is on the named deal's
// own log, is the signed anchor, and meets the request's coverage. It
// returns why it does not, or "".
func verifyDealCopy(req dealEvidenceRequest, a dealArtifactResponse, checkpointKey string, directory *witness.Directory) string {
	if a.ArtifactKind != dealArtifactBundle {
		return fmt.Sprintf("artifact kind %q is not %s", a.ArtifactKind, dealArtifactBundle)
	}
	value, err := cli.DecodeBundleJSON(a.Artifact)
	if err != nil {
		return "the artifact is not a JSON bundle"
	}
	if value["bundle_kind"] != "evidence-bundle/v2" {
		return "the artifact is not an evidence-bundle/v2"
	}
	if verdict, _ := cli.BundleVerdict(value, directory); verdict != "VALID" {
		return "the copy verifies " + verdict + ", not VALID"
	}
	_, statement := cli.CheckpointClaim(value)
	signed, err := cli.SignedCheckpoint(statement)
	if err != nil {
		return "the copy's checkpoint does not verify"
	}
	if signed["key_id"] != checkpointKey {
		return "the copy's checkpoint is not signed by --responder-checkpoint-key"
	}
	if signed["log_id"] != cli.DealLogID(req.Subject.Value) {
		return "the copy is not of the requested deal's log"
	}
	root, _ := signed["root"].(string)
	size, err := cli.JSONUint(signed["mmr_size"])
	if err != nil {
		return "the copy's checkpoint states no size"
	}
	if fmt.Sprintf("%s:%d", root, size) != a.Anchor {
		return "the copy is not covered by the signed anchor"
	}
	if pin := req.Coverage.ExpectedPin; pin != nil && (*pin != dealPin{Root: root, MMRSize: size}) {
		return "the copy's anchor is not the pinned checkpoint"
	}
	if floor := req.Coverage.MinFreshness; floor != nil {
		cert, _ := value["completeness_certificate"].(map[string]interface{})
		last, err := cli.JSONUint(cert["last_seq"])
		if err != nil || last < floor.Size {
			return fmt.Sprintf("the copy covers fewer than the %d records asked for", floor.Size)
		}
		if floor.Time != "" {
			notBefore, err := time.Parse(time.RFC3339Nano, floor.Time)
			issued, issuedErr := time.Parse(time.RFC3339Nano, fmt.Sprint(signed["timestamp"]))
			if err != nil || issuedErr != nil || issued.Before(notBefore) {
				return "the copy's checkpoint is older than the freshness asked for"
			}
		}
	}
	return ""
}

// dealRecordAbsence records that nothing came back for a recorded deal
// request by windowEnd, under the same rules as evidencebook's
// RecordAbsence: never while pending, never over a recorded outcome.
func dealRecordAbsence(c *cobra.Command, p cli.Profile, forID string, windowEnd time.Time, route string) (err error) {
	ctx := c.Context()
	s, e := openDealSession(ctx, p)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, s.close()) }()
	sent, e := s.sentRequest(ctx, forID)
	if e != nil {
		return e
	}
	if windowEnd.After(cli.DealClock()) {
		return cli.InputError("the waiting window has not closed; the request is pending")
	}
	asked, e := time.Parse(time.RFC3339Nano, fmt.Sprint(sent.row.record["at"]))
	if e != nil {
		return errors.Join(cli.ErrConflict, e)
	}
	if windowEnd.Before(asked.Add(evidencebook.MinimumAbsenceWindow)) {
		return cli.InputError(fmt.Sprintf("a waiting window must end at least %s after the request was recorded", evidencebook.MinimumAbsenceWindow))
	}
	if sent.request.Deadline != "" {
		if deadline, e := time.Parse(time.RFC3339Nano, sent.request.Deadline); e == nil && windowEnd.Before(deadline) {
			return cli.InputError("the window closes before the request's own deadline")
		}
	}
	if outcome, e := s.outcomeOf(ctx, forID); e != nil || outcome != nil {
		return errors.Join(e, cli.InputError("an outcome is already recorded for request "+forID))
	}
	record := map[string]interface{}{
		"request_record": forID, "request_digest": sent.digest,
		"window_start": sent.row.record["at"], "window_end": windowEnd.UTC().Format(time.RFC3339Nano),
	}
	if route != "" {
		record["route"] = route
	}
	id, e := s.recordExchange(ctx, sent.row.dealID, dealExchangeAbsence, forID, record)
	if e != nil {
		return e
	}
	return cli.Output(c, requestResult{RecordID: id, RequestDigest: sent.digest, Outcome: evidencebook.RecordTypeAbsence})
}

// -- answering -------------------------------------------------------------

// dealRespond answers a request for a deal copy with a signed artifact (the
// copy `disclose --deal --share counterparty` makes, its share sealed the
// same way and naming the request) or a signed refusal, and records the
// answer either way: on the deal's requests log, or on dealUnroutedLog when
// the request names no deal this profile holds.
//
// Coverage:
//   - min_freshness: a fresh copy, its report sealed and a checkpoint cut, as
//     a share makes one.
//   - expected_pin: the copy at exactly that checkpoint. It can be served
//     only while the deal's log ends at the pin with a copy's report as its
//     newest record (the copy is then re-served, nothing new sealed);
//     otherwise, as when the deal has moved past the pin, the answer is a
//     signed coverage_unsatisfiable refusal.
func dealRespond(c *cobra.Command, p cli.Profile, raw []byte, requester, path string) (err error) {
	if requester == "" {
		return cli.InputError("--requester is required on a deal profile: the copy is a share, and the disclosure record names who it is for")
	}
	ctx := c.Context()
	s, e := openDealSession(ctx, p)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, s.close()) }()
	signer, e := evidencebook.NewEd25519Signer(s.key)
	if e != nil {
		return e
	}
	digest := cli.BytesDigest(raw)
	answered := map[string]interface{}{"request_digest": digest, "requester": requester}
	dealID := ""
	refuse := func(reason string) error {
		refusal := evidencebook.Refusal{RequestDigest: digest, Reason: reason, IssuedAt: cli.DealClock().UTC().Format(time.RFC3339Nano), KeyID: evidencebook.KeyID(signer)}
		body, e := refusal.SigningBody()
		if e != nil {
			return e
		}
		sig, e := signer.Sign(ctx, body)
		if e != nil {
			return e
		}
		refusal.Sig = hex.EncodeToString(sig)
		answered["outcome"], answered["reason"] = evidencebook.OutcomeRefusal, reason
		return s.writeAnswer(c, dealID, answered, dealResponse{Refusal: &refusal}, path)
	}
	req, e := parseDealRequest(raw)
	if e != nil || !dealSubject(req.Subject) {
		if e == nil && req.Subject.Kind != "" {
			answered["subject_kind"] = req.Subject.Kind
			return refuse(evidencebook.ReasonNoSuchSubject)
		}
		return refuse(evidencebook.ReasonRequestMalformed)
	}
	answered["subject_kind"] = req.Subject.Kind
	var holds int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys WHERE deal_id=?`, req.Subject.Value).Scan(&holds); e != nil {
		return e
	}
	if holds == 0 {
		return refuse(evidencebook.ReasonNoSuchSubject)
	}
	dealID = req.Subject.Value
	if (req.Coverage.ExpectedPin == nil) == (req.Coverage.MinFreshness == nil) {
		return refuse(evidencebook.ReasonCoverageUnsatisfiable)
	}
	if req.Deadline != "" {
		if _, e = time.Parse(time.RFC3339Nano, req.Deadline); e != nil {
			return refuse(evidencebook.ReasonRequestMalformed)
		}
	}
	if floor := req.Coverage.MinFreshness; floor != nil && floor.Time != "" {
		notBefore, e := time.Parse(time.RFC3339Nano, floor.Time)
		if e != nil {
			return refuse(evidencebook.ReasonRequestMalformed)
		}
		if notBefore.After(cli.DealClock()) {
			return refuse(evidencebook.ReasonCoverageUnsatisfiable)
		}
	}
	if req.Derivation != "" {
		return refuse(evidencebook.ReasonDerivationUnsupported)
	}
	if e = s.useDeal(ctx, dealID, false); e != nil {
		return e
	}
	events, e := s.load(ctx, dealID)
	if e != nil {
		return e
	}
	entries, e := s.logEntries(ctx)
	if e != nil {
		return e
	}
	held := ""
	if req.Coverage.ExpectedPin != nil {
		if held, e = s.reportAtHead(ctx, dealID, entries); e != nil {
			return e
		}
		if held == "" {
			return refuse(evidencebook.ReasonCoverageUnsatisfiable)
		}
	} else if req.Coverage.MinFreshness.Size > uint64(len(entries))+1 {
		// The copy adds its report: one record past what the log holds now.
		return refuse(evidencebook.ReasonCoverageUnsatisfiable)
	}
	report, e := s.dealReportFor(ctx, events)
	if e != nil {
		return e
	}
	b, e := s.dealReportBundleAt(ctx, events, report, dealCopyAudience, dealVerifyCommand("bundle.json"), held == "", held)
	if e != nil {
		return e
	}
	cp, _ := b["checkpoint"].(map[string]interface{})
	root, _ := cp["root"].(string)
	size, e := cli.JSONUint(cp["mmr_size"])
	if e != nil {
		return errors.Join(cli.ErrConflict, e)
	}
	if pin := req.Coverage.ExpectedPin; pin != nil && (*pin != dealPin{Root: root, MMRSize: size}) {
		return refuse(evidencebook.ReasonCoverageUnsatisfiable)
	}
	// The artifact is the copy's JCS bytes, so the digest the response signs
	// is the copy's bundle digest: a composer's member digest and the signed
	// artifact_digest are the one value.
	encoded, e := json.Marshal(b)
	if e != nil {
		return e
	}
	generic, e := cli.DecodeBundleJSON(encoded)
	if e != nil {
		return e
	}
	artifact, e := canonical.JCS(generic)
	if e != nil {
		return e
	}
	if e = dealShareGate(artifact, events, dealCopyAudience); e != nil {
		return e
	}
	share, e := s.recordShare(ctx, dealID, b, dealCopyAudience, requester, dealRole(events), artifact, digest)
	if e != nil {
		return e
	}
	anchor := fmt.Sprintf("%s:%d", root, size)
	response := dealArtifactResponse{
		RequestDigest: digest, Anchor: anchor, ArtifactKind: dealArtifactBundle, Artifact: artifact,
		ArtifactDigest: cli.BytesDigest(artifact), IssuedAt: cli.DealClock().UTC().Format(time.RFC3339Nano), KeyID: evidencebook.KeyID(signer),
	}
	body, e := response.SigningBody()
	if e != nil {
		return e
	}
	sig, e := signer.Sign(ctx, body)
	if e != nil {
		return e
	}
	response.Sig = hex.EncodeToString(sig)
	answered["outcome"], answered["anchor"], answered["disclosure_record"] = evidencebook.OutcomeArtifact, anchor, share["disclosure_record"]
	return s.writeAnswer(c, dealID, answered, dealResponse{Artifact: &response}, path)
}

// reportAtHead is the copy's report the deal's log ends with, when its
// newest record is one (and it was cut for the audience a served copy is
// for), or "".
func (s *dealSession) reportAtHead(ctx context.Context, dealID string, entries []cll.Entry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	last := entries[len(entries)-1]
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT capsule_id FROM deal_reports WHERE deal_id=? AND audience=? AND cll_sequence=?`, dealID, dealCopyAudience, last.Seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || hex.EncodeToString(last.Value) != id {
		return "", errors.Join(err, cli.ErrConflict)
	}
	return id, nil
}

// writeAnswer records the answer and writes the signed response. The answer
// is on record before the file: a failure writing it still reports both.
func (s *dealSession) writeAnswer(c *cobra.Command, dealID string, answered map[string]interface{}, response dealResponse, path string) error {
	id, err := s.recordExchange(c.Context(), dealID, dealExchangeAnswered, "", answered)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	reason, _ := answered["reason"].(string)
	// The requester is named by the caller and checked against nothing, so
	// it is identified, never a counterparty.
	result := respondResult{RecordID: id, RequestDigest: answered["request_digest"].(string), Outcome: answered["outcome"].(string), Reason: reason, Relationship: evidencebook.RelationshipIdentified, Output: path}
	if err = cli.AtomicFile(path, encoded, false); err != nil {
		result.Output, result.Response = "", encoded
		return errors.Join(err, cli.Output(c, result))
	}
	return cli.Output(c, result)
}

// -- composing -------------------------------------------------------------

// dealObserverRole is the role every observer of a deal composition is
// declared with: each is a party to the deal.
const dealObserverRole = "party"

type composeMember struct {
	recordID, responsePath string
}

func parseComposeMembers(values []string) ([]composeMember, error) {
	members := make([]composeMember, 0, len(values))
	seen := map[string]bool{}
	for _, v := range values {
		id, path, _ := strings.Cut(v, "=")
		if !cli.Hex64.MatchString(id) {
			return nil, cli.InputError("--member is REQUEST_RECORD_ID (its recorded answer or absence is read from the store), or REQUEST_RECORD_ID=RESPONSE_FILE for an answer recorded before replies were kept: the request record id is the 64-hex record_id `deal request --request` printed")
		}
		if seen[id] {
			return nil, cli.InputError("--member names request " + id + " twice")
		}
		seen[id] = true
		members = append(members, composeMember{recordID: id, responsePath: path})
	}
	if len(members) < 2 {
		return nil, cli.InputError("compose needs at least two --member answers")
	}
	return members, nil
}

func composeCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "compose", Short: "Compose a deal's recorded evidence-request answers into one composed/v1 bundle (each member's request is on record)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		values, _ := c.Flags().GetStringArray("member")
		members, err := parseComposeMembers(values)
		if err != nil {
			return err
		}
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return cli.InputError("--output is required: the file to write the composed bundle to")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, _ []sealedEvent) error {
			return s.compose(c, dealID, members, path)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID the answers are about")
	cmd.Flags().StringArray("member", nil, "REQUEST_RECORD_ID: a request this profile recorded, whose recorded answer (kept in the artifact store) or absence is the member; REQUEST_RECORD_ID=RESPONSE_FILE for an answer recorded before replies were kept. Give two or more")
	cmd.Flags().String("output", "", "Write the composed bundle here")
	return cmd
}

// compose builds composed/v1 from recorded answers only. Each member is a
// request this profile recorded for the deal, its one recorded outcome, and,
// for a response, the exact bytes that were recorded, read from the
// artifact store (their digest is on record). Its request_digest is the recorded request's digest; its observer
// is the responder key the response verified under (the composer's own key
// for an absence, which only the composer observed). The containing bundle's
// root is a composition record sealed on the composer's own log, naming the
// set by its composed digest. A copy's member carries its signed response
// without the artifact bytes, and each pair of members is joined on the
// deal id (dealJoins).
func (s *dealSession) compose(c *cobra.Command, dealID string, members []composeMember, path string) error {
	ctx := c.Context()
	own := hex.EncodeToString(s.key.Public().(ed25519.PublicKey))
	ownCheckpoint, err := cli.PrivateKey(s.p.Checkpoint.Signing)
	if err != nil {
		return err
	}
	blockMembers := make([]interface{}, 0, len(members))
	observers := map[string]map[string]interface{}{}
	// Every observer is a party to the deal; its custody domain is the
	// checkpoint key its copies are anchored under.
	observe := func(id, custody string) error {
		if prior, ok := observers[id]; ok && prior["custody_domain"] != custody {
			return cli.InputError("observer " + id + " answered under two checkpoint keys; it cannot be declared once")
		}
		observers[id] = map[string]interface{}{"id": id, "role": dealObserverRole, "custody_domain": custody}
		return nil
	}
	for _, m := range members {
		member, err := s.composeMember(ctx, dealID, m)
		if err != nil {
			return err
		}
		if member["outcome"] == "absence" {
			err = observe(own, hex.EncodeToString(ownCheckpoint.Public().(ed25519.PublicKey)))
		} else {
			err = observe(member["observer"].(string), member["custody"].(string))
		}
		if err != nil {
			return err
		}
		if member["outcome"] == "absence" {
			member["observer"] = own
		}
		delete(member, "custody")
		blockMembers = append(blockMembers, member)
	}
	ids := make([]string, 0, len(observers))
	for id := range observers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	observerList := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		observerList = append(observerList, observers[id])
	}
	sort.Slice(blockMembers, func(i, j int) bool {
		return blockMembers[i].(map[string]interface{})["id"].(string) < blockMembers[j].(map[string]interface{})["id"].(string)
	})
	block := map[string]interface{}{"members": blockMembers, "observers": observerList, "joins": dealJoins(dealID, blockMembers)}
	digest, err := composedDigest(block)
	if err != nil {
		return err
	}
	container, err := s.sealComposition(ctx, dealID, digest, members)
	if err != nil {
		return err
	}
	extensions, _ := container["extensions"].(map[string]interface{})
	if extensions == nil {
		extensions = map[string]interface{}{}
	}
	extensions[aacbundle.ComposedKind] = block
	container["extensions"] = extensions
	encoded, err := json.Marshal(container)
	if err != nil {
		return err
	}
	if err = cli.AtomicFile(path, encoded, false); err != nil {
		return err
	}
	return cli.Output(c, map[string]any{"deal_id": dealID, "output": path, "composed_digest": digest, "members": len(blockMembers)})
}

// composeMember is one member from a recorded request and its recorded
// outcome. It carries "custody" (the responder's checkpoint key) for the
// caller to declare the observer with.
func (s *dealSession) composeMember(ctx context.Context, dealID string, m composeMember) (map[string]interface{}, error) {
	sent, err := s.sentRequest(ctx, m.recordID)
	if err != nil {
		return nil, cli.Hint(cli.ErrInput, "member "+m.recordID+" has no recorded request: "+err.Error())
	}
	if sent.row.dealID != dealID {
		return nil, cli.InputError("member " + m.recordID + " is a request about another deal")
	}
	outcome, err := s.outcomeOf(ctx, m.recordID)
	if err != nil {
		return nil, err
	}
	if outcome == nil {
		return nil, cli.InputError("member " + m.recordID + " has no recorded outcome: a pending request is not a member (record the answer with `request --response`, or its absence)")
	}
	member := map[string]interface{}{"id": m.recordID, "request_digest": sent.digest}
	if outcome.kind == dealExchangeAbsence {
		if m.responsePath != "" {
			return nil, cli.InputError("member " + m.recordID + " is a recorded absence: nothing came back, so it takes no response file")
		}
		absence := map[string]interface{}{
			"request_digest": sent.digest,
			"window":         map[string]interface{}{"from": outcome.record["window_start"], "to": outcome.record["window_end"]},
		}
		if route, ok := outcome.record["route"]; ok {
			absence["route"] = route
		}
		if member["digest"], err = canonical.JSONDigest(absence); err != nil {
			return nil, err
		}
		member["outcome"], member["absence"] = "absence", absence
		return member, nil
	}
	// The reply as recorded: kept in the artifact store under its response
	// record. A reply recorded before replies were kept is given as a file,
	// and only the bytes that were recorded are taken: their digest is on
	// record.
	var raw []byte
	if m.responsePath == "" {
		kept, held, err := s.keptReply(ctx, outcome)
		if err != nil {
			return nil, err
		}
		if !held {
			return nil, cli.InputError("member " + m.recordID + ": its response was recorded before capsulectl kept replies in the artifact store; give the file: REQUEST_RECORD_ID=RESPONSE_FILE")
		}
		raw = kept
	} else {
		if raw, err = cli.ReadInput(m.responsePath); err != nil {
			return nil, err
		}
		if cli.BytesDigest(raw) != outcome.record["response_digest"] {
			return nil, cli.InputError("member " + m.recordID + ": " + m.responsePath + " is not the response recorded for this request")
		}
	}
	var resp struct {
		Artifact *json.RawMessage `json:"artifact"`
		Refusal  *json.RawMessage `json:"refusal"`
	}
	if err = json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	member["observer"], member["custody"] = outcome.record["key_id"], outcome.record["checkpoint_key_id"]
	switch outcome.record["outcome"] {
	case evidencebook.OutcomeRefusal:
		refusal, err := cli.DecodeBundleJSON(*resp.Refusal)
		if err != nil {
			return nil, err
		}
		if member["digest"], err = canonical.JSONDigest(refusal); err != nil {
			return nil, err
		}
		member["outcome"], member["refusal"] = "refusal", refusal
	default:
		// A recorded artifact, or one recorded as failed: a failed artifact
		// is still an artifact member, and the verifier reports its failure.
		var a dealArtifactResponse
		if err = json.Unmarshal(*resp.Artifact, &a); err != nil {
			return nil, err
		}
		bundle, err := cli.DecodeBundleJSON(a.Artifact)
		if err != nil {
			return nil, err
		}
		if member["digest"], err = aacbundle.BundleDigest(bundle); err != nil {
			return nil, err
		}
		member["outcome"], member["bundle"] = "artifact", bundle
		// The signed envelope without its artifact bytes binds the bundle to
		// the request it answered.
		member["response"] = map[string]interface{}{
			"request_digest": a.RequestDigest, "anchor": a.Anchor, "artifact_kind": a.ArtifactKind,
			"artifact_digest": a.ArtifactDigest, "issued_at": a.IssuedAt, "key_id": a.KeyID, "sig": a.Sig,
		}
	}
	return member, nil
}

// dealJoinPointer is where a copy's root record holds the deal id: the
// top-level deal_id member a step's Capsule carries (dealIDMember).
const dealJoinPointer = "/" + dealIDMember

// dealJoins declares one pre_agreed_identifier join on the deal id per pair
// of members, in the state the verifier derives: unjoined with no copy
// served, one_sided with one, and with two, agree when both roots carry this
// deal's id, otherwise unjoined (a copy whose steps predate the deal_id
// member). members are sorted by id.
func dealJoins(dealID string, members []interface{}) []interface{} {
	identifier := cli.BytesDigest([]byte(dealID))
	joins := []interface{}{}
	for i := range members {
		for k := i + 1; k < len(members); k++ {
			a, b := members[i].(map[string]interface{}), members[k].(map[string]interface{})
			served := 0
			for _, m := range []map[string]interface{}{a, b} {
				if m["outcome"] == "artifact" {
					served++
				}
			}
			state := "unjoined"
			switch {
			case served == 1:
				state = "one_sided"
			case served == 2 && rootDealID(a) == dealID && rootDealID(b) == dealID:
				state = "agree"
			}
			joins = append(joins, map[string]interface{}{
				"members": []interface{}{a["id"], b["id"]}, "basis": "pre_agreed_identifier",
				"pointer": dealJoinPointer, "identifier_digest": identifier, "state": state,
			})
		}
	}
	return joins
}

// rootDealID is the deal_id member of an artifact member's root record, or "".
func rootDealID(member map[string]interface{}) string {
	bundle, _ := member["bundle"].(map[string]interface{})
	root, _ := bundle["root"].(string)
	records, _ := bundle["records"].([]interface{})
	for _, raw := range records {
		if record, ok := raw.(map[string]interface{}); ok && record["capsule_id"] == root {
			id, _ := record[dealIDMember].(string)
			return id
		}
	}
	return ""
}

// composedDigest is the block's composed digest. The block is parsed whole,
// so composed_digest holds a placeholder until it is computed.
func composedDigest(block map[string]interface{}) (string, error) {
	block["composed_digest"] = strings.Repeat("0", 64)
	generic, err := genericJSON(block)
	if err != nil {
		return "", err
	}
	digest, err := aacbundle.ComposedDigest(generic)
	if err != nil {
		return "", err
	}
	block["composed_digest"] = digest
	return digest, nil
}

// genericJSON is v as the verifier reads it: decoded JSON, numbers exact.
func genericJSON(v interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return cli.DecodeBundleJSON(encoded)
}

// sealComposition seals the composer's composition record on the deal's
// compositions log and returns the bundle rooted at it.
func (s *dealSession) sealComposition(ctx context.Context, dealID, digest string, members []composeMember) (map[string]interface{}, error) {
	ids := make([]interface{}, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.recordID)
	}
	raw, err := json.Marshal(map[string]interface{}{
		"type": "deal_composition", "profile": cli.DealProfile, "deal_id": dealID, "composed_digest": digest, "request_records": ids,
	})
	if err != nil {
		return nil, err
	}
	generic, err := cli.DecodeBundleJSON(raw)
	if err != nil {
		return nil, err
	}
	payload, err := canonical.JCS(generic)
	if err != nil {
		return nil, err
	}
	request := cli.Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID: dealCompositionActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal",
			Timestamp: cli.DealClock().UTC().Truncate(time.Second),
		},
		Payload: payload,
	}
	lp := s.p
	lp.LogID = dealCompositionsLogID(dealID)
	t, err := cli.OpenTarget(ctx, lp, cli.UseInitialization)
	if err != nil {
		return nil, err
	}
	defer func() { _ = t.Close() }()
	pub, err := t.Publish(ctx, request, s.key)
	if err != nil {
		return nil, err
	}
	if _, err = cli.CutCheckpoint(ctx, localOnly(lp), t.Log); err != nil {
		return nil, err
	}
	b, err := cli.AssembleBundle(ctx, t.Artifacts, t.Log, lp.LogID, cli.BundleOptions{Root: pub.CapsuleID, Payloads: "selected", WithDisclosure: true})
	if err != nil {
		return nil, err
	}
	cp, _ := b["checkpoint"].(map[string]interface{})
	if statement, ok := cp["statement"].(string); ok {
		if decoded, err := base64.StdEncoding.DecodeString(statement); err == nil {
			cp["cose"] = base64.RawURLEncoding.EncodeToString(decoded)
		}
	}
	return b, nil
}

// dealRequest is `request` on a deal profile: ask (requestBytes), record an
// answer (responseBytes), or record an absence (windowEnd), each for a deal
// copy. keys are the responder's, pinned by the caller, for an answer.
func dealRequest(c *cobra.Command, p cli.Profile, requestBytes, responseBytes []byte, forID string, windowEnd time.Time, keys evidencebook.ResponderKeys) error {
	switch {
	case requestBytes != nil:
		responder, _ := c.Flags().GetString("responder")
		path, _ := c.Flags().GetString("output")
		if responder == "" || path == "" {
			return cli.InputError("--request needs --responder and --output")
		}
		return dealAsk(c, p, requestBytes, responder, path)
	case responseBytes != nil:
		var directory *witness.Directory
		if path, _ := c.Flags().GetString("witness-directory"); path != "" {
			rows, err := cli.LoadWitnessDirectory(path)
			if err != nil {
				return err
			}
			directory = &rows
		}
		return dealRecordResponse(c, p, forID, responseBytes, keys, directory)
	default:
		route, _ := c.Flags().GetString("route")
		return dealRecordAbsence(c, p, forID, windowEnd, route)
	}
}
