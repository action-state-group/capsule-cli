package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mailboxFixture is a responder (an Ed25519 evidence key and a mailbox key)
// with a commitment that names a relay, and an asker's request.
type mailboxFixture struct {
	dir        string
	relay      *memoryRelay
	seed       ed25519.PrivateKey
	nodeKey    string
	mailboxKey string // the secret file
	mailboxPub string
	commitment string
	request    string
	requestOct []byte
	now        time.Time
}

func setClock(t *testing.T, at time.Time) {
	t.Helper()
	old := nostrNow
	nostrNow = func() time.Time { return at }
	t.Cleanup(func() { nostrNow = old })
}

// requestWith is a conforming request, as `request --output` writes it, with a
// nonce and a deadline.
func requestWith(t *testing.T, nonce string, deadline time.Time) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "deal-staple-printed", "staple-card.json"))
	require.NoError(t, err)
	var f struct {
		Staple struct {
			Request map[string]any `json:"request"`
		} `json:"staple"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	req := f.Staple.Request
	req["nonce"] = nonce
	if !deadline.IsZero() {
		req["deadline"] = deadline.UTC().Format("2006-01-02T15:04:05Z")
	}
	out, err := evidencerequest.EncodeJCS(req)
	require.NoError(t, err)
	return out
}

// answerFor is the responder's signed envelope for the request octets: the
// fixture's envelope bound to this request's digest and signed by key.
func answerFor(t *testing.T, key ed25519.PrivateKey, request []byte) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "deal-staple-printed", "staple-card.json"))
	require.NoError(t, err)
	var f struct {
		Staple struct {
			Reply json.RawMessage `json:"reply"`
		} `json:"staple"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	env := mustDecode(t, f.Staple.Reply).(map[string]any)
	artifact := env["artifact"]
	delete(env, "sig")
	delete(env, "artifact")
	env["responder"] = hex.EncodeToString(key.Public().(ed25519.PublicKey))
	env["request_digest"] = requestDigest(request)
	body, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	env["sig"] = hex.EncodeToString(ed25519.Sign(key, body))
	env["artifact"] = artifact
	out, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	return out
}

func newMailboxFixture(t *testing.T) *mailboxFixture {
	t.Helper()
	f := &mailboxFixture{dir: t.TempDir(), relay: newMemoryRelay(t), now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	setClock(t, f.now)
	_, seed, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	f.seed, f.nodeKey = seed, hex.EncodeToString(seed.Public().(ed25519.PublicKey))
	out, err := invoke(t, "", "mailbox", "key", "new", "--output", filepath.Join(f.dir, "mailbox.key"))
	require.NoError(t, err, out)
	f.mailboxKey = filepath.Join(f.dir, "mailbox.key")
	key, err := readNostrKey(f.mailboxKey)
	require.NoError(t, err)
	f.mailboxPub = nostrPublicKey(key)
	commitment := resignWith(t, seed, func(doc map[string]any) {
		doc["ask"].(map[string]any)["nostr"] = map[string]any{"mailbox_key": f.mailboxPub, "relays": []any{f.relay.URL}}
		doc["issued_at"] = f.now.Add(-time.Hour).Format("2006-01-02T15:04:05Z")
	})
	f.commitment = filepath.Join(f.dir, "commitment.json")
	require.NoError(t, os.WriteFile(f.commitment, commitment, 0o644))
	f.requestOct = requestWith(t, "n-1", f.now.Add(time.Minute))
	f.request = filepath.Join(f.dir, "request.json")
	require.NoError(t, os.WriteFile(f.request, f.requestOct, 0o644))
	return f
}

type askResult = mailboxAskReport

func (f *mailboxFixture) ask(t *testing.T, extra ...string) (askResult, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"mailbox", "ask", "--commitment", f.commitment, "--node-key", f.nodeKey, "--request", f.request, "--allow-private-network"}, extra...)...)
	var rep askResult
	if len(out) > 0 && out[0] == '{' {
		require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	}
	return rep, err
}

