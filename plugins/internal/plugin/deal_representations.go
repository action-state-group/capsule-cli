package plugin

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/action-state-group/capsule-cli/cli"
)

// A seller's representations (Seller PRD §3.1, §3.5; capsule-cli#269). What
// the agent of a user who sells tells the buyer is an agent claim with a
// class. From the deal (or sale) that opens with ordered_representations in
// its intent, every such claim is noted against a represent check:
//
//   - a class the user's task authority orders ("tell them about the
//     scratch") passes the check (DO) and the claim is authorized by the
//     task authority alone: no approval is sealed;
//   - any other class asks (ASK), and only the user's own approval, in
//     their words, of those exact words authorizes the claim;
//   - a claim nothing authorized is still sealed, since the words may
//     already have been said, as unchecked, with the rule saying why.
//
// And no offer, commit, sign, pay or share goes ahead while an ordered class
// has no authorized claim in this deal: the check asks, with no way to
// proceed past it, until the agent states it or the user changes what they
// ordered (confirm_limits). On a sale every thread is its own deal, so each
// thread states it to its own buyer.

// representAction is the check a seller's agent runs before it tells the
// buyer something. It is a check action only: never an act, never in
// intent.allowed.
const representAction = "represent"

// ruleRepresentationMissing is the finding of a check held because an
// ordered representation has not been made; ruleRepresentationOutside is a
// represent check of a class the task authority does not order.
const (
	ruleRepresentationMissing = "required_representation_missing"
	ruleRepresentationOutside = "representation_outside_authority"
	// ruleAttributedClaim is capsulectl's own ask on every represent check
	// attributed to the user: whatever the task authority orders, only the
	// user can say these words are their claim (capsule-engine#189).
	ruleAttributedClaim = "attributed_claim"
)

// The user's own claim, said by the seller's agent (capsule-engine#189,
// Steven's A7 ruling): a represent check may carry attributed_to "user", only
// on an authenticity claim, and only in words that follow one fixed
// template, one sentence that names the role, never the person, and ends
// marked unverified: "The seller says it's authentic (unverified)". The
// seller ruleset passes such a claim (promise_never/1.1.0) where it refuses
// the agent's own authenticity claim, and checks the same template when it
// can open the words.
const (
	attributedToUser   = "user"
	attributedClass    = "authenticity"
	attributedPrefix   = "The seller says "
	attributedSuffix   = " (unverified)"
	attributedTemplate = `"The seller says … (unverified)"`
	// attributedSentenceMarks end a sentence or a line, in any script: every
	// Unicode 17.0 Sentence_Terminal code point, every line terminator
	// Python's str.splitlines uses, and U+2026, as explicit code points so the
	// set stays frozen whatever Go's unicode tables become. The engine's
	// promise_never/1.1.0 reads the same list (attributed_template.sentence_marks);
	// testdata/sentence-marks.txt holds it, byte-identical in both repositories.
	attributedSentenceMarks = "" +
		"\u000A\u000B\u000C\u000D\u001C\u001D\u001E\u0021\u002E\u003F\u0085\u0589\u061D" +
		"\u061E\u061F\u06D4\u0700\u0701\u0702\u07F9\u0837\u0839\u083D\u083E\u0964\u0965" +
		"\u104A\u104B\u1362\u1367\u1368\u166E\u1735\u1736\u17D4\u17D5\u1803\u1809\u1944" +
		"\u1945\u1AA8\u1AA9\u1AAA\u1AAB\u1B4E\u1B4F\u1B5A\u1B5B\u1B5E\u1B5F\u1B7D\u1B7E" +
		"\u1B7F\u1C3B\u1C3C\u1C7E\u1C7F\u2024\u2026\u2028\u2029\u203C\u203D\u2047\u2048" +
		"\u2049\u2CF9\u2CFA\u2CFB\u2E2E\u2E3C\u2E53\u2E54\u3002\uA4FF\uA60E\uA60F\uA6F3" +
		"\uA6F7\uA876\uA877\uA8CE\uA8CF\uA92F\uA9C8\uA9C9\uAA5D\uAA5E\uAA5F\uAAF0\uAAF1" +
		"\uABEB\uFE12\uFE15\uFE16\uFE52\uFE56\uFE57\uFF01\uFF0E\uFF1F\uFF61\U00010A56" +
		"\U00010A57\U00010F55\U00010F56\U00010F57\U00010F58\U00010F59\U00010F86\U00010F87" +
		"\U00010F88\U00010F89\U00011047\U00011048\U000110BE\U000110BF\U000110C0\U000110C1" +
		"\U00011141\U00011142\U00011143\U000111C5\U000111C6\U000111CD\U000111DE\U000111DF" +
		"\U00011238\U00011239\U0001123B\U0001123C\U000112A9\U000113D4\U000113D5\U0001144B" +
		"\U0001144C\U000115C2\U000115C3\U000115C9\U000115CA\U000115CB\U000115CC\U000115CD" +
		"\U000115CE\U000115CF\U000115D0\U000115D1\U000115D2\U000115D3\U000115D4\U000115D5" +
		"\U000115D6\U000115D7\U00011641\U00011642\U0001173C\U0001173D\U0001173E\U00011944" +
		"\U00011946\U00011A42\U00011A43\U00011A9B\U00011A9C\U00011C41\U00011C42\U00011EF7" +
		"\U00011EF8\U00011F43\U00011F44\U00016A6E\U00016A6F\U00016AF5\U00016B37\U00016B38" +
		"\U00016B44\U00016D6E\U00016D6F\U00016E98\U0001BC9F\U0001DA88"
)

