package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"golang.org/x/net/websocket"
)

// Publishing to Nostr, as far as the commitment announce (ECC §5.3) and the
// mailbox events (NMB §4) need it: a BIP-340 key, a NIP-01 event signed by it
// and one EVENT sent to a relay that answers OK. The relay is only a store
// and forward hop; nothing it says is believed beyond whether it took the
// event.

// nostrKeyHex reads a BIP-340 secret key file: 64 lowercase hex characters,
// a regular file readable only by its owner (as a profile's signing secret
// is). The secret is never printed.
func readNostrKey(path string) (*btcec.PrivateKey, error) {
	raw, err := readProtected(path)
	if err != nil {
		return nil, InputError("cannot read the Nostr key file " + path + ": it must exist, be a regular file, and be readable only by its owner (chmod 600)")
	}
	text := strings.TrimSpace(string(raw))
	b, err := hex.DecodeString(text)
	if err != nil || len(b) != 32 || text != strings.ToLower(text) {
		return nil, InputError("the Nostr key file holds a 32-byte secp256k1 secret in lowercase hex")
	}
	key, _ := btcec.PrivKeyFromBytes(b)
	if key.Key.IsZero() {
		return nil, InputError("the Nostr key file holds an invalid secp256k1 secret")
	}
	return key, nil
}

// nostrPublicKey is the key's x-only public key in lowercase hex: the Nostr
// pubkey and, for a mailbox, the mailbox key.
func nostrPublicKey(key *btcec.PrivateKey) string {
	return hex.EncodeToString(schnorr.SerializePubKey(key.PubKey()))
}

// newNostrKey draws a fresh secp256k1 secret.
func newNostrKey() (*btcec.PrivateKey, error) {
	for {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		key, _ := btcec.PrivKeyFromBytes(b[:])
		if !key.Key.IsZero() {
			return key, nil
		}
	}
}

// signNostrEvent builds a NIP-01 event: its id the SHA-256 of the serialization
// and its signature BIP-340 over the id under the key.
func signNostrEvent(key *btcec.PrivateKey, kind int, tags [][]string, content string, createdAt int64) (nostrEvent, error) {
	ev := nostrEvent{Pubkey: nostrPublicKey(key), CreatedAt: createdAt, Kind: kind, Tags: tags, Content: content}
	pre, err := ev.serialize()
	if err != nil {
		return nostrEvent{}, err
	}
	sum := sha256.Sum256(pre)
	sig, err := schnorr.Sign(key, sum[:])
	if err != nil {
		return nostrEvent{}, err
	}
	ev.ID, ev.Sig = hex.EncodeToString(sum[:]), hex.EncodeToString(sig.Serialize())
	if ev.Tags == nil {
		ev.Tags = [][]string{}
	}
	return ev, nil
}

// publishRelay sends one event to one relay and returns when the relay says
// OK. A relay that says OK false, closes or stays silent past relayTimeout is
// an error here and a transport condition to the caller, never a refusal.
func publishRelay(relay string, ev nostrEvent) error {
	conn, cancel, err := dialRelay(relay)
	if err != nil {
		return err
	}
	defer cancel()
	defer conn.Close()
	if err := websocket.JSON.Send(conn, []any{"EVENT", ev}); err != nil {
		return fmt.Errorf("relay %s: %w", relay, err)
	}
	for {
		var frame []json.RawMessage
		if err := websocket.JSON.Receive(conn, &frame); err != nil {
			return fmt.Errorf("relay %s did not confirm the event: %w", relay, err)
		}
		var kind, id string
		if len(frame) >= 2 {
			_ = json.Unmarshal(frame[0], &kind)
			_ = json.Unmarshal(frame[1], &id)
		}
		if kind != "OK" || id != ev.ID {
			continue
		}
		var accepted bool
		var why string
		if len(frame) > 2 {
			_ = json.Unmarshal(frame[2], &accepted)
		}
		if len(frame) > 3 {
			_ = json.Unmarshal(frame[3], &why)
		}
		if !accepted {
			return fmt.Errorf("relay %s did not take the event: %s", relay, why)
		}
		return nil
	}
}

// publishAll sends the event to every relay and returns the relays that took
// it and the failures. The caller decides how many it needs.
func publishAll(relays []string, ev nostrEvent) (took []string, failed map[string]string) {
	failed = map[string]string{}
	for _, relay := range relays {
		if err := publishRelay(relay, ev); err != nil {
			failed[relay] = err.Error()
			continue
		}
		took = append(took, relay)
	}
	return took, failed
}

// relayForm is the one written form of a relay URL a commitment's ask.nostr
// lists, so that distinct means distinct relays: wss, a lowercase host, no
// credentials, query or fragment, no default port and no bare "/" path.
var relayForm = regexp.MustCompile(`^wss://(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*|\[[0-9a-f:.]+\])(?::([1-9][0-9]{0,4}))?(/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*)?$`)

// relayFormProblem says why relay is not in that form, or "".
func relayFormProblem(relay string) string {
	if nostrDialInsecure && strings.HasPrefix(relay, "ws://") {
		return "" // a test relay
	}
	m := relayForm.FindStringSubmatch(relay)
	switch {
	case m == nil:
		return "is not wss://host[:port][/path] with a lowercase host, and no credentials, query or fragment"
	case m[1] != "" && len(m[1]) <= 5 && atoiPort(m[1]) > 65535:
		return "has a port above 65535"
	case m[1] == "443":
		return "names the default port 443; leave it out"
	case m[2] == "/":
		return "ends in a bare /; leave it out"
	}
	if u, err := url.Parse(relay); err != nil || u.Host == "" {
		return "is not a URL"
	} else if host := u.Hostname(); strings.Contains(u.Host, "[") {
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is6() || addr.String() != host {
			return "writes its IPv6 literal other than in its compressed form"
		}
	}
	return ""
}

func atoiPort(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// checkRelayList refuses a relay list that is empty, has a relay outside the
// written form or names one relay twice.
func checkRelayList(relays []string) error {
	if len(relays) == 0 {
		return errors.New("at least one relay is needed")
	}
	seen := map[string]bool{}
	for _, relay := range relays {
		if why := relayFormProblem(relay); why != "" {
			return fmt.Errorf("relay %s %s", relay, why)
		}
		if seen[relay] {
			return fmt.Errorf("relay %s is listed twice", relay)
		}
		seen[relay] = true
	}
	return nil
}

// nostrNow is the clock the events are dated by; a test replaces it.
var nostrNow = func() time.Time { return time.Now().UTC() }