func (f *mailboxFixture) poll(t *testing.T, eventID string, extra ...string) (mailboxPollReport, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"mailbox", "poll", "--commitment", f.commitment, "--node-key", f.nodeKey, "--request", f.request,
		"--request-event", eventID, "--output", filepath.Join(f.dir, "answer.json"), "--allow-private-network"}, extra...)...)
	var rep mailboxPollReport
	require.True(t, len(out) > 0 && out[0] == '{', "%s %v", out, err)
	require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	return rep, err
}

func (f *mailboxFixture) inbox(t *testing.T) mailboxInboxReport {
	t.Helper()
	out, err := invoke(t, "", "mailbox", "inbox", "--mailbox-key", f.mailboxKey, "--nostr-relay", f.relay.URL, "--out-dir", filepath.Join(f.dir, "inbox"), "--allow-private-network")
	require.NoError(t, err, out)
	var rep mailboxInboxReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	return rep
}

func (f *mailboxFixture) reply(t *testing.T, eventFile string, answer []byte, extra ...string) (string, error) {
	t.Helper()
	path := filepath.Join(f.dir, "reply.json")
	require.NoError(t, os.WriteFile(path, answer, 0o644))
	return invoke(t, "", append([]string{"mailbox", "reply", "--mailbox-key", f.mailboxKey, "--event", eventFile, "--reply", path, "--allow-private-network"}, extra...)...)
}

func TestMailboxRoundTripAsksAndAnswers(t *testing.T) {
	f := newMailboxFixture(t)
	asked, err := f.ask(t)
	require.NoError(t, err)
	assert.Equal(t, requestDigest(f.requestOct), asked.RequestDigest)
	assert.Equal(t, f.mailboxPub, asked.MailboxKey)
	assert.Equal(t, []string{f.relay.URL}, asked.Took)
	assert.Equal(t, "2026-11-18T12:01:00Z", asked.Deadline)

	// The event on the relay is the one NMB §4.1 fixes.
	events := f.relay.Events()
	require.Len(t, events, 1)
	ev := events[0]
	require.NoError(t, ev.verify())
	assert.Equal(t, mailboxRequestKind, ev.Kind)
	assert.Equal(t, asked.RequestEventID, ev.ID)
	assert.Equal(t, asked.CarriagePubkey, ev.Pubkey)
	assert.Equal(t, [][]string{{"p", f.mailboxPub}, {"m", "application/json"}, {"expiration", "1795003260"}}, ev.Tags)
	content, ok := decodeContent(ev.Content)
	require.True(t, ok)
	assert.Equal(t, f.requestOct, content, "the content is the request octets, unchanged")

	pending, err := f.poll(t, asked.RequestEventID)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "pending", pending.Status)
	assert.NoFileExists(t, filepath.Join(f.dir, "answer.json"))

	inbox := f.inbox(t)
	require.Len(t, inbox.Requests, 1)
	item := inbox.Requests[0]
	assert.Equal(t, asked.RequestEventID, item.EventID)
	assert.False(t, item.Replied)
	octets, err := os.ReadFile(item.RequestFile)
	require.NoError(t, err)
	assert.Equal(t, f.requestOct, octets)

	answer := answerFor(t, f.seed, octets)
	out, err := f.reply(t, item.EventFile, answer)
	require.NoError(t, err, out)
	reply := f.relay.Events()[1]
	require.NoError(t, reply.verify())
	assert.Equal(t, mailboxReplyKind, reply.Kind)
	assert.Equal(t, f.mailboxPub, reply.Pubkey, "the reply is signed by the mailbox key")
	assert.Equal(t, "e", reply.Tags[0][0])
	assert.Equal(t, asked.RequestEventID, reply.Tags[0][1])
	assert.Equal(t, []string{"p", asked.CarriagePubkey}, reply.Tags[1])

	got, err := f.poll(t, asked.RequestEventID)
	require.NoError(t, err)
	assert.Equal(t, "answered", got.Status)
	assert.Equal(t, "envelope", got.Outcome)
	assert.Equal(t, reply.ID, got.ReplyEventID)
	written, err := os.ReadFile(filepath.Join(f.dir, "answer.json"))
	require.NoError(t, err)
	assert.Equal(t, answer, written, "the answer octets are exactly what was signed")

	again := f.inbox(t)
	require.Len(t, again.Requests, 1, "the request is listed once")
	assert.True(t, again.Requests[0].Replied)
	_, err = f.reply(t, item.EventFile, answer)
	require.ErrorIs(t, err, ErrInput, "a request event is answered once")
}

