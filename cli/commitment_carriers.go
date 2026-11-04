package cli

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// The carriers of an evidence commitment (agent-action-capsule
// spec/bindings/evidence-commitment-carriers-v0.md, cited as ECC): a directory
// record's attestation entry (§5.1), an A2A Agent Card extension entry (§5.2)
// and a Nostr announce event (§5.3). A carrier is only a pointer to the
// commitment octets, or a holder of them; nothing a carrier says is believed
// (V1). The functions here read the form of an entry (V0) and name the failing
// check; the octets are verified in commitment_verify.go.

// The V0 check names (ECC §6).
const (
	checkDirectoryEntry  = "directory_entry"
	checkA2AEntry        = "a2a_entry"
	checkDiscoveryObject = "discovery_object"
	checkNostrEvent      = "nostr_event"
)

const (
	// a2aCommitmentExtension is the A2A extension URI that names ECC (§5.2),
	// compared as a string and never dereferenced.
	a2aCommitmentExtension = "https://github.com/action-state-group/agent-action-capsule/blob/main/spec/bindings/evidence-commitment-carriers-v0.md"
	// a2aEvidenceRequestExtension is the evidence-request entry of IP §11.4.4,
	// a discovery object that must carry no commitment member (§5.2).
	a2aEvidenceRequestExtension = "https://datatracker.ietf.org/doc/draft-mih-agent-evidence-request/"
	// nostrAnnounceKind is a placeholder in NIP-01's addressable range (ECC
	// C-1), unregistered.
	nostrAnnounceKind = 34700
)

// commitmentProfiles are the profiles ECC §3 reads.
var commitmentProfiles = []string{"evidence-commitment/1", "evidence-commitment/0"}

// sha256Prefixed is a directory, A2A or ERC-8004 entry's digest (ECC §4); the
// Nostr x tag writes the 64 hex alone.
var sha256Prefixed = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// commitmentPointer is one commitment a carrier points to or holds. Digest is
// 64 lowercase hex or empty. A pointer holding Inline octets needs no fetch.
type commitmentPointer struct {
	Carrier string `json:"carrier"`
	// URI is where the octets are served (https:), or the nostr: naddr of the
	// announce event; empty when the carrier holds them.
	URI    string `json:"uri,omitempty"`
	Digest string `json:"digest,omitempty"`
	Inline []byte `json:"-"`
	// Event is the Nostr event the pointer came from.
	Event *nostrEvent `json:"-"`
}

// carrierFailure is an entry that failed its V0 check.
type carrierFailure struct {
	Carrier string `json:"carrier"`
	Check   string `json:"check"`
	Reason  string `json:"reason"`
}

// walkObjects calls fn on every JSON object under v, v included.
func walkObjects(v any, fn func(map[string]any)) {
	switch v := v.(type) {
	case map[string]any:
		fn(v)
		for _, child := range v {
			walkObjects(child, fn)
		}
	case []any:
		for _, child := range v {
			walkObjects(child, fn)
		}
	}
}

// directoryPointers reads every commitment attestation under any
// trustManifest / trust_manifest of an ARD file or a NANDA Index record
// (ECC §5.1): an entry whose type is a commitment profile. An entry of any
// other type is not ours and is left alone.
func directoryPointers(doc []byte) ([]commitmentPointer, []carrierFailure) {
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, []carrierFailure{{"directory", checkDirectoryEntry, "the record is not JSON: " + err.Error()}}
	}
	var pointers []commitmentPointer
	var failures []carrierFailure
	walkObjects(root, func(o map[string]any) {
		for _, key := range []string{"trustManifest", "trust_manifest"} {
			manifest, _ := o[key].(map[string]any)
			entries, _ := manifest["attestations"].([]any)
			for _, e := range entries {
				entry, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := entry["type"].(string); !slices.Contains(commitmentProfiles, t) {
					continue
				}
				p, reason := directoryEntry(entry)
				if reason != "" {
					failures = append(failures, carrierFailure{"directory", checkDirectoryEntry, reason})
					continue
				}
				pointers = append(pointers, p)
			}
		}
	})
	return pointers, failures
}

// directoryEntry is the form of §5.1's entry: type, uri, mediaType and digest,
// and optionally a text description. A size is a count, so it, or any other
// member, fails the form.
func directoryEntry(entry map[string]any) (commitmentPointer, string) {
	for name := range entry {
		if !slices.Contains([]string{"type", "uri", "mediaType", "digest", "description"}, name) {
			return commitmentPointer{}, "the entry has the member " + name + " (a size is a count, and no other member is read)"
		}
	}
	uri, _ := entry["uri"].(string)
	digest, _ := entry["digest"].(string)
	if mt, _ := entry["mediaType"].(string); mt != "application/json" {
		return commitmentPointer{}, "mediaType is not application/json"
	}
	if d, present := entry["description"]; present {
		if _, ok := d.(string); !ok {
			return commitmentPointer{}, "description is not text"
		}
	}
	if !sha256Prefixed.MatchString(digest) {
		return commitmentPointer{}, "digest is not sha256: and 64 lowercase hex"
	}
	if !pointerURI(uri) {
		return commitmentPointer{}, "uri is neither an https: URI nor a nostr: naddr"
	}
	return commitmentPointer{Carrier: "directory", URI: uri, Digest: strings.TrimPrefix(digest, "sha256:")}, ""
}

