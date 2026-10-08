package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/spf13/cobra"
)

// ErrNoAnswer means the profile's transport did not return an answer from the
// party: unreachable, timed out, or refused at the transport. It is never a
// statement about the party's book.
var ErrNoAnswer = errors.New("no answer from the party through the profile's transport")

// evidenceSubjectKinds are the evidence-request/1 subject kinds a responder
// answers; anything else is refused by the responder as request_malformed.
var evidenceSubjectKinds = map[string]bool{"record": true, "range": true, "chain_segment": true, "correlation": true}

var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// portableInteger reports whether a JSON number is an integer that every
// encoder on the path keeps as the same integer: within int64 or uint64, and
// not -0 (serde_json re-emits both of those as floats).
func portableInteger(n []byte) bool {
	if !integerLiteral.Match(n) || string(n) == "-0" {
		return false
	}
	if _, e := strconv.ParseInt(string(n), 10, 64); e == nil {
		return true
	}
	_, e := strconv.ParseUint(string(n), 10, 64)
	return e == nil
}

// canonicalRequest returns the request as sorted-key, compact JSON and its
// subject.kind. The party digests the bytes it receives, and a transport may
// decode and re-encode the request on the way, so only a form that every
// encoder reproduces byte for byte is sent: printable ASCII, integers only,
// and no string escapes other than \" and \\.
func canonicalRequest(request []byte) ([]byte, string, error) {
	var shape struct {
		Subject *struct {
			Kind string `json:"kind"`
		} `json:"subject"`
	}
	if e := json.Unmarshal(request, &shape); e != nil || shape.Subject == nil || !evidenceSubjectKinds[shape.Subject.Kind] {
		return nil, "", inputError("evidence request needs a JSON object with subject.kind record, range, chain_segment or correlation")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(request))
	dec.UseNumber()
	if e := dec.Decode(&value); e != nil {
		return nil, "", inputError("evidence request is not valid JSON")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if e := enc.Encode(value); e != nil {
		return nil, "", inputError("evidence request is not valid JSON")
	}
	canonical := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if !portableJSON(canonical) {
		return nil, "", inputError("evidence request must be printable ASCII with integer numbers and no escapes other than \\\" and \\\\")
	}
	return canonical, shape.Subject.Kind, nil
}

// portableJSON reports whether compact JSON is printable ASCII, has integer
// numbers only, and escapes nothing inside strings except \" and \\.
func portableJSON(b []byte) bool {
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
		switch {
		case inString && c == '\\':
			if i+1 >= len(b) || (b[i+1] != '"' && b[i+1] != '\\') {
				return false
			}
			i++
		case c == '"':
			inString = !inString
		case !inString && (c == '-' || (c >= '0' && c <= '9')):
			j := i
			for j < len(b) && strings.IndexByte("-+.eE0123456789", b[j]) >= 0 {
				j++
			}
			if !portableInteger(b[i:j]) {
				return false
			}
			i = j - 1
		}
	}
	return !inString
}

