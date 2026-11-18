package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/btcsuite/btcd/btcec/v2"
)

// `commitment publish` is the seller's side of the carriers binding (ECC): it
// signs one evidence-commitment/1 under the profile's own key with the
// profile's History card stapled, and, only after the seller's yes, hands it
// to the carriers the seller names. The reader's rules (V0 to V6) are
// commitment_verify.go and commitment_find.go; the octets written here are
// checked by the same code before anything is written.

const (
	commitmentProfile = "evidence-commitment/1"
	policyProfile     = "disclosure-policy/0"
	ardDescription    = "Answers evidence requests about its own records; verify the commitment, not this entry"
	a2aDescription    = "Points to this agent's signed evidence commitment; verify the commitment, not this entry"
)

var (
	// durationForm is ISO 8601 in days, hours, minutes and seconds, the one
	// form the loop's commitment checker accepts.
	durationForm = regexp.MustCompile(`^P(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+S)?)?$`)
	tokenForm    = regexp.MustCompile(`^[a-z0-9][a-z0-9_./:+-]*$`)
	classForm    = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z0-9][a-z0-9_./-]*$`)
	// publisherForm is a fully qualified domain name (ARD Appendix C).
	publisherForm = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	communities   = []string{"mesh-llm", "muse", "goose"}
)

// publishInput is what `commitment publish` read, checked.
type publishInput struct {
	Staple []byte
	// Seed is the profile's own signing key: it signs the commitment, and the
	// card's responder must be its public key.
	Seed ed25519.PrivateKey
	// CheckpointKey is the public key of the log the card's checkpoints are
	// signed by, 64 lowercase hex.
	CheckpointKey              string
	Community                  string
	NodeID                     string
	Endpoint                   string
	Bindings, Subjects, Derivs []string
	PredicateClasses           []string
	HumanGated                 bool
	AnswersWithin              string
	ShareDealHalf              string
	MailboxKey                 *btcec.PrivateKey
	Relays                     []string
	Now                        time.Time
	// CommitmentURI is where the seller serves the commitment octets (https:).
	CommitmentURI string
	ARDEntry      map[string]any
	WantA2A       bool
	WantAnnounce  bool
	// AnnounceURIOnly leaves the octets out of the Nostr event (ECC §5.3,
	// Size): the event then carries the r tag alone.
	AnnounceURIOnly bool
}

// publishPlan is everything the verb builds before it writes or sends
// anything.
type publishPlan struct {
	Commitment []byte
	Policy     []byte
	NodeID     string
	KeyID      string
	Digest     string
	Stream     string
	ARDEntry   map[string]any
	A2AEntry   map[string]any
	Announce   *nostrEvent
}

// printedCardStaple is `deal standard-history staple` as it prints: {derivation,
// staple: {request, reply}}.
type printedCardStaple struct {
	Derivation string `json:"derivation"`
	Staple     *struct {
		Request json.RawMessage `json:"request"`
		Reply   json.RawMessage `json:"reply"`
	} `json:"staple"`
}

// stapledReply reads the card's reply octets from a printed staple. They are
// the exact octets the responder signed over, with the printer's HTML escapes
// read back (printedOctets), and nothing else of the staple is kept: the
// commitment has no member for the request (a verifier sees request_binding
// NOT_PRESENT, as for any commitment).
func stapledReply(raw []byte) ([]byte, error) {
	var f printedCardStaple
	if err := json.Unmarshal(raw, &f); err != nil || f.Staple == nil || len(f.Staple.Reply) == 0 {
		return nil, InputError("--staple is the output of `deal standard-history staple --derivation history_card/1`: {derivation, staple: {request, reply}}")
	}
	if f.Derivation != stapleCard {
		return nil, InputError("--staple holds a " + f.Derivation + " staple; a commitment staples history_card/1")
	}
	text, ok := printedOctets(f.Staple.Reply)
	if !ok {
		return nil, InputError("--staple's reply is not JSON")
	}
	return []byte(text), nil
}

// cardOf reads the stapled reply as a history_card/1 envelope signed by key:
// the same key that signs the commitment, so one key stands behind both. It
// returns the card's stream and its newest checkpoint statement.
func cardOf(octets []byte, key string) (stream string, latest []byte, err error) {
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON})
	if err != nil || reply.Envelope == nil {
		return "", nil, InputError("--staple's reply is not an evidence envelope")
	}
	if reply.Envelope.Responder != key {
		return "", nil, InputError("the card is signed by " + reply.Envelope.Responder + ", not by this profile's signing key " + key + ": a commitment and its card are signed by one key")
	}
	if err := reply.Envelope.Verify(); err != nil {
		return "", nil, InputError("the card's envelope signature does not verify: " + err.Error())
	}
	chain, ok := chainOf(octets)
	if !ok || len(chain.Statements) == 0 {
		return "", nil, InputError("--staple's reply is not a well-formed history_card/1")
	}
	return chain.Stream, chain.Statements[len(chain.Statements)-1], nil
}

// checkPublishInput refuses the members a loop checker would refuse of what
// the caller set.
func checkPublishInput(in publishInput) error {
	switch {
	case !slices.Contains(communities, in.Community):
		return InputError("--community is one of " + strings.Join(communities, ", "))
	case !lowerHex64.MatchString(in.NodeID):
		return InputError("--node-id is 64 lowercase hex characters")
	case !durationForm.MatchString(in.AnswersWithin) || in.AnswersWithin == "P" || strings.HasSuffix(in.AnswersWithin, "T"):
		return InputError("--answers-within is an ISO 8601 duration in days, hours, minutes or seconds, such as PT24H")
	case strings.TrimSpace(in.Endpoint) == "":
		return InputError("--endpoint is not empty")
	case in.ShareDealHalf != "" && in.ShareDealHalf != "counterparty":
		return InputError("--share-deal-half is counterparty, or left out")
	case !lowerHex64.MatchString(in.CheckpointKey):
		return InputError("the card's checkpoint key is 64 lowercase hex characters: --checkpoint-key, or a profile with checkpoint.signing")
	}
	for name, values := range map[string][]string{"--binding": in.Bindings, "--subject": in.Subjects, "--derivation": in.Derivs} {
		if len(values) == 0 {
			return InputError(name + " needs at least one value")
		}
		for _, v := range values {
			if !tokenForm.MatchString(v) {
				return InputError(name + " values are lowercase names, such as evidencebook/respond")
			}
		}
	}
	for _, class := range in.PredicateClasses {
		if !classForm.MatchString(class) {
			return InputError("--predicate-class is <class>:<name>, such as performed:counterparty_half")
		}
	}
	if (in.MailboxKey == nil) != (len(in.Relays) == 0) {
		return InputError("--mailbox-key and --nostr-relay go together: a mailbox key without relays cannot be reached, and relays without a key address nobody")
	}
	if len(in.Relays) > 0 {
		if err := checkRelayList(in.Relays); err != nil {
			return InputError("--nostr-relay: " + err.Error())
		}
	}
	return nil
}

// signedWith adds sig to doc: Ed25519 over the JCS form of doc without it.
func signedWith(doc map[string]any, key ed25519.PrivateKey) (map[string]any, error) {
	body := copyObject(doc)
	delete(body, "sig")
	signed, err := canonical.JCS(body)
	if err != nil {
		return nil, err
	}
	body["sig"] = map[string]any{"alg": "Ed25519", "canon": "jcs", "value": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signed))}
	return body, nil
}

// dumped is the octets a document is served as: indented JSON with sorted
// members and a closing newline. A digest is taken over these exact bytes.
func dumped(doc map[string]any) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// buildPublishPlan signs the commitment and its policy, then builds the
// carrier entries; nothing is written or sent. The signed octets are checked
// by the reader's own V2 and V3 before they are returned.
func buildPublishPlan(in publishInput) (publishPlan, error) {
	public := in.Seed.Public().(ed25519.PublicKey)
	keyID := hex.EncodeToString(public)
	card, err := stapledReply(in.Staple)
	if err != nil {
		return publishPlan{}, err
	}
	stream, latest, err := cardOf(card, keyID)
	if err != nil {
		return publishPlan{}, err
	}
	if in.NodeID == "" {
		in.NodeID = keyID
	}
	if in.Endpoint == "" {
		in.Endpoint = "book:" + stream
	}
	if err := checkPublishInput(in); err != nil {
		return publishPlan{}, err
	}
	checkpointKey, err := hex.DecodeString(in.CheckpointKey)
	if err != nil {
		return publishPlan{}, InputError("the card's checkpoint key is 64 lowercase hex characters")
	}
	checkpointSPKI, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(checkpointKey))
	if err != nil {
		return publishPlan{}, err
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return publishPlan{}, err
	}
	policy := map[string]any{"stranger_safe": []any{"history_card/1"}}
	if in.ShareDealHalf != "" {
		policy["share_deal_half"] = in.ShareDealHalf
	}
	signedPolicy, err := signedWith(map[string]any{"profile": policyProfile, "key_id": keyID, "policy": policy}, in.Seed)
	if err != nil {
		return publishPlan{}, err
	}
	policyOctets, err := dumped(signedPolicy)
	if err != nil {
		return publishPlan{}, err
	}
	policySum := sha256.Sum256(policyOctets)
	cpSum, cardSum := sha256.Sum256(latest), sha256.Sum256(card)
	ask := map[string]any{"endpoint": in.Endpoint, "bindings": strs(in.Bindings)}
	if in.MailboxKey != nil {
		ask["nostr"] = map[string]any{"mailbox_key": nostrPublicKey(in.MailboxKey), "relays": strs(in.Relays)}
	}
	classes := in.PredicateClasses
	if classes == nil {
		classes = []string{}
	}
	doc := map[string]any{
		"profile":   commitmentProfile,
		"node_id":   in.NodeID,
		"key_id":    keyID,
		"spki":      base64.StdEncoding.EncodeToString(der),
		"issued_at": in.Now.UTC().Format("2006-01-02T15:04:05Z"),
		"ask":       ask,
		"answers":   map[string]any{"subjects": strs(in.Subjects), "derivations": strs(in.Derivs)},
		"lcap": map[string]any{
			"policy_digest":     hex.EncodeToString(policySum[:]),
			"policy_uri":        "policies/" + in.NodeID + ".json",
			"predicate_classes": strs(classes),
		},
		"checkpoint":     map[string]any{"sha256": hex.EncodeToString(cpSum[:]), "bytes": base64.StdEncoding.EncodeToString(latest)},
		"human_gated":    in.HumanGated,
		"answers_within": in.AnswersWithin,
		"history_cards": []any{map[string]any{
			"stream":         stream,
			"community":      in.Community,
			"checkpoint_key": base64.RawURLEncoding.EncodeToString(checkpointSPKI),
			"derivation":     stapleCard,
			"sha256":         hex.EncodeToString(cardSum[:]),
			"bytes":          base64.StdEncoding.EncodeToString(card),
		}},
	}
	signed, err := signedWith(doc, in.Seed)
	if err != nil {
		return publishPlan{}, err
	}
	octets, err := dumped(signed)
	if err != nil {
		return publishPlan{}, err
	}
	// The reader's own V2 and V3: what is written is what `commitment find`
	// accepts.
	if checked := checkCommitmentOctets(octets); checked.Refused != "" {
		return publishPlan{}, fmt.Errorf("the commitment built does not pass the reader's %s: %s", checked.Refused, checked.Detail)
	}
	sum := sha256.Sum256(octets)
	plan := publishPlan{Commitment: octets, Policy: policyOctets, NodeID: in.NodeID, KeyID: keyID, Digest: hex.EncodeToString(sum[:]), Stream: stream}
	if in.ARDEntry != nil {
		if plan.ARDEntry, err = ardEntryWith(in.ARDEntry, in.CommitmentURI, plan.Digest); err != nil {
			return publishPlan{}, err
		}
	}
	if in.WantA2A {
		plan.A2AEntry = map[string]any{"uri": a2aCommitmentExtension, "description": a2aDescription, "required": false,
			"params": map[string]any{"commitment_uri": in.CommitmentURI, "commitment_digest": "sha256:" + plan.Digest}}
	}
	if in.WantAnnounce {
		tags := [][]string{{"d", in.NodeID}, {"t", "evidence-commitment"}, {"x", plan.Digest}}
		content := base64.RawURLEncoding.EncodeToString(octets)
		if in.CommitmentURI != "" {
			tags = append(tags, []string{"r", in.CommitmentURI})
		}
		if in.AnnounceURIOnly {
			content = ""
		}
		ev, err := signNostrEvent(in.MailboxKey, nostrAnnounceKind, tags, content, in.Now.Unix())
		if err != nil {
			return publishPlan{}, err
		}
		plan.Announce = &ev
	}
	return plan, nil
}

func strs(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// ardEntryWith is the seller's own listing entry with the commitment pointer
// added (ECC §5.1) and its identity aligned with its publisher (ARD §4.5.1).
// The seller's entry is otherwise left as it is: a listing is the seller's.
func ardEntryWith(listing map[string]any, uri, digest string) (map[string]any, error) {
	ident, _ := listing["identifier"].(string)
	parts := strings.Split(ident, ":")
	if len(parts) < 4 || parts[0] != "urn" || parts[1] != "air" || !publisherForm.MatchString(parts[2]) {
		return nil, InputError("--ard-entry's identifier is urn:air:<publisher>:...: its publisher is a fully qualified domain name (ARD §4.5.1)")
	}
	publisher := parts[2]
	entry := copyObject(listing)
	manifest, _ := entry["trustManifest"].(map[string]any)
	manifest = copyObject(manifest)
	if id, present := manifest["identity"]; present && id != publisher {
		return nil, InputError(fmt.Sprintf("--ard-entry's trustManifest.identity is %v but its publisher is %s: they must name the same domain (ARD §4.5.1)", id, publisher))
	}
	manifest["identity"] = publisher
	var attestations []any
	for _, a := range asList(manifest["attestations"]) {
		if o, ok := a.(map[string]any); ok && slices.Contains(commitmentProfiles, fmt.Sprint(o["type"])) {
			continue // the commitment's own entry is rewritten, not repeated
		}
		attestations = append(attestations, a)
	}
	manifest["attestations"] = append(attestations, map[string]any{
		"type": commitmentProfile, "uri": uri, "mediaType": "application/json", "digest": "sha256:" + digest, "description": ardDescription})
	entry["trustManifest"] = manifest
	tags := asList(entry["tags"])
	if !slices.Contains(tags, any("evidence-request")) {
		tags = append(tags, "evidence-request")
	}
	entry["tags"] = tags
	return entry, nil
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// mergeARDFile writes the entry into the seller's own ard.json, replacing the
// entry with the same identifier or adding it. ARD §5.1 holds the entries
// under "entries"; a file that holds them under "resources" keeps that name.
func mergeARDFile(path string, entry map[string]any) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			return InputError(path + " is not a JSON object")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	member := "entries"
	if _, ok := doc["resources"]; ok {
		if _, ok := doc["entries"]; !ok {
			member = "resources"
		}
	}
	list := asList(doc[member])
	replaced := false
	for i, e := range list {
		if o, ok := e.(map[string]any); ok && o["identifier"] == entry["identifier"] {
			list[i], replaced = entry, true
		}
	}
	if !replaced {
		list = append(list, entry)
	}
	doc[member] = list
	out, err := dumped(doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// mergeA2ACard writes the commitment extension entry into the card file's
// capabilities.extensions, replacing the commitment entry that is there. The
// evidence-request entry is never touched (ECC §5.2).
func mergeA2ACard(path string, entry map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return InputError("--a2a-card: " + err.Error())
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil || card == nil {
		return InputError(path + " is not a JSON object")
	}
	caps, _ := card["capabilities"].(map[string]any)
	caps = copyObject(caps)
	var extensions []any
	for _, e := range asList(caps["extensions"]) {
		if o, ok := e.(map[string]any); ok && o["uri"] == a2aCommitmentExtension {
			continue
		}
		extensions = append(extensions, e)
	}
	caps["extensions"] = append(extensions, entry)
	card["capabilities"] = caps
	out, err := dumped(card)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}

// writeFileAtomic writes beside the target and renames over it, so a reader
// of the seller's ard.json never sees half a file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".capsulectl-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