// attributedBlank is whether a claim is blank as Python's str.strip sees it,
// as the engine does: Unicode white space and U+001C-U+001F.
func attributedBlank(claim string) bool {
	return strings.TrimFunc(claim, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }) == ""
}

// attributedWords is whether text follows the attribution template exactly:
// the prefix, a claim that is not blank and holds no sentence mark
// (attributedSentenceMarks), so it stays one sentence, then the suffix.
func attributedWords(text string) bool {
	if !strings.HasPrefix(text, attributedPrefix) || !strings.HasSuffix(text, attributedSuffix) ||
		len(text) < len(attributedPrefix)+len(attributedSuffix) {
		return false
	}
	claim := text[len(attributedPrefix) : len(text)-len(attributedSuffix)]
	return !attributedBlank(claim) && !strings.ContainsAny(claim, attributedSentenceMarks)
}

// representationGated are the acts an ordered representation must come
// before: every act that moves toward or carries out a commitment, and every
// share. A cancel is never held: the seller can always walk away.
var representationGated = map[string]bool{
	"offer": true, "commit": true, "sign": true, "pay": true, "share_contact": true, "share_credentials": true,
}

// dealRepresenting is what a represent check is about: the class of the
// statement and its exact words, and where they will be said when given.
type dealRepresenting struct {
	Class  string `json:"class"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
	// AttributedTo is "user" when the words are the user's own claim, said
	// as theirs and marked unverified (attributedWords); empty when they are
	// the agent's.
	AttributedTo string `json:"attributed_to,omitempty"`
}

// representationRules is whether a deal follows the representation rules:
// a deal where the user sells whose opening intent records the ordered
// representations (empty or not). A deal opened before keeps its old rules,
// and a sale's thread follows its sale: it carries the sale's intent.
func representationRules(events []sealedEvent) bool {
	if len(events) == 0 || events[0].Event.Open == nil || dealRole(events) != dealRoleSeller {
		return false
	}
	return events[0].Event.Open.Intent.OrderedRepresentations != nil
}

// normalizeOrdered makes an ordered list the profile's classes, unique and
// sorted, or refuses it.
func normalizeOrdered(list *[]string) error {
	if list == nil {
		return nil
	}
	out := []string{}
	for _, c := range *list {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "delivery_promise" {
			c = "delivery_date"
		}
		if !slices.Contains(dealClaimClassList, c) {
			return cli.InputError("ordered_representations names a class that is not a representation class: " + c + " (one of " + strings.Join(dealClaimClassList, ", ") + ")")
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	*list = out
	return nil
}

// ordered is the ordered representations in force, nil on a deal that has
// none.
func (s dealState) ordered() []string {
	if s.intent.OrderedRepresentations == nil {
		return nil
	}
	return *s.intent.OrderedRepresentations
}

// missingRepresentations are the ordered classes no authorized agent claim
// of this deal has stated yet.
func (s dealState) missingRepresentations() []string {
	var out []string
	for _, c := range s.ordered() {
		if !slices.ContainsFunc(s.claims, func(cl dealClaim) bool {
			return cl.SourceKind == "agent" && cl.Class == c && cl.AuthorizedBy != "" && !cl.Unchecked
		}) {
			out = append(out, c)
		}
	}
	return out
}

// evaluateRepresentation is the built-in rule for a represent check: an
// ordered class passes (DO), any other asks for the user's word on those
// words. An external checker may deny either (promise_never).
func evaluateRepresentation(r *dealCheckResult, s dealState, snap dealSnapshot, add func(q, rule, field, text string)) {
	c := snap.Representing.Class
	// An attributed claim always asks: a class the user ordered says what may
	// be said, not that the user made this claim.
	if snap.Representing.AttributedTo == attributedToUser {
		add("asked", ruleAttributedClaim, "representation", "These words go out as your claim: \""+snap.Representing.Text+"\". Only you can say they're yours")
	}
	if !slices.Contains(s.ordered(), c) {
		add("asked", ruleRepresentationOutside, "representation", "You didn't ask me to tell the buyer about the "+classWord(c)+". I need your OK on these exact words")
	}
}

// checkRepresenting validates a represent check's snapshot on this deal.
func checkRepresenting(events []sealedEvent, snap *dealSnapshot) error {
	if snap.Action != representAction {
		if snap.Representing != nil {
			return cli.InputError(`representing goes with a represent check`)
		}
		return nil
	}
	switch {
	case dealRole(events) != dealRoleSeller:
		return cli.InputError("a represent check is made on a deal where the user sells (intent.party_role seller)")
	case !representationRules(events):
		return cli.InputError("this deal opened before representations were checked: note the agent's claims as before")
	case snap.Representing == nil:
		return cli.InputError(`a represent check needs "representing": {"class": ..., "text": "<the exact words>"}`)
	}
	rp := snap.Representing
	claim := dealClaim{Text: rp.Text, Source: rp.Source, SourceKind: "agent", Class: rp.Class}
	if err := normalizeClaim(&claim); err != nil {
		return err
	}
	if claim.Class == "" {
		return cli.InputError(`a represent check names the class of what is about to be said: one of ` + strings.Join(dealClaimClassList, ", "))
	}
	if field := statesBound(claim.Text, events, true); field != "" {
		return cli.InputError("refusing to check: the words would tell the buyer " + field + "; leave the user's bounds out of what the agent says")
	}
	if snap.AmountMinor != nil || snap.Terms != nil || snap.Recourse != nil || snap.Who != nil || len(snap.Disclosing) > 0 {
		return cli.InputError("a represent check is about the words only: leave amounts, terms, recourse, who and disclosing out")
	}
	if err := checkAttribution(rp.AttributedTo, claim.Class, claim.Text); err != nil {
		return err
	}
	*rp = dealRepresenting{Class: claim.Class, Text: claim.Text, Source: claim.Source, AttributedTo: rp.AttributedTo}
	return nil
}

// checkAttribution refuses an attribution outside its closed set, on any
// class but authenticity, or in words that do not follow the template.
func checkAttribution(to, class, text string) error {
	switch {
	case to == "":
		return nil
	case to != attributedToUser:
		return cli.InputError(`representing.attributed_to is "user" (the user's own claim) or left out (the agent's)`)
	case class != attributedClass:
		return cli.InputError("only an authenticity claim can be attributed to the user: leave attributed_to out of a " + classWord(class) + " claim")
	case !attributedWords(text):
		return cli.InputError("refusing to check: the user's claim must read " + attributedTemplate + ", one sentence, with no full stop, !, ? or line break before \"(unverified)\" (in any script), so the buyer sees whose claim it is and that it is unverified")
	}
	return nil
}

// checkSellerClaim holds a claim noted on a deal under the representation
// rules to them, before anything is authorized: the agent's own claims name
// their class, and a claim said to be someone else's that names no source
// cannot be told from the agent's own.
func checkSellerClaim(events []sealedEvent, c *dealClaim) error {
	if !representationRules(events) {
		if c.Check != "" {
			return cli.InputError("--check goes with an agent's claim on a deal where the user sells, opened with ordered_representations")
		}
		return nil
	}
	switch {
	case c.SourceKind == "agent" && c.Class == "":
		return cli.InputError(`a claim the agent makes to the buyer names its class: one of ` + strings.Join(dealClaimClassList, ", ") + ` ("other" for anything else)`)
	case (c.SourceKind == "external" || c.SourceKind == "merchant" || c.SourceKind == "platform") && c.Source == "":
		return cli.InputError(`a claim that is not the agent's or the user's names where it was read (source): without it, it cannot be told from the agent's own`)
	case c.SourceKind != "agent" && c.Check != "":
		return cli.InputError("--check goes with the agent's own claim (source_kind agent)")
	}
	return nil
}

