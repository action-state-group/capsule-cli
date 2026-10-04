package cli

import (
	"encoding/base64"
	"encoding/hex"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Random identifiers are not card numbers. A 13-19 digit run inside a hex id,
// a UUID or a base64 value passes the Luhn check one time in ten, so before
// the run had to stand alone the gate refused a shared copy at random: about
// 1 in 300 for a 40-character hex id, 1 in 600 for a UUID. Seeded, so a
// failure here reproduces. Left out on purpose: an id of 19 characters or
// fewer that is all digits (one 8-byte hex id in about 2,000) is a bare
// number, as a card number is, and is still read as one.
func TestDealShareGateIgnoresRandomIdentifiers(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.UintN(256))
		}
		return b
	}
	// A slice, not a map: map order would change which kind draws which
	// random bytes, and the run would not reproduce.
	kinds := []struct {
		name string
		gen  func() string
	}{
		{"hex id (16 bytes)", func() string { return hex.EncodeToString(random(16)) }},
		{"hex id (20 bytes)", func() string { return hex.EncodeToString(random(20)) }},
		{"uuid", func() string {
			h := hex.EncodeToString(random(16))
			return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		}},
		{"base64 (16 bytes)", func() string { return base64.StdEncoding.EncodeToString(random(16)) }},
		{"base64url (32 bytes)", func() string { return base64.RawURLEncoding.EncodeToString(random(32)) }},
		{"hex id, all digits (32)", func() string { return strings.Repeat("4111", 8) }},
		{"RFC 3339 time", func() string {
			return time.Unix(1_700_000_000+r.Int64N(400_000_000), r.Int64N(1e9)).UTC().Format(time.RFC3339Nano)
		}},
		{"RFC 3339 time, millis", func() string {
			return time.UnixMilli(1_700_000_000_000 + r.Int64N(400_000_000_000)).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		}},
	}
	for _, k := range kinds {
		for range raceRounds(3000) {
			id := k.gen()
			for _, page := range [][]byte{
				gatePage(t, "Reference "+id+" on file"),
				[]byte(`<!doctype html><script>window.__BUNDLE__ = {"line":"Did: pay $6.27 by card","id":"` + id + `"};</script>`),
			} {
				if err := dealPageGate(page, nil); err != nil {
					assert.Fail(t, "a random identifier was refused", "%s %q: %v", k.name, id, err)
					return
				}
			}
		}
	}
	// A written card number is still refused: alone, with separators, joined
	// to a label or glued to a word on one side, folded from fullwidth digits
	// or split by zero-width spaces, whether or not it is this deal's.
	for _, card := range []string{
		"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111", "4242 4242 4242 4242",
		"card 5555555555554444.", "(378282246310005)",
		"pan=4111111111111111", "?card_number=4111111111111111", "VISA-4111-1111-1111-1111",
		"ref-4111111111111111", "pan_4111111111111111", "PAN4111111111111111", "4111111111111111x",
		"card:4111111111111111", "4111111111111111=", "/4111111111111111/",
		"\uff14\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11\uff11",
		"4111\u200b1111\u200b1111\u200b1111",
		// followed or preceded by a code or a date
		"4111111111111111123", "4111 1111 1111 1111 123", "4111-1111-1111-1111-0428", "123 4111 1111 1111 1111",
		"0428 4111111111111111", "4111111111111111 12/28",
	} {
		assert.Error(t, dealPageGate(gatePage(t, "Paid with "+card), nil), card)
		assert.Error(t, dealPageGate([]byte(`<!doctype html><script>window.__BUNDLE__ = {"line":"Paid with `+card+`"};</script>`), nil), card)
	}
}

// How often a bare number is read as a card number. By its digits a bare
// 13-19 digit number is one, and the anchored windows try up to fourteen
// readings of a longer run, so this is not zero. Logged, not asserted: pages
// carry times as RFC 3339 text (above, never refused), not as epoch numbers.
func TestDealShareGateBareNumberRate(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, k := range []struct {
		name string
		gen  func() string
	}{
		{"epoch milliseconds (13)", func() string { return strconv.FormatInt(1_700_000_000_000+r.Int64N(400_000_000_000), 10) }},
		{"epoch nanoseconds (19)", func() string {
			return strconv.FormatInt(1_700_000_000_000_000_000+r.Int64N(400_000_000_000_000_000), 10)
		}},
		{"random uint64 (mostly 20)", func() string { return strconv.FormatUint(r.Uint64(), 10) }},
	} {
		n := raceRounds(5000)
		refused := 0
		for range n {
			if dealPageGate(gatePage(t, "at "+k.gen()), nil) != nil {
				refused++
			}
		}
		t.Logf("%-26s refused %4d/%d (%.1f%%)", k.name, refused, n, 100*float64(refused)/float64(n))
	}
}
