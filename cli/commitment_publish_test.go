package cli

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publishFixture is a profile with a fresh signing key and a printed
// `deal standard-history staple` whose card is the real fixture's, signed again
// under that key as a real node's own staple is.
type publishFixture struct {
	profile string
	public  string
	staple  string
	dir     string
	seed    ed25519.PrivateKey
}

func newPublishFixture(t *testing.T) publishFixture {
	t.Helper()
	return newPublishFixtureFrom(t, func(t *testing.T) (string, json.RawMessage, json.RawMessage) {
		raw, err := os.ReadFile(filepath.Join("testdata", "deal-staple-printed", "staple-card.json"))
		require.NoError(t, err)
		var printed struct {
			Derivation string `json:"derivation"`
			Staple     struct {
				Request json.RawMessage `json:"request"`
				Reply   json.RawMessage `json:"reply"`
			} `json:"staple"`
		}
		require.NoError(t, json.Unmarshal(raw, &printed))
		return printed.Derivation, printed.Staple.Request, printed.Staple.Reply
	})
}

// newPublishFixtureFrom is a profile with a fresh signing key whose staple is
// the card source returns, signed again under that key.
func newPublishFixtureFrom(t *testing.T, source func(*testing.T) (string, json.RawMessage, json.RawMessage)) publishFixture {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	_, seed, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	seedFile := filepath.Join(dir, "signing.seed")
	require.NoError(t, os.WriteFile(seedFile, []byte(hex.EncodeToString(seed.Seed())), 0o600))
	p := Profile{Name: "seller", Type: "jsonl", LogID: "seller", Log: LogCLL}
	p.Connection.Database = filepath.Join(dir, "seller.jsonl")
	p.Signing = Secret{File: seedFile}
	require.NoError(t, SaveProfile(p, false))
	public := hex.EncodeToString(seed.Public().(ed25519.PublicKey))

	derivation, request, cardReply := source(t)
	env := mustDecode(t, cardReply).(map[string]any)
	artifact := env["artifact"]
	delete(env, "sig")
	delete(env, "artifact")
	env["responder"] = public
	body, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	env["sig"] = hex.EncodeToString(ed25519.Sign(seed, body))
	env["artifact"] = artifact
	reply, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	staple, err := json.Marshal(map[string]any{"derivation": derivation, "voice": "silent",
		"staple": map[string]any{"request": request, "reply": json.RawMessage(reply)}})
	require.NoError(t, err)
	stapleFile := filepath.Join(dir, "staple.json")
	require.NoError(t, os.WriteFile(stapleFile, staple, 0o600))
	return publishFixture{profile: "seller", public: public, staple: stapleFile, dir: dir, seed: seed}
}

func (f publishFixture) publish(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"commitment", "publish", "--profile", f.profile, "--staple", f.staple, "--community", "goose", "--binding", "acme/respond",
		"--out", filepath.Join(f.dir, "out")}, extra...)
	return invoke(t, "", args...)
}

// The core has no default binding: a default would sign one library's name
// into every commitment (capsule-cli#428).
func TestPublishNeedsABinding(t *testing.T) {
	f := newPublishFixture(t)
	_, err := invoke(t, "", "commitment", "publish", "--profile", f.profile, "--staple", f.staple, "--community", "goose",
		"--out", filepath.Join(f.dir, "out"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--binding needs at least one value, such as acme/respond")
}

func TestPublishWritesADraftThatFindAccepts(t *testing.T) {
	f := newPublishFixture(t)
	out, err := f.publish(t)
	require.NoError(t, err, out)
	var rep publishReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.True(t, rep.Draft)
	assert.Empty(t, rep.Submitted)
	assert.Equal(t, f.public, rep.KeyID, "the commitment is signed by the profile's own key")
	octets, err := os.ReadFile(filepath.Join(f.dir, "out", rep.NodeID+".json"))
	require.NoError(t, err)
	assert.Equal(t, sha(octets), rep.Digest, "the digest is over the octets as written")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(octets, &doc))
	assert.Equal(t, f.public, doc["key_id"])
	assert.Equal(t, "evidence-commitment/1", doc["profile"])
	card := doc["history_cards"].([]any)[0].(map[string]any)
	cardOctets, err := base64.StdEncoding.DecodeString(card["bytes"].(string))
	require.NoError(t, err)
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: cardOctets, MediaType: evidencerequest.MediaTypeJSON})
	require.NoError(t, err)
	assert.Equal(t, f.public, reply.Envelope.Responder, "the card is signed by the same key")
	assert.NoError(t, reply.Envelope.Verify())

	rep2, ferr := find(t, "--file", filepath.Join(f.dir, "out", rep.NodeID+".json"), "--node-key", f.public, "--no-pin", "--offline")
	require.Len(t, rep2.Nodes, 1, "%v", rep2)
	assert.Empty(t, rep2.Refused)
	assert.Equal(t, "current", rep2.Nodes[0].Commitment.Status)
	assert.Equal(t, "PASS", rep2.Nodes[0].KeyBinding["node_key"])
	_ = ferr
}

