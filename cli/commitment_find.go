package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
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
var commitmentClient = &http.Client{
	Timeout:       20 * time.Second,
	Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext, Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// maxPointers bounds the commitments one carrier document may send the verb
// to fetch.
const maxPointers = 16

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
	AllowPrivate               bool
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
	Reasons  []string         `json:"reasons,omitempty"`
	Carriers []carrierRead    `json:"carriers"`
	Failures []carrierFailure `json:"carrier_failures,omitempty"`
	Nodes    []nodeVerdict    `json:"nodes"`
	Refused  []*commitmentDoc `json:"refused,omitempty"`
	NotRun   []string         `json:"not_run,omitempty"`
}

// resolvePointer reads a pointer's octets: the event's content, or the URI,
// or the announce event an naddr names.
func resolvePointer(p commitmentPointer, depth int) ([]byte, error) {
	if len(p.Inline) > 0 {
		return p.Inline, nil
	}
	if strings.HasPrefix(p.URI, "nostr:") {
		if depth > 0 {
			return nil, fmt.Errorf("an announce event whose r tag is an naddr")
		}
		a, err := parseNaddr(p.URI)
		if err != nil || a.Kind != nostrAnnounceKind || len(a.Relays) == 0 || len(a.Relays) > maxRelays {
			return nil, fmt.Errorf("%s is not an naddr of an announce event with one to %d relay hints", p.URI, maxRelays)
		}
		events, _, err := collectEvents(a.Relays, map[string]any{"kinds": []int{nostrAnnounceKind}, "authors": []string{a.Author}, "#d": []string{a.D}})
		if err != nil {
			return nil, err
		}
		// The relay only filters if it chooses to: the address is checked here.
		var newest *nostrEvent
		for i, ev := range events {
			if ev.Pubkey == a.Author && ev.Kind == int(a.Kind) && hasTag(ev, "d", a.D) && (newest == nil || ev.CreatedAt > newest.CreatedAt) {
				newest = &events[i]
			}
		}
		if newest == nil {
			return nil, fmt.Errorf("no relay has the event %s names", p.URI)
		}
		q, reason := nostrPointer(*newest)
		if reason != "" {
			return nil, fmt.Errorf("%s: %s", checkNostrEvent, reason)
		}
		return resolvePointer(q, depth+1)
	}
	return fetchHTTPS(http.MethodGet, p.URI, nil)
}

// maxRelays bounds the relays one reference may name.
const maxRelays = 8

func hasTag(ev nostrEvent, name, value string) bool {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == name && t[1] == value {
			return true
		}
	}
	return false
}