// authorizeClaim finds what authorizes an agent's claim under the represent
// check it names: the check's evaluation when it passed (DO), or the user's
// own proceed approval of it (ASK). The check must be of exactly these
// words, of this deal, unused, still current, and under the task authority
// in force. Anything else leaves the claim unchecked, with the rule why.
func authorizeClaim(events []sealedEvent, c dealClaim, now time.Time) (authorizedBy, reason, rule string) {
	if c.Check == "" {
		return "", "no represent check was named for it", "no_check"
	}
	at := -1
	for i, se := range events {
		if se.CapsuleID == c.Check && se.Event.Kind == "check" {
			at = i
		}
	}
	if at < 0 {
		return "", "--check names no check of this deal", "no_check"
	}
	ck := events[at].Event.Check
	var snap *dealSnapshot
	for _, se := range events[:at] {
		if se.CapsuleID == ck.Snapshot {
			snap = se.Event.Snapshot
		}
	}
	if ck.Action != representAction || snap == nil || snap.Representing == nil {
		return "", "that check is not a represent check", "no_check"
	}
	if rp := snap.Representing; rp.Class != c.Class || rp.Text != c.Text || rp.Source != c.Source {
		return "", "the claim is not the words that were checked: check these words", "differs_from_check"
	}
	attributed := snap.Representing.AttributedTo == attributedToUser
	if ck.Verdict == "deny" {
		return "", "the check denied it", "denied"
	}
	for _, later := range events[at+1:] {
		if changesDetails(later.Event) {
			return "", "details changed after the check: check again", "changed_after_check"
		}
	}
	if ck.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, ck.ValidUntil); err == nil && now.After(until) {
			return "", "the check went stale at " + ck.ValidUntil + ": check again before saying it", "check_expired"
		}
	}
	if taskAuthorityAt(events[:at], "") != taskAuthorityAt(events, "") {
		return "", "what you asked the agent to say changed after the check: check again", "authority_changed"
	}
	used := usedAuthorizations(events)
	// The user's own claim is authorized only by the user's own approval, in
	// their words: never by a passing check alone.
	if ck.Verdict == "pass" && !attributed {
		if used[c.Check] {
			return "", "that check already covered an earlier claim", "approval_used"
		}
		return c.Check, "", ""
	}
	for _, later := range events[at+1:] {
		a := later.Event.Approval
		if later.Event.Kind != "approval" || a == nil || a.Check != c.Check {
			continue
		}
		switch {
		case !a.Proceed:
			return "", "the check asked and the answer was " + a.Choice, "not_approved"
		case a.Approver != "user" && attributed:
			return "", "these words go out as your claim, and only your own approval, in your words, says they're yours", "not_approved"
		case a.Approver != "user":
			return "", "the check asked, and only your own approval, in your words, covers what the agent says for you", "not_approved"
		case used[later.CapsuleID]:
			return "", "that approval already covered an earlier claim", "approval_used"
		}
		return later.CapsuleID, "", ""
	}
	return "", "the check asked and there is no sealed approval", "not_approved"
}

