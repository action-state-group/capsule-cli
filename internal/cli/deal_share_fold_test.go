package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The compiled table is exactly the vendored Unicode file's mappings, and
// the vendored file is the one its digest names.
func TestConfusablesAreTheVendoredUnicodeData(t *testing.T) {
	txt, err := os.ReadFile("../../third_party/unicode/confusables.txt")
	require.NoError(t, err)
	recorded, err := os.ReadFile("../../third_party/unicode/confusables.txt.sha256")
	require.NoError(t, err)
	sum := sha256.Sum256(txt)
	assert.Equal(t, strings.TrimSpace(string(recorded)), hex.EncodeToString(sum[:]), "refresh with scripts/update-confusables.sh; never hand-edit")
	assert.Equal(t, confusablesSourceSHA256, hex.EncodeToString(sum[:]))
	assert.Contains(t, string(txt), "# Unicode Security Mechanisms for UTS #39")
	version, table, err := ConfusablesTable(txt)
	require.NoError(t, err)
	assert.Equal(t, confusablesVersion, version)
	assert.Equal(t, "18.0.0", version)
	assert.Equal(t, confusablesTable, table, "regenerate with go generate ./internal/cli")
}

// The leaks a byte match, NFKC and a hand table missed. Each one reads as the
// planted value once folded and skeletonized.
var listedLeaks = []string{
	"Laŗkspur", "Làrkspur", "Spríngfield", "73̧9142", "٧٣٩١٤٢", "७३९१४२", "Larkspսr", "Larᴋspur", "ᏞᎪᎡkspur",
	// Hangul fillers: default-ignorable, drawn as blank or not at all.
	"Lar\u115fkspur", "Lar\u1160kspur", "Lar\u3164kspur", "Lar\uffa0kspur", "739\u3164142",
}

func plantedEvents() []sealedEvent {
	return []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Who:    dealWho{Name: shopName},
		Terms:  dealTerms{Place: homeAddress},
		Intent: dealIntent{Verbatim: "ship to " + homeAddress + ", card " + cardNumber + ", code " + verifyCode},
	}}}}
}

// gatePage is a shared page as far as the gate is concerned: a bundle slot
// with a timestamp, an amount, a digest-length signature and one text line.
func gatePage(t testing.TB, text string) []byte {
	line, err := json.Marshal(text)
	require.NoError(t, err)
	return []byte(`<!doctype html><script>window.__BUNDLE__ = {"at":"2026-09-27T18:00:00Z","amount_minor":627,"line":` + string(line) +
		`,"sig":"` + strings.Repeat("ab", 40) + `"};</script>`)
}

func TestDealShareListedLeaksAreWithheldAndRefused(t *testing.T) {
	events := plantedEvents()
	p := dealPrivateValues(events)
	for _, leak := range listedLeaks {
		text := "Here it is: " + leak + " (thanks)"
		assert.Error(t, dealPageGate(gatePage(t, text), events), "the gate alone refuses %q", leak)
		scrubbed := p.scrub(text)
		assert.Equal(t, "Here it is: [withheld] (thanks)", scrubbed, leak)
		assert.NoError(t, dealPageGate(gatePage(t, scrubbed), events), "the scrubbed text passes the gate: %q", scrubbed)
	}
}

// The same leaks end to end: in a message, through `deal report --share
// adjudicator`, each withheld from the copy (or the share refused).
func TestDealShareListedLeaksEndToEnd(t *testing.T) {
	for _, leak := range listedLeaks {
		t.Run(leak, func(t *testing.T) {
			dealFixture(t)
			dealID := openPrivateDeal(t)
			msg, err := json.Marshal(map[string]any{"from": "counterparty", "channel": "web", "text": "Here it is: " + leak + " (thanks)"})
			require.NoError(t, err)
			dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, string(msg)))
			page := filepath.Join(t.TempDir(), "r.html")
			shareRun(t, dealID, "adjudicator", page)
			raw, err := os.ReadFile(page)
			require.NoError(t, err)
			assertCarriesNone(t, string(raw))
			assert.NotContains(t, string(raw), leak)
			assert.Contains(t, string(raw), "Here it is: [withheld] (thanks)")
		})
	}
}

// Dates, times and amounts are not codes: a shared copy keeps them, and the
// gate does not refuse a page for showing them.
func TestDealShareKeepsDatesAndAmounts(t *testing.T) {
	text := "Delivered 09/27/2026 at 18:00, also 27.09.2026 and 2026-09-27, on October 3, 2026 or 3 Oct 2026; total $1,200.00, then 1,200.00 and $6.27"
	events := plantedEvents()
	events = append(events, sealedEvent{Event: dealEvent{Kind: "message", Message: &dealMessage{From: "counterparty", Text: text}}})
	p := dealPrivateValues(events)
	assert.Equal(t, text, p.scrub(text))
	assert.NoError(t, dealPageGate(gatePage(t, text), events))
	// A code is still a code, and a known value is withheld even when it is
	// written like a date.
	assert.Equal(t, "code [withheld]", p.scrub("code 2026"))
	assert.Equal(t, "on [withheld]", p.scrub("on 73/91/42"))
}

// A random rewriting of a planted value, built from the operations that
// change how text is written without changing how it reads.
type leakGen struct {
	r       *rand.Rand
	sources map[string][]rune // a skeleton to the characters that read as it
	digits  [10][]rune        // every script's decimal digit of each value
}

