package cli

import (
	"bufio"
	"bytes"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// foldText is the written form both the scrubber and the page gate match
// in, so a value cannot slip past either by changing how it is written:
//
//   - percent-escapes, HTML character references and \uXXXX escapes are
//     decoded, repeatedly, so an escape inside another is read too;
//   - every format character is removed (Cf: zero-width space and joiners,
//     word joiner, byte-order mark, soft hyphen, and the bidirectional
//     controls that reorder text on screen);
//   - NFKD, then every nonspacing mark (Mn) is dropped: fullwidth and other
//     compatibility forms become plain ones, and accents and other marks
//     come off (Làrkspur, Laŗkspur and 73̧9142 read Larkspur and 739142);
//   - every decimal digit (Nd) of any script becomes the ASCII digit of the
//     same value (٧٣٩١٤٢ and ७३९१४२ read 739142).
//
// Lookalike letters from other scripts are not folded here: matching reads
// them through confusableSkeleton.
func foldText(s string) string {
	for i := 0; i < 4; i++ {
		before := s
		// A backslash escaped as in a JSON string (\\u0037) is one backslash.
		s = strings.ReplaceAll(s, `\\`, `\`)
		s = jsonEscape.ReplaceAllStringFunc(s, func(m string) string {
			// One escape, or a surrogate pair of two (an astral character).
			var units []uint16
			for _, h := range strings.Split(m, `\u`)[1:] {
				v, err := strconv.ParseUint(h, 16, 16)
				if err != nil {
					return m
				}
				units = append(units, uint16(v))
			}
			return string(utf16.Decode(units))
		})
		s = percentRun.ReplaceAllStringFunc(s, func(m string) string {
			if plain, err := url.PathUnescape(m); err == nil {
				return plain
			}
			return m
		})
		s = html.UnescapeString(s)
		if s == before {
			break
		}
	}
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = norm.NFKD.String(s)
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Mn, r):
			return -1
		case r > 0x7f && unicode.Is(unicode.Nd, r):
			return '0' + digitValue(r)
		}
		return r
	}, s)
}

var (
	jsonEscape = regexp.MustCompile(`(?:\\u[dD][89abAB][0-9A-Fa-f]{2}\\u[dD][c-fC-F][0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4})`)
	percentRun = regexp.MustCompile(`(?:%[0-9A-Fa-f]{2})+`)
)

// digitValue is the value of a decimal digit (Nd). Unicode encodes every
// script's decimal digits as contiguous runs of ten, zero first, so the
// value is the distance from the start of the run, counted in tens.
func digitValue(r rune) rune {
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return (r - start) % 10
}

// The TR39 confusables data comes from Unicode's official confusables.txt,
// vendored unmodified in third_party/unicode/ and compacted into
// confusables_table.go by scripts/genconfusables (go generate). Only the
// mappings are compiled in; a test regenerates the table from the vendored
// file and checks both the file's digest and the table.
//
//go:generate go run ../../scripts/genconfusables

// ConfusablesTable compacts confusables.txt into its Unicode version and one
// line per mapping: the source and its prototype, as hex code points.
func ConfusablesTable(txt []byte) (version, table string, err error) {
	var b strings.Builder
	lines := bufio.NewScanner(bytes.NewReader(txt))
	for lines.Scan() {
		line := lines.Text()
		if v, ok := strings.CutPrefix(line, "# Version: "); ok {
			version = strings.TrimSpace(v)
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Split(line, ";")
		if len(fields) < 2 || strings.TrimSpace(fields[0]) == "" {
			continue
		}
		b.WriteString(strings.TrimSpace(fields[0]))
		for _, h := range strings.Fields(fields[1]) {
			b.WriteString(" " + h)
		}
		b.WriteString("\n")
	}
	if err = lines.Err(); err == nil && version == "" {
		err = errors.New("confusables.txt has no version line")
	}
	return version, b.String(), err
}

var (
	confusablesOnce     sync.Once
	confusablePrototype map[rune]string // TR39: a character to its prototype
	confusableSources   map[rune][]rune // a one-character prototype to the characters mapped to it
)

// loadConfusables reads the compiled table: each line maps one character to
// its prototype (one or more characters).
func loadConfusables() {
	confusablePrototype = map[rune]string{}
	confusableSources = map[rune][]rune{}
	for _, line := range strings.Split(confusablesTable, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		src, err := strconv.ParseUint(fields[0], 16, 32)
		if err != nil {
			continue
		}
		var proto []rune
		for _, h := range fields[1:] {
			if v, err := strconv.ParseUint(h, 16, 32); err == nil {
				proto = append(proto, rune(v))
			}
		}
		if len(proto) == 0 {
			continue
		}
		confusablePrototype[rune(src)] = string(proto)
		if len(proto) == 1 {
			confusableSources[proto[0]] = append(confusableSources[proto[0]], rune(src))
		}
	}
}

// prototypeLetter is how matching reads one prototype character. An ASCII
// prototype reads as itself, lower-cased. TR39 skeletons are case-sensitive,
// so a non-ASCII prototype takes one step through case, never more: to the
// ASCII letter that a case variant of it, or of a character mapped to it,
// has as prototype. ᴋ's prototype is ĸ; κ is also mapped to ĸ, and κ's upper
// case Κ is mapped to K, so ĸ reads k. The step is not repeated, so classes
// never chain (a transitive closure under case merges r, e, t, u and y).
func prototypeLetter(p rune) rune {
	if p < 0x80 {
		return unicode.ToLower(p)
	}
	best := rune(-1)
	consider := func(c rune) {
		proto, ok := confusablePrototype[c]
		if !ok {
			proto = string(c)
		}
		if q := []rune(proto); len(q) == 1 && q[0] < 0x80 && (unicode.IsLetter(q[0]) || unicode.IsDigit(q[0])) {
			if l := unicode.ToLower(q[0]); best < 0 || l < best {
				best = l
			}
		}
	}
	for _, x := range append([]rune{p}, confusableSources[p]...) {
		for _, c := range []rune{unicode.ToLower(x), unicode.ToUpper(x), unicode.ToTitle(x)} {
			if c != x {
				consider(c)
			}
		}
	}
	if best < 0 {
		return unicode.ToLower(p)
	}
	return best
}

// confusableSkeleton is r as matching reads it: folded (foldText), each
// character mapped to its TR39 prototype (confusables.txt) and folded again,
// each character of that read by prototypeLetter. Strings that look alike, in any case, read the same:
// Larkspur, Larkspսr, Larᴋspur and ᏞᎪᎡkspur all read larkspur.
func confusableSkeleton(r rune) string {
	if cached, ok := skeletonCache.Load(r); ok {
		return cached.(string)
	}
	out := skeletonOf(r)
	skeletonCache.Store(r, out)
	return out
}

var skeletonCache sync.Map

func skeletonOf(r rune) string {
	confusablesOnce.Do(loadConfusables)
	// As TR39: fold, map each character to its prototype, fold again.
	var b strings.Builder
	for _, q := range foldText(string(r)) {
		proto, ok := confusablePrototype[q]
		if !ok {
			proto = string(q)
		}
		for _, p := range foldText(proto) {
			b.WriteRune(prototypeLetter(p))
		}
	}
	return b.String()
}