func TestPublishRefusesACardSignedByAnotherKey(t *testing.T) {
	f := newPublishFixture(t)
	// The real fixture's card is signed by a key this profile does not hold.
	real, err := os.ReadFile(filepath.Join("testdata", "deal-staple-printed", "staple-card.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.staple, real, 0o600))
	out, err := f.publish(t)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, out+err.Error(), "signed by")
	_, statErr := os.Stat(filepath.Join(f.dir, "out"))
	assert.True(t, os.IsNotExist(statErr), "nothing is written")
}

func TestPublishRefusesAStapleThatIsNotACard(t *testing.T) {
	f := newPublishFixture(t)
	std, err := os.ReadFile(filepath.Join("testdata", "deal-staple-printed", "staple-standard.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.staple, std, 0o600))
	_, err = f.publish(t)
	require.ErrorIs(t, err, ErrInput)
}

func TestPublishRefusesASubmissionWithoutTheSellersYes(t *testing.T) {
	f := newPublishFixture(t)
	ard := filepath.Join(f.dir, "ard.json")
	card := filepath.Join(f.dir, "card.json")
	require.NoError(t, os.WriteFile(card, []byte(`{"name":"seller"}`), 0o600))
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges","displayName":"Seller","type":"application/json","url":"https://seller.example/badges"}`), 0o600))
	for _, carrier := range []string{"ard", "a2a", "nostr"} {
		out, err := f.publish(t, "--to", carrier, "--ard-file", ard, "--ard-entry", listing, "--a2a-card", card, "--commitment-uri", "https://seller.example/c.json")
		require.ErrorIs(t, err, ErrInput, carrier)
		assert.Contains(t, out+err.Error(), "--yes", carrier)
	}
	_, statErr := os.Stat(ard)
	assert.True(t, os.IsNotExist(statErr), "the seller's ard.json is not touched")
	_, statErr = os.Stat(filepath.Join(f.dir, "out"))
	assert.True(t, os.IsNotExist(statErr), "not even the draft is written for a refused submission")
	cardBytes, _ := os.ReadFile(card)
	assert.Equal(t, `{"name":"seller"}`, string(cardBytes))
	// And a yes with nothing named to submit to is not a yes to anything.
	_, err := f.publish(t, "--yes")
	require.ErrorIs(t, err, ErrInput)
}

func TestPublishSubmitsToARDAndA2AAfterTheYes(t *testing.T) {
	f := newPublishFixture(t)
	ard := filepath.Join(f.dir, "ard.json")
	require.NoError(t, os.WriteFile(ard, []byte(`{"entries":[{"identifier":"urn:air:other.example:print:x"}]}`), 0o600))
	card := filepath.Join(f.dir, "card.json")
	require.NoError(t, os.WriteFile(card, []byte(`{"name":"seller","capabilities":{"extensions":[{"uri":"`+a2aEvidenceRequestExtension+`","params":{"profiles":["evidence-interop/1"]}}]}}`), 0o600))
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges","displayName":"Seller","type":"application/json","url":"https://seller.example/badges","x-offer":{"price":1}}`), 0o600))
	out, err := f.publish(t, "--yes", "--to", "ard", "--to", "a2a", "--ard-file", ard, "--ard-entry", listing, "--a2a-card", card, "--commitment-uri", "https://seller.example/c.json")
	require.NoError(t, err, out)
	var rep publishReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.False(t, rep.Draft)

	var ardDoc struct {
		Entries []map[string]any `json:"entries"`
	}
	raw, _ := os.ReadFile(ard)
	require.NoError(t, json.Unmarshal(raw, &ardDoc))
	require.Len(t, ardDoc.Entries, 2, "the seller's other entries stay")
	mine := ardDoc.Entries[1]
	manifest := mine["trustManifest"].(map[string]any)
	assert.Equal(t, "seller.example", manifest["identity"], "identity aligns with the entry's publisher (ARD §4.5.1)")
	assert.Equal(t, map[string]any{"price": float64(1)}, mine["x-offer"], "the seller's own members are untouched")
	assert.Contains(t, mine["tags"], "evidence-request")
	att := manifest["attestations"].([]any)[0].(map[string]any)
	assert.Equal(t, "sha256:"+rep.Digest, att["digest"])
	assert.Equal(t, "https://seller.example/c.json", att["uri"])

	// A reader of the carriers finds the same pointers (V0).
	pointers, failures := directoryPointers(raw)
	require.Empty(t, failures)
	require.Len(t, pointers, 1)
	assert.Equal(t, rep.Digest, pointers[0].Digest)
	cardRaw, _ := os.ReadFile(card)
	a2a, a2aFailures := a2aPointers(cardRaw)
	require.Empty(t, a2aFailures)
	require.Len(t, a2a, 1)
	assert.Equal(t, rep.Digest, a2a[0].Digest)
	assert.Contains(t, string(cardRaw), a2aEvidenceRequestExtension, "the evidence-request entry stays")

	// Publishing again replaces the entry rather than repeating it.
	_, err = f.publish(t, "--yes", "--to", "ard", "--ard-file", ard, "--ard-entry", listing, "--commitment-uri", "https://seller.example/c.json")
	require.NoError(t, err)
	raw, _ = os.ReadFile(ard)
	require.NoError(t, json.Unmarshal(raw, &ardDoc))
	assert.Len(t, ardDoc.Entries, 2)
}

func TestPublishRefusesAnIdentityThatIsNotThePublisher(t *testing.T) {
	f := newPublishFixture(t)
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges","trustManifest":{"identity":"someone-else.example"}}`), 0o600))
	_, err := f.publish(t, "--ard-entry", listing, "--commitment-uri", "https://seller.example/c.json")
	require.ErrorIs(t, err, ErrInput)
	listing2 := filepath.Join(f.dir, "listing2.json")
	require.NoError(t, os.WriteFile(listing2, []byte(`{"identifier":"badges-no-urn"}`), 0o600))
	_, err = f.publish(t, "--ard-entry", listing2, "--commitment-uri", "https://seller.example/c.json")
	require.ErrorIs(t, err, ErrInput)
}

func TestPublishAnnouncesOnNostrAfterTheYes(t *testing.T) {
	f := newPublishFixture(t)
	relay := newMemoryRelay(t)
	key, err := newNostrKey()
	require.NoError(t, err)
	keyFile := filepath.Join(f.dir, "mailbox.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(key.Serialize())), 0o600))
	out, err := f.publish(t, "--mailbox-key", keyFile, "--nostr-relay", relay.URL, "--yes", "--to", "nostr", "--allow-private-network")
	require.NoError(t, err, out)
	var rep publishReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	events := relay.Events()
	require.Len(t, events, 1)
	require.NoError(t, events[0].verify())
	assert.Equal(t, nostrAnnounceKind, events[0].Kind)
	assert.Equal(t, nostrPublicKey(key), events[0].Pubkey, "signed by the mailbox key")
	pointer, why := nostrPointer(events[0])
	require.Empty(t, why)
	assert.Equal(t, rep.Digest, pointer.Digest)
	assert.Equal(t, sha(pointer.Inline), rep.Digest)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(pointer.Inline, &doc))
	assert.Equal(t, map[string]any{"mailbox_key": nostrPublicKey(key), "relays": []any{relay.URL}}, doc["ask"].(map[string]any)["nostr"])
	_ = time.Second
}

func TestPublishAnswersWithinIsADuration(t *testing.T) {
	f := newPublishFixture(t)
	for _, bad := range []string{"24h", "P", "PT", "P1W"} {
		_, err := f.publish(t, "--answers-within", bad)
		require.ErrorIs(t, err, ErrInput, bad)
	}
}

func TestMailboxKeyNewWritesASecretOnlyTheOwnerReads(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mailbox.key")
	out, err := invoke(t, "", "mailbox", "key", "new", "--output", file)
	require.NoError(t, err, out)
	var rep struct {
		MailboxKey string `json:"mailbox_key"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	key, err := readNostrKey(file)
	require.NoError(t, err)
	assert.Equal(t, rep.MailboxKey, nostrPublicKey(key))
	assert.NotContains(t, out, "secret\":", "the secret is not printed")
	_, err = invoke(t, "", "mailbox", "key", "new", "--output", file)
	require.ErrorIs(t, err, ErrInput, "a key is never overwritten")
}

func TestPublishWritesAMergedFileWithoutChangingNumbersThatAreNotItsOwn(t *testing.T) {
	f := newPublishFixture(t)
	ard := filepath.Join(f.dir, "ard.json")
	require.NoError(t, os.WriteFile(ard, []byte(`{"entries":[{"identifier":"urn:air:other.example:print:x","x-id":9007199254740993}]}`), 0o600))
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges","x-big":18446744073709551615}`), 0o600))
	_, err := f.publish(t, "--yes", "--to", "ard", "--ard-file", ard, "--ard-entry", listing, "--commitment-uri", "https://seller.example/c.json")
	require.NoError(t, err)
	raw, err := os.ReadFile(ard)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "9007199254740993", "another entry's number is kept exactly")
	assert.Contains(t, string(raw), "18446744073709551615", "the seller's own number is kept exactly")
}

