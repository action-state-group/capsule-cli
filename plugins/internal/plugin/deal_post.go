package plugin

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
)

// A sale's listing post (capsule-engine#187, #190). A listing is the
// agent's words to every buyer, before any buyer's thread, so it is a step
// of the sale's own log:
//
//   - `deal check --deal sale-…` with action post and posting {text,
//     classes, platform}: the text by commitment, the representation
//     classes it states and the platform (the site's domain) in the clear,
//     classed communication.publish;
//   - every post check asks (capsulectl's own public_post difference): no
//     rules checker runs on it, pinned or not (rules status not_applicable),
//     and only the user's own approval, in their words, on the card shown,
//     authorizes the post act, which carries exactly the approved text;
//   - a listing never carries the agent's own authenticity claim: a word
//     from the authenticity list outside the seller's attributed template
//     is refused, and the authenticity class is only the seller's claim in
//     that template.
//
// Each offer and commit check on a sale's thread then seals the rule input
// listing_classes: the classes every live listing states, the intersection
// over the latest post act per platform when the check is made.

// postAction is a sale's listing post: a check action and an act, only on a
// sale's own log, never in intent.allowed.
const postAction = "post"

// rulePublicPost is the difference every post check carries: posting a
// listing always asks the user first.
const rulePublicPost = "public_post"

// rulesNotApplicable is the rules status of a check no rules checker runs
// on (a sale's post), pinned or not: unlike not_configured, which says none
// is pinned.
const rulesNotApplicable = "not_applicable"

// saleStepsAnnotation marks a command that takes a sale id as --deal: the
// steps of a sale's own listing post (check, and note's approval and act).
const saleStepsAnnotation = "capsulectl/sale-steps"

// dealPosting is what a post check is about: the listing's exact text, the
// representation classes it states (the agent's label, shown on the card),
// and the platform it goes on, by the site's domain. A later post on the same
// platform replaces the earlier one there.
type dealPosting struct {
	Text     string   `json:"text"`
	Classes  []string `json:"classes"`
	Platform string   `json:"platform"`
}

// The listing template is the represent check's (deal_representations.go):
// attributedPrefix, attributedSuffix and the one list of sentence marks,
// attributedSentenceMarks (capsule-cli#391), which capsule-engine's
// promise_never/1.1.0 reads too; testdata/sentence-marks.txt holds it.

// authenticityStems and authenticityWholeWords are the words that read as an
// authenticity claim: a stem matches every word it starts, a whole word only
// itself; "knock off" matches with anything between that draws as a gap or a
// dash (knockGap).
var (
	authenticityStems      = []string{"authentic", "genuine", "legit", "counterfeit", "replica", "fake"}
	authenticityWholeWords = []string{"certified", "original"}
)

const knockGap = `[\p{Z}\s\p{Pd}\p{Sm}\p{Cc}_]*`

// authenticityWords matches those words in a listing's skeleton (readRune).
// The patterns go through the same reader as the text (skeletonOfWord), so
// they stay in step with it: "legit" is read "iegit" on both sides. A
// vertical stroke stays a stroke in the skeleton, and each one is read both
// ways at once: the pattern's letter i (which l and 1 read as too) also
// matches a stroke, and a word is bounded by anything that is not a letter or
// a digit of any script, a stroke included, never by ASCII \b alone. So one
// match covers every mix of readings ("|AUTHENT|C|", "used|GENU|NE"), and
// "_genuine" or "x·fake" still match.
var authenticityWords = func() *regexp.Regexp {
	word := func(w string) string {
		return strings.ReplaceAll(regexp.QuoteMeta(skeletonOfWord(w)), "i", `[i|]`)
	}
	var stems, whole []string
	for _, w := range authenticityStems {
		stems = append(stems, word(w))
	}
	for _, w := range authenticityWholeWords {
		whole = append(whole, word(w))
	}
	knock := word("knock") + knockGap + word("off")
	return regexp.MustCompile(`(?:^|[^\p{L}\p{N}])((?:` + strings.Join(stems, "|") + `)[\p{L}\p{N}]*|` + knock + `|` +
		strings.Join(whole, "|") + `)(?:$|[^\p{L}\p{N}])`)
}()