// evidenceAnswer is one answer: an artifact or a signed refusal. Response is
// the party's answer JSON; the command's output re-encodes it compactly, so a
// verifier re-checks its signed fields, not a hash of those bytes. RequestDigest
// is the SHA-256 of the canonical request bytes this command sent; only a
// refusal carries the party's own statement of it. SignerPinned is true only
// for a refusal whose signer matched --responder-key; nothing in an artifact
// is verified here.
type evidenceAnswer struct {
	RequestDigest string          `json:"request_digest"`
	Answer        string          `json:"answer"`
	SubjectKind   string          `json:"subject_kind,omitempty"`
	CapsuleIDs    []string        `json:"capsule_ids,omitempty"`
	NextPageToken string          `json:"next_page_token,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Signer        string          `json:"signer,omitempty"`
	SignerPinned  bool            `json:"signer_pinned"`
	Response      json.RawMessage `json:"response"`
}

type artifactWire struct {
	V             int               `json:"v"`
	SubjectKind   string            `json:"subject_kind"`
	Bundles       []json.RawMessage `json:"bundles"`
	NextPageToken *string           `json:"next_page_token"`
}

type refusalWire struct {
	RequestDigest string `json:"request_digest"`
	Reason        string `json:"reason"`
	IssuedAt      string `json:"issued_at"`
	KeyID         string `json:"key_id"`
	Sig           string `json:"sig"`
}

// classifyAnswer decodes the party's answer to the canonical request of the
// given subject kind. An artifact must answer that kind. A refusal counts only
// when its signature verifies offline and it names this exact request; with
// responderKey set, it must also be signed by that key.
func classifyAnswer(request, response []byte, kind, responderKey string) (evidenceAnswer, error) {
	sum := sha256.Sum256(request)
	result := evidenceAnswer{RequestDigest: hex.EncodeToString(sum[:]), Response: json.RawMessage(response)}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(response, &fields); e != nil {
		return result, errors.Join(ErrPartial, errors.New("answer is not a JSON object"))
	}
	_, hasBundles := fields["bundles"]
	_, hasSig := fields["sig"]
	switch {
	case hasBundles && hasSig:
		return result, errors.Join(ErrPartial, errors.New("answer claims to be both an artifact and a refusal"))
	case hasBundles:
		var a artifactWire
		if e := json.Unmarshal(response, &a); e != nil || a.V != 1 || a.SubjectKind == "" || len(a.Bundles) == 0 {
			return result, errors.Join(ErrPartial, errors.New("artifact answer is malformed"))
		}
		if a.SubjectKind != kind {
			return result, errors.Join(ErrConflict, errors.New("artifact answers a different subject kind than the one asked"))
		}
		result.Answer, result.SubjectKind = "artifact", a.SubjectKind
		if a.NextPageToken != nil {
			result.NextPageToken = *a.NextPageToken
		}
		for _, b := range a.Bundles {
			var id struct {
				CapsuleID string `json:"capsule_id"`
			}
			if json.Unmarshal(b, &id) == nil && id.CapsuleID != "" {
				result.CapsuleIDs = append(result.CapsuleIDs, id.CapsuleID)
			}
		}
		return result, nil
	case hasSig:
		var r refusalWire
		if e := json.Unmarshal(response, &r); e != nil {
			return result, errors.Join(ErrPartial, errors.New("refusal answer is malformed"))
		}
		if r.RequestDigest != result.RequestDigest {
			return result, errors.Join(ErrConflict, errors.New("refusal names a different request"))
		}
		if e := verifyRefusal(r); e != nil {
			return result, errors.Join(ErrPartial, e)
		}
		if responderKey != "" {
			if !strings.EqualFold(responderKey, r.KeyID) {
				return result, errors.Join(ErrConflict, errors.New("refusal is signed by a key other than --responder-key"))
			}
			result.SignerPinned = true
		}
		result.Answer, result.Reason, result.Signer = "refusal", r.Reason, r.KeyID
		return result, nil
	default:
		return result, errors.Join(ErrPartial, errors.New("answer is neither an artifact nor a signed refusal"))
	}
}

// verifyRefusal checks the refusal's Ed25519 signature over the responder's
// signing body: the three signed fields as sorted, compact, ASCII JSON, with
// key_id the raw public key in hex. Values outside printable ASCII are
// rejected rather than guessed at, since the two encoders escape them
// differently.
func verifyRefusal(r refusalWire) error {
	for _, v := range []string{r.RequestDigest, r.Reason, r.IssuedAt} {
		for i := 0; i < len(v); i++ {
			if v[i] < 0x20 || v[i] > 0x7e {
				return errors.New("refusal field is not printable ASCII")
			}
		}
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if e := enc.Encode(struct {
		IssuedAt      string `json:"issued_at"`
		Reason        string `json:"reason"`
		RequestDigest string `json:"request_digest"`
	}{r.IssuedAt, r.Reason, r.RequestDigest}); e != nil {
		return e
	}
	key, e := hex.DecodeString(r.KeyID)
	sig, se := hex.DecodeString(r.Sig)
	if e != nil || se != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("refusal key or signature is not hex Ed25519")
	}
	if !ed25519.Verify(ed25519.PublicKey(key), bytes.TrimSuffix(body.Bytes(), []byte("\n")), sig) {
		return errors.New("refusal signature does not verify")
	}
	return nil
}

// chainHead is the newest checkpoint a party's chain_segment answer reports,
// after its signed COSE statement was verified and every field below matched
// to it. It proves the checkpoint's key signed it; it does not prove the
// party has no newer one.
type chainHead struct {
	LogID     string `json:"log_id"`
	MMRSize   uint64 `json:"mmr_size"`
	Root      string `json:"root"`
	KeyID     string `json:"key_id"`
	Timestamp string `json:"timestamp"`
	Pinned    bool   `json:"signer_pinned"`
}

// verifyChainHead reads the last link of a one-bundle chain_segment answer.
// pin, when set, is the party's checkpoint public key obtained independently.
func verifyChainHead(response []byte, pin string) (chainHead, error) {
	var a struct {
		SubjectKind string `json:"subject_kind"`
		Bundles     []struct {
			Links []struct {
				Checkpoint struct {
					LogID     string `json:"log_id"`
					MMRSize   uint64 `json:"mmr_size"`
					Root      string `json:"root"`
					PrevSize  uint64 `json:"prev_size"`
					PrevRoot  string `json:"prev_root"`
					KeyID     string `json:"key_id"`
					Timestamp string `json:"timestamp"`
				} `json:"checkpoint"`
				CheckpointCOSE string `json:"checkpoint_cose"`
			} `json:"links"`
		} `json:"bundles"`
	}
	if e := json.Unmarshal(response, &a); e != nil || a.SubjectKind != "chain_segment" || len(a.Bundles) != 1 || len(a.Bundles[0].Links) == 0 {
		return chainHead{}, errors.Join(ErrPartial, errors.New("answer is not a one-segment chain_segment artifact"))
	}
	link := a.Bundles[0].Links[len(a.Bundles[0].Links)-1]
	raw, e := hex.DecodeString(link.CheckpointCOSE)
	if e != nil || len(raw) == 0 {
		return chainHead{}, errors.Join(ErrPartial, errors.New("head checkpoint carries no signed COSE statement"))
	}
	record, e := checkpoint.ParseRecord(raw)
	if e != nil {
		return chainHead{}, errors.Join(ErrPartial, e)
	}
	if e = record.VerifySignature(); e != nil {
		return chainHead{}, errors.Join(ErrPartial, e)
	}
	cp := link.Checkpoint
	stamped, e := time.Parse(time.RFC3339Nano, cp.Timestamp)
	if e != nil || !stamped.Equal(record.Timestamp) || cp.LogID != record.LogID || cp.MMRSize != record.MMRSize || cp.Root != record.Root || cp.KeyID != record.KeyID || cp.PrevSize != record.PrevSize || cp.PrevRoot != record.PrevRoot {
		return chainHead{}, errors.Join(ErrConflict, errors.New("head checkpoint JSON disagrees with its signed statement"))
	}
	head := chainHead{LogID: record.LogID, MMRSize: record.MMRSize, Root: record.Root, KeyID: record.KeyID, Timestamp: cp.Timestamp}
	if pin != "" {
		if !strings.EqualFold(pin, record.KeyID) {
			return head, errors.Join(ErrConflict, errors.New("head checkpoint is signed by a key other than the pinned one"))
		}
		head.Pinned = true
	}
	return head, nil
}

// askParty canonicalizes request, sends it to --party through the profile's
// transport, and classifies the answer against it.
func askParty(c *cobra.Command, request []byte) (evidenceAnswer, error) {
	p, e := selected(c)
	if e != nil {
		return evidenceAnswer{}, e
	}
	party, _ := c.Flags().GetString("party")
	if party == "" || len(party) > 256 || strings.ContainsFunc(party, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return evidenceAnswer{}, inputError("--party is required: the party's id as the profile's transport addresses it")
	}
	responderKey, _ := c.Flags().GetString("responder-key")
	if responderKey != "" {
		if _, e = parseKeys([]string{responderKey}); e != nil {
			return evidenceAnswer{}, e
		}
	}
	if request, e = withRequesterID(request, p); e != nil {
		return evidenceAnswer{}, e
	}
	canonical, kind, e := canonicalRequest(request)
	if e != nil {
		return evidenceAnswer{}, e
	}
	door, e := openEvidenceDoor(p)
	if e != nil {
		return evidenceAnswer{}, e
	}
	response, e := door.ask(c.Context(), party, canonical)
	if e != nil {
		return evidenceAnswer{}, e
	}
	return classifyAnswer(canonical, response, kind, responderKey)
}

// withRequesterID names the profile's requester_id in a request that names
// none. The party digests the request it receives, and a transport may name
// a requester itself when the request does not; naming it here keeps the
// sent bytes the ones the party digests, so its refusal binds this request.
// A request that already names one is sent as it is. The id is the
// operator's to set; it is never derived.
func withRequesterID(request []byte, p Profile) ([]byte, error) {
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(request, &fields); e != nil {
		return nil, inputError("evidence request is not a JSON object")
	}
	if named, ok := fields["requester_id"]; ok {
		// Held to the same form as the profile's: a node's full peer id.
		var id string
		if json.Unmarshal(named, &id) != nil || !hex64.MatchString(id) {
			return nil, inputError("evidence request's requester_id must be a node's full peer id: 64 lowercase hex")
		}
		return request, nil
	}
	if p.Connection.RequesterID == "" {
		return nil, inputError("profile " + p.Name + " names no requester id: set the node's own full peer id (64 hex, not the short id its status shows) with `capsulectl profile update --profile " + p.Name + " --requester-id <id>`, or name requester_id in the request")
	}
	id, e := json.Marshal(p.Connection.RequesterID)
	if e != nil {
		return nil, e
	}
	fields["requester_id"] = id
	return json.Marshal(fields)
}

// evidenceRequest is the request map the book verbs build themselves.
type evidenceRequest struct {
	Subject evidenceSubject `json:"subject"`
	Page    *evidencePage   `json:"page,omitempty"`
}

type evidenceSubject struct {
	Kind      string `json:"kind"`
	CapsuleID string `json:"capsule_id,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Last      int    `json:"last,omitempty"`
}

