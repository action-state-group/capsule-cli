package deal

import (
	"regexp"
	"strings"
)

// placeWord is one word of a place, as samePlace reads it.
var placeWord = regexp.MustCompile(`[\p{L}\p{N}]+(-[0-9]+)?`)

// placeRefinement is a word a place may gain and still be the same place: a
// postal code (94131, 94131-2210, 10115) or a two-letter region code (CA).
var placeRefinement = regexp.MustCompile(`^([0-9]{4,6}(-[0-9]{4})?|[a-z]{2})$`)

// samePlace reports whether b is a refinement of the same place as a: every
// word of a appears in b, in order, and every word b adds is a postal code
// or a two-letter region code. "San Francisco" and "San Francisco, CA
// 94131" are one place; "San Jose" is another, and so is "San Francisco
// Airport". Case and punctuation are not compared.
func samePlace(a, b string) bool {
	wa := placeWord.FindAllString(strings.ToLower(a), -1)
	wb := placeWord.FindAllString(strings.ToLower(b), -1)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	// Either may be the refined one.
	if len(wa) > len(wb) {
		wa, wb = wb, wa
	}
	i := 0
	for _, w := range wb {
		if i < len(wa) && w == wa[i] {
			i++
			continue
		}
		if !placeRefinement.MatchString(w) {
			return false
		}
	}
	return i == len(wa)
}
