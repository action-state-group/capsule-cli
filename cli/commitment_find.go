package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// `commitment find` finds an advertised evidence commitment from a carrier an
// asker was given and says whether to believe it, before the asker asks or
// pays. It takes the carrier reference (a directory record or ARD file URL, an
// A2A Agent Card URL, or Nostr relays and a node id), fetches the commitment
// octets and applies the verifier rules of the carriers binding (ECC §6):
// the commitment's signature, the key match, the staple digests, the newest
// issued_at and a consistency check against the node. It confers no trust on
// the node: the verdict is about the commitment's own bytes.

// maxCommitmentBytes bounds what one fetch reads.
const maxCommitmentBytes = 8 << 20

// commitmentClient fetches carrier documents and asks a node. A test replaces
// it with the test server's client.
var commitmentClient = &http.Client{Timeout: 20 * time.Second}

// commitmentSkew is the clock skew within which a commitment's issued_at is
// not future_dated (ECC §6 V4).
const commitmentSkew = 5 * time.Minute

// fetchHTTPS reads one https: document, bounded.
func fetchHTTPS(method, uri string, body []byte) ([]byte, error) {
	if !strings.HasPrefix(uri, "https://") {
		return nil, fmt.Errorf("%s is not an https: URI", uri)
	}
	req, err := http.NewRequest(method, uri, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", evidencerequest.MediaTypeJSON)
	}
	req.Header.Set("Accept", evidencerequest.MediaTypeJSON)
	resp, err := commitmentClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", uri, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCommitmentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCommitmentBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", uri, maxCommitmentBytes)
	}
	return raw, nil
}

// findInput is what the asker holds and chose; nothing in it is read from a
// carrier.
type findInput struct {
	Directories, Cards, Relays []string
	Files                      []string
	NodeID                     string
	NodeKey                    string
	CheckpointKeys             []string
	Receipts                   [][]byte
	Policy                     witness.AnchorWitnessPolicy
	PinDir                     string
	RefuseUnwitnessed, Offline bool
}

// carrierRead is one carrier reference as it was read.
type carrierRead struct {
	Reference string `json:"reference"`
	Carrier   string `json:"carrier"`
	Found     int    `json:"commitments"`
	Error     string `json:"error,omitempty"`
}

