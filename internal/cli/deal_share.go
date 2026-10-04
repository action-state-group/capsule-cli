package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	},
	dealAudienceAdjudicator: {
		"home address", "names, payees and contact details", "card and payment identifiers", "verification codes",
		"your own words", "item, place and condition details",
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
type dealLocal struct{ ids, places, texts []string }

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
		}
	}
	return l
}

// dealPrivate is what a shared copy must never carry, read from the local
// store: every counterparty identifier, every place, every payment
// reference, every card number, code, phone, email or street address found
// in any text the deal holds, and the identifying fragments of every place
// and address (withheld as whole words wherever they appear).
type dealPrivate struct {
	values    []string
	fragments *regexp.Regexp
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
	return p
}

// codeLocs finds every code, card number, PIN, phone, house or order number
// in s: a numberish run holding four or more digits, wherever it sits (inside
// a word like G739142 or code739142, or split like 739 142). Only a date
// and a money amount (1200.00) are left.
func codeLocs(s string) [][]int {
	var out [][]int
	for _, loc := range shareNumberish.FindAllStringIndex(s, -1) {
		m := s[loc[0]:loc[1]]
		if len(nonDigit.ReplaceAllString(m, "")) < 4 || shareDateTime.MatchString(m) || shareMoney.MatchString(m) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

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
	// Matched in its plain form: no zero-width characters, no fullwidth
	// digits, no lookalike letters (foldText).
	s = foldText(s)
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
	var out [][]int
	for _, loc := range scanPhone.FindAllStringIndex(s, -1) {
		m := s[loc[0]:loc[1]]
		if len(nonDigit.ReplaceAllString(m, "")) < 7 || shareDateTime.MatchString(m) {
			continue
		}
		out = append(out, loc)
	}
	return out
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
		allowed := dealShareKeys[key] || strings.HasSuffix(key, "_commitment") || (audience == dealAudienceAdjudicator && dealAdjudicatorKeys[key])
		return allowed && p.clean(x)
	default:
		return true
	}
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

// dealShareAnomaly is a counterparty-copy anomaly line: fixed words per kind,
// never the values the local line was written from.
var dealShareAnomaly = map[string]string{
	"changed_identifier":    "A payee or contact detail changed after first contact",
	"recourse_changed":      "The way to pay changed after it was agreed",
	"irreversible_rail":     "Payment by a rail with no card protection",
	"domain_recent":         "The website was registered recently",
	"unsealed_approval":     "Went ahead without your approval",
	"asked_vs_did":          "Tried something other than what you asked",
	"skipped_check":         "Acted without a check first",
	"deadline_pressure":     "Pushed you to decide fast",
	"code_request":          "Asked for a verification code",
	"channel_hop":           "Asked to move off the platform",
	"unverified_claim":      "A claim that was not verified",
	"delivered_differs":     "What arrived differs from what was agreed",
	"pay_before_seeing":     "Paying before seeing the item",
	"credentials_requested": "Asked for a login or code",
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
	withheld := make([]interface{}, 0, len(dealWithheldFields[audience]))
	for _, f := range dealWithheldFields[audience] {
		withheld = append(withheld, f)
	}
	return map[string]interface{}{
		"deal_id": events[0].Event.DealID, "scope": dealScopeLine, "audience": audience, "withheld": withheld, "steps": steps,
		"asked_step": report.AskedStep, "did": items(report.Did, false), "anomalies": items(report.Anomalies, true),
	}
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