// pointerURI is whether uri is where octets can be read from: https: or the
// nostr: URI of an announce event.
func pointerURI(uri string) bool {
	return strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "nostr:naddr1")
}

// a2aPointers reads the commitment extension entry of an Agent Card (ECC
// §5.2) and refuses a card whose evidence-request entry carries a commitment
// member (§6 V0, discovery_object). A card with no commitment entry has no
// result: it is not a carrier entry.
func a2aPointers(doc []byte) ([]commitmentPointer, []carrierFailure) {
	var card struct {
		Capabilities struct {
			Extensions []map[string]any `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(doc, &card); err != nil {
		return nil, []carrierFailure{{"a2a", checkA2AEntry, "the card is not JSON: " + err.Error()}}
	}
	var pointers []commitmentPointer
	var failures []carrierFailure
	for _, ext := range card.Capabilities.Extensions {
		uri, _ := ext["uri"].(string)
		params, _ := ext["params"].(map[string]any)
		switch uri {
		case a2aEvidenceRequestExtension:
			for _, member := range []string{"commitment", "commitment_uri", "commitment_digest", "checkpoint", "history_card", "history_cards", "issued_at"} {
				if _, present := params[member]; present {
					failures = append(failures, carrierFailure{"a2a", checkDiscoveryObject, "the evidence-request entry carries the commitment member " + member + " (IP §11.4.3)"})
				}
			}
		case a2aCommitmentExtension:
			p, reason := a2aEntry(ext, params)
			if reason != "" {
				failures = append(failures, carrierFailure{"a2a", checkA2AEntry, reason})
				continue
			}
			pointers = append(pointers, p)
		}
	}
	// A card whose evidence-request entry carries a commitment member fails V0:
	// its commitment entry is then not read (ECC §6).
	for _, f := range failures {
		if f.Check == checkDiscoveryObject {
			return nil, failures
		}
	}
	return pointers, failures
}

func a2aEntry(ext, params map[string]any) (commitmentPointer, string) {
	if required, present := ext["required"]; present && required != false {
		return commitmentPointer{}, "required is set (a client that ignores the entry still works)"
	}
	if d, present := ext["description"]; present {
		if _, ok := d.(string); !ok {
			return commitmentPointer{}, "description is not text"
		}
	}
	for name := range params {
		if name != "commitment_uri" && name != "commitment_digest" {
			return commitmentPointer{}, "params has the member " + name + " (exactly commitment_uri and commitment_digest)"
		}
	}
	uri, _ := params["commitment_uri"].(string)
	digest, _ := params["commitment_digest"].(string)
	if !strings.HasPrefix(uri, "https://") {
		return commitmentPointer{}, "commitment_uri is not an https: URI"
	}
	if !sha256Prefixed.MatchString(digest) {
		return commitmentPointer{}, "commitment_digest is not sha256: and 64 lowercase hex"
	}
	return commitmentPointer{Carrier: "a2a", URI: uri, Digest: strings.TrimPrefix(digest, "sha256:")}, ""
}

// nostrPointer reads a Nostr announce event whose id and signature already
// verified (ECC §5.3): kind 34700; exactly one d, t=evidence-commitment and x
// tag; at most one r tag naming an https: URI; content the octets in base64url
// without padding, or empty. An empty content needs an r tag to be findable.
func nostrPointer(ev nostrEvent) (commitmentPointer, string) {
	if ev.Kind != nostrAnnounceKind {
		return commitmentPointer{}, "the kind is not the announce kind"
	}
	tags := map[string][]string{}
	for _, tag := range ev.Tags {
		if len(tag) < 2 {
			continue
		}
		tags[tag[0]] = append(tags[tag[0]], tag[1])
		if (tag[0] == "d" || tag[0] == "t" || tag[0] == "x" || tag[0] == "r") && len(tag) != 2 {
			return commitmentPointer{}, "the " + tag[0] + " tag has members beyond its value"
		}
	}
	if len(tags["d"]) != 1 || !lowerHex64.MatchString(tags["d"][0]) {
		return commitmentPointer{}, "the event has not exactly one d tag holding a node id"
	}
	if len(tags["t"]) != 1 || tags["t"][0] != "evidence-commitment" {
		return commitmentPointer{}, "the event has not exactly one t tag evidence-commitment"
	}
	if len(tags["x"]) != 1 || !lowerHex64.MatchString(tags["x"][0]) {
		return commitmentPointer{}, "the event has not exactly one x tag holding a digest in 64 lowercase hex"
	}
	if len(tags["r"]) > 1 || (len(tags["r"]) == 1 && !strings.HasPrefix(tags["r"][0], "https://")) {
		return commitmentPointer{}, "the r tag is more than one, or not an https: URI"
	}
	p := commitmentPointer{Carrier: "nostr", Digest: tags["x"][0], Event: &ev}
	if len(tags["r"]) == 1 {
		p.URI = tags["r"][0]
	}
	if ev.Content != "" {
		octets, err := base64.RawURLEncoding.DecodeString(ev.Content)
		if err != nil {
			return commitmentPointer{}, "content is not base64url without padding"
		}
		p.Inline = octets
	} else if p.URI == "" {
		return commitmentPointer{}, "empty content and no r tag: the octets cannot be read"
	}
	return p, ""
}
