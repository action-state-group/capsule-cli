package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// A deal receipt is the user's file. `deal report --html` writes the user's
// own copy (audience keep: nothing withheld). `--share counterparty|adjudicator`
// writes a copy for someone else, and sharing is a disclose act: the copy
// withholds what that audience must not get, and a disclosure record naming
// what was shared, with whom, in what mode and what was withheld is sealed
// onto the deal's disclosure log BEFORE the file is written. Nothing is
// hosted; the copy is a local file the user hands over.
const (
	dealAudienceKeep         = "keep"
	dealAudienceCounterparty = "counterparty"
	dealAudienceAdjudicator  = "adjudicator"
)

// dealWithheldFields is, per shared audience, what the copy leaves out, in
// the words the page shows and the disclosure record seals. Neither shared
// copy can carry a home address, a verification code or a card number.
var dealWithheldFields = map[string][]string{
	dealAudienceCounterparty: {
		"home address", "names, payees and contact details", "card and payment identifiers", "verification codes",
		"message text", "claim text and sources", "your own words", "item, place and condition details",
		"the merchant email's text, items and signing domain", "booking, confirmation and reservation codes, and any order number its signed email does not confirm",
		"your spending limit",
	},
	dealAudienceAdjudicator: {
		"home address", "names, payees and contact details", "card and payment identifiers", "verification codes",
		"your own words", "item, place and condition details",
		"the merchant email's text and signing domain", "order, booking and confirmation numbers",
	},
}

// dealShareKeys are the record keys whose string values a shared copy may
// disclose: tokens, timestamps, rails, currencies, digests and commitments.
// A sealed record is disclosed whole or not at all (its digest binds every
// byte), so a record with any other string value is withheld: it still
// verifies as WITHHELD, its place in the log still proven.
var dealShareKeys = map[string]bool{
	"profile": true, "canonicalization": true, "deal_id": true, "record_type": true, "at": true,
	"type": true, "digest_alg": true, "digest": true, "rel": true, "fp_alg": true, "channel": true,
	"deal_type": true, "action": true, "currency": true, "rail": true, "result": true, "pack_id": true,
	"question": true, "rule": true, "field": true, "options": true, "notes": true, "changed": true,
	"choice": true, "approver": true, "status": true, "outcome": true, "from": true, "kind": true,
	"response_digest": true,
	// A sealed merchant email's record: digests, the DKIM and DMARC verdicts,
	// where the keys came from, and dates.
	"key_source": true, "dkim": true, "dmarc_policy": true, "dmarc_source": true, "method": true,
	"cancel_by": true, "sent_at": true,
	// When the deal is expected to close: a date.
	"expect_close_by": true,
}

// The adjudicator's copy adds claim text and sources.
var dealAdjudicatorKeys = map[string]bool{"text": true, "source": true, "unverified": true}

