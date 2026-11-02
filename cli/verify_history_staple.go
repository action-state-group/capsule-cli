package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/spf13/cobra"
)

// A stapled card is read as the derivation it is, history_card/1 (ER §8.1),
// never as a History frame instance and never as standard_history/1: it
// carries no record content and no counts (Sherlock2, Buzz 71efdf5a, from
// REGISTRY.md and history-frame-v0 §6). The file names it by its own id:
// an evidence-commitment/1 that staples cards, or {history, request, reply}
// with history "history_card/1".
const (
	stapleCommitment = "evidence-commitment/1"
	stapleCard       = "history_card/1"
)

// Property statuses are IP v1 §7.1's. A staple has no History properties
// and no counts; these are its own checks.
const (
	statusPass       = "PASS"
	statusFail       = "FAIL"
	statusNotChecked = "NOT_CHECKED"
	statusNotPresent = "NOT_PRESENT"
)

// stapleMustPass are the properties a staple needs before it is ACCEPTED.
// request_binding is NOT_PRESENT for a commitment, which staples the reply
// octets only, so a commitment is never ACCEPTED (Sherlock2's finding on the
// Goose staple): it reports what it can and says why.
var stapleMustPass = []string{"envelope_signature", "responder_trust", "request_binding", "card_form", "subject_scope", "checkpoint_anchor", "checkpoint_signature", "checkpoint_key", "checkpoint_consistency"}

// stapleRow is one stapled card's checks.
type stapleRow struct {
	Stream     string            `json:"stream,omitempty"`
	Properties map[string]string `json:"properties"`
	Findings   []string          `json:"findings"`
}

var lowerHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// stapleKeys reads --counterparty-key and --checkpoint-key. The
// counterparty key is the reader's own input, never read from the staple.
func stapleKeys(c *cobra.Command) (counterparty string, checkpointKeys []string, err error) {
	counterparty, _ = c.Flags().GetString("counterparty-key")
	checkpointKeys, _ = c.Flags().GetStringArray("checkpoint-key")
	for _, k := range append([]string{counterparty}, checkpointKeys...) {
		if k != "" && !lowerHex64.MatchString(k) {
			return "", nil, InputError("--counterparty-key and --checkpoint-key are 64 lowercase hex characters")
		}
	}
	return counterparty, checkpointKeys, nil
}