// skeletonRune is one folded rune as the authenticity guard reads it, so a
// word cannot slip past by how it is written:
//
//   - every mark (\p{M}: enclosing marks such as U+20DD and U+0488, spacing
//     marks such as U+0903) reads as nothing;
//   - the negative circled and negative squared Latin capitals, which have
//     no compatibility form (🅖, 🅶), read as their letter (the squared ones,
//     🄶, already did in foldText);
//   - a non-ASCII letter reads as its TR39 confusable skeleton, taken as
//     written, before any case change (a Greek capital eta reads H, a Greek
//     alpha a), when that skeleton is plain ASCII letters;
//   - then lower case, and i, l and 1 read as one letter (i), and o and 0 as
//     one (o): "AUTHENTlC", "GENU1NE" and "0RIGINAL" read as written. A
//     vertical stroke stays a stroke (readRune, authenticityWords).
func skeletonRune(r rune) string {
	switch {
	case unicode.Is(unicode.M, r):
		return ""
	case enclosedLetter(r) != 0:
		r = enclosedLetter(r)
	}
	out := string(r)
	if r > 0x7f && unicode.IsLetter(r) {
		if sk := confusableSkeleton(r); sk != "" && isASCIILetters(sk) {
			out = sk
		}
	}
	return strings.Map(func(c rune) rune {
		switch c = unicode.ToLower(c); c {
		case 'l', '1':
			return 'i'
		case '0':
			return 'o'
		}
		return c
	}, out)
}

// pipeLike are the vertical strokes a word could be split or spelled with:
// "|" and every character outside ASCII whose TR39 prototype is l (｜, ǀ, ∣,
// │, ￨, ׀, ١, ו, ⵏ, ꓲ, the danda …). Each reads as the stroke "|", which the
// patterns take as the letter i and as a gap at once (authenticityWords).
// ASCII l, I and 1, and the styled forms that fold to them (fullwidth ｌ,
// math-alphanumeric 𝐥, ℓ, Roman ⅼ, １ …), stay letters only: as gaps they
// would split ordinary words ("il|legitimate", "origina||y").
func pipeLike(r rune) bool {
	if r == '|' {
		return true
	}
	if r <= 0x7f {
		return false
	}
	if f := norm.NFKD.String(string(r)); f == "l" || f == "I" || f == "1" {
		return false
	}
	confusablesOnce.Do(loadConfusables)
	return confusablePrototype[r] == "l"
}

// readRune is one rune of a listing, its escapes decoded, as the guard reads
// it: its folded form (foldDecoded, which the template is found in), and its
// skeleton. A non-ASCII letter or digit's TR39 prototype is looked up as
// written, before any compatibility fold, so a lunate sigma reads c and a
// long s f, and a digit look-alike such as U+3007 or an Arabic-Indic five
// reads o; a rune that is not a letter or a digit, or has no prototype in
// plain letters and digits, has each rune of its fold read by skeletonRune.
func readRune(r rune) (folded, read string) {
	folded = foldDecoded(string(r))
	if pipeLike(r) {
		return folded, "|"
	}
	// Only a letter or a digit is read as one: a mark of punctuation whose
	// prototype is a letter (the danda's is l) stays a gap between words.
	if r > 0x7f && (unicode.IsLetter(r) || unicode.IsNumber(r)) {
		confusablesOnce.Do(loadConfusables)
		if proto, ok := confusablePrototype[r]; ok {
			if p := foldDecoded(proto); p != "" && isASCIIAlnum(p) {
				return folded, skeletonOfWord(p)
			}
		}
	}
	var b strings.Builder
	for _, q := range folded {
		b.WriteString(skeletonRune(q))
	}
	return folded, b.String()
}