// collectEvents asks every relay and keeps the events whose id and signature
// verify, naming each that does not. It fails only when no relay answered.
func collectEvents(relays []string, filter map[string]any) ([]nostrEvent, []carrierFailure, error) {
	var out []nostrEvent
	var fails []carrierFailure
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
			if seen[ev.ID] {
				continue
			}
			seen[ev.ID] = true
			if err := ev.verify(); err != nil {
				fails = append(fails, carrierFailure{"nostr", checkNostrEvent, "an event from " + relay + ": " + err.Error()})
				continue
			}
			out = append(out, ev)
		}
	}
	if !answered {
		return nil, fails, fmt.Errorf("no relay answered: %s", strings.Join(errs, "; "))
	}
	return out, fails, nil
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
		if p.Event != nil && d.Refused == "" {
			d.Attribution = nostrAttribution(d, p.Event)
		}
	}
	follow := func(ref, carrier string, pointers []commitmentPointer, fails []carrierFailure) {
		r := carrierRead{Reference: ref, Carrier: carrier}
		failures = append(failures, fails...)
		if len(pointers) > maxPointers {
			r.Error = fmt.Sprintf("%d commitments listed; the first %d were read", len(pointers), maxPointers)
			pointers = pointers[:maxPointers]
		}
		for _, p := range pointers {
			octets, err := resolvePointer(p, 0)
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
		events, bad, err := collectEvents(in.Relays, map[string]any{"kinds": []int{nostrAnnounceKind}, "#t": []string{"evidence-commitment"}, "#d": []string{in.NodeID}})
		failures = append(failures, bad...)
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
	if key, _ := nostr["mailbox_key"].(string); key != "" && ev.Pubkey == key && hasTag(*ev, "d", d.NodeID) {
		return "announced"
	}
	return "relayed"
}

// Node verdicts. VERIFIED needs the node key the asker holds (V5): a
// commitment whose key nobody gave is only a statement by whoever signed it,
// so it is KEY_NOT_BOUND and hands nothing on. NOT_DESCRIBING is a valid
// commitment under another key than the asker's.
const (
	verdictVerified    = "VERIFIED"
	verdictNotVerified = "NOT_VERIFIED"
	verdictKeyUnbound  = "KEY_NOT_BOUND"
	verdictNotThisNode = "NOT_DESCRIBING"
)

func runFind(in findInput) (findReport, error) {
	now := DealClock()
	allowPrivateAddresses = in.AllowPrivate
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
	for _, n := range order {
		nv, err := judgeNode(in, now, groups[n])
		if err != nil {
			return rep, err
		}
		rep.Nodes = append(rep.Nodes, nv)
	}
	// The verdict is about the node the asker holds a key for. A stranger's
	// valid commitment under another key for the same node id describes
	// someone else and cannot change it.
	rep.Verdict = verdictVerified
	counted := 0
	for _, nv := range rep.Nodes {
		switch nv.Verdict {
		case verdictNotThisNode:
			continue
		case verdictNotVerified:
			rep.Verdict = verdictNotVerified
		case verdictKeyUnbound:
			if rep.Verdict == verdictVerified {
				rep.Verdict = verdictKeyUnbound
			}
		}
		counted++
	}
	if counted == 0 {
		rep.Verdict = verdictNotVerified
		switch {
		case len(rep.Nodes) > 0:
			rep.Reasons = []string{reasonKeyMismatch}
		case len(rep.Refused) == 0 && len(failures) == 0:
			rep.NotRun = append(rep.NotRun, "no carrier led to a commitment")
		}
	}
	if len(rep.Refused) > 0 || len(failures) > 0 {
		// A refused commitment or a malformed carrier entry is reported by name;
		// it does not fail a node whose current commitment verifies.
		rep.NotRun = append(rep.NotRun, "a refused commitment or a malformed carrier entry is listed above and is not read")
	}
	return rep, nil
}

// stapleFailures names what failed in a commitment's staples. The commitment's
// top-level checkpoint is one stream's, so a card anchored elsewhere is a
// failure only when no card is anchored on it (ECC §6 V3 reads it for nothing
// else).
func stapleFailures(rows []cardRow) map[string]string {
	anchored := slices.ContainsFunc(rows, func(r cardRow) bool { return r.Properties["commitment_anchor"] == statusPass })
	out := map[string]string{}
	for _, row := range rows {
		for p, status := range row.Properties {
			if status != statusFail || (p == "commitment_anchor" && anchored) {
				continue
			}
			name := "staple_" + p
			if p == "card_digest" {
				name = reasonStapleDigest
			}
			out[name] = strings.Join(row.Findings, "; ")
		}
	}
	return out
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
		nv.Verdict = verdictNotVerified
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
	// V5: the key is the asker's, never the carrier's. A commitment under
	// another key does not describe the node; nothing more is read from it, and
	// the endpoint it names is not contacted.
	if in.NodeKey != "" && in.NodeKey != current.KeyID {
		nv.Verdict = verdictNotThisNode
		nv.KeyBinding["node_key"] = statusFail
		nv.Reasons = append(nv.Reasons, reasonKeyMismatch)
		nv.Findings = append(nv.Findings, reasonKeyMismatch+": the commitment's key_id is not the node key you hold, so it does not describe that node")
		return nv, nil
	}
	bound := in.NodeKey != ""
	fail := func(reason, finding string) {
		if !slices.Contains(nv.Reasons, reason) {
			nv.Reasons = append(nv.Reasons, reason)
		}
		nv.Findings = append(nv.Findings, reason+": "+finding)
	}
	for _, c := range current.DigestMismatch {
		nv.Findings = append(nv.Findings, reasonDigestMismatch+": the "+c+" entry's digest is not these octets (a stale pointer; the octets verify on their own)")
	}
	if bound {
		nv.KeyBinding["node_key"] = statusPass
	} else {
		nv.KeyBinding["node_key"] = statusNotChecked
	}
	counterparty := ""
	if bound {
		counterparty = current.KeyID
	}
	cardRows, err := commitmentStapleRows(current.octets, counterparty, in.CheckpointKeys)
	if err != nil {
		fail(reasonCommitmentForm, err.Error())
	}
	if len(cardRows) == 0 {
		fail("staples", "the commitment staples no history_card/1")
	}
	for name, why := range stapleFailures(cardRows) {
		fail(name, why)
	}
	// Each chain is read from the octets its row checked, one card per stream.
	chains := make([]cardChain, 0, len(cardRows))
	streams := map[string]bool{}
	for _, row := range cardRows {
		nv.Staples = append(nv.Staples, row.stapleRow)
		if streams[row.Stream] {
			fail("staple_stream", "two cards claim the stream "+row.Stream)
			continue
		}
		streams[row.Stream] = true
		chain, ok := chainOf(row.octets)
		if !ok {
			fail("staple_card_form", "the card for stream "+row.Stream+" is not a readable history_card/1")
			continue
		}
		chains = append(chains, chain)
	}
	label := deriveWitness(cardRows, current.KeyID, in)
	nv.Witness = label
	// An existing pin is always checked, whatever the label says: a receipt
	// does not show that a log extends the checkpoint the asker first saw. A
	// new pin is made only for an unwitnessed node.
	var pins *commitmentPins
	if in.PinDir != "" && bound {
		if pins, err = loadPins(in.PinDir, current.KeyID); err != nil {
			return nv, err
		}
		defer pins.release()
		nv.Pin = pinChecks(pins, chains, now, label.Label == "unwitnessed", fail)
	}
	if bound {
		nv.Live = liveCheck(in, current, chains, fail, &nv)
	} else {
		nv.Live = "NOT_CHECKED: the key is not bound, so the endpoint the commitment names is not contacted"
	}
	if in.RefuseUnwitnessed && label.Label == "unwitnessed" {
		fail("unwitnessed_refused", "you asked to refuse a node no configured witness vouches for: "+label.Reason)
	}
	switch {
	case len(nv.Reasons) > 0:
		nv.Verdict = verdictNotVerified
	case !bound:
		nv.Verdict = verdictKeyUnbound
		nv.Reasons = append(nv.Reasons, "key_unbound")
		nv.Findings = append(nv.Findings, "key_unbound: give --node-key, the key you hold for this node from the deal or request you hold; a commitment under a key nobody gave is only its signer's statement (ECC §6 V5)")
	default:
		nv.Verdict = verdictVerified
		if pins != nil {
			if err := pins.save(); err != nil {
				return nv, err
			}
		}
	}
	return nv, nil
}

// deriveWitness is the label (loop#319): witnessed only when each stapled
// card, signed by the commitment's key, has an anchor with a receipt that
// verifies under a witness the asker's own policy lists. Receipts the node
// stapled from witnesses the asker does not know count for nothing, since a
// node can mint its own; the label is derived here and nothing the node or
// carrier says sets it. "Since" is the latest of the streams' witnessed
// times, the moment every stream was witnessed.
func deriveWitness(rows []cardRow, keyID string, in findInput) witnessView {
	v := witnessView{Label: "witnessed"}
	var since *int64
	var why []string
	for _, row := range rows {
		r := anchorWitnessed(row.anchor, in.Receipts, in.Policy)
		if row.responder != keyID {
			// A card some other responder signed, stapled here, is that
			// responder's history and earns this key nothing.
			r = anchorWitnessedView{Status: witness.AnchorFail}
			why = append(why, "a card is signed by another key than the commitment's")
		}
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
		if r.WitnessedTime != nil && (since == nil || *r.WitnessedTime > *since) {
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

// pinChecks checks each stream against its pin and, for an unwitnessed node,
// pins a stream seen for the first time: the stapled chain must contain the
// pin and run on from it (rule 4). A stream missing from a later commitment
// is dropped, and a new one is new, so restarting a log under a new name never
// passes silently.
func pinChecks(pins *commitmentPins, chains []cardChain, now time.Time, create bool, fail func(reason, finding string)) []string {
	var notes []string
	seen := map[string]bool{}
	hadPins := len(pins.Streams) > 0
	for _, c := range chains {
		seen[c.Stream] = true
		last := c.Parsed[len(c.Parsed)-1]
		pin, held := pins.Streams[c.Stream]
		if !held {
			switch {
			case !create:
			case hadPins:
				pins.add(c.Stream, streamPin{LogID: last.LogID, Size: last.MMRSize, Root: last.Root, SeenAt: now.Format(time.RFC3339)})
				notes = append(notes, "new stream since "+now.Format(time.RFC3339)+": "+c.Stream)
			default:
				pins.add(c.Stream, streamPin{LogID: last.LogID, Size: last.MMRSize, Root: last.Root, SeenAt: now.Format(time.RFC3339)})
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
// HTTP, for a fresh history_card/1, and checks that the answer is signed by
// key_id, binds the request and extends the chain the commitment staples for
// that stream. A request names no stream, so one answer covers one stream; the
// others rest on their stapled chains, and the result says so. A node that
// does not answer, or answers with a refusal or another form, leaves only the
// stapled chains checked (NOT_CHECKED), which is what the report says. The
// result is a status followed by its reason.
func liveCheck(in findInput, d *commitmentDoc, chains []cardChain, fail func(reason, finding string), nv *nodeVerdict) string {
	ask, _ := d.doc["ask"].(map[string]any)
	endpoint, _ := ask["endpoint"].(string)
	bindings, _ := ask["bindings"].([]any)
	httpBound := slices.Contains(bindings, any("http"))
	switch {
	case in.Offline:
		return "NOT_CHECKED: --offline; only the stapled chains were checked"
	case !httpBound || !strings.HasPrefix(endpoint, "https://"):
		return "NOT_CHECKED: the commitment names no https: endpoint with the http binding to ask; only the stapled chains were checked"
	case len(chains) == 0:
		return "NOT_CHECKED: no readable stapled card to extend"
	}
	floor := chains[0].Parsed[len(chains[0].Parsed)-1].MMRSize
	for _, c := range chains {
		floor = min(floor, c.Parsed[len(c.Parsed)-1].MMRSize)
	}
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	req := evidencerequest.Request{
		Subject:    evidencerequest.Subject{Kind: evidencerequest.SubjectCheckpoints},
		Coverage:   evidencerequest.Coverage{MinFreshness: &evidencerequest.Freshness{Size: &floor}},
		Derivation: stapleCard,
		Nonce:      hex.EncodeToString(nonce[:]),
		Profile:    evidencerequest.ProfileIDv1,
	}
	octets, err := evidencerequest.EncodeRequest(req, evidencerequest.MediaTypeJSON)
	if err != nil {
		fail("live_answer", "cannot form the request: "+err.Error())
		return "FAIL: cannot form the request"
	}
	reply, err := fetchHTTPS(http.MethodPost, endpoint, octets)
	if err != nil {
		return "NOT_CHECKED: the node did not answer (" + err.Error() + "); only the stapled chains were checked"
	}
	parsed, perr := evidencerequest.ParseReply(evidencerequest.Message{Octets: reply, MediaType: evidencerequest.MediaTypeJSON})
	if perr != nil || parsed.Envelope == nil {
		return "NOT_CHECKED: the node answered with a refusal or another form; only the stapled chains were checked"
	}
	msg := evidencerequest.Message{Octets: octets, MediaType: evidencerequest.MediaTypeJSON}
	row := cardChecks(reply, &msg, d.KeyID, in.CheckpointKeys)
	var bad []string
	for p, status := range row.Properties {
		if status == statusFail {
			bad = append(bad, p)
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		fail("live_answer", "the node's fresh card fails "+strings.Join(bad, ", ")+": "+strings.Join(row.Findings, "; "))
		return "FAIL: the node's fresh card does not verify"
	}
	nv.KeyBinding["answers_under_key_id"] = statusPass
	fresh, ok := chainOf(reply)
	if !ok {
		fail("live_answer", "the node's answer is not a readable history_card/1")
		return "FAIL: the node's answer is not a readable history_card/1"
	}
	for _, held := range chains {
		if held.Stream != fresh.Stream {
			continue
		}
		last := held.Parsed[len(held.Parsed)-1]
		if why := fresh.extendsPin(last.MMRSize, last.Root); why != "" {
			fail("live_consistency", "stream "+held.Stream+": "+why)
			return "FAIL: stream " + held.Stream + ": " + why
		}
		out := "PASS: the node's fresh card for stream " + held.Stream + " extends the checkpoint held"
		if len(chains) > 1 {
			out += "; the other streams rest on their stapled chains"
		}
		return out
	}
	return "NOT_CHECKED: the node answered a stream the commitment does not staple (" + fresh.Stream + "); only the stapled chains were checked"
}
