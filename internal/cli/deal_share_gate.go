package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// dealPageGate is the last check before a shared copy is written. It reads
// the FINAL page bytes, and it is independent of the scrubber: it reads the
// deal's local steps itself and derives its own needles with its own
// detectors, so a gap in the scrubber is not also a gap here. The only thing
// the two share is foldText, the written form both match in (format
// characters removed, NFKC, lookalike letters folded), applied to the page
// and the needles alike. Any hit refuses the copy: nothing is written and
// nothing is put on record.
//
//  1. Every private value of the deal (each place and its identifying
//     pieces, each counterparty identifier, each payment reference, and each
//     code, card, phone or email in any local text), in every form a page
//     could carry it: as is, any case, digits only, with separators removed,
//     JSON- and HTML-escaped, base64 (all four alphabets), hex, URL-escaped.
//  2. Generic patterns: a Luhn-valid card number, and a code word (code,
//     OTP, PIN, passcode) followed by a number.
//
// The two vendored scripts are taken out first: they are fixed code, pinned
// by digest, not data (if they are not found byte for byte, the whole page
// is checked). A needle shorter than eight characters is not counted inside a
// digest or a signature: a run of 64 or more hex or base64 characters, where
// a six-digit code occurs by chance. Anywhere else (G739142,
// ?order_ref=G739142, track/ABCDEFGHIJ739142) it counts.
func dealPageGate(page []byte, events []sealedEvent) error {
	data := bytes.Replace(page, evidenceGraphIIFE, nil, 1)
	data = bytes.Replace(data, []byte(dealViewJS), nil, 1)
	text := []byte(strings.ToLower(foldText(string(data))))
	// The page with separators between digits removed, for "739 142" or
	// "4111-1111-1111-1111" against the digits-only value.
	squashed := text
	for {
		next := gateDigitSeparators.ReplaceAll(squashed, []byte("$1$2"))
		if bytes.Equal(next, squashed) {
			break
		}
		squashed = next
	}
	secrets := gateSecrets(events)
	for _, secret := range secrets {
		for _, form := range gateForms(secret) {
			needle := []byte(strings.ToLower(form))
			if gateContains(text, needle) || isDigits(form) && gateContains(squashed, needle) {
				return inputError("refusing to write the shared copy: the page would carry a private value")
			}
		}
	}
	// The same values read by their letters and digits alone, forwards and
	// backwards, with digits written as words read as digits: L-a-r-k-s-p-u-r,
	// 7/3/9/1/4/2, "seven three nine one four two" and rupskraL all spell a
	// secret. Digests and signatures are left out, and JSON punctuation
	// separates, so no spelling runs across two fields.
	letters := gateLetters(text)
	for _, secret := range secrets {
		sk := gateLettersOf(secret)
		if len(sk) < 4 || !isDigits(sk) && len(sk) < 5 {
			continue
		}
		for _, form := range []string{sk, gateBackwards(sk)} {
			if gateSpells(letters, form) {
				return inputError("refusing to write the shared copy: the page would spell out a private value")
			}
		}
	}
	for _, loc := range gatePAN.FindAllIndex(text, -1) {
		if !gateInDigest(text, loc[0], loc[1]) && luhn(nonDigit.ReplaceAllString(string(text[loc[0]:loc[1]]), "")) {
			return inputError("refusing to write the shared copy: the page would carry a card number")
		}
	}
	if gateCode.Match(text) {
		return inputError("refusing to write the shared copy: the page would carry a code")
	}
	return nil
}

var (
	gateDigitSeparators = regexp.MustCompile(`(\d)[ .-](\d)`)
	gatePAN             = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)
	gateCode            = regexp.MustCompile(`(?i)\b(?:code|otp|pin|passcode)\b\W{0,3}(?:(?:is|was)\W{1,3})?[A-Za-z-]{0,4}\d(?:[ .-]?\d){3,7}`)
	gateRun             = regexp.MustCompile(`[0-9A-Za-z]{4,}`)
	gateNumber          = regexp.MustCompile(`\d(?:[\s().-]*\d){3,}`)
	gateEmail           = regexp.MustCompile(`[^\s@"<>]+@[^\s@"<>]+\.[A-Za-z]{2,}`)
	gateDate            = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	gateAmount          = regexp.MustCompile(`^\d+\.\d{2}$`)
	gateWord            = regexp.MustCompile(`[0-9A-Za-z]+`)
	gateDigest          = regexp.MustCompile(`^(?:[0-9a-f]{64,}|[0-9A-Za-z+/=_-]{64,})$`)
)