func TestMailboxRefusesAnAnswerNotBoundToTheRequest(t *testing.T) {
	f := newMailboxFixture(t)
	asked, err := f.ask(t)
	require.NoError(t, err)
	inbox := f.inbox(t)
	t.Run("the verb that publishes refuses it", func(t *testing.T) {
		other := requestWith(t, "n-2", f.now.Add(time.Minute))
		out, err := f.reply(t, inbox.Requests[0].EventFile, answerFor(t, f.seed, other))
		require.ErrorIs(t, err, ErrInput, out)
		assert.Len(t, f.relay.Events(), 1, "nothing is published")
	})
	t.Run("one published anyway is refused by the asker, never written", func(t *testing.T) {
		// A signed answer to the same request but another nonce (so another
		// digest), delivered under the mailbox key: attributed, and failed.
		other := requestWith(t, "n-2", f.now.Add(time.Minute))
		key, err := readNostrKey(f.mailboxKey)
		require.NoError(t, err)
		var request nostrEvent
		for _, ev := range f.relay.Events() {
			request = ev
		}
		ev, err := replyEvent(key, request, f.relay.URL, "application/json", answerFor(t, f.seed, other), f.now)
		require.NoError(t, err)
		f.relay.Put(ev)
		rep, err := f.poll(t, asked.RequestEventID)
		require.ErrorIs(t, err, ErrPartial)
		assert.Equal(t, "pending", rep.Status)
		require.Len(t, rep.Rejected, 1)
		assert.Contains(t, rep.Rejected[0].Reason, "request_binding")
		assert.NoFileExists(t, filepath.Join(f.dir, "answer.json"))
	})
	t.Run("an answer under another key is refused", func(t *testing.T) {
		_, impostor, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		key, _ := readNostrKey(f.mailboxKey)
		var request nostrEvent
		for _, ev := range f.relay.Events() {
			if ev.Kind == mailboxRequestKind {
				request = ev
			}
		}
		ev, err := replyEvent(key, request, "", "application/json", answerFor(t, impostor, f.requestOct), f.now.Add(time.Second))
		require.NoError(t, err)
		f.relay.Put(ev)
		rep, _ := f.poll(t, asked.RequestEventID)
		assert.Equal(t, "pending", rep.Status)
		reasons := ""
		for _, r := range rep.Rejected {
			reasons += r.Reason
		}
		assert.Contains(t, reasons, "responder_trust")
	})
}

func TestMailboxAnswerRepublishedByAnotherKeyStillCounts(t *testing.T) {
	f := newMailboxFixture(t)
	asked, err := f.ask(t)
	require.NoError(t, err)
	other, err := newNostrKey()
	require.NoError(t, err)
	var request nostrEvent
	for _, ev := range f.relay.Events() {
		request = ev
	}
	ev, err := replyEvent(other, request, "", "application/json", answerFor(t, f.seed, f.requestOct), f.now)
	require.NoError(t, err)
	f.relay.Put(ev)
	rep, err := f.poll(t, asked.RequestEventID)
	require.NoError(t, err)
	assert.Equal(t, "answered", rep.Status, "the responder's own signature is what counts (NMB §4.2, reply by content)")
	assert.Equal(t, 1, rep.Events["reply_by_content"])
}