func isASCIIAlnum(s string) bool {
	for _, r := range s {
		if r > 0x7f || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// skeletonOfWord is a word of the guard's own list read by skeletonRune.
func skeletonOfWord(w string) string {
	var b strings.Builder
	for _, r := range w {
		b.WriteString(skeletonRune(r))
	}
	return b.String()
}

// enclosedLetter is the Latin letter a squared, negative circled or
// negative squared capital (U+1F130-1F149, 1F150-1F169, 1F170-1F189) draws,
// which NFKD leaves as it is; 0 for any other rune.
func enclosedLetter(r rune) rune {
	for _, base := range []rune{0x1F130, 0x1F150, 0x1F170} {
		if r >= base && r < base+26 {
			return 'A' + r - base
		}
	}
	return 0
}

func isASCIILetters(s string) bool {
	for _, r := range s {
		if r > 0x7f || !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// attributedSpans are the byte spans of folded (foldText, never the
// skeleton) that are a sentence in the seller's attributed template, read
// exactly as capsulectl's attributedWords and capsule-engine's promise_never
// read it: "The seller says " (this case, these letters) at the start of a
// sentence, a claim that is not blank and holds no sentence mark, and
// " (unverified)" right after it, ending the sentence. The skeleton only
// spots forbidden words; it never grants the template's exception.
func attributedSpans(folded string) [][2]int {
	var spans [][2]int
	for from := 0; ; {
		i := strings.Index(folded[from:], attributedPrefix)
		if i < 0 {
			return spans
		}
		i += from
		from = i + len(attributedPrefix)
		if before := strings.TrimRight(folded[:i], " \t"); before != "" {
			last, _ := utf8.DecodeLastRuneInString(before)
			if !strings.ContainsRune(attributedSentenceMarks, last) {
				continue
			}
		}
		end := strings.Index(folded[from:], attributedSuffix)
		if end < 0 {
			continue
		}
		claim := folded[from : from+end]
		if attributedBlank(claim) || strings.ContainsAny(claim, attributedSentenceMarks) {
			continue
		}
		stop := from + end + len(attributedSuffix)
		if rest := folded[stop:]; rest != "" {
			next, _ := utf8.DecodeRuneInString(rest)
			if !strings.ContainsRune(attributedSentenceMarks+" \t", next) {
				continue
			}
		}
		spans = append(spans, [2]int{i, stop})
		from = stop
	}
}

// checkListingWords holds a listing's words to its classes on authenticity:
// a word that reads as an authenticity claim on its skeleton (readRune,
// authenticityWords) is refused outside the seller's attributed template
// (found exactly, on the folded text), whatever the labels say; and
// authenticity is in classes exactly when a sentence in that template claims
// it, read the same way, so the card never names fewer classes than the
// words state, nor one they do not.
func checkListingWords(p dealPosting) error {
	type piece struct {
		at           int // byte offset in folded
		folded, read string
	}
	var pieces []piece
	var fb strings.Builder
	for _, r := range decodeEscapes(p.Text) {
		f, read := readRune(r)
		pieces = append(pieces, piece{fb.Len(), f, read})
		fb.WriteString(f)
	}
	folded := fb.String()
	spans := attributedSpans(folded)
	// The skeleton outside the template sentences (a rune inside one reads as
	// a space), with where each byte came from in folded; and each template
	// claim's own skeleton.
	var outside strings.Builder
	var from, to []int
	claims := make([]strings.Builder, len(spans))
	for _, pc := range pieces {
		read := pc.read
		for k, sp := range spans {
			if pc.at >= sp[0] && pc.at < sp[1] {
				if pc.at >= sp[0]+len(attributedPrefix) && pc.at < sp[1]-len(attributedSuffix) {
					claims[k].WriteString(pc.read)
				}
				read = " "
			}
		}
		outside.WriteString(read)
		for range len(read) {
			from, to = append(from, pc.at), append(to, pc.at+len(pc.folded))
		}
	}
	claimed := false
	for k := range claims {
		claimed = claimed || authenticityWords.MatchString(claims[k].String())
	}
	// The refusal quotes the word as written (folded), not as it was read.
	if m := authenticityWords.FindStringSubmatchIndex(outside.String()); m != nil {
		word := folded[from[m[2]]:to[m[3]-1]]
		return cli.InputError(`refusing to check the post: the listing says "` + word + `", which reads as a claim that the item is authentic, and you never make that claim. ` +
			`Either put it as the seller's own claim in one sentence, exactly: "The seller says … (unverified)", and add authenticity to classes; ` +
			`or reword it without that word (for example, "comes with its original box" becomes "comes with its box"). Then check again`)
	}
	labelled := slices.Contains(p.Classes, "authenticity")
	switch {
	case labelled && !claimed:
		return cli.InputError(`refusing to check the post: a listing may state authenticity only as the seller's own claim, in one sentence, exactly: "The seller says … (unverified)" ` +
			`(for example "The seller says it's authentic (unverified)"). Put it in those words and check again, or take authenticity out of classes`)
	case claimed && !labelled:
		return cli.InputError("refusing to check the post: the listing states authenticity as the seller's claim: add authenticity to classes and check again")
	}
	return nil
}

// normalizePosting checks a post's posting and puts it in the profile's
// form: the text as given (an approval covers exactly these words), the
// classes as the profile's representation classes, unique and sorted, and
// the platform as its registrable domain.
func normalizePosting(p *dealPosting) error {
	if p == nil {
		return cli.InputError(`a post check needs "posting": {"text": "<the listing's exact words>", "classes": [...], "platform": "<the site's domain>"}`)
	}
	if strings.TrimSpace(p.Text) == "" {
		return cli.InputError("posting.text is the listing's exact words: it cannot be empty")
	}
	if p.Classes == nil {
		return cli.InputError(`posting.classes names what the listing states about the item, for example ["condition", "price"] ([] when it states none of ` + strings.Join(dealClaimClassList, ", ") + `)`)
	}
	classes := []string{}
	for _, c := range p.Classes {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "delivery_promise" {
			c = "delivery_date"
		}
		if !slices.Contains(dealClaimClassList, c) {
			return cli.InputError("posting.classes names a class that is not a representation class: " + c + " (one of " + strings.Join(dealClaimClassList, ", ") + ")")
		}
		if !slices.Contains(classes, c) {
			classes = append(classes, c)
		}
	}
	sort.Strings(classes)
	p.Classes = classes
	platform, err := normDomain(p.Platform)
	if err != nil {
		return cli.InputError(`posting.platform is the site the listing goes on, by its domain (for example "marketplace.example")`)
	}
	p.Platform = platform
	return checkListingWords(*p)
}

// samePosting is whether two normalized postings are the same listing.
func samePosting(a, b dealPosting) bool {
	return a.Text == b.Text && a.Platform == b.Platform && slices.Equal(a.Classes, b.Classes)
}

// postCard is a post check's card: it quotes the exact text, names the
// platform and the classes the listing states, and asks. Its text is what
// the user's "post it" is sealed against.
func postCard(w cli.Wording, p dealPosting, r dealCheckResult, demo bool) cli.Ask {
	ask := cli.Ask{Tag: cardTag(w, r, demo)}
	ask.Question = w.Line("card.question.post", cli.V{"platform": p.Platform, "text": p.Text})
	if len(p.Classes) == 0 {
		ask.Facts = w.Line("card.facts.post.none", nil)
	} else {
		words := make([]string, len(p.Classes))
		for i, c := range p.Classes {
			words[i] = classWord(c)
		}
		ask.Facts = w.Line("card.facts.post", cli.V{"classes": listWords(words, "and")})
	}
	ask.Why = []string{w.Line("card.why.public_post", nil)}
	for _, o := range r.Options {
		ask.Options = append(ask.Options, cli.AskOption{Label: o.Label, Choice: o.ID})
	}
	return ask
}

// isSaleLog is whether --deal names a sale's own log.
func isSaleLog(id string) bool { return cli.SaleIDPattern.MatchString(id) }

// checkPost is `deal check` on a sale's own log: only a post, and it always
// asks. Nothing else is checked at sale level, and no rules checker runs.
func (s *dealSession) checkPost(ctx context.Context, c *cobra.Command, saleID string, events []sealedEvent, snap dealSnapshot, staleAfter time.Duration) error {
	if saleClosed(events) {
		return cli.InputError("the sale " + saleID + " is closed: nothing more is posted for it")
	}
	if snap.Action != postAction {
		return cli.InputError("a sale's own log takes only a post check (action post, with posting): check an offer, commit or share on the buyer's thread (deal open --sale)")
	}
	if snap.AmountMinor != nil || snap.Terms != nil || snap.Recourse != nil || snap.Who != nil || len(snap.Disclosing) > 0 ||
		snap.DisclosingTo != "" || snap.Recipient != nil || snap.Representing != nil || snap.Description != "" ||
		snap.SeenItem != nil || snap.FeeMinor != nil || snap.AuthorizedMaxMinor != nil || snap.ListingClasses != nil {
		return cli.InputError("a post check is about the listing only: posting {text, classes, platform}; leave everything else out")
	}
	snap.ItemRef = ""
	if err := normalizePosting(snap.Posting); err != nil {
		return err
	}
	predicate, err := cli.PinnedMateriality(s.p)
	if err != nil {
		return err
	}
	snapped, err := s.seal(ctx, saleID, events, dealEvent{Kind: "snapshot", Snapshot: &snap})
	if err != nil {
		return err
	}
	events = append(events, snapped)
	// capsulectl's own ask, unconditional: no rule reads a post, so no
	// rules checker runs on it, pinned or not.
	words, _ := cli.ActiveWording(s.p)
	result := dealCheckResult{
		Action: postAction, Snapshot: snapped.CapsuleID, Verdict: "pause",
		Differences: []dealDifference{{Question: "asked", Rule: rulePublicPost, Field: "posting", Text: words.Line("card.why.public_post", nil)}},
		Options:     []dealOption{{ID: "hold"}, {ID: "proceed"}},
		Rules:       &dealRules{Status: rulesNotApplicable},
		Materiality: predicate.Ref(),
		Unverified:  []string{}, Notes: []string{}, Asked: []cli.DealAttribute{}, Picked: []cli.DealAttribute{},
	}
	for i, o := range result.Options {
		key := "card.button.hold"
		if o.ID == "proceed" {
			key = "card.button.post"
		}
		result.Options[i].Label = words.Line(key, nil)
	}
	card := postCard(words, *snap.Posting, result, events[0].Event.Open.Demo)
	result.Card = cli.AskText(words, card)
	result.ValidUntil = cli.DealClock().UTC().Truncate(time.Second).Add(staleAfter).Format(time.RFC3339)
	if result.RulesetDigest, err = rulesetDigest(result.Materiality.PredicateDigest()); err != nil {
		return err
	}
	checked, err := s.seal(ctx, saleID, events, dealEvent{Kind: "check", Check: &result})
	if err != nil {
		return err
	}
	events = append(events, checked)
	out := stepOutput(saleID, checked)
	out["sale_id"] = saleID
	out["check_response"] = checkResponseFor(checked.Event, events, checked.Digest)
	out["check_id"] = checked.CapsuleID
	out["snapshot_id"] = snapped.CapsuleID
	out["verdict"] = result.Verdict
	out["proceed"] = false
	out["card"] = result.Card
	out["options"] = result.Options
	out["differences"] = result.Differences
	out["materiality"] = result.Materiality
	out["rules"] = result.Rules
	out["checked_at"] = checked.Event.At
	out["stale_after_minutes"] = int(staleAfter / time.Minute)
	out["approval_text"] = result.Card
	out["next"] = `show the user this card exactly; post only after their own yes to it: deal note --deal ` + saleID +
		` --kind approval --check ` + checked.CapsuleID + ` --choice proceed --said "<their words>" --shown-card <the card>, then deal note --kind act with {"action": "post", "posting": <the same posting>}`
	cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
		return cli.Narration{Voice: cli.VoiceAsk, Say: result.Card, Ask: &card}
	})
	return cli.Output(c, out)
}

// notePost is `deal note` on a sale's own log: the user's answer to a post
// check, or the post act itself. Every other note goes on a buyer's thread.
func (s *dealSession) notePost(ctx context.Context, c *cobra.Command, saleID string, events []sealedEvent, ev dealEvent, act dealActInput) error {
	if saleClosed(events) {
		return cli.InputError("the sale " + saleID + " is closed: nothing more is posted for it")
	}
	switch ev.Kind {
	case "approval":
		a := ev.Approval
		if a.Choice == "confirm_limits" {
			return cli.InputError("a sale's one task authority does not change: confirm_limits has nothing to confirm on a sale")
		}
		if err := judgeApproval(events, a); err != nil {
			return err
		}
		if err := shownCardMatches(events, *a); err != nil {
			return err
		}
		if a.Approver == "user" && a.ShownCard == "" {
			return cli.InputError("the user's answer to a post check is bound to the card it was given on: pass that card with --shown-card")
		}
		se, err := s.seal(ctx, saleID, events, ev)
		if err != nil {
			return err
		}
		out := stepOutput(saleID, se)
		out["sale_id"] = saleID
		out["proceed"] = a.Proceed && a.Reason == "" && a.Approver == "user"
		out["reason"] = a.Reason
		if a.Proceed && a.Reason == "" && a.Approver != "user" {
			out["reason"] = postNeedsUserWords
		}
		cp, err := s.milestone(ctx)
		if err != nil {
			return err
		}
		out["checkpoint"] = cp
		cli.Narrate(out, cli.Narration{Voice: cli.VoiceSilent})
		return cli.Output(c, out)
	case "act":
		if act.Action != postAction {
			return cli.InputError("a sale's own log takes only the post act: an offer, commit or share goes on the buyer's thread")
		}
		if act.Posting == nil || act.AmountMinor != nil || act.Payee != "" || act.Rail != "" || act.Reference != "" || act.FeeMinor != nil || act.Currency != "" || act.Description != "" {
			return cli.InputError(`a post act carries the listing exactly as approved: {"action": "post", "posting": {"text": ..., "classes": [...], "platform": ...}} and nothing else`)
		}
		posting := *act.Posting
		if err := normalizePosting(&posting); err != nil {
			return err
		}
		authorizedBy, reason := authorizePost(events, posting, cli.DealClock().UTC())
		if authorizedBy == "" {
			return cli.InputError("refusing the post: " + reason)
		}
		ev.Act = &dealAct{Action: postAction, AuthorizedBy: authorizedBy, Posting: &posting}
		se, err := s.seal(ctx, saleID, events, ev)
		if err != nil {
			return err
		}
		out := stepOutput(saleID, se)
		out["sale_id"] = saleID
		out["unchecked"] = false
		out["authorized_by"] = authorizedBy
		out["platform"] = posting.Platform
		out["classes"] = posting.Classes
		cli.Narrate(out, cli.Narration{Voice: cli.VoiceSilent})
		return cli.Output(c, out)
	}
	return cli.InputError("a sale's own log takes only a post's steps (deal check with action post, then --kind approval and --kind act): note everything else on the buyer's thread")
}

// postNeedsUserWords is why a post is not authorized by anything but the
// user's own approval on the card shown.
const postNeedsUserWords = "only the user's own approval, in their words (--said), on the card shown, authorizes a post. A tap on a card button alone does not"

// postSuperseded is why an answer or act after a newer post proposal
// authorizes nothing: the same words the profile checker says.
const postSuperseded = "a newer post check superseded the one this post rests on: show the user the newer card and post only after their yes"

// authorizePost finds what authorizes a post act: the latest post check on
// the sale's log, of exactly this listing, still current, answered first by
// the user's own proceed approval (in their words, on the card shown), which
// no earlier post used. Anything else refuses the post, with the reason.
func authorizePost(events []sealedEvent, p dealPosting, now time.Time) (authorizedBy, reason string) {
	used := usedAuthorizations(events)
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i].Event
		// A newer post proposal, even one whose check never sealed (an
		// interrupted check), supersedes every earlier post check, as the
		// profile checker reads it.
		if e.Kind == "snapshot" && e.Snapshot != nil && e.Snapshot.Action == postAction {
			return "", postSuperseded
		}
		if e.Kind != "check" || e.Check == nil || e.Check.Action != postAction {
			continue
		}
		ck := e.Check
		// A check evaluates the post proposal sealed right before it, never an
		// earlier one.
		var checked *dealPosting
		for k := i - 1; k >= 0; k-- {
			if sn := events[k].Event.Snapshot; events[k].Event.Kind == "snapshot" && sn != nil && sn.Action == postAction {
				if events[k].CapsuleID == ck.Snapshot {
					checked = sn.Posting
				}
				break
			}
		}
		if checked == nil || !samePosting(*checked, p) {
			return "", "the listing is not the one last checked: check these exact words, show the user the card, and post only after their yes"
		}
		if until, err := time.Parse(time.RFC3339, ck.ValidUntil); err == nil && now.After(until) {
			return "", "the check went stale at " + ck.ValidUntil + ": check again before posting"
		}
		for _, later := range events[i+1:] {
			a := later.Event.Approval
			if later.Event.Kind != "approval" || a == nil || a.Check != events[i].CapsuleID {
				continue
			}
			switch {
			case !a.Proceed:
				return "", "the user answered " + a.Choice + " on that check: post only after their yes"
			case a.Reason == approvalChanged:
				return "", postSuperseded
			case a.Reason != "":
				return "", "that answer came after the check was answered (" + a.Reason + ")"
			case a.Approver != "user" || a.ShownCard == "":
				return "", postNeedsUserWords
			case used[later.CapsuleID]:
				return "", "that approval already covered an earlier post: check again"
			}
			return later.CapsuleID, ""
		}
		return "", "the post check asked and there is no approval of the user's: show them the card and post only after their yes"
	}
	return "", "no post check before this post: run deal check with action post first"
}