var (
	shareAddress = regexp.MustCompile(`(?i)\b\d{1,6}[a-z]?\s+(?:[a-z0-9.'-]+\s+){0,4}(?:street|st|avenue|ave|road|rd|lane|ln|drive|dr|boulevard|blvd|court|ct|way|place|pl|terrace|circle|highway|hwy|parkway|pkwy)\b\.?(?:,?\s*(?:apt|apartment|unit|suite|ste|#)\.?\s*[a-z0-9-]+)?`)
	// shareNumberish is a run of letters and digits holding a digit, such
	// runs joined by one space, dot or dash, with a lettered prefix: "G739142",
	// "code739142", "G-7391", "739 142", "4111-1111-1111-1111".
	shareNumberish = regexp.MustCompile(`(?:[A-Za-z]+-)?[A-Za-z0-9]*[0-9][A-Za-z0-9]*(?:[ .-][A-Za-z0-9]*[0-9][A-Za-z0-9]*)*`)
	shareDateTime  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2})?Z?)?$`)
	shareMoney     = regexp.MustCompile(`^\d+\.\d{2}$`)
	shareWord      = regexp.MustCompile(`[A-Za-z0-9]+`)
)

// shareAddressStop are address words too common to withhold on their own.
var shareAddressStop = map[string]bool{
	"street": true, "st": true, "avenue": true, "ave": true, "road": true, "rd": true, "lane": true, "ln": true,
	"drive": true, "dr": true, "boulevard": true, "blvd": true, "court": true, "ct": true, "way": true,
	"place": true, "pl": true, "terrace": true, "circle": true, "highway": true, "hwy": true, "parkway": true,
	"pkwy": true, "apt": true, "apartment": true, "unit": true, "suite": true, "ste": true, "floor": true,
	"the": true, "and": true, "of": true, "no": true,
}

// addressFragments are the parts of an address that identify it on their
// own: its names (three letters or more, not a street word) and its numbers
// (two digits or more). "41 Larkspur Lane, Apt 3, Springfield" gives 41,
// Larkspur and Springfield, so "Larkspur Lane 41" is caught too.
func addressFragments(v string) []string {
	var out []string
	for _, w := range shareWord.FindAllString(v, -1) {
		if isDigits(w) && len(w) >= 2 || !isDigits(w) && len(w) >= 3 && !shareAddressStop[strings.ToLower(w)] {
			out = append(out, w)
		}
	}
	return out
}

// dealLocal is everything private the deal's local store holds: the
// identifiers and references, the places, and every free text.
type dealLocal struct{ ids, places, texts, names []string }

func dealLocalData(events []sealedEvent) dealLocal {
	var l dealLocal
	who := func(w *dealWho) {
		if w == nil {
			return
		}
		for _, f := range whoFields(*w) {
			l.ids = append(l.ids, f.value)
		}
	}
	terms := func(t *dealTerms) {
		if t == nil {
			return
		}
		l.places = append(l.places, t.Place)
		l.texts = append(l.texts, t.Item)
		for _, v := range t.Conditions {
			l.texts = append(l.texts, v)
		}
	}
	text := func(s ...string) { l.texts = append(l.texts, s...) }
	for _, se := range events {
		e := se.Event
		switch {
		case e.Open != nil:
			who(&e.Open.Who)
			terms(&e.Open.Terms)
			terms(&e.Open.Intent.Asked)
			text(e.Open.Intent.Verbatim)
			for _, c := range e.Open.Claims {
				text(c.Text)
			}
		case e.Intent != nil:
			terms(&e.Intent.Asked)
			text(e.Intent.Verbatim)
		case e.Message != nil:
			who(e.Message.Who)
			text(e.Message.Text)
		case e.Claim != nil:
			text(e.Claim.Text)
		case e.Evidence != nil:
			who(e.Evidence.Who)
			text(e.Evidence.About, e.Evidence.Detail)
			if m := e.Evidence.Email; m != nil {
				// A merchant's email, sealed raw: its order id, its headers'
				// addresses and its text are all private.
				l.ids = append(l.ids, m.Parsed.OrderID, m.Parsed.Tracking)
				l.names = append(l.names, recipientNames(m)...)
				text(merchantEmailTexts(m)...)
			}
		case e.Change != nil:
			who(e.Change.Who)
			terms(e.Change.Terms)
		case e.Snapshot != nil:
			who(e.Snapshot.Who)
			terms(e.Snapshot.Terms)
			text(e.Snapshot.Description)
		case e.Approval != nil:
			text(e.Approval.Said)
		case e.Act != nil:
			l.ids = append(l.ids, e.Act.Payee, e.Act.Reference)
			text(e.Act.Description)
		case e.Outcome != nil:
			terms(e.Outcome.Delivered)
			text(e.Outcome.Note)
		case e.Close != nil:
			terms(e.Close.Delivered)
		case e.Disclosure != nil:
			// What the agent told someone: every value is private, and so is
			// the person it was told to.
			who(e.Disclosure.Who)
			for _, f := range e.Disclosure.Fields {
				l.ids = append(l.ids, f.Value)
				if f.Class == "home_address" || f.Class == "address" || f.Class == "pickup_location" {
					l.places = append(l.places, f.Value)
				}
			}
		}
	}
	return l
}

// merchantEmailTexts are the texts of a sealed merchant email: its decoded
// text, the headers that carry names and addresses, its subject and the items
// read from it.
func merchantEmailTexts(m *merchantEmail) []string {
	msg, body := emailText(m.Raw)
	out := []string{body, m.Parsed.Subject}
	if msg != nil {
		for _, h := range []string{"From", "To", "Cc", "Reply-To", "Sender", "Subject", "Delivered-To", "Return-Path"} {
			out = append(out, msg.Header.Get(h))
		}
	}
	for _, it := range m.Parsed.Items {
		out = append(out, it.Text)
	}
	return out
}

// recipientNames are the display names on a merchant email's To, Cc and
// Delivered-To headers: the customer's own name ("Sam Customer"), which is
// as private as their address.
func recipientNames(m *merchantEmail) []string {
	msg, _ := emailText(m.Raw)
	if msg == nil {
		return nil
	}
	var names []string
	for _, h := range []string{"To", "Cc", "Delivered-To"} {
		list, err := mail.ParseAddressList(msg.Header.Get(h))
		if err != nil {
			continue
		}
		for _, a := range list {
			if strings.TrimSpace(a.Name) != "" {
				names = append(names, a.Name)
			}
		}
	}
	return names
}

// dealPrivate is what a shared copy must never carry, read from the local
// store: every counterparty identifier, every place, every payment
// reference, every card number, code, phone, email or street address found
// in any text the deal holds, and the identifying fragments of every place
// and address (withheld as whole words wherever they appear).
type dealPrivate struct {
	values    []string
	fragments *regexp.Regexp
	// spelled are the private values and place names as their letters and
	// digits alone, forwards and backwards: "larkspur", "rupskral",
	// "739142". A text that spells one, whatever stands between the
	// characters (L-a-r-k-s-p-u-r, 7/3/9/1/4/2), has that stretch withheld.
	spelled []spelledForm
}

func dealPrivateValues(events []sealedEvent) dealPrivate {
	l := dealLocalData(events)
	for _, list := range [][]string{l.ids, l.places, l.texts} {
		for i := range list {
			list[i] = foldText(list[i])
		}
	}
	seen := map[string]bool{}
	var p dealPrivate
	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) < 4 || seen[foldCase.String(v)] {
			return
		}
		seen[foldCase.String(v)] = true
		p.values = append(p.values, v)
	}
	fragments := map[string]bool{}
	addresses := append([]string{}, l.places...)
	for _, v := range l.ids {
		add(v)
	}
	for _, v := range l.places {
		add(v)
	}
	for _, s := range l.texts {
		for _, m := range scanEmail.FindAllString(s, -1) {
			add(m)
		}
		for _, m := range shareAddress.FindAllString(s, -1) {
			add(m)
			addresses = append(addresses, m)
		}
		for _, loc := range append(phoneLocs(s), codeLocs(s)...) {
			m := s[loc[0]:loc[1]]
			add(m)
			add(nonDigit.ReplaceAllString(m, ""))
		}
	}
	for _, a := range addresses {
		for _, f := range addressFragments(a) {
			fragments[strings.ToLower(f)] = true
		}
	}
	if len(fragments) > 0 {
		words := make([]string, 0, len(fragments))
		for f := range fragments {
			words = append(words, regexp.QuoteMeta(f))
		}
		sort.Slice(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
		p.fragments = regexp.MustCompile(`(?i)\b(?:` + strings.Join(words, "|") + `)\b`)
	}
	// Longest first, so a value is withheld whole before any part of it.
	sort.SliceStable(p.values, func(i, j int) bool { return len(p.values[i]) > len(p.values[j]) })
	spelledSeen := map[string]bool{}
	spell := func(v string) {
		sp := spelling(v)
		form := spelledForm{text: sp.lowerSkeleton}
		if isDigits(sp.plain) {
			form = spelledForm{text: sp.plain, numeric: true}
		}
		if len(form.text) < 4 || !form.numeric && len(form.text) < 5 {
			return
		}
		for i, text := range []string{form.text, reverseString(form.text)} {
			if !spelledSeen[text] {
				spelledSeen[text] = true
				p.spelled = append(p.spelled, spelledForm{text: text, numeric: form.numeric, backwards: i == 1})
			}
		}
	}
	for _, v := range p.values {
		spell(v)
	}
	for f := range fragments {
		if !isDigits(f) {
			spell(f)
		}
	}
	// The customer's display names: a role ("Customer", "Sales Team") is
	// not a name and is skipped; a name of several words is a value like any
	// other; a one-word name of three letters or more ("Amy") is matched only
	// as a whole word, in any of its spellings, so it never takes a bite out
	// of a longer word; a shorter one is never matched.
	for _, name := range l.names {
		name = strings.TrimSpace(foldText(name))
		words := nameWord.FindAllString(strings.ToLower(name), -1)
		switch {
		case len(words) == 0 || roleName(words):
		case len(words) > 1:
			add(name)
			spell(name)
		case utf8.RuneCountInString(words[0]) < 3:
			// A one- or two-letter name ("Al", "Jo") is never matched: it
			// would take out words and abbreviations that are not names.
		default:
			sk := spelling(name).lowerSkeleton
			if sk == "" {
				continue
			}
			for i, text := range []string{sk, reverseString(sk)} {
				p.spelled = append(p.spelled, spelledForm{text: text, bounded: true, backwards: i == 1})
			}
		}
	}
	sort.SliceStable(p.values, func(i, j int) bool { return len(p.values[i]) > len(p.values[j]) })
	return p
}

var nameWord = regexp.MustCompile(`[\p{L}\p{N}]+`)

// roleWords are the words a mailbox's display name uses for a role, not a
// person: "Customer", "Orders", "Sales Team", "Billing".
var roleWords = map[string]bool{
	"customer": true, "customers": true, "client": true, "order": true, "orders": true, "sales": true,
	"support": true, "team": true, "info": true, "billing": true, "accounts": true, "account": true,
	"service": true, "services": true, "help": true, "helpdesk": true, "noreply": true, "no": true,
	"reply": true, "notifications": true, "notification": true, "admin": true, "contact": true,
	"hello": true, "mail": true, "newsletter": true, "shipping": true, "returns": true, "care": true,
	"payments": true, "receipts": true, "bookings": true, "reservations": true, "dear": true, "valued": true,
}

// roleName reports a display name made of role words only.
func roleName(words []string) bool {
	for _, w := range words {
		if !roleWords[w] {
			return false
		}
	}
	return true
}

// spelt is a text as its letters and digits alone, read two ways: plain
// (lower case; digits are ASCII after foldText) and as its confusable
// skeleton (confusableSkeleton), each with the byte span in the text that
// every character came from.
type spelt struct {
	plain, skeleton, lowerSkeleton       string
	plainAt, skeletonAt, lowerSkeletonAt [][2]int
}

func spelling(s string) spelt {
	var sp spelt
	var plain, skeleton, lowerSkeleton strings.Builder
	for i, r := range s {
		span := [2]int{i, i + utf8.RuneLen(r)}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			lower := string(unicode.ToLower(r))
			plain.WriteString(lower)
			for range []byte(lower) {
				sp.plainAt = append(sp.plainAt, span)
			}
		}
		// Every character counts in the skeleton, not only letters: a
		// symbol drawn like a letter (| for l) spells that letter.
		// TR39 skeletons are case-sensitive (I reads l), so the text is read
		// both as written and lower-cased (SPRINGFIELD reads springfield).
		for _, c := range confusableSkeleton(r) {
			if unicode.IsLetter(c) || unicode.IsDigit(c) {
				skeleton.WriteRune(c)
				for range []byte(string(c)) {
					sp.skeletonAt = append(sp.skeletonAt, span)
				}
			}
		}
		for _, c := range confusableSkeleton(unicode.ToLower(r)) {
			if unicode.IsLetter(c) || unicode.IsDigit(c) {
				lowerSkeleton.WriteRune(c)
				for range []byte(string(c)) {
					sp.lowerSkeletonAt = append(sp.lowerSkeletonAt, span)
				}
			}
		}
	}
	sp.plain, sp.skeleton, sp.lowerSkeleton = plain.String(), skeleton.String(), lowerSkeleton.String()
	return sp
}

// spelledForm is a private value as matching looks for it: a number by its
// plain digits, anything with a letter by its skeleton.
type spelledForm struct {
	text    string
	numeric bool
	// bounded marks a one-word name, matched only as a whole word: Grace is
	// withheld, the "grace" inside "disgrace" is not.
	bounded bool
	// backwards marks a letter form read right to left, so each
	// character's reading is reversed too (m reads nr).
	backwards bool
}

// spelledRune is one character of a text as the letter matcher reads it:
// each way it can read (its skeleton as written, lower-cased and
// upper-cased: I reads l, and i), "" for a character that spells nothing (a
// separator), and the span of the text it came from.
type spelledRune struct {
	alts []string
	span [2]int
	word bool // a letter or a digit, as written
}

func spelledRunes(s string) []spelledRune {
	var out []spelledRune
	for i, r := range s {
		var alts []string
		for _, c := range []rune{r, unicode.ToLower(r), unicode.ToUpper(r)} {
			var b strings.Builder
			for _, x := range confusableSkeleton(c) {
				if unicode.IsLetter(x) || unicode.IsDigit(x) {
					b.WriteRune(x)
				}
			}
			if a := b.String(); !slices.Contains(alts, a) {
				alts = append(alts, a)
			}
		}
		out = append(out, spelledRune{alts: alts, span: [2]int{i, i + utf8.RuneLen(r)}, word: unicode.IsLetter(r) || unicode.IsDigit(r)})
	}
	return out
}

// spellsFrom returns the index just past a spelling of target that starts
// at rs[j], taking any one reading of each character, or -1. A character
// that spells nothing may stand inside the spelling, never at its start.
func spellsFrom(rs []spelledRune, j int, target string, backwards bool) int {
	memo := map[[2]int]int{}
	var walk func(j, k int) int
	walk = func(j, k int) int {
		if k == len(target) {
			return j
		}
		if j == len(rs) {
			return -1
		}
		key := [2]int{j, k}
		if v, ok := memo[key]; ok {
			return v
		}
		end := -1
		for _, a := range rs[j].alts {
			if backwards {
				a = reverseString(a)
			}
			switch {
			case a == "" && k > 0:
				end = walk(j+1, k)
			case a != "" && strings.HasPrefix(target[k:], a):
				end = walk(j+1, k+len(a))
			}
			if end >= 0 {
				break
			}
		}
		memo[key] = end
		return end
	}
	return walk(j, 0)
}

func reverseString(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// withholdSpelled withholds every stretch of s that spells a private value.
func (p dealPrivate) withholdSpelled(s string) string {
	sp := spelling(s)
	rs := spelledRunes(s)
	var locs [][]int
	for _, v := range p.spelled {
		if !v.numeric {
			for j := range rs {
				if v.bounded && j > 0 && rs[j-1].word {
					continue
				}
				if end := spellsFrom(rs, j, v.text, v.backwards); end > j && !(v.bounded && end < len(rs) && rs[end].word) {
					locs = append(locs, []int{rs[j].span[0], rs[end-1].span[1]})
				}
			}
			continue
		}
		in, at := sp.plain, sp.plainAt
		for from := 0; ; {
			i := strings.Index(in[from:], v.text)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(v.text)
			// A short number (under six digits) counts only where no digit
			// adjoins it; a code or a card number counts anywhere.
			if len(v.text) >= 6 || !(start > 0 && isDigits(in[start-1:start]) || end < len(in) && isDigits(in[end:end+1])) {
				locs = append(locs, []int{at[start][0], at[end-1][1]})
			}
			from = start + 1
		}
	}
	return withholdAt(s, mergeLocs(locs))
}

// mergeLocs sorts spans and joins the ones that overlap.
func mergeLocs(locs [][]int) [][]int {
	sort.Slice(locs, func(i, j int) bool { return locs[i][0] < locs[j][0] })
	var out [][]int
	for _, l := range locs {
		if n := len(out); n > 0 && l[0] <= out[n-1][1] {
			if l[1] > out[n-1][1] {
				out[n-1][1] = l[1]
			}
			continue
		}
		out = append(out, l)
	}
	return out
}

// numberWords is four or more digits written as words: "seven three nine one".
var numberWords = regexp.MustCompile(`(?i)\b(?:zero|oh|one|two|three|four|five|six|seven|eight|nine)(?:[^A-Za-z0-9]+(?:zero|oh|one|two|three|four|five|six|seven|eight|nine)){3,}\b`)

var shareEncoded = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)

// decodedText reads run as base64 (any alphabet) or hex, and returns the text
// inside when it is valid UTF-8 and printable.
func decodedText(run string) (string, bool) {
	decoders := []func(string) ([]byte, error){hex.DecodeString, base64.StdEncoding.DecodeString,
		base64.URLEncoding.DecodeString, base64.RawStdEncoding.DecodeString, base64.RawURLEncoding.DecodeString}
	for _, decode := range decoders {
		b, err := decode(run)
		if err != nil || len(b) < 4 || !utf8.Valid(b) {
			continue
		}
		// Text, not binary: no control characters other than whitespace.
		ok := true
		for _, r := range string(b) {
			ok = ok && r != utf8.RuneError && (!unicode.Is(unicode.Cc, r) || unicode.IsSpace(r))
		}
		if ok {
			return string(b), true
		}
	}
	return "", false
}

// codeLocs finds every code, card number, PIN, phone, house or order number
// in s: a numberish run holding four or more digits, wherever it sits (inside
// a word like G739142 or code739142, or split like 739 142). Only a date
// and a money amount (1200.00) are left.
func codeLocs(s string) [][]int {
	keep := shareKept.FindAllStringIndex(s, -1)
	var out [][]int
	for _, loc := range shareNumberish.FindAllStringIndex(s, -1) {
		m := s[loc[0]:loc[1]]
		if len(nonDigit.ReplaceAllString(m, "")) < 4 || shareDateTime.MatchString(m) || shareMoney.MatchString(m) || withinAny(loc, keep) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// shareKept are dates, times and money amounts: numbers a shared copy keeps
// ("09/27/2026", "October 3, 2026", "18:00", "$1,200.00"). Only the generic
// code detector spares them; a known private value is withheld wherever it
// stands, a date-shaped one included.
var shareKept = regexp.MustCompile(`(?i)\b\d{4}[-/.]\d{1,2}[-/.]\d{1,2}(?:[T ]\d{1,2}:\d{2}(?::\d{2})?Z?)?\b` +
	`|\b\d{1,2}[-/.]\d{1,2}[-/.](?:\d{4}|\d{2})\b` +
	`|\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+\d{4}\b` +
	`|\b\d{1,2}(?:st|nd|rd|th)?\s+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?,?\s+\d{4}\b` +
	`|\b\d{1,2}:\d{2}(?::\d{2})?\b` +
	`|[$€£¥]\s?\d[\d,]*(?:\.\d{2})?|\b\d{1,3}(?:,\d{3})+(?:\.\d{2})?\b`)

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// scrub withholds every private value, every fragment of a place or
// address, and every card number, email, phone, street address and code in
// s. The text it returns is in folded form.
func (p dealPrivate) scrub(s string) string {
	return p.scrubDepth(s, 0)
}

func (p dealPrivate) scrubDepth(s string, depth int) string {
	// Matched in its plain form: no zero-width characters, no fullwidth
	// digits, no lookalike letters (foldText).
	s = foldText(s)
	// A base64 or hex run whose text holds a private value is withheld
	// whole.
	if depth < 2 {
		s = shareEncoded.ReplaceAllStringFunc(s, func(run string) string {
			if inner, ok := decodedText(run); ok && p.scrubDepth(inner, depth+1) != foldText(inner) {
				return "[withheld]"
			}
			return run
		})
	}
	// Codes first, so a code is withheld whole ("G739142", not "G[withheld]").
	s = withholdAt(s, codeLocs(s))
	for _, v := range p.values {
		s = replaceFold(s, v, "[withheld]")
	}
	s = scanEmail.ReplaceAllString(s, "[withheld]")
	s = withholdAt(s, phoneLocs(s))
	s = shareAddress.ReplaceAllString(s, "[withheld]")
	if p.fragments != nil {
		s = p.fragments.ReplaceAllString(s, "[withheld]")
	}
	s = numberWords.ReplaceAllString(s, "[withheld]")
	s = p.withholdSpelled(s)
	return withholdAt(s, codeLocs(s))
}

func withholdAt(s string, locs [][]int) string {
	for i := len(locs) - 1; i >= 0; i-- {
		s = s[:locs[i][0]] + "[withheld]" + s[locs[i][1]:]
	}
	return s
}

// phoneLocs finds phone numbers (seven digits or more, "(555) 010-7788"
// included) that are not dates.
func phoneLocs(s string) [][]int {
	keep := shareKept.FindAllStringIndex(s, -1)
	var out [][]int
	for _, loc := range scanPhone.FindAllStringIndex(s, -1) {
		m := s[loc[0]:loc[1]]
		if len(nonDigit.ReplaceAllString(m, "")) < 7 || shareDateTime.MatchString(m) || withinAny(loc, keep) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// withinAny reports loc lying inside one of spans.
func withinAny(loc []int, spans [][]int) bool {
	for _, k := range spans {
		if loc[0] >= k[0] && loc[1] <= k[1] {
			return true
		}
	}
	return false
}

// clean reports a string a shared copy may carry as is.
func (p dealPrivate) clean(s string) bool {
	return scanExempt.MatchString(s) || p.scrub(s) == s
}

func replaceFold(s, old, replacement string) string {
	if old == "" {
		return s
	}
	lowS, lowOld := strings.ToLower(s), strings.ToLower(old)
	if len(lowS) != len(s) || len(lowOld) != len(old) {
		return strings.ReplaceAll(s, old, replacement)
	}
	var b strings.Builder
	for {
		i := strings.Index(lowS, lowOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(replacement)
		s, lowS = s[i+len(old):], lowS[i+len(old):]
	}
}

// dealRecordShareable reports whether a sealed record may be disclosed to the
// audience: every string value sits under an allowed key and is clean.
func dealRecordShareable(v interface{}, key string, audience string, p dealPrivate) bool {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if k == "ids" {
				// Counterparty fingerprints: keyed HMACs, not the identifiers.
				ids, ok := child.(map[string]interface{})
				if !ok {
					return false
				}
				for _, fp := range ids {
					if s, ok := fp.(string); !ok || !isLowerHex(s) {
						return false
					}
				}
				continue
			}
			if k == "max_total_minor" && audience == dealAudienceCounterparty {
				// The user's spending limit: never the counterparty's to see.
				return false
			}
			if k == "producer" {
				// The software build that sealed the record: shareable in
				// exactly its own shape, without opening "name" to every
				// member of every record.
				if !shareableProducer(child) {
					return false
				}
				continue
			}
			if !dealRecordShareable(child, k, audience, p) {
				return false
			}
		}
		return true
	case []interface{}:
		for _, child := range x {
			if !dealRecordShareable(child, key, audience, p) {
				return false
			}
		}
		return true
	case string:
		allowed := dealShareKeys[key] || strings.HasSuffix(key, "_commitment") || strings.HasSuffix(key, "_digest") ||
			(audience == dealAudienceAdjudicator && dealAdjudicatorKeys[key]) ||
			key == "source" && x == "merchant_email" // a fixed token, not a claim's source
		return allowed && p.clean(x)
	default:
		return true
	}
}

// The producer's members, in the only shapes a share discloses: a build
// name, a release version (a pre-release tag of a few letters and at most
// three digits, so no code or number can ride in it), and a commit hash.
// They are checked by shape rather than scrubbed: "v0.1.0-rc4" would read
// as a code to the scrubber.
var (
	producerName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	producerVersion = regexp.MustCompile(`^v?\d{1,4}\.\d{1,4}\.\d{1,4}(-(rc|alpha|beta|dev)\d{0,3})?$`)
	producerCommit  = regexp.MustCompile(`^([0-9a-f]{7,40}|unknown)$`)
)

func shareableProducer(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok || len(m) != 3 {
		return false
	}
	name, _ := m["name"].(string)
	version, _ := m["version"].(string)
	commit, _ := m["commit"].(string)
	return producerName.MatchString(name) && producerVersion.MatchString(version) && producerCommit.MatchString(commit)
}

// dealWithholdRecords picks the records a shared copy withholds.
func (s *dealSession) dealWithholdRecords(events []sealedEvent, audience string, p dealPrivate) (map[string]bool, error) {
	withhold := map[string]bool{}
	var before []sealedEvent
	for _, se := range events {
		payload, _, err := encodeDealRecord(se.Event, before, s.dkey)
		if err != nil {
			return nil, err
		}
		before = append(before, se)
		var record map[string]interface{}
		if err = json.Unmarshal(payload, &record); err != nil {
			return nil, err
		}
		if !dealRecordShareable(record, "", audience, p) {
			withhold[se.CapsuleID] = true
		}
	}
	return withhold, nil
}

// dealShareableIDs are the merchant order ids and tracking numbers a copy for
// audience may carry: in the counterparty's copy only, each one
// shareableOrderID or shareableTracking allows.
func dealShareableIDs(events []sealedEvent, audience string) []string {
	if audience != dealAudienceCounterparty || len(events) == 0 || events[0].Event.Open == nil {
		return nil
	}
	first := events[0].Event.Open.Who
	var out []string
	for _, se := range events {
		if ev := se.Event.Evidence; ev != nil {
			for _, id := range []string{ev.Email.shareableOrderID(first), ev.Email.shareableTracking(first)} {
				if id != "" {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// dealShareAnomaly is a counterparty-copy anomaly line: fixed words per kind,
// never the values the local line was written from.
var dealShareAnomaly = map[string]string{
	"changed_identifier":       "A payee or contact detail changed after first contact",
	"recourse_changed":         "The way to pay changed after it was agreed",
	"irreversible_rail":        "Payment by a rail with no card protection",
	"domain_recent":            "The website was registered recently",
	"unsealed_approval":        "Went ahead without your approval",
	"asked_vs_did":             "Tried something other than what you asked",
	"skipped_check":            "Acted without a check first",
	"deadline_pressure":        "Pushed you to decide fast",
	"code_request":             "Asked for a verification code",
	"channel_hop":              "Asked to move off the platform",
	"unverified_claim":         "A claim that was not verified",
	"delivered_differs":        "What arrived differs from what was agreed",
	"pay_before_seeing":        "Paying before seeing the item",
	"credentials_requested":    "Asked for a login or code",
	"charged_differs":          "The merchant's email shows a different charge than you approved",
	"charged_before_cancel_by": "Charged before the cancel-by date in the merchant's own email",
	"quantity_differs":         "The merchant's email lists a different quantity than agreed",
	"duplicate_charge":         "Possibly charged twice, by the merchant's own emails",
	"unapproved_disclosure":    "Told the other party about you without your approval",
}

// dealShareStepLine is one step in plain words for a shared copy.
func dealShareStepLine(e dealEvent, audience string, p dealPrivate, currency string) string {
	adjudicator := audience == dealAudienceAdjudicator
	switch e.Kind {
	case "open":
		return "Opened a " + e.Open.Type + " deal (details withheld)"
	case "intent":
		return "You changed what you asked (your words withheld)"
	case "message":
		if adjudicator {
			return fmt.Sprintf("%s: %q", e.Message.From, p.scrub(e.Message.Text))
		}
		return "Message from " + e.Message.From + " (text withheld)"
	case "claim":
		if adjudicator {
			return p.scrub("Claim recorded (" + e.Claim.Source + "): " + e.Claim.Text)
		}
		return "Claim recorded (withheld)"
	case "evidence":
		state := "not verified"
		if e.Evidence.Verified {
			state = "verified"
		}
		if adjudicator {
			return p.scrub("Evidence (" + e.Evidence.Source + "): " + state)
		}
		return "Evidence recorded: " + state
	case "change":
		if adjudicator {
			return p.scrub("Counterparty changed details (" + e.Change.Source + ")")
		}
		return "Counterparty changed details"
	case "snapshot":
		line := "About to " + e.Snapshot.Action
		if e.Snapshot.AmountMinor != nil {
			line += " " + formatMoney(*e.Snapshot.AmountMinor, currency)
		}
		return line
	case "check":
		if e.Check.Verdict == "pass" {
			return "Check: no differences"
		}
		if adjudicator {
			var texts []string
			for _, d := range e.Check.Differences {
				if d.Text != "" {
					texts = append(texts, p.scrub(d.Text))
				}
			}
			return "Check flagged: " + strings.Join(texts, " · ")
		}
		return fmt.Sprintf("Check flagged %d difference(s)", len(e.Check.Differences))
	case "approval":
		if e.Approval.Approver == "standing_intent" {
			return "Went ahead on what you already allowed"
		}
		if e.Approval.Approver == "agent_card" {
			return "The card was answered: " + e.Approval.Choice
		}
		if l := e.Approval.Limits; l != nil && e.Approval.Proceed {
			return "You confirmed new limits"
		}
		return "Your answer: " + e.Approval.Choice
	case "act":
		line := "Did: " + dealShareAct(*e.Act, currency)
		if e.Act.Unchecked {
			line = "⚠️ " + line + " without a passing check or your approval"
		}
		return line
	case "outcome":
		return "Observed: " + e.Outcome.Status + " (" + e.Outcome.Outcome + ")"
	case "close":
		return "Closed: " + e.Close.Outcome
	case "disclosure":
		line := "Told " + e.Disclosure.recipientWord() + ": " + e.Disclosure.classList() + " (values withheld)"
		if e.Disclosure.AuthorizedBy == "" {
			line = "⚠️ " + line + ", without your approval"
		}
		return line
	}
	return e.Kind
}

// dealShareAct is an action as amount and rail only: no payee, no reference.
func dealShareAct(a dealAct, currency string) string {
	a.Payee, a.Reference, a.Description = "", "", ""
	return actText(a, currency)
}

// dealShareExtension is the x-deal-v0 extension of a shared copy: the steps,
// what the agent did and the anomalies, rewritten for the audience.
func dealShareExtension(events []sealedEvent, report dealReport, audience string, p dealPrivate, withhold map[string]bool) map[string]interface{} {
	currency := events[0].Event.Open.Terms.Currency
	byID := map[string]dealEvent{}
	steps := make([]interface{}, len(events))
	for i, se := range events {
		byID[se.CapsuleID] = se.Event
		steps[i] = map[string]interface{}{
			"n": integer(uint64(se.Event.N)), "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "at": se.Event.At,
			"line": p.scrub(dealShareStepLine(se.Event, audience, p, currency)), "withheld": withhold[se.CapsuleID],
		}
	}
	// lastAct is the act an item was read from, if any, for its amount and rail.
	lastAct := func(ids []string) *dealAct {
		for i := len(ids) - 1; i >= 0; i-- {
			if e, ok := byID[ids[i]]; ok && e.Kind == "act" {
				return e.Act
			}
		}
		return nil
	}
	items := func(list []dealReportItem, anomalies bool) []interface{} {
		out := make([]interface{}, len(list))
		for i, item := range list {
			ids := make([]interface{}, len(item.Steps))
			for j, id := range item.Steps {
				ids[j] = id
			}
			text := item.Text
			switch {
			case item.Shared != "" && audience == dealAudienceCounterparty:
				text = item.Shared
			case item.Kind == "act":
				if a := lastAct(item.Steps); a != nil {
					text = "Did: " + dealShareAct(*a, currency)
					if a.Unchecked {
						text += " ⚠️"
					}
				}
			case anomalies && audience == dealAudienceCounterparty:
				words, ok := dealShareAnomaly[item.Kind]
				if !ok {
					words = "Flagged: " + strings.ReplaceAll(item.Kind, "_", " ")
				}
				if a := lastAct(item.Steps); a != nil {
					words += ": " + dealShareAct(*a, currency)
				}
				text = words
			}
			m := map[string]interface{}{"kind": item.Kind, "text": p.scrub(text), "steps": ids}
			if item.Side != "" {
				m["side"] = item.Side
			}
			out[i] = m
		}
		return out
	}
	// The merchant's own emails, for the audience: the signature's verdict in
	// fixed words (the signing domain is the counterparty's: withheld),
	// amounts and dates, the order id and tracking number only where
	// shareableOrderID and shareableTracking allow them
	// (the counterparty's copy), and the items only for an adjudicator.
	first := events[0].Event.Open.Who
	var paid *dealAct
	for _, se := range events {
		if a := se.Event.Act; a != nil && a.Action == "pay" && a.AmountMinor != nil {
			paid = a
		}
	}
	merchant := make([]interface{}, 0, len(report.Merchant))
	for _, row := range report.Merchant {
		ids := make([]interface{}, len(row.Steps))
		for j, id := range row.Steps {
			ids[j] = id
		}
		says := "not confirmed by the merchant's signature"
		if row.Verified {
			says = "merchant-confirmed: the merchant's signature checks out, and the email has not been changed since"
		}
		m := map[string]interface{}{"steps": ids, "verified": row.Verified, "merchant_says": says, "we_say": ourSealWords, "key_source": row.KeySource}
		approved, basis := row.Approved, row.ApprovedBasis
		if audience == dealAudienceCounterparty && basis == dealBasisYourLimit {
			// What was approved was only the user's spending limit: private.
			approved, basis = "", ""
		}
		for k, v := range map[string]string{"approved": approved, "approved_basis": basis, "charged": row.Charged, "charged_on": row.ChargedOn, "cancel_by": row.CancelBy, "key_size": row.KeySize} {
			if v != "" {
				m[k] = v
			}
		}
		if paid != nil {
			m["agent_reported"] = dealShareAct(*paid, currency)
		}
		if audience == dealAudienceCounterparty && len(row.Steps) > 0 {
			if e, ok := byID[row.Steps[0]]; ok && e.Evidence != nil {
				if id := e.Evidence.Email.shareableOrderID(first); id != "" {
					m["order_id"] = id
				}
				if tr := e.Evidence.Email.shareableTracking(first); tr != "" {
					m["tracking"] = tr
				}
			}
		}
		if audience == dealAudienceAdjudicator && len(row.Items) > 0 {
			items := make([]interface{}, len(row.Items))
			for j, it := range row.Items {
				items[j] = p.scrub(it)
			}
			m["items"] = items
		}
		merchant = append(merchant, m)
	}
	withheld := make([]interface{}, 0, len(dealWithheldFields[audience]))
	for _, f := range dealWithheldFields[audience] {
		withheld = append(withheld, f)
	}
	return map[string]interface{}{
		"deal_id": events[0].Event.DealID, "scope": dealScopeLine, "audience": audience, "withheld": withheld, "steps": steps,
		"asked_step": report.AskedStep, "did": items(report.Did, false), "anomalies": items(report.Anomalies, true),
		"merchant": merchant, "email_scope": emailScopeLine,
		"told":      scrubTold(toldItems(report.Told, false), p),
		"lifecycle": shareLifecycle(buildDealLifecycle(events, dealClock())),
	}
}

// shareLifecycle is where the deal stands, for a shared copy: the state,
// the counts and when, in fixed words; a linked record's own line (which can
// name the merchant) stays in your copy.
func shareLifecycle(l dealLifecycle) map[string]interface{} {
	later := make([]interface{}, len(l.Later))
	for i, r := range l.Later {
		later[i] = map[string]interface{}{"at": r.At, "relation": r.Relation, "capsule_id": r.CapsuleID, "text": "a record sealed after the close"}
	}
	return map[string]interface{}{"state": l.State, "as_of": l.AsOf, "text": l.Text, "later": later, "may_change": l.MayChange}
}

// dealVerifyCommand is the one command a stranger runs to check the file
// offline, with nothing but the file and capsulectl.
func dealVerifyCommand(htmlPath string) string {
	name := "receipt.html" // the name the email attaches it under
	if htmlPath != "" {
		name = filepath.Base(htmlPath)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(name) {
		name = "'" + strings.ReplaceAll(name, "'", `'\''`) + "'"
	}
	return "capsulectl verify --bundle " + name
}

// recordShare seals the disclose act before the shared copy is written:
// the disclosure record names the root, the payloads mode, the members and
// records withheld, the audience, the recipient, what the copy leaves out
// and the digest of the exact page. It is kept in the local store and its
// digest is appended to the deal's own disclosure log (beside the deal's
// log, which holds only steps) under a fresh signed checkpoint.
func (s *dealSession) recordShare(ctx context.Context, dealID string, b map[string]interface{}, audience, recipient string, page []byte) (map[string]any, error) {
	record, err := disclosureRecord(b)
	if err != nil {
		return nil, err
	}
	revealed, _ := record["revealed"].(map[string]interface{})
	var withheld []interface{}
	records, _ := b["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		if id, _ := r["capsule_id"].(string); id != "" {
			if _, ok := revealed[id]; !ok {
				withheld = append(withheld, id)
			}
		}
	}
	sort.Slice(withheld, func(i, j int) bool { return withheld[i].(string) < withheld[j].(string) })
	fields := make([]interface{}, 0, len(dealWithheldFields[audience]))
	for _, f := range dealWithheldFields[audience] {
		fields = append(fields, f)
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(page)
	record["deal_id"] = dealID
	record["audience"] = audience
	record["recipient"] = recipient
	record["withheld_records"] = append([]interface{}{}, withheld...)
	record["suppressed_receipt_fields"] = fields
	record["receipt_sha256"] = hex.EncodeToString(sum[:])
	record["at"] = dealClock().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	// The nonce keeps two identical shares distinct and the recipient
	// unguessable from the digest on the log.
	record["nonce"] = hex.EncodeToString(nonce)
	digest, err := canonical.JSONDigest(record)
	if err != nil {
		return nil, err
	}
	local, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	lp := s.dp
	lp.LogID = dealDisclosureLogID(dealID)
	lp.Namespace = ""
	t, err := openTarget(ctx, lp, useInitialization)
	if err != nil {
		return nil, err
	}
	defer func() { _ = t.close() }()
	var n int64
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_disclosures WHERE deal_id=?`, dealID).Scan(&n); err != nil {
		return nil, err
	}
	n++
	if _, err = s.db.ExecContext(ctx, `INSERT INTO deal_disclosures (deal_id, n, record_digest, cll_sequence, record) VALUES (?,?,?,0,?)`, dealID, n, digest, string(local)); err != nil {
		return nil, err
	}
	entry, err := appendRecordDigest(ctx, t.log, record)
	if err != nil {
		_, delErr := s.db.ExecContext(ctx, `DELETE FROM deal_disclosures WHERE deal_id=? AND n=?`, dealID, n)
		if delErr != nil {
			return nil, delErr
		}
		return nil, err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE deal_disclosures SET cll_sequence=? WHERE deal_id=? AND n=?`, entry.Seq, dealID, n); err != nil {
		return nil, err
	}
	cp, err := cutCheckpoint(ctx, lp, t.log)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"audience": audience, "recipient": recipient, "disclosure_record": digest,
		"log_id": lp.LogID, "sequence": entry.Seq, "checkpoint": cp.Size,
	}, nil
}

// dealDisclosureLogID names the log a deal's disclosure records go on.
func dealDisclosureLogID(dealID string) string { return dealLogID(dealID) + "/disclosures" }

// scrubTold runs every text of a shared copy's told list through the
// scrubber: the list names classes, never values, and this makes sure.
func scrubTold(items []interface{}, p dealPrivate) []interface{} {
	for _, raw := range items {
		m, _ := raw.(map[string]interface{})
		for _, k := range []string{"text", "authority_text"} {
			if v, ok := m[k].(string); ok {
				m[k] = p.scrub(v)
			}
		}
	}
	return items
}