func TestMailboxNoAnswerIsAnAbsenceAndALateAnswerIsKept(t *testing.T) {
	f := newMailboxFixture(t)
	asked, err := f.ask(t)
	require.NoError(t, err)
	setClock(t, f.now.Add(2*time.Minute))
	rep, err := f.poll(t, asked.RequestEventID)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "absent", rep.Status)
	assert.False(t, rep.Late)
	assert.Equal(t, "2026-11-18T12:01:00Z", rep.Deadline)
	want, err := nprofileURI(f.mailboxPub, []string{f.relay.URL})
	require.NoError(t, err)
	assert.Equal(t, want, rep.Route, "the route an absence is recorded with")

	// The answer arrives after the window: kept beside the absence, never
	// rewriting it (NMB §5.3).
	inbox := f.inbox(t)
	_, err = f.reply(t, inbox.Requests[0].EventFile, answerFor(t, f.seed, f.requestOct))
	require.NoError(t, err)
	late, err := f.poll(t, asked.RequestEventID)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "absent", late.Status)
	assert.True(t, late.Late)
	assert.FileExists(t, filepath.Join(f.dir, "answer.json"))
}

func TestMailboxAskUsesOnlyACommitmentUnderTheKeyYouHold(t *testing.T) {
	f := newMailboxFixture(t)
	_, impostor, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	f.nodeKey = hex.EncodeToString(impostor.Public().(ed25519.PublicKey))
	out, err := invoke(t, "", "mailbox", "ask", "--commitment", f.commitment, "--node-key", f.nodeKey, "--request", f.request, "--allow-private-network")
	require.ErrorIs(t, err, ErrInput, out)
	assert.Contains(t, err.Error()+out, "key you hold")
	assert.Empty(t, f.relay.Events())

	t.Run("a commitment that names no mailbox", func(t *testing.T) {
		plain := resignWith(t, f.seed, nil)
		require.NoError(t, os.WriteFile(f.commitment, plain, 0o644))
		f.nodeKey = hex.EncodeToString(f.seed.Public().(ed25519.PublicKey))
		_, err := f.ask(t)
		require.ErrorIs(t, err, ErrInput)
	})
	t.Run("a commitment dated after now", func(t *testing.T) {
		future := resignWith(t, f.seed, func(doc map[string]any) {
			doc["ask"].(map[string]any)["nostr"] = map[string]any{"mailbox_key": f.mailboxPub, "relays": []any{f.relay.URL}}
			doc["issued_at"] = f.now.Add(time.Hour).Format("2006-01-02T15:04:05Z")
		})
		require.NoError(t, os.WriteFile(f.commitment, future, 0o644))
		_, err := f.ask(t)
		require.ErrorIs(t, err, ErrInput)
	})
	t.Run("a commitment whose signature fails", func(t *testing.T) {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(gooseOctets(t), &doc))
		doc["answers_within"] = "PT1H"
		altered, _ := json.Marshal(doc)
		require.NoError(t, os.WriteFile(f.commitment, altered, 0o644))
		f.nodeKey = gooseNodeKey
		_, err := f.ask(t)
		require.ErrorIs(t, err, ErrInput)
	})
}

func TestMailboxAskRefusesWhatIsNotARequest(t *testing.T) {
	f := newMailboxFixture(t)
	require.NoError(t, os.WriteFile(f.request, []byte(`{"subject":"nothing"}`), 0o644))
	_, err := f.ask(t)
	require.ErrorIs(t, err, ErrInput)
	assert.Empty(t, f.relay.Events())
}

func TestMailboxAskNeedsARelayToTakeTheRequest(t *testing.T) {
	f := newMailboxFixture(t)
	f.relay.Reject = true
	_, err := f.ask(t)
	require.ErrorIs(t, err, ErrInput)
}

