package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/spf13/cobra"
)

// mailboxRoute is the Nostr route of a commitment the asker trusts: the
// mailbox key and the relays its responder reads (NMB §4.3), taken only from
// a commitment that passes V2 to V4 and whose key is the one the asker holds
// for the responder (ECC §3, "Which route to use").
type mailboxRoute struct {
	ResponderKey string
	MailboxKey   string
	Relays       []string
}

// routeFromCommitment reads the route from commitment octets. nodeKey is the
// responder key the asker holds, from the deal or request it holds and never
// from a carrier: a commitment under another key is a route to whoever holds
// that key, so its mailbox is not used.
func routeFromCommitment(octets []byte, nodeKey string, now time.Time) (mailboxRoute, error) {
	if !lowerHex64.MatchString(nodeKey) {
		return mailboxRoute{}, InputError("--node-key is the responder key you hold, 64 lowercase hex: a mailbox is taken only from a commitment under it")
	}
	d := checkCommitmentOctets(octets)
	if d.Refused != "" {
		return mailboxRoute{}, InputError("the commitment is refused (" + d.Refused + "): " + d.Detail)
	}
	rankCommitments([]*commitmentDoc{d}, now, commitmentSkew)
	switch {
	case d.Status == "future_dated":
		return mailboxRoute{}, InputError("the commitment is dated after now: it is not used (V4)")
	case d.KeyID != nodeKey:
		return mailboxRoute{}, InputError("the commitment is under key " + d.KeyID + ", not the key you hold for the responder: its mailbox is a route to whoever holds that key (ECC §3)")
	}
	ask, _ := d.doc["ask"].(map[string]any)
	nostr, ok := ask["nostr"].(map[string]any)
	if !ok {
		return mailboxRoute{}, InputError("the commitment names no mailbox (ask.nostr): this node cannot be asked this way")
	}
	route := mailboxRoute{ResponderKey: d.KeyID}
	route.MailboxKey, _ = nostr["mailbox_key"].(string)
	for _, r := range asList(nostr["relays"]) {
		if s, ok := r.(string); ok {
			route.Relays = append(route.Relays, s)
		}
	}
	if !lowerHex64.MatchString(route.MailboxKey) || len(nostr) != 2 {
		return mailboxRoute{}, InputError("ask.nostr is exactly a 64 lowercase hex mailbox_key and a list of relays")
	}
	if err := checkRelayList(route.Relays); err != nil {
		return mailboxRoute{}, InputError("ask.nostr.relays: " + err.Error())
	}
	return route, nil
}

// mediaTypeFlag reads --media-type and refuses any but IP §11.4.1 rule 7's two.
func mediaTypeFlag(c *cobra.Command) (string, error) {
	media, _ := c.Flags().GetString("media-type")
	if !slices.Contains(mailboxMediaTypes, media) {
		return "", InputError("--media-type is " + strings.Join(mailboxMediaTypes, " or "))
	}
	return media, nil
}