// verifyStaple reads a stapled history_card/1 file.
func verifyStaple(c *cobra.Command, raw []byte) (frameReport, error) {
	r := frameReport{Kind: stapleCard, Findings: []string{}, NotRun: []string{
		"the staple's nonce and freshness (min_freshness) are the reader's policy",
		"no History property and no count: a history_card/1 carries neither",
	}}
	counterparty, ckKeys, err := stapleKeys(c)
	if err != nil {
		return r, err
	}
	receipts, policy, err := loadWitnessInputs(c)
	if err != nil {
		return r, err
	}
	var head struct {
		Profile      string `json:"profile"`
		History      string `json:"history"`
		HistoryCards []struct {
			Stream     string `json:"stream"`
			Derivation string `json:"derivation"`
			Bytes      string `json:"bytes"`
			SHA256     string `json:"sha256"`
		} `json:"history_cards"`
		Request carried `json:"request"`
		Reply   carried `json:"reply"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return r, InputError("--history is not a stapled history_card/1: " + err.Error())
	}
	var anchors []string
	switch {
	case head.Profile == stapleCommitment:
		commitment := commitmentChecks(raw, counterparty)
		cards := 0
		for _, hc := range head.HistoryCards {
			if hc.Derivation != stapleCard {
				continue
			}
			cards++
			octets, err := base64.StdEncoding.DecodeString(hc.Bytes)
			if err != nil {
				return r, InputError("--history history_cards bytes: " + err.Error())
			}
			row := cardChecks(octets, nil, counterparty, ckKeys)
			row.Stream = hc.Stream
			if row.stream != hc.Stream {
				row.set("stream_match", statusFail, "the commitment labels the card "+hc.Stream+" but the card is of stream "+row.stream)
			} else {
				row.set("stream_match", statusPass, "")
			}
			if sum := sha256.Sum256(octets); hc.SHA256 != hex.EncodeToString(sum[:]) {
				row.set("card_digest", statusFail, "the commitment's sha256 for the card is not the card's digest")
			} else {
				row.set("card_digest", statusPass, "")
			}
			for k, v := range commitment.Properties {
				row.set(k, v, "")
			}
			row.Findings = append(row.Findings, commitment.Findings...)
			switch sum, ok := commitmentCheckpoint(raw); {
			case !ok:
				row.set("commitment_anchor", statusNotPresent, "")
			case row.anchor != sum:
				row.set("commitment_anchor", statusFail, "the commitment's checkpoint is not the card's anchor")
			default:
				row.set("commitment_anchor", statusPass, "")
			}
			r.Staples = append(r.Staples, row.stapleRow)
			anchors = append(anchors, row.anchor)
		}
		if cards == 0 {
			return r, InputError("--history commitment staples no history_card/1")
		}
		if cards > 1 && len(receipts) > 0 {
			return r, InputError("--receipt witnesses one anchor; this commitment staples several cards, so give it a commitment with one")
		}
		r.NotRun = append(r.NotRun, "request_binding: a commitment staples the reply octets only, with no request or nonce")
	case head.History == stapleCard:
		req, err := head.Request.message()
		if err != nil {
			return r, InputError("--history request: " + err.Error())
		}
		rep, err := head.Reply.message()
		if err != nil {
			return r, InputError("--history reply: " + err.Error())
		}
		if req.MediaType != evidencerequest.MediaTypeJSON || rep.MediaType != evidencerequest.MediaTypeJSON {
			return r, InputError("--history a stapled history_card/1 is JSON: its request and reply are media_type " + evidencerequest.MediaTypeJSON)
		}
		row := cardChecks(rep.Octets, &req, counterparty, ckKeys)
		r.Staples = append(r.Staples, row.stapleRow)
		anchors = append(anchors, row.anchor)
	default:
		return r, InputError("--history is not a stapled history_card/1: want an evidence-commitment/1 or {history: \"history_card/1\", request, reply}")
	}
	r.Verdict = "ACCEPTED"
	for _, row := range r.Staples {
		for p, status := range row.Properties {
			if status == statusFail || (status != statusPass && (slices.Contains(stapleMustPass, p) || slices.Contains(envelopeMustPass, p) || p == "commitment_signature")) {
				r.Verdict = "NOT_ACCEPTED"
			}
		}
		for _, p := range stapleMustPass {
			if _, ok := row.Properties[p]; !ok {
				r.Verdict = "NOT_ACCEPTED"
			}
		}
	}
	// One anchor per staple; the receipts sit beside the first card.
	w := anchorWitnessed(anchors[0], receipts, policy)
	r.AnchorWitnessed = &w
	return r, nil
}

// cardRow is a stapleRow plus the anchor the card's envelope names.
type cardRow struct {
	stapleRow
	anchor string
	// stream is the card's own evidence_stream.
	stream string
}

func (r *cardRow) set(property, status, finding string) {
	if r.Properties == nil {
		r.Properties = map[string]string{}
	}
	r.Properties[property] = status
	if status == statusFail && finding != "" {
		r.Findings = append(r.Findings, property+" FAIL: "+finding)
	}
}

// cardChecks checks one stapled history_card/1 reply: the envelope under
// the counterparty key the reader holds, the card's form, and the
// checkpoints it names. request is nil when the staple carries none.
func cardChecks(octets []byte, request *evidencerequest.Message, counterparty string, ckKeys []string) cardRow {
	row := cardRow{stapleRow: stapleRow{Properties: map[string]string{}, Findings: []string{}}}
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON})
	if err != nil || reply.Envelope == nil {
		row.set("envelope_signature", statusFail, "the staple is not a signed envelope")
		return row
	}
	e := reply.Envelope
	row.anchor = e.Anchor.Digest
	if err := e.Verify(); err != nil {
		row.set("envelope_signature", statusFail, err.Error())
	} else {
		row.set("envelope_signature", statusPass, "")
	}
	// Attack F: the key is the reader's own input, checked against the
	// envelope's responder; nothing is read from the staple to name it.
	switch {
	case counterparty == "":
		row.set("responder_trust", statusNotChecked, "")
	case e.Responder == counterparty:
		row.set("responder_trust", statusPass, "")
	default:
		row.set("responder_trust", statusFail, "the staple's responder is not the counterparty key you hold")
	}
	if request == nil {
		row.set("request_binding", statusNotPresent, "")
	} else {
		// AAC's verifier owns the binding to the request: its profile,
		// subject, derivation, anchor and artifact digest. The responder is
		// the counterparty key the reader holds, or none is accepted.
		trust := evidencerequest.TrustPolicy{}
		if counterparty != "" {
			trust.ResponderKeys = []string{counterparty}
		}
		v, err := evidencerequest.NewVerifier(trust, evidencerequest.VerificationPolicyFromPin(nil), evidencerequest.ProfileTime(DealClock()), nil)
		var res evidencerequest.VerificationResult
		if err == nil {
			res, err = v.Verify([]evidencerequest.Exchange{{Request: *request, Replies: []evidencerequest.Message{reply.Message}}}, evidencerequest.VerifyOptions{})
		}
		if err != nil {
			row.set("request_binding", statusFail, "the request does not verify: "+err.Error())
		} else {
			got := res.Exchanges[0].Replies[0]
			for _, p := range envelopeMustPass {
				if p == "responder_trust" {
					continue
				}
				row.set(p, got.Status(p), p+" is "+got.Status(p))
			}
			// The signature check above is the envelope's own; the verifier's
			// agrees or this row already says FAIL.
		}
	}
	checkpoints, proofs, stream, form := cardForm(e)
	row.stream = stream
	// ER §8.1: history_card/1 is the proof-carrying form of the checkpoints
	// subject, so a staple over another subject (the Goose node asks
	// full_history) is outside it.
	if e.Subject.Kind == evidencerequest.SubjectCheckpoints {
		row.set("subject_scope", statusPass, "")
	} else {
		row.set("subject_scope", statusFail, "a history_card/1 answers the checkpoints subject, not "+string(e.Subject.Kind))
	}
	if form != "" {
		row.set("card_form", statusFail, form)
	} else {
		row.set("card_form", statusPass, "")
	}
	checkpointChecks(&row, e.Anchor.Digest, checkpoints, proofs, ckKeys)
	return row
}

// cardForm checks the history_card/1 artifact (ER §8.1) has the card's
// members and nothing else, as evidencebook builds it: anchor, derivation,
// evidence_stream, history and subject, each history entry a hex checkpoint
// and the consistency proof from the one before (null for the first). It
// returns the checkpoint statements, the card's stream and a reason when
// the form is wrong.
func cardForm(e *evidencerequest.Envelope) ([][]byte, [][]byte, string, string) {
	if e.Derivation != stapleCard {
		return nil, nil, "", "the envelope's derivation is " + e.Derivation + ", not " + stapleCard
	}
	decoded, err := evidencerequest.DecodeJSON(e.Artifact)
	card, ok := decoded.(map[string]any)
	if err != nil || !ok {
		return nil, nil, "", "the card is not a JSON object"
	}
	keys := make([]string, 0, len(card))
	for k := range card {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"anchor", "derivation", "evidence_stream", "history", "subject"}) {
		return nil, nil, "", "the card has members other than the card's"
	}
	subject, _ := card["subject"].(map[string]any)
	if len(subject) != 1 || !reflect.DeepEqual(subject, e.Subject.Value()) {
		return nil, nil, "", "the card's subject is not the envelope's"
	}
	stream, _ := card["evidence_stream"].(string)
	if card["derivation"] != stapleCard || card["anchor"] != e.Anchor.Digest || stream == "" {
		return nil, nil, "", "the card does not name its derivation, anchor and stream"
	}
	history, _ := card["history"].([]any)
	if len(history) == 0 {
		return nil, nil, "", "the card has no history"
	}
	var statements, proofs [][]byte
	for i, item := range history {
		entry, _ := item.(map[string]any)
		checkpoint, _ := entry["checkpoint"].(string)
		proof, isText := entry["consistency_from_previous"].(string)
		statement, derr := hex.DecodeString(checkpoint)
		if len(entry) != 2 || derr != nil || checkpoint == "" || strings.ToLower(checkpoint) != checkpoint || (i == 0) != (entry["consistency_from_previous"] == nil) || (i > 0 && !isText) {
			return nil, nil, "", fmt.Sprintf("history entry %d is not a hex checkpoint with its consistency proof", i)
		}
		raw, err := hex.DecodeString(proof)
		if err != nil || strings.ToLower(proof) != proof {
			return nil, nil, "", fmt.Sprintf("history entry %d's consistency proof is not hex", i)
		}
		statements = append(statements, statement)
		proofs = append(proofs, raw)
	}
	return statements, proofs, stream, ""
}

// checkpointChecks is IP v1 §6.2.1 for the card's checkpoints: the last is
// the envelope's anchor, each is signed by the key it names, and that key
// is one the reader accepts. The log key need not be the responder's, so
// it is never compared with the counterparty key (IP §6.2.1).
func checkpointChecks(row *cardRow, anchor string, statements, proofs [][]byte, accepted []string) {
	if len(statements) == 0 {
		row.set("checkpoint_anchor", statusNotChecked, "")
		row.set("checkpoint_signature", statusNotChecked, "")
		row.set("checkpoint_key", statusNotChecked, "")
		row.set("checkpoint_consistency", statusNotChecked, "")
		return
	}
	if sum := sha256.Sum256(statements[len(statements)-1]); hex.EncodeToString(sum[:]) != anchor {
		row.set("checkpoint_anchor", statusFail, "the card's last checkpoint is not the envelope's anchor")
	} else {
		row.set("checkpoint_anchor", statusPass, "")
	}
	signature, key := statusPass, statusPass
	if len(accepted) == 0 {
		key = statusNotChecked
	}
	parsed := make([]witness.SignedCheckpoint, len(statements))
	readable := true
	for i, s := range statements {
		signed, err := witness.ParseSignedCheckpoint(s)
		parsed[i] = signed
		if err != nil {
			readable = false
			signature = statusFail
			row.Findings = append(row.Findings, fmt.Sprintf("checkpoint_signature FAIL: checkpoint %d: %v", i, err))
			continue
		}
		if len(accepted) > 0 && !slices.Contains(accepted, signed.KeyID) {
			key = statusFail
			row.Findings = append(row.Findings, fmt.Sprintf("checkpoint_key FAIL: checkpoint %d is signed by %s, not a key you accept", i, signed.KeyID))
		}
	}
	row.set("checkpoint_signature", signature, "")
	row.set("checkpoint_key", key, "")
	// ER §8.1: the card is a chain, each checkpoint extending the one before,
	// which is all a D0 card claims. Unrelated checkpoints, each signed by an
	// accepted key, are not a history.
	if !readable {
		row.set("checkpoint_consistency", statusNotChecked, "")
		return
	}
	row.set("checkpoint_consistency", statusPass, "")
	for i := 1; i < len(parsed); i++ {
		if why := chainFailure(parsed[i-1], parsed[i], statements[i-1], statements[i], proofs[i]); why != "" {
			row.set("checkpoint_consistency", statusFail, fmt.Sprintf("checkpoint %d: %s", i, why))
			break
		}
	}
}

// commitmentChecks is the commitment's own signature: the sender's key_id
// must be the counterparty key the reader holds, and sig must verify under
// it over the JCS form without sig. A man in the middle who re-signs the
// commitment under his own key matches its own key_id, so the key comes from
// the reader and not from the commitment.
func commitmentChecks(raw []byte, counterparty string) cardRow {
	row := cardRow{stapleRow: stapleRow{Properties: map[string]string{}, Findings: []string{}}}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		row.set("commitment_signature", statusFail, err.Error())
		return row
	}
	sig, _ := doc["sig"].(map[string]any)
	value, _ := sig["value"].(string)
	delete(doc, "sig")
	keyID, _ := doc["key_id"].(string)
	switch {
	case counterparty == "":
		row.set("commitment_signature", statusNotChecked, "")
	case keyID != counterparty:
		row.set("commitment_signature", statusFail, "the commitment's key_id is not the counterparty key you hold")
	default:
		body, err := canonical.JCS(doc)
		public, perr := hex.DecodeString(counterparty)
		signature, serr := base64.RawURLEncoding.DecodeString(value)
		if err != nil || perr != nil || serr != nil || sig["alg"] != "Ed25519" || sig["canon"] != "jcs" ||
			len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, body, signature) {
			row.set("commitment_signature", statusFail, "sig does not verify under the counterparty key")
		} else {
			row.set("commitment_signature", statusPass, "")
		}
	}
	return row
}

// commitmentCheckpoint is the digest of the commitment's own checkpoint
// statement, the one its stapled card is anchored on.
func commitmentCheckpoint(raw []byte) (string, bool) {
	var doc struct {
		Checkpoint struct {
			Bytes string `json:"bytes"`
		} `json:"checkpoint"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", false
	}
	statement, err := base64.StdEncoding.DecodeString(doc.Checkpoint.Bytes)
	if err != nil || len(bytes.TrimSpace(statement)) == 0 {
		return "", false
	}
	sum := sha256.Sum256(statement)
	return hex.EncodeToString(sum[:]), true
}