func newLeakGen(seed int64) *leakGen {
	g := &leakGen{r: rand.New(rand.NewSource(seed)), sources: map[string][]rune{}}
	confusablesOnce.Do(loadConfusables)
	for r := range confusablePrototype {
		sk := confusableSkeleton(r)
		g.sources[sk] = append(g.sources[sk], r)
	}
	for _, table := range []*unicode.RangeTable{unicode.Nd} {
		for _, r16 := range table.R16 {
			for c := rune(r16.Lo); c <= rune(r16.Hi); c += rune(r16.Stride) {
				g.digits[digitValue(c)] = append(g.digits[digitValue(c)], c)
			}
		}
		for _, r32 := range table.R32 {
			for c := rune(r32.Lo); c <= rune(r32.Hi); c += rune(r32.Stride) {
				g.digits[digitValue(c)] = append(g.digits[digitValue(c)], c)
			}
		}
	}
	return g
}

var leakSeparators = []string{" ", "-", ".", "_", "/", "​", "­", "⁠", "‮", "‍", "\ufeff", "\u115f", "\u1160", "\u3164", "\uffa0"}

// rewrite returns a rewriting of v and whether it is an encoding the
// scrubber cannot read (base64), where refusal by the gate is the only
// acceptable outcome.
func (g *leakGen) rewrite(v string) (string, bool) {
	var b strings.Builder
	sep := ""
	if g.r.Intn(3) == 0 {
		sep = leakSeparators[g.r.Intn(len(leakSeparators))]
	}
	first := true
	for _, c := range v {
		if c == ' ' {
			c = []rune{' ', ' ', ' ', '　'}[g.r.Intn(4)]
		}
		if !first && sep != "" && c != ' ' {
			b.WriteString(sep)
		}
		first = false
		out := string(c)
		switch {
		case c >= '0' && c <= '9' && g.r.Intn(2) == 0:
			list := g.digits[c-'0']
			out = string(list[g.r.Intn(len(list))])
		case unicode.IsLetter(c) && g.r.Intn(3) == 0:
			if list := g.sources[confusableSkeleton(c)]; len(list) > 0 {
				out = string(list[g.r.Intn(len(list))])
			}
		case unicode.IsLetter(c) && g.r.Intn(4) == 0:
			out = string(unicode.ToUpper(c))
		}
		if g.r.Intn(4) == 0 {
			out += string(rune(0x300 + g.r.Intn(0x70))) // a combining mark
		}
		b.WriteString(out)
	}
	s := b.String()
	if g.r.Intn(6) == 0 {
		s = reverse(s)
	}
	switch g.r.Intn(12) {
	case 0:
		return percentAll(s), false
	case 1:
		return entities(s, "&#%d;"), false
	case 2:
		var b strings.Builder
		for _, u := range utf16.Encode([]rune(s)) {
			fmt.Fprintf(&b, `\u%04x`, u)
		}
		return b.String(), false
	case 3:
		return base64.StdEncoding.EncodeToString([]byte(s)), false
	case 4:
		return "https://shop.example/t?ref=" + url.QueryEscape(s), false
	}
	return s, false
}

// The property, over random rewritings of the planted address, code and card
// number: the gate alone refuses every page that carries one, and the
// scrubber withholds every one it can read, so its output passes the gate.
// DEAL_LEAK_SEED and DEAL_LEAK_ROUNDS widen the search.
func TestDealShareRandomRewritingsAreWithheldOrRefused(t *testing.T) {
	seed, rounds := int64(20261003), 600
	if v, err := strconv.ParseInt(os.Getenv("DEAL_LEAK_SEED"), 10, 64); err == nil {
		seed = v
	}
	if v, err := strconv.Atoi(os.Getenv("DEAL_LEAK_ROUNDS")); err == nil {
		rounds = v
	}
	t.Logf("seed %d, %d rounds per value", seed, rounds)
	g := newLeakGen(seed)
	events := plantedEvents()
	p := dealPrivateValues(events)
	for _, value := range []string{homeAddress, verifyCode, cardNumber, "Larkspur", "Springfield"} {
		for i := 0; i < rounds; i++ {
			leak, encoded := g.rewrite(value)
			text := "Here it is: " + leak + " (thanks)"
			if !assert.Error(t, dealPageGate(gatePage(t, text), events), "the gate missed %q (from %q)", leak, value) {
				continue
			}
			if !encoded {
				scrubbed := p.scrub(text)
				assert.NoError(t, dealPageGate(gatePage(t, scrubbed), events), "the scrubber left a trace of %q in %q", leak, scrubbed)
			}
		}
	}
}

// FuzzDealSharePageGate: any text that reads as a planted value (once folded
// and skeletonized) is refused by the gate, and the scrubber's output of it
// passes the gate. `go test` runs the seeds; `go test -fuzz
// FuzzDealSharePageGate` searches.
func FuzzDealSharePageGate(f *testing.F) {
	for _, s := range append(append([]string{}, listedLeaks...), homeAddress, verifyCode, cardNumber, "7/3/9/1/4/2", "rupskraL", "%37%33%39%31%34%32") {
		f.Add(s)
	}
	events := plantedEvents()
	p := dealPrivateValues(events)
	reads := func(s string) bool {
		sp := spelling(foldText(s))
		for _, v := range p.spelled {
			in := sp.skeleton
			if v.numeric {
				in = sp.plain
			}
			if strings.Contains(in, v.text) {
				return true
			}
		}
		return false
	}
	f.Fuzz(func(t *testing.T, s string) {
		text := "Here it is: " + s + " (thanks)"
		if reads(s) {
			assert.Error(t, dealPageGate(gatePage(t, text), events), fmt.Sprintf("the gate missed %q", s))
		}
		assert.NoError(t, dealPageGate(gatePage(t, p.scrub(text)), events), fmt.Sprintf("the scrubber left a trace of %q", s))
	})
}