func TestPublishRefusesACardFileTheReaderWouldRefuse(t *testing.T) {
	f := newPublishFixture(t)
	// The evidence-request entry carries a commitment member: V0 refuses the
	// whole card, so the verb writes nothing.
	original := `{"capabilities":{"extensions":[{"uri":"` + a2aEvidenceRequestExtension + `","params":{"commitment_uri":"https://x.example/c.json"}}]}}`
	card := filepath.Join(f.dir, "card.json")
	require.NoError(t, os.WriteFile(card, []byte(original), 0o600))
	_, err := f.publish(t, "--yes", "--to", "a2a", "--a2a-card", card, "--commitment-uri", "https://seller.example/c.json")
	require.ErrorIs(t, err, ErrInput)
	got, _ := os.ReadFile(card)
	assert.Equal(t, original, string(got), "the seller's card is untouched")
}

func TestPublishRefusesInputsTheLoopCheckerRefuses(t *testing.T) {
	f := newPublishFixture(t)
	for name, args := range map[string][]string{
		"no history_card/1 among the derivations":      {"--derivation", "checkpoints"},
		"a predicate class that is not <class>:<name>": {"--predicate-class", "performed"},
		"an endpoint that is a number":                 {"--endpoint", "12345"},
		"a subject that is a number":                   {"--subject", "2026"},
		"a checkpoint key that is not the card's":      {"--checkpoint-key", strings.Repeat("ab", 32)},
		"a relay IPv6 literal not in compressed form":  {"--mailbox-key", f.mailboxKey(t), "--nostr-relay", "wss://[0:0:0:0:0:0:0:1]"},
		"a relay on the default port":                  {"--mailbox-key", f.mailboxKey(t), "--nostr-relay", "wss://relay.example:443"},
		"a mailbox key without relays":                 {"--mailbox-key", f.mailboxKey(t)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.publish(t, args...)
			require.ErrorIs(t, err, ErrInput)
		})
	}
}

