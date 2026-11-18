package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// The Nostr mailbox carriage of an evidence request (agent-action-capsule
// spec/bindings/nostr-mailbox-v0.md, cited NMB). This file is the carriage
// alone: events in and out, and which events are replies. The request and the
// reply are opaque octets here; the plugin that keeps the log seals, answers
// and records them (`request`, `respond`), and the checks on a reply's
// signature, key and binding are the evidence-request library's.

const (
	mailboxRequestKind = 4700
	mailboxReplyKind   = 4701
	// contentVerificationLimit bounds the events that name a request, are not
	// attributed to the mailbox and are verified by content, for one request
	// (NMB §4.2, "A limit on replies by content").
	contentVerificationLimit = 16
)

// mailboxMediaTypes are the two values IP §11.4.1 rule 7 allows.
var mailboxMediaTypes = []string{evidencerequest.MediaTypeJSON, evidencerequest.MediaTypeCBOR}

// base64url without padding, strictly: NMB §4 carries octets as IP §11.4.5
// does.
func encodeContent(octets []byte) string { return base64.RawURLEncoding.EncodeToString(octets) }

func decodeContent(content string) ([]byte, bool) {
	octets, err := base64.RawURLEncoding.Strict().DecodeString(content)
	return octets, err == nil
}

// tagsNamed returns the tags whose first member is name.
func tagsNamed(ev nostrEvent, name string) [][]string {
	var out [][]string
	for _, tag := range ev.Tags {
		if len(tag) >= 1 && tag[0] == name {
			out = append(out, tag)
		}
	}
	return out
}

// requestEvent builds the request event (NMB §4.1): kind 4700 under the
// requester's carriage key, one p tag addressed to the mailbox key, one m tag
// and, when the request carries a deadline, the expiration tag that repeats
// it.
func requestEvent(carriage *btcec.PrivateKey, mailboxKey, media string, octets []byte, deadline time.Time, now time.Time) (nostrEvent, error) {
	tags := [][]string{{"p", mailboxKey}, {"m", media}}
	if !deadline.IsZero() {
		tags = append(tags, []string{"expiration", strconv.FormatInt(deadline.Unix(), 10)})
	}
	return signNostrEvent(carriage, mailboxRequestKind, tags, encodeContent(octets), now.Unix())
}

// replyEvent builds the reply event (NMB §4.2): kind 4701 under the mailbox
// key, one e tag naming the request event, one p tag naming its pubkey and
// one m tag with the request's media type.
func replyEvent(mailbox *btcec.PrivateKey, request nostrEvent, relay, media string, octets []byte, now time.Time) (nostrEvent, error) {
	e := []string{"e", request.ID}
	if relay != "" {
		e = append(e, relay)
	}
	return signNostrEvent(mailbox, mailboxReplyKind, [][]string{e, {"p", request.Pubkey}, {"m", media}}, encodeContent(octets), now.Unix())
}

// replyClass is how one delivered event was classified (NMB §4.2), named as
// the vectors name it: reply, reply_by_content:<check>, not_a_reply:<check>
// or duplicate.
type replyClass struct {
	Event  nostrEvent
	Class  string
	Octets []byte
}

// isReply is whether the event's octets are the responder's answer to this
// request, attributed to the mailbox or by content.
func (c replyClass) isReply() bool {
	return c.Class == "reply" || strings.HasPrefix(c.Class, "reply_by_content:")
}

// classifyReplies applies NMB §4.2's checks, in order, to each event a relay
// delivered for the request event's reply subscription. proves says whether
// decoded octets are the responder's own answer to this request (its
// signature, its key and its binding to the request digest): the only thing
// that lets an event not attributed to the mailbox count. At most limit such
// events are verified; an event is counted by the hash recomputed from it,
// never by its claimed id, so a delivery repeated is counted once.
func classifyReplies(events []nostrEvent, requestID, media, mailboxKey string, proves func([]byte) bool, limit int) []replyClass {
	var out []replyClass
	// An event delivered again, identical, is one event; one that shares a
	// claimed id but differs is another (NMB §4.2). The limit counts by hash.
	seen := map[string]bool{}
	within := map[string]bool{}
	for _, ev := range events {
		pre, err := ev.serialize()
		if err != nil {
			out = append(out, replyClass{Event: ev, Class: "not_a_reply:content"})
			continue
		}
		sum := sha256.Sum256(pre)
		hash := hex.EncodeToString(sum[:])
		if key := ev.ID + "|" + hash; seen[key] {
			out = append(out, replyClass{Event: ev, Class: "duplicate"})
			continue
		} else {
			seen[key] = true
		}
		notReply := func(check string) { out = append(out, replyClass{Event: ev, Class: "not_a_reply:" + check}) }
		// Format checks 1 to 4.
		if ev.Kind != mailboxReplyKind {
			notReply("kind")
			continue
		}
		if e := tagsNamed(ev, "e"); len(e) != 1 || len(e[0]) < 2 || e[0][1] != requestID {
			notReply("reply_to")
			continue
		}
		if m := tagsNamed(ev, "m"); len(m) != 1 || len(m[0]) < 2 || m[0][1] != media {
			notReply("media_type")
			continue
		}
		octets, ok := decodeContent(ev.Content)
		if !ok {
			notReply("content")
			continue
		}
		// Attribution checks 5 to 7.
		failed := ""
		switch {
		case ev.ID != hash:
			failed = "id"
		case !signatureVerifies(ev):
			failed = "signature"
		case ev.Pubkey != mailboxKey:
			failed = "author"
		}
		if failed == "" {
			out = append(out, replyClass{Event: ev, Class: "reply", Octets: octets})
			continue
		}
		ok, counted := within[hash]
		if !counted {
			ok = len(within) < limit
			within[hash] = ok
		}
		if !ok {
			notReply("limit")
			continue
		}
		if proves(octets) {
			out = append(out, replyClass{Event: ev, Class: "reply_by_content:" + failed, Octets: octets})
		} else {
			notReply(failed)
		}
	}
	return out
}

