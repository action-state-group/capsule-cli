package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"golang.org/x/net/websocket"
)

// Nostr, as far as finding a commitment needs it: NIP-01's event id and
// BIP-340 signature, the NIP-19 `naddr` a directory entry may carry, and one
// subscription to a relay. The relay is a bulletin board, never believed: an
// event counts only after its id and signature verify, and what the event
// carries is verified as any commitment is.

// nostrEvent is a NIP-01 event.
type nostrEvent struct {
	ID        string     `json:"id"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// serialize is NIP-01's id preimage: [0, pubkey, created_at, kind, tags,
// content] as compact JSON, without HTML escaping.
func (e nostrEvent) serialize() ([]byte, error) {
	tags := e.Tags
	if tags == nil {
		tags = [][]string{}
	}
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode([]any{0, e.Pubkey, e.CreatedAt, e.Kind, tags, e.Content}); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(out.String(), "\n")), nil
}

// verify checks the event's id (the SHA-256 of its serialization) and its
// BIP-340 signature over that id under pubkey.
func (e nostrEvent) verify() error {
	pre, err := e.serialize()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pre)
	if !lowerHex64.MatchString(e.ID) || e.ID != hex.EncodeToString(sum[:]) {
		return errors.New("the id is not the SHA-256 of the event")
	}
	pub, perr := hex.DecodeString(e.Pubkey)
	sig, serr := hex.DecodeString(e.Sig)
	if perr != nil || serr != nil || len(pub) != 32 || len(sig) != 64 || e.Pubkey != strings.ToLower(e.Pubkey) {
		return errors.New("pubkey or sig is not lowercase hex of the right length")
	}
	key, err := schnorr.ParsePubKey(pub)
	if err != nil {
		return errors.New("pubkey is not a BIP-340 key")
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil || !parsed.Verify(sum[:], key) {
		return errors.New("the BIP-340 signature does not verify")
	}
	return nil
}

// naddr is a NIP-19 `naddr` (TLV 0 the d value, 1 a relay, 2 the author, 3 the
// kind).
type naddr struct {
	D      string
	Relays []string
	Author string
	Kind   uint32
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32Verify is BIP-173's checksum over the data part (5-bit values with the
// six checksum values last) under hrp.
func bech32Verify(hrp string, data []byte) bool {
	exp := make([]byte, 0, len(hrp)*2+1+len(data))
	for _, c := range []byte(hrp) {
		exp = append(exp, c>>5)
	}
	exp = append(exp, 0)
	for _, c := range []byte(hrp) {
		exp = append(exp, c&31)
	}
	return bech32Polymod(append(exp, data...)) == 1
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// parseNaddr reads a NIP-21 `nostr:naddr1...` URI (NIP-19's bech32, not
// bech32m).
func parseNaddr(uri string) (naddr, error) {
	s := strings.TrimPrefix(uri, "nostr:")
	if s != strings.ToLower(s) || !strings.HasPrefix(s, "naddr1") || len(s) < 6+6 {
		return naddr{}, errors.New("not a lowercase naddr1 string")
	}
	data := make([]byte, 0, len(s))
	for _, c := range s[len("naddr1"):] {
		i := strings.IndexRune(bech32Charset, c)
		if i < 0 {
			return naddr{}, errors.New("not bech32")
		}
		data = append(data, byte(i))
	}
	if !bech32Verify("naddr", data) {
		return naddr{}, errors.New("bad bech32 checksum")
	}
	payload, ok := convertBits(data[:len(data)-6], 5, 8)
	if !ok {
		return naddr{}, errors.New("bad bech32 padding")
	}
	var out naddr
	seen := map[byte]bool{}
	for len(payload) >= 2 {
		typ, n := payload[0], int(payload[1])
		if len(payload) < 2+n {
			return naddr{}, errors.New("truncated TLV")
		}
		v := payload[2 : 2+n]
		payload = payload[2+n:]
		switch typ {
		case 0:
			if seen[0] {
				return naddr{}, errors.New("two d values")
			}
			out.D = string(v)
		case 1:
			out.Relays = append(out.Relays, string(v))
		case 2:
			if len(v) != 32 {
				return naddr{}, errors.New("author is not 32 bytes")
			}
			out.Author = hex.EncodeToString(v)
		case 3:
			if len(v) != 4 {
				return naddr{}, errors.New("kind is not 32 bits")
			}
			out.Kind = uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
		}
		seen[typ] = true
	}
	if len(payload) != 0 || !seen[0] || !seen[2] || !seen[3] {
		return naddr{}, errors.New("an naddr needs d, author and kind")
	}
	return out, nil
}

func convertBits(data []byte, from, to uint) ([]byte, bool) {
	var acc, bits uint
	var out []byte
	for _, v := range data {
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&(1<<to-1)))
		}
	}
	// Padding bits must be zero and shorter than a group.
	if bits >= from || acc<<(to-bits)&(1<<to-1) != 0 {
		return nil, false
	}
	return out, true
}

// allowPrivateAddresses lets the verb reach loopback, private and link-local
// hosts (--allow-private-network). Off by default: a carrier or a commitment
// names the hosts the verb contacts, and a stranger's pointer must not aim the
// asker's machine at its own network.
var allowPrivateAddresses = false

// publicOnly is a dialer Control that refuses a connection to an address that
// is not public, after the name was resolved (so a name that resolves to a
// private address is refused too).
func publicOnly(network, address string, _ syscall.RawConn) error {
	if allowPrivateAddresses {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if addr, ok := netip.AddrFromSlice(ip); ok {
		addr = addr.Unmap()
		for _, p := range nonPublicPrefixes {
			if p.Contains(addr) {
				return fmt.Errorf("%s is not a public address (--allow-private-network reaches private hosts)", host)
			}
		}
	}
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%s is not a public address (--allow-private-network reaches private hosts)", host)
	}
	return nil
}

// nonPublicPrefixes are the ranges the standard library's predicates miss:
// this-network, carrier-grade NAT, IETF protocol assignments, benchmarking,
// reserved, site-local and NAT64.
var nonPublicPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "fec0::/10", "64:ff9b::/96"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// errRelayTruncated marks a relay that sent more than the bounds allow.
var errRelayTruncated = errors.New("relay sent more than the bounds allow")

// Bounds on what one relay may send: a relay is a stranger's server.
const (
	maxRelayEvents = 64
	maxRelayBytes  = 4 << 20
)

// nostrDialInsecure lets a test reach a ws:// relay; a real relay is wss://.
var nostrDialInsecure = false

// relayTimeout bounds one subscription.
var relayTimeout = 15 * time.Second

// queryRelay asks one relay for the events a filter matches (NIP-01 REQ) and
// returns what it sent before EOSE, the end of stored events, or the time out.
func queryRelay(relay string, filter map[string]any) ([]nostrEvent, error) {
	u, err := url.Parse(relay)
	if err != nil || (u.Scheme != "wss" && !(nostrDialInsecure && u.Scheme == "ws")) || u.Host == "" {
		return nil, fmt.Errorf("%s is not a wss:// relay URL", relay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()
	cfg, err := websocket.NewConfig(relay, "http://localhost/")
	if err != nil {
		return nil, err
	}
	cfg.Dialer = &net.Dialer{Timeout: relayTimeout, Control: publicOnly}
	conn, err := cfg.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", relay, err)
	}
	defer conn.Close()
	conn.MaxPayloadBytes = maxRelayBytes
	_ = conn.SetDeadline(time.Now().Add(relayTimeout))
	const sub = "capsulectl"
	if err := websocket.JSON.Send(conn, []any{"REQ", sub, filter}); err != nil {
		return nil, err
	}
	var events []nostrEvent
	total := 0
	for {
		var frame []json.RawMessage
		if err := websocket.JSON.Receive(conn, &frame); err != nil {
			return events, fmt.Errorf("relay %s: %w", relay, err)
		}
		if len(frame) < 2 {
			continue
		}
		var kind, id string
		_ = json.Unmarshal(frame[0], &kind)
		_ = json.Unmarshal(frame[1], &id)
		switch kind {
		case "EOSE":
			if id == sub {
				_ = websocket.JSON.Send(conn, []any{"CLOSE", sub})
				return events, nil
			}
		case "EVENT":
			var ev nostrEvent
			if len(frame) == 3 && id == sub && json.Unmarshal(frame[2], &ev) == nil {
				if total += len(frame[2]); len(events) >= maxRelayEvents || total > maxRelayBytes {
					return events, fmt.Errorf("relay %s sent more than %d events or %d bytes: %w", relay, maxRelayEvents, maxRelayBytes, errRelayTruncated)
				}
				events = append(events, ev)
			}
		}
	}
}