// witnessView is the label the verifier derives; the node sets none of it.
type witnessView struct {
	Label   string `json:"label"`
	Since   string `json:"since,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Streams []struct {
		Stream string `json:"stream"`
		Status string `json:"status"`
	} `json:"streams,omitempty"`
}

// nodeVerdict is the verdict for one node key and id.
type nodeVerdict struct {
	NodeID     string            `json:"node_id"`
	KeyID      string            `json:"key_id"`
	Verdict    string            `json:"verdict"`
	Reasons    []string          `json:"reasons"`
	Commitment *commitmentDoc    `json:"commitment"`
	Other      []*commitmentDoc  `json:"other_commitments,omitempty"`
	KeyBinding map[string]string `json:"key_binding"`
	Witness    witnessView       `json:"witness"`
	Pin        []string          `json:"pin,omitempty"`
	Live       string            `json:"live_consistency"`
	Staples    []stapleRow       `json:"staples,omitempty"`
	Findings   []string          `json:"findings"`
}

type findReport struct {
	Verdict  string           `json:"verdict"`
	Carriers []carrierRead    `json:"carriers"`
	Failures []carrierFailure `json:"carrier_failures,omitempty"`
	Nodes    []nodeVerdict    `json:"nodes"`
	Refused  []*commitmentDoc `json:"refused,omitempty"`
	NotRun   []string         `json:"not_run,omitempty"`
}

// resolvePointer reads a pointer's octets: the event's content, or the URI,
// or the announce event an naddr names.
func resolvePointer(p commitmentPointer, depth int) ([]byte, string, error) {
	if len(p.Inline) > 0 {
		return p.Inline, "", nil
	}
	if strings.HasPrefix(p.URI, "nostr:") {
		if depth > 0 {
			return nil, "", fmt.Errorf("an announce event whose r tag is an naddr")
		}
		a, err := parseNaddr(p.URI)
		if err != nil || a.Kind != nostrAnnounceKind || len(a.Relays) == 0 {
			return nil, "", fmt.Errorf("%s is not an naddr of an announce event with a relay hint", p.URI)
		}
		events, err := collectEvents(a.Relays, map[string]any{"kinds": []int{nostrAnnounceKind}, "authors": []string{a.Author}, "#d": []string{a.D}})
		if err != nil {
			return nil, "", err
		}
		var newest *nostrEvent
		for i := range events {
			if newest == nil || events[i].CreatedAt > newest.CreatedAt {
				newest = &events[i]
			}
		}
		if newest == nil {
			return nil, "", fmt.Errorf("no relay has the event %s names", p.URI)
		}
		q, reason := nostrPointer(*newest)
		if reason != "" {
			return nil, "", fmt.Errorf("%s: %s", checkNostrEvent, reason)
		}
		return resolvePointer(q, depth+1)
	}
	octets, err := fetchHTTPS(http.MethodGet, p.URI, nil)
	return octets, "", err
}

// collectEvents asks every relay and keeps the events whose id and signature
// verify. It fails only when no relay answered.
func collectEvents(relays []string, filter map[string]any) ([]nostrEvent, error) {
	var out []nostrEvent
	var errs []string
	answered := false
	seen := map[string]bool{}
	for _, relay := range relays {
		events, err := queryRelay(relay, filter)
		if err != nil {
			errs = append(errs, err.Error())
		}
		if err == nil || len(events) > 0 {
			answered = true
		}
		for _, ev := range events {
			if !seen[ev.ID] && ev.verify() == nil {
				seen[ev.ID] = true
				out = append(out, ev)
			}
		}
	}
	if !answered {
		return nil, fmt.Errorf("no relay answered: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// gather reads every reference and returns the distinct commitments they
// lead to, each with the carriers that held it.
func gather(in findInput) ([]*commitmentDoc, []carrierRead, []carrierFailure) {
	var reads []carrierRead
	var failures []carrierFailure
	byDigest := map[string]*commitmentDoc{}
	var docs []*commitmentDoc
	add := func(p commitmentPointer, octets []byte) {
		d := checkCommitmentOctets(octets)
		if prior, seen := byDigest[d.SHA256]; seen {
			d = prior
		} else {
			byDigest[d.SHA256] = d
			docs = append(docs, d)
		}
		if !slices.Contains(d.Sources, p.Carrier) {
			d.Sources = append(d.Sources, p.Carrier)
		}
		// V1: a carrier's digest that is not these octets is a stale pointer.
		if p.Digest != "" && p.Digest != d.SHA256 && !slices.Contains(d.DigestMismatch, p.Carrier) {
			d.DigestMismatch = append(d.DigestMismatch, p.Carrier)
		}
	}
	follow := func(ref, carrier string, pointers []commitmentPointer, fails []carrierFailure) {
		r := carrierRead{Reference: ref, Carrier: carrier}
		failures = append(failures, fails...)
		for _, p := range pointers {
			octets, _, err := resolvePointer(p, 0)
			if err != nil {
				r.Error = err.Error()
				continue
			}
			add(p, octets)
			r.Found++
		}
		reads = append(reads, r)
	}
	for _, uri := range in.Directories {
		doc, err := fetchHTTPS(http.MethodGet, uri, nil)
		if err != nil {
			reads = append(reads, carrierRead{Reference: uri, Carrier: "directory", Error: err.Error()})
			continue
		}
		pointers, fails := directoryPointers(doc)
		follow(uri, "directory", pointers, fails)
	}
	for _, uri := range in.Cards {
		doc, err := fetchHTTPS(http.MethodGet, uri, nil)
		if err != nil {
			reads = append(reads, carrierRead{Reference: uri, Carrier: "a2a", Error: err.Error()})
			continue
		}
		pointers, fails := a2aPointers(doc)
		follow(uri, "a2a", pointers, fails)
	}
	if len(in.Relays) > 0 {
		ref := strings.Join(in.Relays, ",") + " " + in.NodeID
		events, err := collectEvents(in.Relays, map[string]any{"kinds": []int{nostrAnnounceKind}, "#t": []string{"evidence-commitment"}, "#d": []string{in.NodeID}})
		if err != nil {
			reads = append(reads, carrierRead{Reference: ref, Carrier: "nostr", Error: err.Error()})
		} else {
			var pointers []commitmentPointer
			var fails []carrierFailure
			for _, ev := range events {
				if p, reason := nostrPointer(ev); reason != "" {
					fails = append(fails, carrierFailure{"nostr", checkNostrEvent, reason})
				} else {
					pointers = append(pointers, p)
				}
			}
			follow(ref, "nostr", pointers, fails)
		}
	}
	for _, path := range in.Files {
		octets, err := os.ReadFile(path)
		if err != nil {
			reads = append(reads, carrierRead{Reference: path, Carrier: "file", Error: err.Error()})
			continue
		}
		add(commitmentPointer{Carrier: "file"}, octets)
		reads = append(reads, carrierRead{Reference: path, Carrier: "file", Found: 1})
	}
	return docs, reads, failures
}

// nostrAttribution is ECC §5.3: after the commitment verifies, an event by the
// commitment's mailbox key for its node id is announced by the responder; any
// other event with the same octets is relayed. The author is never compared
// with key_id (V5).
func nostrAttribution(d *commitmentDoc, ev *nostrEvent) string {
	ask, _ := d.doc["ask"].(map[string]any)
	nostr, _ := ask["nostr"].(map[string]any)
	if key, _ := nostr["mailbox_key"].(string); key != "" && ev.Pubkey == key {
		return "announced"
	}
	return "relayed"
}

func runFind(in findInput) (findReport, error) {
	now := DealClock()
	docs, reads, failures := gather(in)
	rep := findReport{Carriers: reads, Failures: failures, NotRun: []string{
		"nothing a carrier says is believed: a digest that is not the octets marks a stale pointer and changes nothing else (ECC §6 V1)",
		"finding a commitment confers no trust; the stapled cards go to your admission pack as history (ECC §6 V5, V6)",
	}}
	rankCommitments(docs, now, commitmentSkew)
	type node struct{ key, id string }
	var order []node
	groups := map[node][]*commitmentDoc{}
	for _, d := range docs {
		if d.Refused != "" {
			rep.Refused = append(rep.Refused, d)
			continue
		}
		if in.NodeID != "" && d.NodeID != in.NodeID {
			continue
		}
		n := node{d.KeyID, d.NodeID}
		if _, seen := groups[n]; !seen {
			order = append(order, n)
		}
		groups[n] = append(groups[n], d)
	}
	rep.Verdict = "VERIFIED"
	if len(order) == 0 {
		rep.Verdict = "NOT_VERIFIED"
		if len(rep.Refused) == 0 && len(failures) == 0 {
			rep.NotRun = append(rep.NotRun, "no carrier led to a commitment")
		}
	}
	for _, n := range order {
		nv, err := judgeNode(in, now, groups[n])
		if err != nil {
			return rep, err
		}
		if nv.Verdict != "VERIFIED" {
			rep.Verdict = "NOT_VERIFIED"
		}
		rep.Nodes = append(rep.Nodes, nv)
	}
	if len(rep.Refused) > 0 || len(failures) > 0 {
		// A refused commitment or a malformed carrier entry is reported by name;
		// it does not fail a node whose current commitment verifies.
		rep.NotRun = append(rep.NotRun, "a refused commitment or a malformed carrier entry is listed above and is not read")
	}
	return rep, nil
}

// judgeNode applies V4 to V6 to one node's commitments.
func judgeNode(in findInput, now time.Time, docs []*commitmentDoc) (nodeVerdict, error) {
	nv := nodeVerdict{NodeID: docs[0].NodeID, KeyID: docs[0].KeyID, Reasons: []string{}, Findings: []string{}, KeyBinding: map[string]string{}}
	var current *commitmentDoc
	for _, d := range docs {
		if d.Status == "current" {
			current = d
		} else {
			nv.Other = append(nv.Other, d)
		}
	}
	if current == nil {
		nv.Verdict = "NOT_VERIFIED"
		nv.Commitment = docs[0]
		for _, d := range docs {
			if d.Status == "conflicting" || d.Status == "future_dated" {
				nv.Reasons = append(nv.Reasons, d.Status)
				break
			}
		}
		nv.Reasons = append(nv.Reasons, "no_current_commitment")
		return nv, nil
	}
	nv.Commitment = current
	fail := func(reason, finding string) {
		if !slices.Contains(nv.Reasons, reason) {
			nv.Reasons = append(nv.Reasons, reason)
		}
		nv.Findings = append(nv.Findings, reason+": "+finding)
	}
	for _, c := range current.DigestMismatch {
		nv.Findings = append(nv.Findings, reasonDigestMismatch+": the "+c+" entry's digest is not these octets (a stale pointer; the octets verify on their own)")
	}
	// V5: the key is the asker's, never the carrier's.
	bound := false
	switch {
	case in.NodeKey != "" && in.NodeKey != current.KeyID:
		nv.KeyBinding["node_key"] = statusFail
		fail(reasonKeyMismatch, "the commitment's key_id is not the node key you hold, so it does not describe that node")
	case in.NodeKey != "":
		nv.KeyBinding["node_key"] = statusPass
		bound = true
	default:
		nv.KeyBinding["node_key"] = statusNotChecked
	}
	cardRows, err := commitmentStapleRows(current.octets, map[bool]string{true: current.KeyID, false: ""}[bound], in.CheckpointKeys)
	if err != nil {
		return nv, err
	}
	if len(cardRows) == 0 {
		fail("staples", "the commitment staples no history_card/1")
	}
	// Live: ask the node for a fresh card that extends the checkpoint held.
	chains := make([]cardChain, 0, len(cardRows))
	var pins *commitmentPins
	for _, row := range cardRows {
		for p, status := range row.Properties {
			if status == statusFail {
				name := "staple_" + p
				if p == "card_digest" {
					name = reasonStapleDigest
				}
				fail(name, strings.Join(row.Findings, "; "))
			}
		}
		nv.Staples = append(nv.Staples, row.stapleRow)
	}
	for _, row := range cardRows {
		octets, _ := stapleOctets(current.doc, row.Stream)
		chain, ok := chainOf(octets)
		if !ok {
			fail("staple_card_form", "the card for stream "+row.Stream+" is not a readable history_card/1")
			continue
		}
		chains = append(chains, chain)
	}
	label := deriveWitness(cardRows, in)
	nv.Witness = label
	if in.PinDir != "" && label.Label == "unwitnessed" {
		if pins, err = loadPins(in.PinDir, current.KeyID); err != nil {
			return nv, err
		}
		nv.Pin = pinChecks(pins, chains, now, fail)
	}
	nv.Live = liveCheck(in, current, chains, bound, fail, &nv)
	if nv.KeyBinding["answers"] == "" {
		nv.KeyBinding["answers"] = statusNotChecked
	}
	if in.RefuseUnwitnessed && label.Label == "unwitnessed" {
		fail("unwitnessed_refused", "you asked to refuse a node no configured witness vouches for: "+label.Reason)
	}
	if len(nv.Reasons) == 0 {
		nv.Verdict = "VERIFIED"
		if pins != nil {
			if err := pins.save(); err != nil {
				return nv, err
			}
		}
	} else {
		nv.Verdict = "NOT_VERIFIED"
	}
	return nv, nil
}

// stapleOctets finds the decoded bytes of the card for a stream.
func stapleOctets(doc map[string]any, stream string) ([]byte, bool) {
	var cards []any
	if list, ok := doc["history_cards"].([]any); ok {
		cards = list
	}
	if one, ok := doc["history_card"]; ok {
		cards = append(cards, one)
	}
	for _, c := range cards {
		o, _ := c.(map[string]any)
		if s, _ := o["stream"].(string); s != stream && o["stream"] != nil {
			continue
		}
		b, _ := o["bytes"].(string)
		if raw, err := decodeStd(b); err == nil {
			return raw, true
		}
	}
	return nil, false
}

// deriveWitness is the label (loop#319): witnessed only when each stapled
// card's anchor has a receipt that verifies under a witness the asker's own
// policy lists. Receipts the node stapled from witnesses the asker does not
// know count for nothing, since a node can mint its own; the label is
// derived here and nothing the node or carrier says sets it.
func deriveWitness(rows []cardRow, in findInput) witnessView {
	v := witnessView{Label: "witnessed"}
	var since *int64
	var why []string
	for _, row := range rows {
		r := anchorWitnessed(row.anchor, in.Receipts, in.Policy)
		v.Streams = append(v.Streams, struct {
			Stream string `json:"stream"`
			Status string `json:"status"`
		}{row.Stream, r.Status})
		if r.Status != witness.AnchorPass {
			v.Label = "unwitnessed"
			switch r.Status {
			case witness.AnchorNotPresent:
				why = append(why, "no receipt: log age and timing are the node's own word")
			case witness.AnchorNotChecked:
				why = append(why, "receipts by witnesses your policy does not list")
			default:
				why = append(why, "no receipt verifies under your witness policy")
			}
			continue
		}
		if r.WitnessedTime != nil && (since == nil || *r.WitnessedTime < *since) {
			since = r.WitnessedTime
		}
	}
	if len(rows) == 0 {
		v.Label, why = "unwitnessed", []string{"no stapled card"}
	}
	if v.Label == "witnessed" && since != nil {
		v.Since = time.Unix(*since, 0).UTC().Format(time.RFC3339)
		v.Label = "witnessed since " + v.Since
	}
	if v.Label == "unwitnessed" {
		v.Reason = strings.Join(slices.Compact(why), "; ")
	}
	return v
}

// pinChecks pins each stream's first checkpoint and checks the later ones:
// the stapled chain must contain the pin and run on from it (rule 4). A
// stream missing from a later commitment is dropped, and a new one is new, so
// restarting a log under a new name never passes silently.
func pinChecks(pins *commitmentPins, chains []cardChain, now time.Time, fail func(reason, finding string)) []string {
	var notes []string
	seen := map[string]bool{}
	for _, c := range chains {
		seen[c.Stream] = true
		last := c.Parsed[len(c.Parsed)-1]
		pin, held := pins.Streams[c.Stream]
		if !held {
			pins.add(c.Stream, streamPin{LogID: last.LogID, Size: last.MMRSize, Root: last.Root, SeenAt: now.Format(time.RFC3339)})
			if len(pins.Streams) > 1 {
				notes = append(notes, "new stream since "+now.Format(time.RFC3339)+": "+c.Stream)
			} else {
				notes = append(notes, "pinned the first checkpoint seen for stream "+c.Stream+" (size "+fmt.Sprint(last.MMRSize)+")")
			}
			continue
		}
		if last.LogID != pin.LogID {
			fail("pin_consistency", "stream "+c.Stream+" is now a different log than the one you pinned on "+pin.SeenAt)
			continue
		}
		if why := c.extendsPin(pin.Size, pin.Root); why != "" {
			fail("pin_consistency", "stream "+c.Stream+": "+why+" (pinned "+pin.SeenAt+")")
			continue
		}
		notes = append(notes, "stream "+c.Stream+" extends the checkpoint pinned on "+pin.SeenAt)
	}
	for stream, pin := range pins.Streams {
		if !seen[stream] {
			notes = append(notes, "dropped since "+pin.SeenAt+": "+stream)
		}
	}
	slices.Sort(notes)
	return notes
}

// liveCheck asks the node, where the commitment says it can be asked over
// HTTP, for a fresh history_card/1 at least as long as the checkpoint held,
// and checks the answer's signature, form and chain. The answer being signed
// by key_id is what proves key_id for a node whose key the asker was not
// given (ECC §6 V5). Where the node cannot be asked this way, only the
// stapled chain was checked, and the result says so.
func liveCheck(in findInput, d *commitmentDoc, chains []cardChain, bound bool, fail func(reason, finding string), nv *nodeVerdict) string {
	ask, _ := d.doc["ask"].(map[string]any)
	endpoint, _ := ask["endpoint"].(string)
	bindings, _ := ask["bindings"].([]any)
	httpBound := false
	for _, b := range bindings {
		httpBound = httpBound || b == "http"
	}
	switch {
	case in.Offline:
		return "NOT_CHECKED: --offline; only the stapled chain was checked"
	case !httpBound || !strings.HasPrefix(endpoint, "https://"):
		return "NOT_CHECKED: the commitment names no https: endpoint with the http binding to ask; only the stapled chain was checked"
	case len(chains) == 0:
		return "NOT_CHECKED: no readable stapled card to extend"
	}
	allOK := true
	for _, held := range chains {
		last := held.Parsed[len(held.Parsed)-1]
		size := last.MMRSize
		var nonce [16]byte
		_, _ = rand.Read(nonce[:])
		req := evidencerequest.Request{
			Subject:    evidencerequest.Subject{Kind: evidencerequest.SubjectCheckpoints},
			Coverage:   evidencerequest.Coverage{MinFreshness: &evidencerequest.Freshness{Size: &size}},
			Derivation: stapleCard,
			Nonce:      hex.EncodeToString(nonce[:]),
			Profile:    evidencerequest.ProfileIDv1,
		}
		octets, err := evidencerequest.EncodeRequest(req, evidencerequest.MediaTypeJSON)
		if err != nil {
			fail("live_answer", "cannot form the request: "+err.Error())
			return "FAIL"
		}
		reply, err := fetchHTTPS(http.MethodPost, endpoint, octets)
		if err != nil {
			nv.Findings = append(nv.Findings, "live_answer NOT_CHECKED: "+err.Error())
			allOK = false
			continue
		}
		msg := evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON}
		row := cardChecks(reply, &msg, d.KeyID, in.CheckpointKeys)
		for p, status := range row.Properties {
			if status == statusFail {
				fail("live_answer", "the node's fresh card fails "+p+": "+strings.Join(row.Findings, "; "))
			}
		}
		fresh, ok := chainOf(reply)
		if !ok {
			fail("live_answer", "the node's answer is not a readable history_card/1 (a refusal or another form)")
			continue
		}
		if fresh.Stream != held.Stream {
			fail("live_answer", "the node's fresh card is of stream "+fresh.Stream+", not "+held.Stream)
			continue
		}
		if why := fresh.extendsPin(last.MMRSize, last.Root); why != "" {
			fail("live_consistency", "stream "+held.Stream+": "+why)
			continue
		}
		if row.Properties["envelope_signature"] == statusPass && !bound && in.NodeKey == "" {
			nv.KeyBinding["answers"] = statusPass
		}
	}
	if !allOK {
		return "NOT_CHECKED: the node did not answer a fresh card; only the stapled chain was checked"
	}
	return "PASS: the node's fresh card extends the checkpoint held"
}

// decodeStd is standard base64.
func decodeStd(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