func (f publishFixture) mailboxKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.dir, "mb.key")
	if _, err := os.Stat(path); err != nil {
		_, err := invoke(t, "", "mailbox", "key", "new", "--output", path)
		require.NoError(t, err)
	}
	return path
}

func TestPublishNamesTheKeyTheCardIsCheckpointedUnder(t *testing.T) {
	f := newPublishFixture(t)
	out, err := f.publish(t)
	require.NoError(t, err, out)
	var rep publishReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	octets, _ := os.ReadFile(filepath.Join(f.dir, "out", rep.NodeID+".json"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(octets, &doc))
	card := doc["history_cards"].([]any)[0].(map[string]any)
	der, err := base64.RawURLEncoding.DecodeString(card["checkpoint_key"].(string))
	require.NoError(t, err)
	cardBytes, _ := base64.StdEncoding.DecodeString(card["bytes"].(string))
	chain, ok := chainOf(cardBytes)
	require.True(t, ok)
	assert.Equal(t, chain.Parsed[len(chain.Parsed)-1].KeyID, hex.EncodeToString(der[len(der)-32:]))
}

func TestPublishRefusesAFileWithTextAfterTheObject(t *testing.T) {
	f := newPublishFixture(t)
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges"}`), 0o600))
	for name, content := range map[string]string{"ard.json": `{"entries":[]} garbage`, "card.json": `{"capabilities":{}} {"x":1}`} {
		path := filepath.Join(f.dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		args := []string{"--yes", "--commitment-uri", "https://seller.example/c.json"}
		if name == "ard.json" {
			args = append(args, "--to", "ard", "--ard-file", path, "--ard-entry", listing)
		} else {
			args = append(args, "--to", "a2a", "--a2a-card", path)
		}
		_, err := f.publish(t, args...)
		require.ErrorIs(t, err, ErrInput, name)
		got, _ := os.ReadFile(path)
		assert.Equal(t, content, string(got), name+" is untouched")
	}
}

func TestPublishIsNotStoppedByAnotherEntryTheReaderRefuses(t *testing.T) {
	f := newPublishFixture(t)
	ard := filepath.Join(f.dir, "ard.json")
	// An old commitment attestation with a size member: V0 refuses that entry.
	old := `{"entries":[{"identifier":"urn:air:old.example:x","trustManifest":{"identity":"old.example","attestations":[{"type":"evidence-commitment/1","uri":"https://old.example/c.json","mediaType":"application/json","digest":"sha256:` + strings.Repeat("0", 64) + `","size":12}]}}]}`
	require.NoError(t, os.WriteFile(ard, []byte(old), 0o600))
	listing := filepath.Join(f.dir, "listing.json")
	require.NoError(t, os.WriteFile(listing, []byte(`{"identifier":"urn:air:seller.example:print:badges"}`), 0o600))
	_, err := f.publish(t, "--yes", "--to", "ard", "--ard-file", ard, "--ard-entry", listing, "--commitment-uri", "https://seller.example/c.json")
	require.NoError(t, err)
	raw, _ := os.ReadFile(ard)
	assert.Contains(t, string(raw), "urn:air:seller.example:print:badges")
	assert.Contains(t, string(raw), "old.example", "the seller's other entry stays as it was")
}

// newCard2PublishFixture is a profile whose staple is the first passing
// history_card/2 vector, signed again under the profile's key.
func newCard2PublishFixture(t *testing.T) publishFixture {
	t.Helper()
	return newPublishFixtureFrom(t, func(t *testing.T) (string, json.RawMessage, json.RawMessage) {
		for _, v := range card2Vectors(t) {
			if v.Pass {
				return "history_card/2", json.RawMessage(v.Request), json.RawMessage(v.Reply)
			}
		}
		t.Fatal("no passing history_card/2 vector")
		return "", nil, nil
	})
}

func TestPublishStaplesAHistoryCard2(t *testing.T) {
	f := newCard2PublishFixture(t)
	out, err := f.publish(t, "--derivation", "history_card/2")
	require.NoError(t, err, out)
	var rep publishReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	path := filepath.Join(f.dir, "out", rep.NodeID+".json")
	octets, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(octets, &doc))
	assert.Equal(t, "history_card/2", doc["history_cards"].([]any)[0].(map[string]any)["derivation"])
	assert.Equal(t, []any{"history_card/2"}, doc["answers"].(map[string]any)["derivations"])
	policy, err := os.ReadFile(filepath.Join(f.dir, "out", "policies", rep.NodeID+".json"))
	require.NoError(t, err)
	assert.Contains(t, string(policy), `"history_card/2"`)
	assert.NotContains(t, string(policy), `"history_card/1"`)

	found, _ := find(t, "--file", path, "--node-key", f.public, "--no-pin", "--offline")
	require.Len(t, found.Nodes, 1, "%v", found)
	assert.Empty(t, found.Refused)
	assert.Equal(t, "current", found.Nodes[0].Commitment.Status)

	got := runStaple(t, path, "--counterparty-key", f.public)
	require.Len(t, got.Staples, 1, got.raw)
	for name, status := range got.Staples[0].Properties {
		assert.NotEqual(t, "FAIL", status, name)
	}
	assert.Equal(t, "PASS", got.Staples[0].Properties["card_form"], got.raw)
}

func TestPublishAHistoryCard2NeedsItsDerivationNamed(t *testing.T) {
	f := newCard2PublishFixture(t)
	_, err := f.publish(t)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "history_card/2")
	_, statErr := os.Stat(filepath.Join(f.dir, "out"))
	assert.True(t, os.IsNotExist(statErr), "nothing is written")
}

func TestPublishRefusesAStapleLabelledForAnotherCard(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture func(*testing.T) publishFixture
		from    string
		to      string
	}{
		"a /1 card labelled /2": {newPublishFixture, "history_card/1", "history_card/2"},
		"a /2 card labelled /1": {newCard2PublishFixture, "history_card/2", "history_card/1"},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.fixture(t)
			raw, err := os.ReadFile(f.staple)
			require.NoError(t, err)
			var staple map[string]any
			require.NoError(t, json.Unmarshal(raw, &staple))
			require.Equal(t, tc.from, staple["derivation"])
			staple["derivation"] = tc.to
			raw, err = json.Marshal(staple)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(f.staple, raw, 0o600))
			_, err = f.publish(t, "--derivation", tc.to)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, err.Error(), "labelled")
			_, statErr := os.Stat(filepath.Join(f.dir, "out"))
			assert.True(t, os.IsNotExist(statErr), "nothing is written")
		})
	}
}