// shareCheckedNonces seals an authorized representation's words under the
// nonces its represent check sealed them with, so its text_commitment (and
// source_ref_commitment) equals the check's: a verifier sees that the words
// said are the words checked, without the words.
func shareCheckedNonces(events []sealedEvent, ev *dealEvent) {
	c := ev.Claim
	if c == nil || c.AuthorizedBy == "" || c.Check == "" {
		return
	}
	var snapshot string
	for _, se := range events {
		if se.CapsuleID == c.Check && se.Event.Check != nil {
			snapshot = se.Event.Check.Snapshot
		}
	}
	for _, se := range events {
		if se.CapsuleID != snapshot || se.Event.Snapshot == nil || se.Event.Snapshot.Representing == nil {
			continue
		}
		ev.Nonces["claim_text"] = se.Event.Nonces["representing_text"]
		if c.Source != "" {
			ev.Nonces["claim_source"] = se.Event.Nonces["representing_source"]
		}
	}
}

// representationHold is, for an act or a disclosure about to be noted, why
// the representation gate holds it now (re-read when it is noted, since the
// user may have ordered more after the check): "" when nothing is missing.
func representationHold(events []sealedEvent, action string) (reason string) {
	if !representationGated[action] || !representationRules(events) {
		return ""
	}
	state, err := foldDeal(events)
	if err != nil {
		return ""
	}
	missing := state.missingRepresentations()
	if len(missing) == 0 {
		return ""
	}
	words := make([]string, len(missing))
	for i, c := range missing {
		words[i] = classWord(c)
	}
	return "you asked the agent to tell the buyer about the " + strings.Join(words, ", ") + " first"
}