// requestOf reads the request octets and checks them as a request, with its
// deadline (zero when it has none).
func requestOf(path, media string) ([]byte, time.Time, error) {
	octets, err := ReadInput(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	check := evidencerequest.ParseRequest(evidencerequest.Message{Octets: octets, MediaType: media})
	if check.Reason != "" {
		return nil, time.Time{}, InputError("--request is not a conforming evidence request (" + check.Reason + "): " + check.Why)
	}
	var deadline time.Time
	if check.Request.Deadline != "" {
		var ok bool
		if deadline, ok = evidencerequest.ParseRequestTime(check.Request.Deadline); !ok {
			return nil, time.Time{}, InputError("--request's deadline is not an RFC 3339 UTC time")
		}
	}
	return octets, deadline, nil
}

// withPrivateNetwork lifts the public-address guard for --allow-private-network
// while fn runs.
func withPrivateNetwork(c *cobra.Command, fn func() error) error {
	allow, _ := c.Flags().GetBool("allow-private-network")
	mapped, _ := c.Flags().GetStringArray("relay-map")
	prev, prevMap := allowPrivateAddresses, relayMap
	relayMap = map[string]string{}
	for k, v := range prevMap {
		relayMap[k] = v
	}
	for _, m := range mapped {
		name, to, ok := strings.Cut(m, "=")
		if !ok || relayFormProblem(name) != "" || (!strings.HasPrefix(to, "wss://") && !strings.HasPrefix(to, "ws://")) {
			return InputError("--relay-map is NAME=URL: the relay name a commitment gives (wss://...) and the ws:// or wss:// address you reach it at")
		}
		relayMap[name] = to
	}
	allowPrivateAddresses = allowPrivateAddresses || allow
	defer func() { allowPrivateAddresses, relayMap = prev, prevMap }()
	return fn()
}

const relayMapHelp = "A relay name a commitment gives, NAME=URL: reach it at this address instead (a relay of your own network; a ws:// address needs --allow-private-network and is refused on a public host) (repeat for each)"

// ---- ask ----

type mailboxAskReport struct {
	RequestEventID string            `json:"request_event_id"`
	RequestDigest  string            `json:"request_digest"`
	MailboxKey     string            `json:"mailbox_key"`
	CarriagePubkey string            `json:"carriage_pubkey"`
	Deadline       string            `json:"deadline,omitempty"`
	Route          string            `json:"route"`
	Took           []string          `json:"took"`
	Failed         map[string]string `json:"failed,omitempty"`
	Notes          []string          `json:"notes,omitempty"`
}

func mailboxAskCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ask", Short: "Publish an evidence request to the mailbox a commitment names (the events are not encrypted: relays and their readers see the request and its answer)", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		commitmentPath, _ := c.Flags().GetString("commitment")
		nodeKey, _ := c.Flags().GetString("node-key")
		requestPath, _ := c.Flags().GetString("request")
		carriagePath, _ := c.Flags().GetString("carriage-key")
		media, err := mediaTypeFlag(c)
		if err != nil {
			return err
		}
		if commitmentPath == "" || requestPath == "" {
			return InputError("--commitment and --request are required: the commitment that names the mailbox, and the request octets `request --output` wrote")
		}
		now := nostrNow()
		raw, err := ReadInput(commitmentPath)
		if err != nil {
			return err
		}
		route, err := routeFromCommitment(raw, nodeKey, now)
		if err != nil {
			return err
		}
		octets, deadline, err := requestOf(requestPath, media)
		if err != nil {
			return err
		}
		carriage, err := newNostrKey()
		if carriagePath != "" {
			carriage, err = readNostrKey(carriagePath)
		}
		if err != nil {
			return err
		}
		ev, err := requestEvent(carriage, route.MailboxKey, media, octets, deadline, now)
		if err != nil {
			return err
		}
		rep := mailboxAskReport{RequestEventID: ev.ID, RequestDigest: requestDigest(octets), MailboxKey: route.MailboxKey, CarriagePubkey: ev.Pubkey}
		if !deadline.IsZero() {
			rep.Deadline = deadline.UTC().Format("2006-01-02T15:04:05Z")
		} else {
			rep.Notes = append(rep.Notes, "the request has no deadline: the wait closes at your own timeout")
		}
		if !bytes.Contains(octets, []byte(`"nonce"`)) {
			rep.Notes = append(rep.Notes, "the request has no nonce: two requests with the same octets have the same digest, and an old answer to one binds to the other")
		}
		err = withPrivateNetwork(c, func() error {
			rep.Took, rep.Failed = publishAll(route.Relays, ev)
			return nil
		})
		if err != nil {
			return err
		}
		if len(rep.Took) == 0 {
			_ = Output(c, rep)
			return Hint(ErrInput, "no relay took the request event: it reached no mailbox")
		}
		if rep.Route, err = nprofileURI(route.MailboxKey, rep.Took); err != nil {
			return err
		}
		if err := Output(c, rep); err != nil {
			return err
		}
		if len(rep.Failed) > 0 {
			return ErrPartial
		}
		return nil
	}}
	f := cmd.Flags()
	f.String("commitment", "", "The evidence commitment that names the mailbox (a file, from `commitment find` or the node)")
	f.String("node-key", "", "The responder key you hold, 64 lowercase hex, from the deal or request you hold, never from a carrier: the mailbox is taken only from a commitment under it")
	f.String("request", "", "The request octets to publish, as `request --output` wrote them")
	f.String("media-type", "application/json", "The request's media type: application/json or application/cbor")
	f.String("carriage-key", "", "A Nostr key file for the request event; default is a fresh key for this request, so your requests are not linked by key")
	f.Bool("allow-private-network", false, "Reach loopback, private and link-local relays")
	f.StringArray("relay-map", nil, relayMapHelp)
	return cmd
}

