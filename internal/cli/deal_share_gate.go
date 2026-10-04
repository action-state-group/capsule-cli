package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
//
// allowed are values the copy may carry although the local store holds them:
// the merchant's order id, in the counterparty's copy, when
// shareableOrderID allows it. Each is taken out of the page, as written and
// JSON-escaped, before any check, so it neither trips the checks nor hides
// anything else.
func dealPageGate(page []byte, events []sealedEvent, allowed ...string) error {
	data := bytes.Replace(page, evidenceGraphIIFE, nil, 1)
	data = bytes.Replace(data, []byte(dealViewJS), nil, 1)
	for _, a := range allowed {
		if strings.TrimSpace(a) == "" {
			continue
		}
		forms := []string{a}
		if quoted, err := json.Marshal(a); err == nil {
			forms = append(forms, string(quoted[1:len(quoted)-1]))
		}
		for _, f := range forms {
			data = bytes.ReplaceAll(data, []byte(f), nil)
		}
	}
	secrets, words := gateSecrets(events)
	return gateCheck(data, secrets, words, 0)
}

// gateCheck runs every check on data, then again on the text inside any
// base64 or hex run of it that decodes to printable text (two levels deep):
// a value encoded after it was rewritten is still read. A digest, a
// signature or a signed statement decodes to binary and is not text.
//
// words are one-word names, read only as whole words: Grace refuses, the
// "grace" inside "disgrace" does not.
func gateCheck(data []byte, secrets, words []string, depth int) error {
	if depth < 2 {
		for _, run := range gateEncoded.FindAll([]byte(foldText(string(data))), -1) {
			if inner, ok := gateDecode(string(run)); ok {
				if err := gateCheck([]byte(inner), secrets, words, depth+1); err != nil {
					return err
				}
			}
		}
	}
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
	for _, secret := range secrets {
		for _, form := range gateForms(secret) {
			needle := []byte(strings.ToLower(form))
			if gateContains(text, needle) || isDigits(form) && gateContains(squashed, needle) {
				return inputError("refusing to write the shared copy: the page would carry a private value")
			}
		}
	}
	for _, w := range words {
		low := strings.ToLower(foldText(w))
		for _, form := range []string{low, gateBackwards(low)} {
			if regexp.MustCompile(`(?:^|[^\p{L}\p{N}])` + regexp.QuoteMeta(form) + `(?:$|[^\p{L}\p{N}])`).Match(text) {
				return inputError("refusing to write the shared copy: the page would carry a private value")
			}
		}
		b := []byte(w)
		for _, enc := range []string{hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
			base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)} {
			if len(enc) >= 8 && gateContains(text, []byte(strings.ToLower(enc))) {
				return inputError("refusing to write the shared copy: the page would carry a private value")
			}
		}
	}
	// The same values read by their letters and digits alone, forwards and
	// backwards, with digits written as words read as digits: L-a-r-k-s-p-u-r,
	// 7/3/9/1/4/2, "seven three nine one four two" and rupskraL all spell a
	// secret. A number is read by its plain digits. Anything with a letter is
	// read character by character, each character in any of its readings:
	// its confusable skeleton as written, lower-cased and upper-cased (TR39
	// is case-sensitive: I reads l as written, i lower-cased). Digests and
	// signatures are left out, and each field of the bundle is read on its
	// own (gateUnits), so no spelling runs from one field into the next.
	for _, unit := range gateUnits(data) {
		folded := foldText(unit)
		plain := gateLetters([]byte(strings.ToLower(folded)), false)
		chars, isWord := gateChars(folded)
		for _, w := range words {
			sk := gateLettersOf(w, true)
			if sk == "" {
				continue
			}
			for _, backwards := range []bool{false, true} {
				form := sk
				if backwards {
					form = gateBackwards(sk)
				}
				for j := range chars {
					if j > 0 && isWord[j-1] {
						continue
					}
					if end := gateSpellsFrom(chars, j, form, backwards); end > j && (end == len(chars) || !isWord[end]) {
						return inputError("refusing to write the shared copy: the page would spell out a private value")
					}
				}
			}
		}
		for _, secret := range secrets {
			if digits := gateLettersOf(secret, false); isDigits(digits) {
				if len(digits) < 4 {
					continue
				}
				for _, form := range []string{digits, gateBackwards(digits)} {
					if gateSpells(plain, form) {
						return inputError("refusing to write the shared copy: the page would spell out a private value")
					}
				}
				continue
			}
			sk := gateLettersOf(secret, true)
			if len(sk) < 5 {
				continue
			}
			for _, backwards := range []bool{false, true} {
				form := sk
				if backwards {
					form = gateBackwards(sk)
				}
				for j := range chars {
					if gateSpellsFrom(chars, j, form, backwards) >= 0 {
						return inputError("refusing to write the shared copy: the page would spell out a private value")
					}
				}
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
	// gateKept: dates ("09/27/2026", "October 3, 2026"), times and money
	// amounts ("$1,200.00").
	gateKept = regexp.MustCompile(`(?i)\b\d{4}[-/.]\d{1,2}[-/.]\d{1,2}\b|\b\d{1,2}[-/.]\d{1,2}[-/.](?:\d{4}|\d{2})\b` +
		`|\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+\d{4}\b` +
		`|\b\d{1,2}(?:st|nd|rd|th)?\s+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?,?\s+\d{4}\b` +
		`|\b\d{1,2}:\d{2}(?::\d{2})?\b|[$€£¥]\s?\d[\d,]*(?:\.\d{2})?|\b\d{1,3}(?:,\d{3})+(?:\.\d{2})?\b`)
	gateWord   = regexp.MustCompile(`[0-9A-Za-z]+`)
	gateDigest = regexp.MustCompile(`^(?:[0-9a-f]{64,}|[0-9A-Za-z+/=_-]{64,})$`)
)

var gateEncoded = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)

// gateDecode reads run as base64 (any alphabet) or hex, and returns the text
// inside when it is printable.
func gateDecode(run string) (string, bool) {
	var tries [][]byte
	if b, err := hex.DecodeString(run); err == nil {
		tries = append(tries, b)
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(run); err == nil {
			tries = append(tries, b)
		}
	}
	for _, b := range tries {
		if len(b) < 4 || !utf8.Valid(b) {
			continue
		}
		// Text, not binary: valid UTF-8 with no control characters other
		// than whitespace. (Not IsPrint: that rejects characters newer than
		// Go's Unicode tables.)
		printable := true
		for _, r := range string(b) {
			if r == utf8.RuneError || unicode.Is(unicode.Cc, r) && !unicode.IsSpace(r) {
				printable = false
				break
			}
		}
		if printable {
			return string(b), true
		}
	}
	return "", false
}

// gateStreetWords are address words too common to be needles on their own.
var gateStreetWords = map[string]bool{
	"street": true, "avenue": true, "road": true, "lane": true, "drive": true, "boulevard": true,
	"court": true, "place": true, "terrace": true, "circle": true, "highway": true, "parkway": true,
	"apartment": true, "unit": true, "suite": true, "floor": true,
}

// gateSecrets are the gate's own needles, read from the deal's local steps.
func gateSecrets(events []sealedEvent) (secrets, words []string) {
	var places, ids, texts, names []string
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
			if m := e.Evidence.Email; m != nil {
				// The merchant's email: its order id, the addresses in its
				// headers, its subject, its text and its items.
				ids = append(ids, m.Parsed.OrderID)
				msg, body := emailText(m.Raw)
				texts = append(texts, body, m.Parsed.Subject)
				if msg != nil {
					for _, h := range []string{"From", "To", "Cc", "Reply-To", "Sender", "Subject", "Delivered-To", "Return-Path"} {
						texts = append(texts, msg.Header.Get(h))
					}
					// The customer's own name, as the recipient headers
					// display it, is a needle on its own.
					for _, h := range []string{"To", "Cc", "Delivered-To"} {
						if list, err := mail.ParseAddressList(msg.Header.Get(h)); err == nil {
							for _, a := range list {
								names = append(names, a.Name)
							}
						}
					}
				}
				for _, it := range m.Parsed.Items {
					texts = append(texts, it.Text)
				}
			}
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
		kept := gateKept.FindAllStringIndex(t, -1)
		for _, loc := range gateNumber.FindAllStringIndex(t, -1) {
			m := t[loc[0]:loc[1]]
			inKept := false
			for _, k := range kept {
				inKept = inKept || loc[0] >= k[0] && loc[1] <= k[1]
			}
			// A date, a time or a money amount is not a secret; a shared
			// copy shows them.
			if !inKept && !gateDate.MatchString(m) && !gateAmount.MatchString(m) {
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
	// The customer's display names: a role ("Customer", "Sales Team") is not
	// a name; a name of several words is a needle like any other; a one-word
	// name of three letters or more is read only as a whole word, and a
	// shorter one never.
	for _, name := range names {
		name = strings.TrimSpace(foldText(name))
		parts := gateNameWord.FindAllString(strings.ToLower(name), -1)
		switch {
		case len(parts) == 0 || gateRole(parts):
		case len(parts) > 1:
			add(name)
		case utf8.RuneCountInString(parts[0]) >= 3:
			// A one-word name of three letters or more; a shorter one ("Al")
			// is never a needle.
			words = append(words, name)
		}
	}
	return out, words
}

var gateNameWord = regexp.MustCompile(`[\p{L}\p{N}]+`)

// gateRoleWords name a mailbox's role, not a person.
var gateRoleWords = map[string]bool{
	"customer": true, "customers": true, "client": true, "order": true, "orders": true, "sales": true,
	"support": true, "team": true, "info": true, "billing": true, "accounts": true, "account": true,
	"service": true, "services": true, "help": true, "helpdesk": true, "noreply": true, "no": true,
	"reply": true, "notifications": true, "notification": true, "admin": true, "contact": true,
	"hello": true, "mail": true, "newsletter": true, "shipping": true, "returns": true, "care": true,
	"payments": true, "receipts": true, "bookings": true, "reservations": true, "dear": true, "valued": true,
}

func gateRole(parts []string) bool {
	for _, p := range parts {
		if !gateRoleWords[p] {
			return false
		}
	}
	return true
}

// gateWordDigits are the number words the gate reads as digits.
var gateWordDigits = regexp.MustCompile(`\b(?:zero|oh|one|two|three|four|five|six|seven|eight|nine)\b`)

var gateDigitOf = map[string]string{"zero": "0", "oh": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9"}

// gateLetters is the lower-cased page as letters and digits alone, number
// words read as digits, and digests and signatures cut out as a break.
func gateLetters(text []byte, skeleton bool) string {
	s := gateWordDigits.ReplaceAllStringFunc(string(text), func(w string) string { return gateDigitOf[w] })
	var b strings.Builder
	for _, run := range gateTokens.FindAllStringIndex(s, -1) {
		if gateDigest.MatchString(s[run[0]:run[1]]) {
			s = s[:run[0]] + strings.Repeat("\x00", run[1]-run[0]) + s[run[1]:]
		}
	}
	for _, r := range s {
		switch {
		case r == 0:
			b.WriteByte(0) // a digest or signature: a break
		case unicode.IsLetter(r) || unicode.IsDigit(r) || skeleton:
			// In the skeleton every character counts: a symbol drawn like
			// a letter (| for l, ⋃ for U) spells that letter.
			gateWriteLetter(&b, r, skeleton)
		}
	}
	return b.String()
}

// gateUnits splits a page into the texts read on their own: every key and
// every string or number in the embedded bundle (window.__BUNDLE__, parsed as
// JSON), and the page around it. A page whose bundle does not parse is one
// text, read whole.
func gateUnits(data []byte) []string {
	const start = "window.__BUNDLE__ = "
	i := bytes.Index(data, []byte(start))
	if i < 0 {
		return []string{string(data)}
	}
	j := bytes.Index(data[i:], []byte(";</script>"))
	if j < 0 {
		return []string{string(data)}
	}
	decoder := json.NewDecoder(bytes.NewReader(data[i+len(start) : i+j]))
	decoder.UseNumber()
	var value interface{}
	if decoder.Decode(&value) != nil {
		return []string{string(data)}
	}
	units := []string{string(data[:i]) + " " + string(data[i+j:])}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, child := range x {
				units = append(units, k)
				walk(child)
			}
		case []interface{}:
			for _, child := range x {
				walk(child)
			}
		case string:
			units = append(units, x)
		case json.Number:
			units = append(units, x.String())
		}
	}
	walk(value)
	return units
}

// gateChar is one character of the page as the gate reads it: each of its
// readings, "" when it spells nothing, and none at all for a break (a
// digest or a signature) that no spelling runs across.
type gateChar []string

func gateChars(s string) ([]gateChar, []bool) {
	for _, run := range gateTokens.FindAllStringIndex(s, -1) {
		if gateDigest.MatchString(s[run[0]:run[1]]) {
			s = s[:run[0]] + strings.Repeat("\x00", run[1]-run[0]) + s[run[1]:]
		}
	}
	var out []gateChar
	var isWord []bool // the character is a letter or a digit, as written
	for _, r := range s {
		isWord = append(isWord, unicode.IsLetter(r) || unicode.IsDigit(r))
		if r == 0 {
			out = append(out, nil) // a digest or signature: a break
			continue
		}
		var c gateChar
		for _, v := range []rune{r, unicode.ToLower(r), unicode.ToUpper(r)} {
			var b strings.Builder
			gateWriteLetter(&b, v, true)
			if a := b.String(); !slices.Contains(c, a) {
				c = append(c, a)
			}
		}
		out = append(out, c)
	}
	return out, isWord
}

// gateSpellsFrom returns the index just past a spelling of form that starts
// at chars[j], taking any reading of each character, or -1; a character
// that spells nothing may stand inside the spelling, not at its start.
func gateSpellsFrom(chars []gateChar, j int, form string, backwards bool) int {
	seen := map[[2]int]bool{}
	var walk func(j, k int) int
	walk = func(j, k int) int {
		if k == len(form) {
			return j
		}
		if j == len(chars) || seen[[2]int{j, k}] {
			return -1
		}
		seen[[2]int{j, k}] = true
		for _, a := range chars[j] {
			if backwards {
				a = gateBackwards(a)
			}
			if a == "" && k > 0 {
				if end := walk(j+1, k); end >= 0 {
					return end
				}
			} else if a != "" && strings.HasPrefix(form[k:], a) {
				if end := walk(j+1, k+len(a)); end >= 0 {
					return end
				}
			}
		}
		return -1
	}
	return walk(j, 0)
}

// gateWriteLetter writes r, or its confusable skeleton's letters and digits.
func gateWriteLetter(b *strings.Builder, r rune, skeleton bool) {
	if !skeleton {
		b.WriteRune(r)
		return
	}
	for _, c := range confusableSkeleton(r) {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			b.WriteRune(c)
		}
	}
}

var gateTokens = regexp.MustCompile(`[0-9A-Za-z+/=_-]{64,}`)

// gateLettersOf is a value as its lower-cased letters and digits alone, or
// as their confusable skeleton.
func gateLettersOf(v string, skeleton bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(foldText(v)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || skeleton {
			gateWriteLetter(&b, r, skeleton)
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
		// A short number (under six digits) counts only where no digit
		// adjoins it (1200 is not read in the amount 120000); a longer one,
		// a code or a card number, counts anywhere.
		if !isDigits(form) || len(form) >= 6 || !digit(start-1) && !digit(end) {
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
// under six digits is not counted where a digit adjoins it (739142 is found
// in G739142 and in 7391420, but 1200 is not found in the amount 120000).
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
		if (len(needle) >= 8 || !gateInDigest(page, start, end)) && !(number && len(needle) < 6 && (digitAt(start-1) || digitAt(end))) {
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