func TestMailboxInboxIgnoresWhatIsNotAVerifiedRequest(t *testing.T) {
	f := newMailboxFixture(t)
	asked, err := f.ask(t)
	require.NoError(t, err)
	// A relay that alters a request leaves an id that fails: not answered.
	good := f.relay.Events()[0]
	tampered := good
	tampered.Content = encodeContent(requestWith(t, "n-evil", f.now.Add(time.Minute)))
	f.relay.mu.Lock()
	f.relay.events = append([]nostrEvent{tampered}, f.relay.events...) // delivered first, under the genuine id
	f.relay.mu.Unlock()
	// A request that is not an evidence request gets no reply.
	key, err := newNostrKey()
	require.NoError(t, err)
	junk, err := signNostrEvent(key, mailboxRequestKind, [][]string{{"p", f.mailboxPub}, {"m", "application/json"}}, encodeContent([]byte(`{"hello":1}`)), f.now.Unix())
	require.NoError(t, err)
	f.relay.Put(junk)
	rep := f.inbox(t)
	require.Len(t, rep.Requests, 1)
	assert.Equal(t, asked.RequestEventID, rep.Requests[0].EventID)
	assert.Len(t, rep.Ignored, 2)
	assert.NoFileExists(t, filepath.Join(f.dir, "inbox", junk.ID+".request"))
	octets, err := os.ReadFile(rep.Requests[0].RequestFile)
	require.NoError(t, err)
	assert.Equal(t, f.requestOct, octets, "the genuine request is written, not the tampered copy")
}

func TestMailboxReplyRefusesAnEventForAnotherMailbox(t *testing.T) {
	f := newMailboxFixture(t)
	_, err := f.ask(t)
	require.NoError(t, err)
	inbox := f.inbox(t)
	other := filepath.Join(f.dir, "other.key")
	out, err := invoke(t, "", "mailbox", "key", "new", "--output", other)
	require.NoError(t, err, out)
	f.mailboxKey = other
	_, err = f.reply(t, inbox.Requests[0].EventFile, answerFor(t, f.seed, f.requestOct))
	require.ErrorIs(t, err, ErrInput)
}

func TestMailboxReachesARelayByTheNameACommitmentGivesIt(t *testing.T) {
	f := newMailboxFixture(t)
	// The commitment names a wss relay; the asker reaches it on its own
	// network at the address it maps the name to.
	nostrDialInsecure = false
	t.Cleanup(func() { nostrDialInsecure = true })
	name := "wss://ard-demo.relay.invalid"
	commitment := resignWith(t, f.seed, func(doc map[string]any) {
		doc["ask"].(map[string]any)["nostr"] = map[string]any{"mailbox_key": f.mailboxPub, "relays": []any{name}}
		doc["issued_at"] = f.now.Add(-time.Hour).Format("2006-01-02T15:04:05Z")
	})
	require.NoError(t, os.WriteFile(f.commitment, commitment, 0o644))
	asked, err := f.ask(t)
	require.ErrorIs(t, err, ErrInput, "unreachable without a map")
	assert.Empty(t, asked.Took)
	rep, err := f.ask(t, "--relay-map", name+"="+f.relay.URL)
	require.NoError(t, err)
	assert.Equal(t, []string{name}, rep.Took, "reported under the name the commitment gives")
	assert.Len(t, f.relay.Events(), 1)

	t.Run("a ws address is reached only on your own network", func(t *testing.T) {
		_, err := f.ask(t, "--relay-map", name+"=ws://203.0.113.9:1")
		require.Error(t, err)
		allowPrivateAddresses = false
		t.Cleanup(func() { allowPrivateAddresses = true })
		_, err = invoke(t, "", "mailbox", "ask", "--commitment", f.commitment, "--node-key", f.nodeKey, "--request", f.request, "--relay-map", name+"="+f.relay.URL)
		require.ErrorIs(t, err, ErrInput, "ws:// needs --allow-private-network")
	})
	t.Run("a malformed map is refused", func(t *testing.T) {
		_, err := f.ask(t, "--relay-map", "nonsense")
		require.ErrorIs(t, err, ErrInput)
	})
}