// gateStreetWords are address words too common to be needles on their own.
var gateStreetWords = map[string]bool{
	"street": true, "avenue": true, "road": true, "lane": true, "drive": true, "boulevard": true,
	"court": true, "place": true, "terrace": true, "circle": true, "highway": true, "parkway": true,
	"apartment": true, "unit": true, "suite": true, "floor": true,
}

// gateSecrets are the gate's own needles, read from the deal's local steps.
func gateSecrets(events []sealedEvent) []string {
	var places, ids, texts []string
	whoValues := func(w *dealWho) {
		if w != nil {
			ids = append(ids, w.Name, w.Domain, w.Phone, w.Email, w.Payee, w.RelayAddress, w.ProfileID)
		}
	}
	termValues := func(t *dealTerms) {
		if t != nil {
			places = append(places, t.Place)
			texts = append(texts, t.Item)
			for _, v := range t.Conditions {
				texts = append(texts, v)
			}
		}
	}
	for _, se := range events {
		e := se.Event
		if e.Open != nil {
			whoValues(&e.Open.Who)
			termValues(&e.Open.Terms)
			termValues(&e.Open.Intent.Asked)
			texts = append(texts, e.Open.Intent.Verbatim)
			for _, c := range e.Open.Claims {
				texts = append(texts, c.Text)
			}
		}
		if e.Intent != nil {
			termValues(&e.Intent.Asked)
			texts = append(texts, e.Intent.Verbatim)
		}
		if e.Message != nil {
			whoValues(e.Message.Who)
			texts = append(texts, e.Message.Text)
		}
		if e.Claim != nil {
			texts = append(texts, e.Claim.Text)
		}
		if e.Evidence != nil {
			whoValues(e.Evidence.Who)
			texts = append(texts, e.Evidence.About, e.Evidence.Detail)
		}
		if e.Change != nil {
			whoValues(e.Change.Who)
			termValues(e.Change.Terms)
		}
		if e.Snapshot != nil {
			whoValues(e.Snapshot.Who)
			termValues(e.Snapshot.Terms)
			texts = append(texts, e.Snapshot.Description)
		}
		if e.Approval != nil {
			texts = append(texts, e.Approval.Said)
		}
		if e.Act != nil {
			ids = append(ids, e.Act.Payee, e.Act.Reference)
			texts = append(texts, e.Act.Description)
		}
		if e.Outcome != nil {
			termValues(e.Outcome.Delivered)
			texts = append(texts, e.Outcome.Note)
		}
		if e.Close != nil {
			termValues(e.Close.Delivered)
		}
	}
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= 4 {
			out = append(out, v)
		}
	}
	for _, v := range ids {
		add(foldText(v))
	}
	for _, v := range places {
		v = foldText(v)
		add(v)
		// Each name in a place (four letters or more, not a street word) is
		// a needle on its own: "Larkspur", "Springfield".
		for _, w := range gateWord.FindAllString(v, -1) {
			if !isDigits(w) && !gateStreetWords[strings.ToLower(w)] {
				add(w)
			}
		}
	}
	for _, t := range texts {
		t = foldText(t)
		// Every number of four digits or more, as written and digits only,
		// and every letter-and-digit run holding four digits.
		for _, m := range gateNumber.FindAllString(t, -1) {
			if !gateDate.MatchString(m) && !gateAmount.MatchString(m) {
				add(m)
				add(nonDigit.ReplaceAllString(m, ""))
			}
		}
		for _, m := range gateRun.FindAllString(t, -1) {
			if len(nonDigit.ReplaceAllString(m, "")) >= 4 && !isDigits(m) {
				add(m)
			}
		}
		for _, m := range gateEmail.FindAllString(t, -1) {
			add(m)
		}
	}
	return out
}

