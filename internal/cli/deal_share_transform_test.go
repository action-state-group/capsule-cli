package cli

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealTransforms rewrite a value the ways a message could carry it so that a
// byte match misses it.
var dealTransforms = map[string]func(string) string{
	"as is":                    func(v string) string { return v },
	"upper case":               strings.ToUpper,
	"lower case":               strings.ToLower,
	"spaced":                   func(v string) string { return joinRunes(v, " ") },
	"double spaced":            func(v string) string { return joinRunes(v, "  ") },
	"dashed":                   func(v string) string { return joinRunes(v, "-") },
	"dotted":                   func(v string) string { return joinRunes(v, ".") },
	"underscored":              func(v string) string { return joinRunes(v, "_") },
	"slashed":                  func(v string) string { return joinRunes(v, "/") },
	"zero-width spaced":        func(v string) string { return joinRunes(v, "​") },
	"soft hyphens":             func(v string) string { return joinRunes(v, "­") },
	"no-break spaces":          func(v string) string { return strings.ReplaceAll(v, " ", " ") },
	"fullwidth":                fullwidth,
	"lookalikes":               lookalike,
	"fullwidth and zero-width": func(v string) string { return joinRunes(fullwidth(v), "​") },
	"percent-encoded":          percentAll,
	"query-escaped":            url.QueryEscape,
	"html decimal":             func(v string) string { return entities(v, "&#%d;") },
	"html hex":                 func(v string) string { return entities(v, "&#x%x;") },
	"json escapes":             func(v string) string { return entities(v, `\u%04x`) },
	"base64":                   func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) },
	"base64url":                func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) },
	"hex":                      func(v string) string { return hex.EncodeToString([]byte(v)) },
	"reversed":                 reverse,
	"in a query string":        func(v string) string { return "https://shop.example/t?ref=" + url.QueryEscape(v) },
	"in a path":                func(v string) string { return "track/ABCDEF" + strings.ReplaceAll(v, " ", "") },
	"in markdown":              func(v string) string { return "**" + v + "**" },
	"digits as words":          digitWords,
}

func joinRunes(v, sep string) string {
	var parts []string
	for _, r := range v {
		if r != ' ' {
			parts = append(parts, string(r))
		}
	}
	return strings.Join(parts, sep)
}

func fullwidth(v string) string {
	return strings.Map(func(r rune) rune {
		if r > ' ' && r < 0x7f {
			return r + 0xfee0
		}
		return r
	}, v)
}

func lookalike(v string) string {
	swap := map[rune]rune{'a': 'а', 'e': 'е', 'o': 'о', 'p': 'р', 'c': 'с', 'i': 'і', 'S': 'Ѕ', 'A': 'А'}
	return strings.Map(func(r rune) rune {
		if l, ok := swap[r]; ok {
			return l
		}
		return r
	}, v)
}

func percentAll(v string) string {
	var b strings.Builder
	for _, c := range []byte(v) {
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func entities(v, format string) string {
	var b strings.Builder
	for _, r := range v {
		fmt.Fprintf(&b, format, r)
	}
	return b.String()
}

func reverse(v string) string {
	r := []rune(v)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func digitWords(v string) string {
	names := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}
	var out []string
	for _, r := range v {
		if r >= '0' && r <= '9' {
			out = append(out, names[r-'0'])
		} else if r != ' ' && r != '-' {
			out = append(out, string(r))
		}
	}
	return strings.Join(out, " ")
}

// Every transform of the planted home address, code and card number, sent
// in a message, is withheld from the adjudicator's copy, or the share is
// refused (nothing written, nothing on record).
func TestDealShareTransformsAreWithheldOrRefused(t *testing.T) {
	planted := map[string]string{"address": homeAddress, "code": verifyCode, "card": cardNumber}
	for name, transform := range dealTransforms {
		for what, value := range planted {
			t.Run(name+"/"+what, func(t *testing.T) {
				raceSampleKey(t, name+"/"+what)
				dealFixture(t)
				open := map[string]any{
					"type": "purchase", "channel": "web",
					"intent": map[string]any{
						"verbatim": "Ship to " + homeAddress + ", card " + cardNumber + ", code " + verifyCode,
						"allowed":  []string{"pay"},
					},
					"who":      map[string]any{"name": shopName, "domain": "stickers.example"},
					"terms":    map[string]any{"item": "sticker", "price_minor": 627, "currency": "USD", "place": homeAddress},
					"recourse": map[string]any{"rail": "card", "refundable": true},
				}
				raw, err := json.Marshal(open)
				require.NoError(t, err)
				dealID := dealRun(t, "open", "--input", writeJSON(t, string(raw)))["deal_id"].(string)
				sent := transform(value)
				msg, err := json.Marshal(map[string]any{"from": "counterparty", "channel": "web", "text": "Here it is: " + sent + " (thanks)"})
				require.NoError(t, err)
				dealRun(t, "note", "--deal", dealID, "--kind", "message", "--input", writeJSON(t, string(msg)))

				page := filepath.Join(t.TempDir(), "r.html")
				out, err := invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID, "--html", page, "--share", "adjudicator", "--to", "x")
				if err != nil {
					// Refused: nothing written, nothing on record.
					_, statErr := os.Stat(page)
					assert.True(t, os.IsNotExist(statErr), "a refused share writes no file")
					records, _ := sharedRecords(t, dealID)
					assert.Empty(t, records, "a refused share is not on record")
					assert.Contains(t, out+err.Error(), "refusing to write the shared copy", out)
					t.Log("refused")
					return
				}
				body, err := os.ReadFile(page)
				require.NoError(t, err)
				html := string(body)
				assertCarriesNone(t, html)
				if len(sent) >= 4 {
					assert.NotContains(t, strings.ToLower(html), strings.ToLower(sent), "the transformed value itself")
				}
				assert.Contains(t, html, "Here it is: ", "the message is in the copy")
			})
		}
	}
}

// The gate alone, as if the scrubber had missed it: a page carrying any
// transform of a planted value is refused.
func TestDealSharePageGateRefusesEveryTransform(t *testing.T) {
	events := []sealedEvent{{Event: dealEvent{Kind: "open", Open: &dealOpen{
		Who:    dealWho{Name: shopName},
		Terms:  dealTerms{Place: homeAddress},
		Intent: dealIntent{Verbatim: "card " + cardNumber + ", code " + verifyCode},
	}}}}
	page := func(text string) []byte {
		line, err := json.Marshal("Here it is: " + text + " (thanks)")
		require.NoError(t, err)
		return []byte(`<!doctype html><script>window.__BUNDLE__ = {"at":"2026-09-27T18:00:00Z","amount_minor":627,"line":` + string(line) +
			`,"sig":"` + strings.Repeat("ab", 40) + `"};</script>`)
	}
	require.NoError(t, dealPageGate(page("Did: pay $6.27 by card"), events), "a clean page passes")
	planted := map[string]string{"address": homeAddress, "code": verifyCode, "card": cardNumber}
	for name, transform := range dealTransforms {
		for what, value := range planted {
			assert.Error(t, dealPageGate(page(transform(value)), events), name+"/"+what)
		}
	}
}