// ---- poll ----

type mailboxRejected struct {
	EventID string `json:"event_id"`
	Class   string `json:"class"`
	Reason  string `json:"reason"`
}

type mailboxOtherReply struct {
	EventID string `json:"event_id"`
	Digest  string `json:"reply_digest"`
	File    string `json:"file"`
}

type mailboxPollReport struct {
	// Status is answered (a verified reply in the window), pending (nothing
	// verified yet, the window open) or absent (the window closed with no
	// verified reply; a late reply, if any, is kept beside it).
	Status         string `json:"status"`
	RequestEventID string `json:"request_event_id"`
	RequestDigest  string `json:"request_digest"`
	Deadline       string `json:"deadline,omitempty"`
	Route          string `json:"route"`
	ReplyEventID   string `json:"reply_event_id,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	ReplyDigest    string `json:"reply_digest,omitempty"`
	Output         string `json:"output,omitempty"`
	Late           bool   `json:"late,omitempty"`
	// OtherReplies are the verified replies whose octets differ from the first:
	// the responder answered one request in more than one way.
	OtherReplies []mailboxOtherReply `json:"other_replies,omitempty"`
	Rejected     []mailboxRejected   `json:"rejected,omitempty"`
	Events       map[string]int      `json:"events"`
	Relays       map[string]string   `json:"relays"`
}

// arrivalKey names one delivered event by everything it carries, so a copy that
// differs is another event.
func arrivalKey(ev nostrEvent) string { return ev.ID + "|" + ev.Sig + "|" + ev.Content }

func mailboxPollCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "poll", Short: "Poll a commitment's mailbox for the answer to a request you published, and write the answer octets for `request --response`", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		commitmentPath, _ := c.Flags().GetString("commitment")
		nodeKey, _ := c.Flags().GetString("node-key")
		requestPath, _ := c.Flags().GetString("request")
		eventID, _ := c.Flags().GetString("request-event")
		output, _ := c.Flags().GetString("output")
		limit, _ := c.Flags().GetInt("content-limit")
		media, err := mediaTypeFlag(c)
		if err != nil {
			return err
		}
		switch {
		case commitmentPath == "" || requestPath == "" || output == "":
			return InputError("--commitment, --request and --output are required: the commitment that names the mailbox, the request you published and where the answer octets go")
		case !lowerHex64.MatchString(eventID):
			return InputError("--request-event is the request event id `mailbox ask` printed, 64 lowercase hex")
		case limit < 1:
			return InputError("--content-limit is a positive number")
		}
		now := nostrNow()
		raw, err := ReadInput(commitmentPath)
		if err != nil {
			return err
		}
		route, err := routeFromCommitment(raw, nodeKey, now)
		if err != nil {
			return err
		}
		octets, deadline, err := requestOf(requestPath, media)
		if err != nil {
			return err
		}
		digest := requestDigest(octets)
		rep := mailboxPollReport{RequestEventID: eventID, RequestDigest: digest, Events: map[string]int{}, Relays: map[string]string{}}
		if !deadline.IsZero() {
			rep.Deadline = deadline.UTC().Format("2006-01-02T15:04:05Z")
		}
		if rep.Route, err = nprofileURI(route.MailboxKey, route.Relays); err != nil {
			return err
		}
		var events []nostrEvent
		// arrived is when each event first reached us: the clock as its relay's
		// answer came in, never created_at (NMB §5.3).
		arrived := map[string]time.Time{}
		answered := 0
		err = withPrivateNetwork(c, func() error {
			for _, relay := range route.Relays {
				got, qerr := queryRelay(relay, map[string]any{"kinds": []int{mailboxReplyKind}, "#e": []string{eventID}})
				// Whatever a relay sent before it failed or was cut off still
				// counts: a relay that sent too much is not a reason to lose
				// the answer it also sent.
				events = append(events, got...)
				at := nostrNow()
				for _, ev := range got {
					if first, seen := arrived[arrivalKey(ev)]; !seen || at.Before(first) {
						arrived[arrivalKey(ev)] = at
					}
				}
				if qerr != nil && !errors.Is(qerr, errRelayTruncated) {
					// What it sent before it failed still counts; the failure
					// is reported beside it.
					rep.Relays[relay] = qerr.Error()
					if len(got) > 0 {
						answered++
					}
					continue
				}
				answered++
				rep.Relays[relay] = "ok"
			}
			return nil
		})
		if err != nil {
			return err
		}
		if answered == 0 {
			_ = Output(c, rep)
			return Hint(ErrInput, "no relay answered: the mailbox could not be read")
		}
		proves := func(b []byte) bool { return answerTo(b, media, route.ResponderKey, digest) == nil }
		var verified []replyClass
		for _, cl := range classifyReplies(events, eventID, media, route.MailboxKey, proves, limit) {
			rep.Events[strings.SplitN(cl.Class, ":", 2)[0]]++
			if !cl.isReply() {
				continue
			}
			// A reply attributed to the mailbox that does not verify is a
			// response received and failed (NMB §4.2): it is refused here and
			// never written, so it cannot stand in for the answer.
			if err := answerTo(cl.Octets, media, route.ResponderKey, digest); err != nil {
				rep.Rejected = append(rep.Rejected, mailboxRejected{EventID: cl.Event.ID, Class: cl.Class, Reason: err.Error()})
				continue
			}
			verified = append(verified, cl)
		}
		// A reply is timely when it reached us by the deadline. Timely replies
		// come first; one that arrives after the window is kept beside the
		// absence, which it never rewrites.
		late := func(v replyClass) bool { return !deadline.IsZero() && arrived[arrivalKey(v.Event)].After(deadline) }
		sort.SliceStable(verified, func(i, j int) bool { return !late(verified[i]) && late(verified[j]) })
		switch {
		case len(verified) == 0 && !deadline.IsZero() && nostrNow().After(deadline):
			rep.Status = "absent"
		case len(verified) == 0:
			rep.Status = "pending"
		case late(verified[0]):
			rep.Status, rep.Late = "absent", true
		default:
			rep.Status = "answered"
		}
		if len(verified) > 0 {
			first := verified[0]
			rep.ReplyEventID = first.Event.ID
			rep.ReplyDigest = requestDigest(first.Octets)
			reply, _ := evidencerequest.ParseReply(evidencerequest.Message{Octets: first.Octets, MediaType: media})
			rep.Outcome = "envelope"
			if reply.Refusal != nil {
				rep.Outcome = "refusal"
			}
			if err := os.WriteFile(output, first.Octets, 0o644); err != nil {
				return err
			}
			rep.Output = output
			// Every distinct verified reply is kept, each in its own file, for
			// the verifier to compare (caller_invariance): the first at
			// --output, the others beside it.
			kept := map[string]bool{rep.ReplyDigest: true}
			for _, v := range verified[1:] {
				digest := requestDigest(v.Octets)
				if kept[digest] {
					continue
				}
				kept[digest] = true
				name := fmt.Sprintf("%s.%d", output, len(rep.OtherReplies)+2)
				if err := os.WriteFile(name, v.Octets, 0o644); err != nil {
					return err
				}
				rep.OtherReplies = append(rep.OtherReplies, mailboxOtherReply{EventID: v.Event.ID, Digest: digest, File: name})
			}
		}
		if err := Output(c, rep); err != nil {
			return err
		}
		if rep.Status != "answered" {
			return ErrPartial
		}
		return nil
	}}
	f := cmd.Flags()
	f.String("commitment", "", "The evidence commitment that names the mailbox (a file)")
	f.String("node-key", "", "The responder key you hold, 64 lowercase hex: an answer counts only when this key signed it for this request")
	f.String("request", "", "The request octets you published")
	f.String("request-event", "", "The request event id `mailbox ask` printed")
	f.String("media-type", "application/json", "The request's media type: application/json or application/cbor")
	f.String("output", "", "Where the answer octets are written, to pass to `request --response`")
	f.Int("content-limit", contentVerificationLimit, "At most this many events not attributed to the mailbox are verified by content; the rest are not replies")
	f.Bool("allow-private-network", false, "Reach loopback, private and link-local relays")
	f.StringArray("relay-map", nil, relayMapHelp)
	return cmd
}

// ---- inbox ----

type mailboxInboxItem struct {
	EventID        string `json:"event_id"`
	MediaType      string `json:"media_type"`
	RequestDigest  string `json:"request_digest"`
	RequestFile    string `json:"request_file"`
	EventFile      string `json:"event_file"`
	Relay          string `json:"relay"`
	CarriagePubkey string `json:"carriage_pubkey"`
	Deadline       string `json:"deadline,omitempty"`
	Expired        bool   `json:"expired,omitempty"`
	Replied        bool   `json:"replied"`
}

type mailboxInboxReport struct {
	MailboxKey string             `json:"mailbox_key"`
	Requests   []mailboxInboxItem `json:"requests"`
	Ignored    []mailboxRejected  `json:"ignored,omitempty"`
	Relays     map[string]string  `json:"relays"`
}

// inboxEvent is the file `mailbox inbox` keeps beside each request for
// `mailbox reply`: the verified request event and the relay it was read from,
// where the reply goes (NMB §4.3).
type inboxEvent struct {
	Event       nostrEvent `json:"event"`
	Relay       string     `json:"relay"`
	MediaType   string     `json:"media_type"`
	RequestFile string     `json:"request_file"`
}

func mailboxInboxCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "inbox", Short: "Read the evidence requests addressed to your mailbox key and write each one's octets, for `respond`", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		keyPath, _ := c.Flags().GetString("mailbox-key")
		relays, _ := c.Flags().GetStringArray("nostr-relay")
		dir, _ := c.Flags().GetString("out-dir")
		if keyPath == "" || dir == "" || len(relays) == 0 {
			return InputError("--mailbox-key, --nostr-relay and --out-dir are required: your mailbox key, the relays you read, and where the requests are written")
		}
		key, err := readNostrKey(keyPath)
		if err != nil {
			return err
		}
		if err := checkRelayList(relays); err != nil {
			return InputError("--nostr-relay: " + err.Error())
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		pub := nostrPublicKey(key)
		rep := mailboxInboxReport{MailboxKey: pub, Requests: []mailboxInboxItem{}, Relays: map[string]string{}}
		now := nostrNow()
		seen := map[string]bool{}
		answered := 0
		err = withPrivateNetwork(c, func() error {
			for _, relay := range relays {
				events, qerr := queryRelay(relay, map[string]any{"kinds": []int{mailboxRequestKind}, "#p": []string{pub}})
				if qerr != nil && !errors.Is(qerr, errRelayTruncated) {
					rep.Relays[relay] = qerr.Error()
					continue
				}
				answered++
				rep.Relays[relay] = "ok"
				for _, ev := range events {
					// An event is one only once it verifies: a relay that
					// delivers a tampered copy under a genuine id first must
					// not hide the genuine one.
					if ev.verify() == nil {
						if seen[ev.ID] {
							continue
						}
						seen[ev.ID] = true
					}
					item, reason := inboxRequest(dir, relay, pub, ev, now)
					if reason != "" {
						rep.Ignored = append(rep.Ignored, mailboxRejected{EventID: ev.ID, Class: "ignored", Reason: reason})
						continue
					}
					rep.Requests = append(rep.Requests, item)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if answered == 0 {
			_ = Output(c, rep)
			return Hint(ErrInput, "no relay answered: the mailbox could not be read")
		}
		return Output(c, rep)
	}}
	f := cmd.Flags()
	f.String("mailbox-key", "", "Your mailbox key file (`mailbox key new`)")
	f.StringArray("nostr-relay", nil, "A relay (wss:) you read your mailbox on (repeat for each)")
	f.String("out-dir", "", "Where each request's octets (<id>.request) and its event (<id>.event.json) are written")
	f.Bool("allow-private-network", false, "Reach loopback, private and link-local relays")
	f.StringArray("relay-map", nil, relayMapHelp)
	return cmd
}

// inboxRequest checks one delivered request event (NMB §4.1) and, when it is a
// conforming evidence request for this mailbox, writes it. An event that fails
// is ignored with the reason: it is not answered (never refused, since the
// carriage key's signature is carriage and not requester authentication).
func inboxRequest(dir, relay, pub string, ev nostrEvent, now time.Time) (mailboxInboxItem, string) {
	if err := ev.verify(); err != nil {
		return mailboxInboxItem{}, "the event does not verify: " + err.Error()
	}
	if ev.Kind != mailboxRequestKind {
		return mailboxInboxItem{}, "the kind is not the request kind"
	}
	if p := tagsNamed(ev, "p"); len(p) != 1 || len(p[0]) < 2 || p[0][1] != pub {
		return mailboxInboxItem{}, "the event has not exactly one p tag naming this mailbox key"
	}
	m := tagsNamed(ev, "m")
	if len(m) != 1 || len(m[0]) < 2 || !slices.Contains(mailboxMediaTypes, m[0][1]) {
		return mailboxInboxItem{}, "the event has not exactly one m tag with a supported media type"
	}
	octets, ok := decodeContent(ev.Content)
	if !ok {
		return mailboxInboxItem{}, "content is not base64url without padding"
	}
	check := evidencerequest.ParseRequest(evidencerequest.Message{Octets: octets, MediaType: m[0][1]})
	if check.Reason != "" {
		return mailboxInboxItem{}, "not a conforming evidence request (" + check.Reason + "): it gets no reply"
	}
	item := mailboxInboxItem{EventID: ev.ID, MediaType: m[0][1], RequestDigest: requestDigest(octets), Relay: relay, CarriagePubkey: ev.Pubkey}
	if check.Request.Deadline != "" {
		item.Deadline = check.Request.Deadline
		if t, ok := evidencerequest.ParseRequestTime(check.Request.Deadline); ok {
			item.Expired = now.After(t)
		}
	}
	item.RequestFile = filepath.Join(dir, ev.ID+".request")
	item.EventFile = filepath.Join(dir, ev.ID+".event.json")
	if err := os.WriteFile(item.RequestFile, octets, 0o644); err != nil {
		return mailboxInboxItem{}, err.Error()
	}
	body, err := json.MarshalIndent(inboxEvent{Event: ev, Relay: relay, MediaType: m[0][1], RequestFile: filepath.Base(item.RequestFile)}, "", "  ")
	if err == nil {
		err = os.WriteFile(item.EventFile, body, 0o644)
	}
	if err != nil {
		return mailboxInboxItem{}, err.Error()
	}
	_, statErr := os.Stat(filepath.Join(dir, ev.ID+".replied"))
	item.Replied = statErr == nil
	return item, ""
}

// ---- reply ----

type mailboxReplyReport struct {
	ReplyEventID   string            `json:"reply_event_id"`
	RequestEventID string            `json:"request_event_id"`
	Took           []string          `json:"took"`
	Failed         map[string]string `json:"failed,omitempty"`
}

func mailboxReplyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "reply", Short: "Publish the signed answer `respond` wrote to the relay a request was read from (not encrypted: relays and their readers see the answer)", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		keyPath, _ := c.Flags().GetString("mailbox-key")
		eventPath, _ := c.Flags().GetString("event")
		replyPath, _ := c.Flags().GetString("reply")
		extra, _ := c.Flags().GetStringArray("nostr-relay")
		again, _ := c.Flags().GetBool("again")
		if keyPath == "" || eventPath == "" || replyPath == "" {
			return InputError("--mailbox-key, --event and --reply are required: your mailbox key, the <id>.event.json `mailbox inbox` wrote and the answer `respond --output` wrote")
		}
		key, err := readNostrKey(keyPath)
		if err != nil {
			return err
		}
		raw, err := ReadInput(eventPath)
		if err != nil {
			return err
		}
		var kept inboxEvent
		if err := json.Unmarshal(raw, &kept); err != nil || kept.Event.ID == "" {
			return InputError("--event is the <id>.event.json file `mailbox inbox` wrote")
		}
		if err := kept.Event.verify(); err != nil || kept.Event.Kind != mailboxRequestKind {
			return InputError("--event does not hold a request event that verifies")
		}
		if p := tagsNamed(kept.Event, "p"); len(p) != 1 || len(p[0]) < 2 || p[0][1] != nostrPublicKey(key) {
			return InputError("--event was not addressed to this mailbox key")
		}
		if !slices.Contains(mailboxMediaTypes, kept.MediaType) || filepath.Base(kept.RequestFile) != kept.RequestFile {
			return InputError("--event is not a file `mailbox inbox` wrote")
		}
		request, err := os.ReadFile(filepath.Join(filepath.Dir(eventPath), kept.RequestFile))
		if err != nil {
			return InputError("the request octets beside --event are missing: " + err.Error())
		}
		if content, ok := decodeContent(kept.Event.Content); !ok || !bytes.Equal(content, request) {
			return InputError("the request octets beside --event are not the event's content")
		}
		answer, err := ReadInput(replyPath)
		if err != nil {
			return err
		}
		parsed, perr := evidencerequest.ParseReply(evidencerequest.Message{Octets: answer, MediaType: kept.MediaType})
		digest := requestDigest(request)
		switch {
		case perr != nil || (parsed.Envelope == nil && parsed.Refusal == nil):
			return InputError("--reply is not an envelope or a refusal in the request's media type")
		case parsed.Envelope != nil && parsed.Envelope.RequestDigest != digest, parsed.Refusal != nil && parsed.Refusal.RequestDigest != digest:
			return InputError("--reply answers another request: its request_digest is not this request's")
		}
		marker := filepath.Join(filepath.Dir(eventPath), kept.Event.ID+".replied")
		if _, err := os.Stat(marker); err == nil && !again {
			return InputError("this request event was already answered: a responder answers each request event once and does not answer a redelivery (--again to publish another reply)")
		}
		ev, err := replyEvent(key, kept.Event, kept.Relay, kept.MediaType, answer, nostrNow())
		if err != nil {
			return err
		}
		relays := slices.Compact(append([]string{kept.Relay}, extra...))
		rep := mailboxReplyReport{ReplyEventID: ev.ID, RequestEventID: kept.Event.ID}
		err = withPrivateNetwork(c, func() error {
			rep.Took, rep.Failed = publishAll(relays, ev)
			return nil
		})
		if err != nil {
			return err
		}
		if len(rep.Took) == 0 {
			_ = Output(c, rep)
			return Hint(ErrInput, "no relay took the reply: the requester cannot read it")
		}
		if err := os.WriteFile(marker, []byte(ev.ID+"\n"), 0o644); err != nil {
			return fmt.Errorf("the reply was published but its marker could not be written: %w", err)
		}
		if err := Output(c, rep); err != nil {
			return err
		}
		if len(rep.Failed) > 0 {
			return ErrPartial
		}
		return nil
	}}
	f := cmd.Flags()
	f.String("mailbox-key", "", "Your mailbox key file: it signs the reply event")
	f.String("event", "", "The <id>.event.json file `mailbox inbox` wrote for the request")
	f.String("reply", "", "The answer octets `respond --output` wrote")
	f.StringArray("nostr-relay", nil, "Another relay to publish the reply to; it always goes to the relay the request was read from (repeat for each)")
	f.Bool("again", false, "Publish a reply for a request event already answered")
	f.Bool("allow-private-network", false, "Reach loopback, private and link-local relays")
	f.StringArray("relay-map", nil, relayMapHelp)
	return cmd
}