// listingClasses is the rule input listing_classes a check on a sale's
// thread seals: the classes every live listing of the sale states, the
// intersection over the latest post act per platform on the sale's log now.
// nil (absent) when the sale has no post act; [] when the listings state no
// class in common. It reads the sale's steps from this device's index while
// the session holds the existing store writer lock (openDealSession's flock), the one lock every post act
// is sealed under, so no post act is sealed between this read and the
// check's own seal.
func (s *dealSession) listingClasses(ctx context.Context, saleID string) (*[]string, error) {
	steps, err := s.index.steps(ctx, stepFilter{DealID: saleID, Kinds: []string{"act"}})
	if err != nil {
		return nil, err
	}
	latest := map[string][]string{}
	for _, st := range steps {
		var ev dealEvent
		if err := json.Unmarshal([]byte(st.Local), &ev); err != nil {
			return nil, cli.ErrConflict
		}
		if a := ev.Act; a != nil && a.Action == postAction && !a.Unchecked && a.Posting != nil {
			latest[a.Posting.Platform] = a.Posting.Classes
		}
	}
	if len(latest) == 0 {
		return nil, nil
	}
	var out []string
	first := true
	for _, classes := range latest {
		if first {
			out, first = slices.Clone(classes), false
			continue
		}
		out = slices.DeleteFunc(out, func(c string) bool { return !slices.Contains(classes, c) })
	}
	if out == nil {
		out = []string{}
	}
	sort.Strings(out)
	return &out, nil
}