type evidencePage struct {
	Size  int    `json:"size,omitempty"`
	Token string `json:"token,omitempty"`
}

// askPartyFor sends a request the verb built itself.
func askPartyFor(c *cobra.Command, request evidenceRequest) (evidenceAnswer, error) {
	body, e := json.Marshal(request)
	if e != nil {
		return evidenceAnswer{}, e
	}
	return askParty(c, body)
}

func bookCommands() *cobra.Command {
	group := &cobra.Command{Use: "book", Short: "Read another party's evidence book through the profile's transport (read-only; answers are carried, not re-signed)"}

	list := &cobra.Command{Use: "list", Short: "List a range of the party's records (one page; follow next_page_token; bundles are carried, not verified)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		selector, _ := c.Flags().GetString("selector")
		if selector == "" {
			return inputError("--selector is required: id1..id2 or id1,id2,...")
		}
		request := evidenceRequest{Subject: evidenceSubject{Kind: "range", Selector: selector}}
		size, _ := c.Flags().GetInt("page-size")
		token, _ := c.Flags().GetString("page-token")
		if size > 0 || token != "" {
			request.Page = &evidencePage{Size: max(size, 0), Token: token}
		}
		answer, e := askPartyFor(c, request)
		if e != nil {
			return e
		}
		return output(c, answer)
	}}
	list.Flags().String("selector", "", "Record selector: id1..id2 or id1,id2,...")
	list.Flags().Int("page-size", 0, "Records per page; the party caps it")
	list.Flags().String("page-token", "", "next_page_token from the previous page")

	get := &cobra.Command{Use: "get", Short: "Fetch one of the party's records as a bundle (carried, not verified; its capsule_id must match the one asked)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		id, _ := c.Flags().GetString("capsule-id")
		if id == "" {
			return inputError("--capsule-id is required")
		}
		answer, e := askPartyFor(c, evidenceRequest{Subject: evidenceSubject{Kind: "record", CapsuleID: id}})
		if e != nil {
			return e
		}
		if answer.Answer == "artifact" && (len(answer.CapsuleIDs) != 1 || answer.CapsuleIDs[0] != id) {
			return errors.Join(ErrConflict, errors.New("party answered with a record other than the one asked for"))
		}
		return output(c, answer)
	}}
	get.Flags().String("capsule-id", "", "The record's full capsule_id")

	request := &cobra.Command{Use: "request", Short: "Send an evidence request (record, range, chain_segment or correlation) and return the classified answer", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("request")
		body, e := readInput(path)
		if e != nil {
			return e
		}
		answer, e := askParty(c, body)
		if e != nil {
			return e
		}
		return output(c, answer)
	}}
	request.Flags().String("request", "", "Evidence request JSON file; sent in canonical form (sorted keys, compact), which is what the party digests")

	head := &cobra.Command{Use: "head", Short: "Read the newest checkpoint the party reports and verify its signature offline (a withheld newer checkpoint is not detected)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		pin, _ := c.Flags().GetString("responder-checkpoint-key")
		// Checked like --responder-key, before anything is sent.
		if pin != "" {
			if _, e := parseKeys([]string{pin}); e != nil {
				return e
			}
		}
		answer, e := askPartyFor(c, evidenceRequest{Subject: evidenceSubject{Kind: "chain_segment", Last: 1}})
		if e != nil {
			return e
		}
		if answer.Answer != "artifact" {
			return output(c, answer)
		}
		h, e := verifyChainHead(answer.Response, pin)
		if e != nil {
			return e
		}
		return output(c, map[string]any{"request_digest": answer.RequestDigest, "answer": answer.Answer, "head": h, "response": answer.Response})
	}}
	head.Flags().String("responder-checkpoint-key", "", "The party's checkpoint public key in hex, obtained independently; a head signed by any other key is refused")

	for _, cmd := range []*cobra.Command{list, get, request, head} {
		cmd.Flags().String("party", "", "The party whose book to read, as the profile's transport addresses it")
		cmd.Flags().String("responder-key", "", "The party's signing public key in hex, obtained independently; a refusal signed by any other key is refused")
	}
	group.AddCommand(list, get, request, head)
	return group
}
