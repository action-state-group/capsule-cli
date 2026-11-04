package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// The verifier rules of the carriers binding (ECC §6), applied to commitment
// octets from any carrier. The first rule that fails names the result. The
// octets are verified on their own: what a carrier says is a hint (V1), and a
// stale digest never rejects octets that verify.

// The named results of V2 to V5.
const (
	reasonCommitmentForm      = "commitment_form"
	reasonKeyIDSPKI           = "key_id_spki"
	reasonCommitmentSignature = "commitment_signature"
	reasonStapleDigest        = "staple_digest"
	reasonKeyMismatch         = "key_mismatch"
	reasonDigestMismatch      = "digest_mismatch"
)

// commitmentDoc is one commitment's octets, verified (V2, V3) or refused.
type commitmentDoc struct {
	SHA256   string   `json:"sha256"`
	Profile  string   `json:"profile,omitempty"`
	NodeID   string   `json:"node_id,omitempty"`
	KeyID    string   `json:"key_id,omitempty"`
	IssuedAt string   `json:"issued_at,omitempty"`
	Sources  []string `json:"carriers"`
	// Refused is the V2 or V3 result that refused the octets; empty when they
	// passed.
	Refused string `json:"refused,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Status is current, superseded, conflicting or future_dated (V4).
	Status string `json:"status,omitempty"`
	// DigestMismatch lists the carriers whose digest hint was not these octets.
	DigestMismatch []string `json:"digest_mismatch,omitempty"`

	octets []byte
	issued time.Time
	doc    map[string]any
}

// checkCommitmentOctets is V2 and V3: the form, the key, the signature and the
// staple digests. It sets Refused on the first failure.
func checkCommitmentOctets(octets []byte) *commitmentDoc {
	sum := sha256.Sum256(octets)
	d := &commitmentDoc{SHA256: hex.EncodeToString(sum[:]), octets: octets}
	refuse := func(reason, detail string) *commitmentDoc {
		d.Refused, d.Detail = reason, detail
		return d
	}
	var doc map[string]any
	if err := json.Unmarshal(octets, &doc); err != nil || doc == nil {
		return refuse(reasonCommitmentForm, "the octets are not a JSON object")
	}
	d.doc = doc
	d.Profile, _ = doc["profile"].(string)
	if !slices.Contains(commitmentProfiles, d.Profile) {
		return refuse(reasonCommitmentForm, "profile is not "+strings.Join(commitmentProfiles, " or "))
	}
	d.NodeID, _ = doc["node_id"].(string)
	d.KeyID, _ = doc["key_id"].(string)
	d.IssuedAt, _ = doc["issued_at"].(string)
	if !lowerHex64.MatchString(d.NodeID) || !lowerHex64.MatchString(d.KeyID) {
		return refuse(reasonCommitmentForm, "node_id and key_id are 64 lowercase hex")
	}
	issued, err := time.Parse("2006-01-02T15:04:05Z", d.IssuedAt)
	if err != nil {
		return refuse(reasonCommitmentForm, "issued_at is not YYYY-MM-DDTHH:MM:SSZ")
	}
	d.issued = issued
	spki, _ := doc["spki"].(string)
	der, err := base64.StdEncoding.DecodeString(spki)
	if err != nil {
		return refuse(reasonKeyIDSPKI, "spki is not base64")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	public, isEd := parsed.(ed25519.PublicKey)
	if err != nil || !isEd {
		return refuse(reasonKeyIDSPKI, "spki is not an Ed25519 SubjectPublicKeyInfo")
	}
	if hex.EncodeToString(public) != d.KeyID {
		return refuse(reasonKeyIDSPKI, "key_id is not the key in spki")
	}
	sig, _ := doc["sig"].(map[string]any)
	value, _ := sig["value"].(string)
	signature, serr := base64.RawURLEncoding.DecodeString(value)
	body := copyObject(doc)
	delete(body, "sig")
	signed, jerr := canonical.JCS(body)
	if sig["alg"] != "Ed25519" || sig["canon"] != "jcs" || serr != nil || jerr != nil ||
		len(signature) != ed25519.SignatureSize || !ed25519.Verify(public, signed, signature) {
		return refuse(reasonCommitmentSignature, "sig does not verify under the key in spki")
	}
	if why := stapleDigestFailure(doc); why != "" {
		return refuse(reasonStapleDigest, why)
	}
	return d
}

// copyObject is a shallow copy of an object, so the sig can be dropped from it.
func copyObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stapleDigestFailure is V3: the checkpoint's and every card's sha256 is that
// of its decoded bytes.
func stapleDigestFailure(doc map[string]any) string {
	check := func(name string, v any) string {
		o, _ := v.(map[string]any)
		b, _ := o["bytes"].(string)
		want, _ := o["sha256"].(string)
		raw, err := base64.StdEncoding.DecodeString(b)
		sum := sha256.Sum256(raw)
		if err != nil || want != hex.EncodeToString(sum[:]) {
			return name + "'s sha256 is not the SHA-256 of its bytes"
		}
		return ""
	}
	if why := check("checkpoint", doc["checkpoint"]); why != "" {
		return why
	}
	var cards []any
	if list, ok := doc["history_cards"].([]any); ok {
		cards = list
	}
	if one, ok := doc["history_card"]; ok {
		cards = append(cards, one)
	}
	for i, card := range cards {
		if why := check(fmt.Sprintf("history card %d", i), card); why != "" {
			return why
		}
	}
	return ""
}

// ranking is V4. A commitment dated past now plus skew is future_dated and
// takes no part; among the others with one key_id and node_id the latest
// issued_at is current and the earlier are superseded; two with the same
// issued_at and different octets conflict, and neither is current until a
// later one is read. Arrival order and carrier never decide.
func rankCommitments(docs []*commitmentDoc, now time.Time, skew time.Duration) {
	groups := map[string][]*commitmentDoc{}
	for _, d := range docs {
		if d.Refused != "" {
			continue
		}
		if d.issued.After(now.Add(skew)) {
			d.Status = "future_dated"
			continue
		}
		key := d.KeyID + "/" + d.NodeID
		groups[key] = append(groups[key], d)
	}
	for _, g := range groups {
		latest := slices.MaxFunc(g, func(a, b *commitmentDoc) int { return a.issued.Compare(b.issued) }).issued
		tied := 0
		for _, d := range g {
			if d.issued.Equal(latest) {
				tied++
			}
		}
		for _, d := range g {
			switch {
			case !d.issued.Equal(latest):
				d.Status = "superseded"
			case tied > 1:
				d.Status = "conflicting"
			default:
				d.Status = "current"
			}
		}
	}
}

// cardChain is a stapled or served card's checkpoints, in order.
type cardChain struct {
	Stream     string
	Statements [][]byte
	Parsed     []witness.SignedCheckpoint
}

// chainOf reads a card envelope's checkpoint chain; ok is false when the
// card is not a well-formed history_card/1 (cardChecks names why).
func chainOf(octets []byte) (cardChain, bool) {
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON})
	if err != nil || reply.Envelope == nil {
		return cardChain{}, false
	}
	statements, _, stream, form := cardForm(reply.Envelope)
	if form != "" {
		return cardChain{}, false
	}
	c := cardChain{Stream: stream, Statements: statements}
	for _, s := range statements {
		p, err := witness.ParseSignedCheckpoint(s)
		if err != nil {
			return cardChain{}, false
		}
		c.Parsed = append(c.Parsed, p)
	}
	return c, true
}

// extendsPin is rule 4: the held checkpoint (size and root) appears in the
// chain, and the chain runs on from it to its end. The chain's own links are
// checked by checkpointChecks (checkpoint_consistency); this finds the held
// checkpoint in it. A chain that ends before the held size, forks from it or
// no longer contains it cannot prove consistency.
func (c cardChain) extendsPin(size uint64, root string) string {
	if len(c.Parsed) == 0 {
		return "the card has no checkpoints"
	}
	last := c.Parsed[len(c.Parsed)-1]
	if last.MMRSize < size {
		return fmt.Sprintf("the log is shorter than the checkpoint you hold (size %d, you hold %d)", last.MMRSize, size)
	}
	for _, p := range c.Parsed {
		if p.MMRSize == size {
			if p.Root == root {
				return ""
			}
			return fmt.Sprintf("the log has a different root at size %d than the checkpoint you hold", size)
		}
	}
	return fmt.Sprintf("the card's chain skips or does not contain the checkpoint you hold (size %d)", size)
}