// gateWordDigits are the number words the gate reads as digits.
var gateWordDigits = regexp.MustCompile(`\b(?:zero|oh|one|two|three|four|five|six|seven|eight|nine)\b`)

var gateDigitOf = map[string]string{"zero": "0", "oh": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9"}

// gateLetters is the lower-cased page as letters and digits alone, number
// words read as digits, digests and signatures cut out, and JSON
// punctuation (quotes, braces, brackets, colons, commas) kept as a break.
func gateLetters(text []byte) string {
	s := gateWordDigits.ReplaceAllStringFunc(string(text), func(w string) string { return gateDigitOf[w] })
	var b strings.Builder
	for _, run := range gateTokens.FindAllStringIndex(s, -1) {
		if gateDigest.MatchString(s[run[0]:run[1]]) {
			s = s[:run[0]] + strings.Repeat("|", run[1]-run[0]) + s[run[1]:]
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case strings.ContainsRune(`"{}[]:,|`, r):
			b.WriteByte('|')
		}
	}
	return b.String()
}

var gateTokens = regexp.MustCompile(`[0-9A-Za-z+/=_-]{64,}`)

// gateLettersOf is a value as its lower-cased letters and digits alone.
func gateLettersOf(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(foldText(v)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func gateBackwards(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// gateSpells reports whether letters holds form; a number only where no
// digit adjoins it.
func gateSpells(letters, form string) bool {
	digit := func(i int) bool { return i >= 0 && i < len(letters) && letters[i] >= '0' && letters[i] <= '9' }
	for from := 0; ; {
		i := strings.Index(letters[from:], form)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(form)
		if !isDigits(form) || !digit(start-1) && !digit(end) {
			return true
		}
		from = start + 1
	}
}

// gateForms is a value in every form a page could carry it.
func gateForms(v string) []string {
	b := []byte(v)
	out := []string{v, url.QueryEscape(v), url.PathEscape(v), hex.EncodeToString(b),
		base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)}
	if quoted, err := json.Marshal(v); err == nil { // escapes <, > and & as the emitter does
		out = append(out, string(quoted[1:len(quoted)-1]))
	}
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		out = append(out, strings.TrimSuffix(strings.TrimSpace(plain.String()), `"`)[1:])
	}
	if digits := nonDigit.ReplaceAllString(v, ""); len(digits) >= 4 && digits != v {
		out = append(out, digits)
	}
	return out
}

// gateContains finds needle in page. A needle shorter than eight characters
// is not counted inside a digest or signature (gateInDigest), and a number
// is not counted where a digit adjoins it (739142 is found in G739142, but
// 1200 is not found in the amount 120000).
func gateContains(page, needle []byte) bool {
	if len(needle) == 0 {
		return false
	}
	number := isDigits(string(needle))
	digitAt := func(i int) bool { return i >= 0 && i < len(page) && page[i] >= '0' && page[i] <= '9' }
	for from := 0; ; {
		i := bytes.Index(page[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		if (len(needle) >= 8 || !gateInDigest(page, start, end)) && !(number && (digitAt(start-1) || digitAt(end))) {
			return true
		}
		from = start + 1
	}
}

// gateInDigest reports whether page[start:end] lies inside a digest or a
// signature: a run of 64 or more characters, all hex or all base64. A shorter
// run (an order reference, a URL path) is text, and a needle in it counts.
func gateInDigest(page []byte, start, end int) bool {
	token := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '+' || c == '/' || c == '_' || c == '-' || c == '='
	}
	for i := start; i < end; i++ {
		if !token(page[i]) {
			return false
		}
	}
	s, e := start, end
	for s > 0 && token(page[s-1]) {
		s--
	}
	for e < len(page) && token(page[e]) {
		e++
	}
	return gateDigest.Match(page[s:e])
}

// luhn reports a Luhn-valid number, the checksum every card number carries.
func luhn(digits string) bool {
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if (len(digits)-1-i)%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return len(digits) >= 13 && sum%10 == 0
}