// signatureVerifies is NMB §4.2 check 6: sig verifies under pubkey by BIP-340
// over the event's claimed id.
func signatureVerifies(ev nostrEvent) bool {
	id, ierr := hex.DecodeString(ev.ID)
	pub, perr := hex.DecodeString(ev.Pubkey)
	sig, serr := hex.DecodeString(ev.Sig)
	if ierr != nil || perr != nil || serr != nil || len(id) != 32 || len(pub) != 32 || len(sig) != 64 || ev.Pubkey != strings.ToLower(ev.Pubkey) {
		return false
	}
	key, err := schnorr.ParsePubKey(pub)
	if err != nil {
		return false
	}
	parsed, err := schnorr.ParseSignature(sig)
	return err == nil && parsed.Verify(id, key)
}

// answerTo is whether octets are the answer the responder key signed for the
// request with this digest: an envelope or a refusal, in the request's media
// type. The recorder (`request --response`) makes the same checks and more.
func answerTo(octets []byte, media, responderKey, requestDigest string) error {
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: octets, MediaType: media})
	if err != nil {
		return errors.New("not an envelope or a refusal")
	}
	switch {
	case reply.Envelope != nil:
		e := reply.Envelope
		switch {
		case e.Responder != responderKey:
			return errors.New("responder_trust: the envelope is signed by another key than the one you hold for the responder")
		case e.RequestDigest != requestDigest:
			return errors.New("request_binding: the envelope answers another request (its request_digest is not this request's)")
		}
		if err := e.Verify(); err != nil {
			return errors.New("envelope_signature: " + err.Error())
		}
		return nil
	case reply.Refusal != nil:
		r := reply.Refusal
		switch {
		case r.KeyID != responderKey:
			return errors.New("responder_trust: the refusal is signed by another key than the one you hold for the responder")
		case r.RequestDigest != requestDigest:
			return errors.New("request_binding: the refusal answers another request (its request_digest is not this request's)")
		}
		if err := r.Verify(); err != nil {
			return errors.New("refusal_signature: " + err.Error())
		}
		return nil
	}
	return errors.New("not an envelope or a refusal")
}

// requestDigest is SHA-256 over the request octets as received, never over
// the event or a re-serialization (NMB §5.1).
func requestDigest(octets []byte) string {
	sum := sha256.Sum256(octets)
	return hex.EncodeToString(sum[:])
}

// bech32Encode is BIP-173's encoding of data (bytes, regrouped to 5-bit
// values with padding) under hrp, as NIP-19 uses it.
func bech32Encode(hrp string, data []byte) string {
	var values []byte
	var acc, bits uint
	for _, b := range data {
		acc = acc<<8 | uint(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			values = append(values, byte(acc>>bits&31))
		}
	}
	if bits > 0 {
		values = append(values, byte(acc<<(5-bits)&31))
	}
	exp := make([]byte, 0, len(hrp)*2+1+len(values)+6)
	for _, c := range []byte(hrp) {
		exp = append(exp, c>>5)
	}
	exp = append(exp, 0)
	for _, c := range []byte(hrp) {
		exp = append(exp, c&31)
	}
	poly := bech32Polymod(append(append(exp, values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var out strings.Builder
	out.WriteString(hrp + "1")
	for _, v := range values {
		out.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		out.WriteByte(bech32Charset[byte(poly>>(5*(5-uint(i)))&31)])
	}
	return out.String()
}

// nprofileURI is the NIP-21 URI of a NIP-19 `nprofile` whose key is the
// mailbox key and whose relay entries are the relays the request was
// published to: the `route` an absence record keeps (NMB §5.3).
func nprofileURI(mailboxKey string, relays []string) (string, error) {
	key, err := hex.DecodeString(mailboxKey)
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("the mailbox key is not 32 bytes")
	}
	payload := append([]byte{0, 32}, key...)
	for _, relay := range relays {
		if len(relay) > 255 {
			return "", fmt.Errorf("relay %s is too long for an nprofile", relay)
		}
		payload = append(append(payload, 1, byte(len(relay))), relay...)
	}
	return "nostr:" + bech32Encode("nprofile", payload), nil
}
