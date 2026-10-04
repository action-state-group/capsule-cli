package cli

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// foldText is the form both the scrubber and the page gate match in, so a
// value cannot slip past either by changing how it is written:
//
//   - every format character is removed (zero-width space and joiners, word
//     joiner, byte-order mark, soft hyphen: 739‌142 is 739142);
//   - NFKC: fullwidth and other compatibility forms become their plain form
//     (７３９１４２ is 739142);
//   - in a word that holds a Latin letter, or that is made only of
//     lookalikes, each Cyrillic or Greek letter that imitates a Latin one
//     becomes that Latin letter (Lаrkspur with a Cyrillic а is Larkspur). A
//     word in Cyrillic or Greek proper is left as written.
func foldText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = norm.NFKC.String(s)
	return foldWord.ReplaceAllStringFunc(s, func(w string) string {
		latin, all := false, true
		for _, r := range w {
			if r < 128 && unicode.IsLetter(r) {
				latin = true
			} else if _, ok := lookalikes[r]; !ok && unicode.IsLetter(r) {
				all = false
			}
		}
		if !latin && !all {
			return w
		}
		return strings.Map(func(r rune) rune {
			if l, ok := lookalikes[r]; ok {
				return l
			}
			return r
		}, w)
	})
}

var foldWord = regexp.MustCompile(`[\p{L}\p{N}\p{M}]+`)

// lookalikes are the Cyrillic and Greek letters drawn like a Latin letter.
var lookalikes = map[rune]rune{
	// Cyrillic
	'а': 'a', 'с': 'c', 'ԁ': 'd', 'е': 'e', 'һ': 'h', 'і': 'i', 'ј': 'j', 'ӏ': 'l', 'о': 'o', 'р': 'p',
	'ԛ': 'q', 'ѕ': 's', 'у': 'y', 'х': 'x', 'ԝ': 'w', 'ү': 'y',
	'А': 'A', 'В': 'B', 'С': 'C', 'Е': 'E', 'Н': 'H', 'І': 'I', 'Ј': 'J', 'К': 'K', 'М': 'M', 'О': 'O',
	'Р': 'P', 'Ѕ': 'S', 'Т': 'T', 'Х': 'X', 'У': 'Y', 'Ү': 'Y', 'Ԝ': 'W', 'Ԛ': 'Q',
	// Greek
	'α': 'a', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x', 'γ': 'y',
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O',
	'Ρ': 'P', 'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
	// Latin forms NFKC keeps apart
	'ı': 'i', 'ȷ': 'j',
}
