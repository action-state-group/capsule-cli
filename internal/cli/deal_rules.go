package cli

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// This file is the deterministic half of `capsulectl deal`: the deal record
// shapes and the four questions asked at a point of no return. It performs no
// I/O. Every input it reads was sealed before it is read (see deal.go), and
// every result it returns is sealed before it is shown.

// dealPointsOfNoReturn lists, per deal type, the actions that cannot easily be
// undone. `deal check` refuses any other action, so an agent cannot reach an
// irreversible step through a name the rules do not cover.
var dealPointsOfNoReturn = map[string][]string{
	"purchase": {"pay", "commit", "cancel", "share_contact", "share_credentials", "offer"},
	"rental":   {"pay", "commit", "sign", "cancel", "share_contact", "share_credentials", "offer"},
	"booking":  {"pay", "commit", "cancel", "share_contact", "share_credentials", "offer"},
	"service":  {"pay", "commit", "sign", "cancel", "share_contact", "share_credentials", "offer"},
}

// offerAction is a seller's offer or counteroffer: its checked snapshot is the
// exact proposal (every material term, under this deal's id), and a later
// offer supersedes it. Only a deal whose party_role is seller makes one.
const offerAction = "offer"

// dealWho identifies the counterparty. Every field is optional; each one that
// was sealed at first contact is compared on every check.
type dealWho struct {
	Name          string `json:"name,omitempty"`
	Domain        string `json:"domain,omitempty"`
	Phone         string `json:"phone,omitempty"`
	Email         string `json:"email,omitempty"`
	Payee         string `json:"payee,omitempty"`
	RelayAddress  string `json:"relay_address,omitempty"`
	ProfileID     string `json:"profile_id,omitempty"`
	DomainAgeDays *int64 `json:"domain_age_days,omitempty"`
}

// dealTerms are what, how much and when. Money is integer minor units (cents):
// sealed payloads never carry floats.
type dealTerms struct {
	Item         string            `json:"item,omitempty"`
	Quantity     int64             `json:"quantity,omitempty"`
	PriceMinor   *int64            `json:"price_minor,omitempty"`
	DepositMinor *int64            `json:"deposit_minor,omitempty"`
	Currency     string            `json:"currency,omitempty"`
	When         string            `json:"when,omitempty"`
	Place        string            `json:"place,omitempty"`
	Conditions   map[string]string `json:"conditions,omitempty"`
}

type dealClaim struct {
	Text string `json:"text"`
	// Source is where the claim was read, in the caller's words (a token);
	// SourceKind is whose it is, from a closed set (dealSourceKinds).
	Source     string `json:"source,omitempty"`
	SourceKind string `json:"source_kind,omitempty"`
	Verified   bool   `json:"verified,omitempty"`
}

type dealRecourse struct {
	Rail       string `json:"rail,omitempty"`
	Refundable *bool  `json:"refundable,omitempty"`
}

// dealIntent is what the user asked for: their verbatim words plus the parts
// of the request the agent could make exact.
type dealIntent struct {
	Verbatim string `json:"verbatim"`
	// PartyRole is which side of the deal the user is on: buyer or seller.
	// Absent means buyer, as for every deal sealed before it was recorded;
	// it is never written in for one. Set when the deal opens and fixed.
	PartyRole     string    `json:"party_role,omitempty"`
	Asked         dealTerms `json:"asked,omitempty"`
	MaxTotalMinor *int64    `json:"max_total_minor,omitempty"`
	// MinTotalMinor is the lowest total the user will take (a seller's
	// floor). It stays on this device: records seal only bounds_commitment,
	// a salted commitment to the commercial-bounds/v0 document holding it
	// (commercialBoundsText), opened in the user's own copy alone.
	MinTotalMinor *int64 `json:"min_total_minor,omitempty"`
	// boundsCommit is the bounds_commitment of the floor in force, set
	// while folding the deal (never stored).
	boundsCommit string
	// Allowed absent (nil) means no restriction; present and empty means
	// nothing is allowed yet ("show me options, don't book").
	Allowed []string `json:"allowed"`
}

type dealOpen struct {
	Type     string       `json:"type"`
	Demo     bool         `json:"demo,omitempty"`
	Channel  string       `json:"channel,omitempty"`
	Intent   dealIntent   `json:"intent"`
	Who      dealWho      `json:"who"`
	Terms    dealTerms    `json:"terms"`
	Claims   []dealClaim  `json:"claims,omitempty"`
	Recourse dealRecourse `json:"recourse"`
	// Skill is set by `deal open --skill`, never from the input file.
	Skill *dealSkill `json:"skill,omitempty"`
	// Records is the record set this deal is sealed in, set by `deal open`
	// (never from the input file): "" for x-deal-v0 records throughout, or
	// recordsTyped for the typed action records (task-authority/v0,
	// proposed-action/v0, action-evaluation/v0, action-approval/v0,
	// action-record/v0, action-outcome/v0) beside x-deal-v0 evidence records,
	// in one chain. Fixed for the deal, so its records always re-derive.
	Records string `json:"records,omitempty"`
	// ExpectCloseBy is the day this deal is expected to be closed
	// (YYYY-MM-DD); the default depends on the deal type (deal_late.go).
	// `deal deadlines` lists an open deal against it.
	ExpectCloseBy string `json:"expect_close_by,omitempty"`
	// Materiality is the materiality predicate the profile pinned when the
	// deal opened (digest "none": none). Set by `deal open` from the
	// profile, never from the input file. A later check under another one
	// is flagged. Absent on deals opened before it was recorded.
	Materiality dealMateriality `json:"materiality,omitzero"`
}

type dealMessage struct {
	From    string   `json:"from"`
	Channel string   `json:"channel,omitempty"`
	Text    string   `json:"text"`
	Who     *dealWho `json:"who,omitempty"`
}

type dealEvidence struct {
	About    string   `json:"about"`
	Source   string   `json:"source"`
	Verified bool     `json:"verified,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Who      *dealWho `json:"who,omitempty"`
	// Email is a merchant's own email, sealed raw (deal_merchant_email.go).
	Email *merchantEmail `json:"email,omitempty"`
	// Obligation is a commitment that takes effect when a date passes
	// (deal_obligation.go), recorded with this evidence as its source.
	Obligation *dealObligation `json:"obligation,omitempty"`
	// Resolves names the obligation step this evidence proves resolved
	// (its capsule id; ResolvesStep, its step number, is accepted on input).
	Resolves     string `json:"resolves,omitempty"`
	ResolvesStep int64  `json:"resolves_step,omitempty"`
}

// dealChange is a detail the counterparty changed after first contact. It is
// never accepted by being recorded: a changed payee is compared against first
// contact on every later check.
type dealChange struct {
	Source   string        `json:"source"`
	Who      *dealWho      `json:"who,omitempty"`
	Terms    *dealTerms    `json:"terms,omitempty"`
	Recourse *dealRecourse `json:"recourse,omitempty"`
}

// dealSnapshot is exactly what is about to happen at a point of no return.
type dealSnapshot struct {
	Action      string        `json:"action"`
	Description string        `json:"description,omitempty"`
	AmountMinor *int64        `json:"amount_minor,omitempty"`
	SeenItem    *bool         `json:"seen_item,omitempty"`
	Who         *dealWho      `json:"who,omitempty"`
	Terms       *dealTerms    `json:"terms,omitempty"`
	Recourse    *dealRecourse `json:"recourse,omitempty"`
	// Disclosing names, for a share, the classes of the user's data about to
	// be given (phone, address, ...): the check pauses on the first time a
	// class goes to this counterparty, never on a repeat.
	Disclosing []string `json:"disclosing,omitempty"`
	// DisclosingTo is who receives it: "counterparty" (the default) or
	// "other", named by Recipient (a courier, a platform, a third person).
	// An approval covers a telling only to the party its check was about.
	DisclosingTo string   `json:"disclosing_to,omitempty"`
	Recipient    *dealWho `json:"recipient,omitempty"`
	// FeeMinor is, on a cancel only, a fee the cancellation costs, recorded
	// as its own amount: never the amount a spend cap evaluates (spend_minor
	// is 0 on every cancel).
	FeeMinor *int64 `json:"fee_minor,omitempty"`
	// AuthorizedMaxMinor is, on a pay, the most the counterparty may take
	// under the payment's authorization (a card hold, a pre-authorization
	// with a buffer for tax settled later): what a limit binds. AmountMinor
	// stays the expected charge.
	AuthorizedMaxMinor *int64 `json:"authorized_max_minor,omitempty"`
}

type dealDifference struct {
	Question string `json:"question"`
	Rule     string `json:"rule"`
	Field    string `json:"field,omitempty"`
	Text     string `json:"text"`
}

type dealOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type dealRemoteResult struct {
	Status         string           `json:"status"`
	ResponseSHA256 string           `json:"response_sha256,omitempty"`
	Differences    []dealDifference `json:"differences,omitempty"`
}

type dealCheckResult struct {
	Action      string           `json:"action"`
	Snapshot    string           `json:"snapshot"`
	Verdict     string           `json:"verdict"`
	Differences []dealDifference `json:"differences"`
	Unverified  []string         `json:"unverified"`
	Notes       []string         `json:"notes"`
	Card        string           `json:"card"`
	Options     []dealOption     `json:"options"`
	Remote      dealRemoteResult `json:"remote"`
	// Rules is what the profile's external rules checker said (evaluated,
	// not evaluated and why, or not configured): absent on checks sealed
	// before there was one.
	Rules *dealRules `json:"rules,omitempty"`
	// The check contract's fields fixed when the evaluation is sealed (typed
	// records), so a later release re-derives the same record: when it stops
	// covering the action, and the rule table that evaluated (with the
	// materiality predicate, Materiality, the one source of which applied).
	ValidUntil    string `json:"valid_until,omitempty"`
	RulesetDigest string `json:"ruleset_digest,omitempty"`
	// Asked are the attributes of what is about to happen that the user
	// specified (in their own words, or by choosing, sealed as an intent);
	// Picked are the ones the agent chose and the user never said.
	Asked  []dealAttribute `json:"asked_attributes"`
	Picked []dealAttribute `json:"picked_by_agent"`
	// Recipient is what the counterparty memory says about them: absent on
	// checks made before there was one.
	Recipient *dealRecipient `json:"recipient,omitempty"`
	// Materiality is which predicate decided the pauses on the agent's picks
	// (digest "none": no predicate, every pick paused). Absent on checks
	// sealed before it was recorded.
	Materiality dealMateriality `json:"materiality,omitzero"`
}

// dealAttribute is one attribute of what is about to happen, and its value.
type dealAttribute struct {
	Field string `json:"field"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// attributeProvenance splits what is about to happen into what the user
// asked for and what the agent picked on its own. An attribute is the user's
// only where their sealed intent names it and the value matches: one that
// differs is not theirs, and is a "Not what you asked" difference instead.
// An attribute the intent does not name is the agent's. Money is not listed:
// the amount, the limit and the price are on the card already.
func attributeProvenance(asked, proposed dealTerms) (yours, agents []dealAttribute) {
	yours, agents = []dealAttribute{}, []dealAttribute{}
	sort := func(field, label, mine, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		a := dealAttribute{Field: field, Label: label, Value: value}
		switch {
		case strings.TrimSpace(mine) == "":
			agents = append(agents, a)
		case strings.EqualFold(strings.TrimSpace(mine), strings.TrimSpace(value)):
			yours = append(yours, a)
		}
	}
	qty := func(v int64) string {
		if v == 0 {
			return ""
		}
		return fmt.Sprint(v)
	}
	sort("item", "item", asked.Item, proposed.Item)
	sort("quantity", "quantity", qty(asked.Quantity), qty(proposed.Quantity))
	sort("when", "dates", asked.When, proposed.When)
	sort("place", "place", asked.Place, proposed.Place)
	keys := make([]string, 0, len(proposed.Conditions))
	for k := range proposed.Conditions {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		sort("conditions."+k, strings.ReplaceAll(k, "_", " "), asked.Conditions[k], proposed.Conditions[k])
	}
	return yours, agents
}

// namedInWords reports whether every word of value appears in words, the
// user's own: "HOU/SJC" in "HOU to SJC", "Oct 21 to Oct 24" in "Oct 21 to
// Oct 24", but not a flight number the user never said. Small joining words
// are not needed. It is a reading aid for the report only: a single word
// (the "2" in "Oct 2") can match by chance, so it never decides a check.
func namedInWords(value, words string) bool {
	token := regexp.MustCompile(`[\p{L}\p{N}]+`)
	said := map[string]bool{}
	for _, w := range token.FindAllString(strings.ToLower(words), -1) {
		said[w] = true
	}
	joining := map[string]bool{"to": true, "and": true, "the": true, "a": true, "of": true, "on": true, "in": true, "at": true, "for": true, "from": true}
	named := 0
	for _, w := range token.FindAllString(strings.ToLower(value), -1) {
		if joining[w] {
			continue
		}
		if !said[w] {
			return false
		}
		named++
	}
	return named > 0
}

// attributeText is a list of attributes as "label value" pairs.
func attributeText(list []dealAttribute) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.Label + " " + a.Value
	}
	return strings.Join(parts, " · ")
}

// dealApproval answers one check (a verdict). Approver is "user" for the
// user's own answer, or "standing_intent" when a passing check is covered by
// what the user already allowed.
type dealApproval struct {
	Check    string `json:"check"`
	Choice   string `json:"choice"`
	Approver string `json:"approver"`
	Said     string `json:"said,omitempty"`
	// ShownCard is the exact card text the answer was given on (deal note
	// --shown-card). The record commits to it under the check's own card
	// nonce, so its card_commitment (in typed records, rendering_commitment)
	// equals the check's exactly when the card shown is the card checked.
	ShownCard string `json:"shown_card,omitempty"`
	Proceed   bool   `json:"proceed"`
	Reason    string `json:"reason,omitempty"`
	// Limits is set on the user's confirm_limits answer to an intent note
	// that proposed a higher limit or more actions: the limits in force
	// before it and the new version it seals. Check then names that note.
	Limits *dealLimits `json:"limits,omitempty"`
}

// dealLimits is one confirmed change to the user's limits: the version in
// force before it and the version it puts in force.
type dealLimits struct {
	Previous dealLimitSet `json:"previous"`
	New      dealLimitSet `json:"new"`
}

// dealLimitSet is a version of the user's limits. Allowed absent (nil) means
// no restriction, as in dealIntent. The floor (MinTotalMinor) is sealed only
// as BoundsCommitment, the commitment of the step that set it.
type dealLimitSet struct {
	MaxTotalMinor    *int64   `json:"max_total_minor,omitempty"`
	MinTotalMinor    *int64   `json:"min_total_minor,omitempty"`
	BoundsCommitment string   `json:"bounds_commitment,omitempty"`
	Allowed          []string `json:"allowed"`
}

// String is a version of the limits in plain words, for the trail.
func (l dealLimitSet) String() string {
	limit := "no limit"
	if l.MaxTotalMinor != nil {
		limit = "limit " + strconv.FormatInt(*l.MaxTotalMinor, 10) + " (minor units)"
	}
	if l.MinTotalMinor != nil {
		limit += ", floor " + strconv.FormatInt(*l.MinTotalMinor, 10) + " (minor units)"
	}
	switch {
	case l.Allowed == nil:
		return limit + ", any action"
	case len(l.Allowed) == 0:
		return limit + ", no action yet"
	}
	return limit + ", " + strings.Join(l.Allowed, ", ")
}

func (i dealIntent) limits() dealLimitSet {
	return dealLimitSet{MaxTotalMinor: i.MaxTotalMinor, MinTotalMinor: i.MinTotalMinor, BoundsCommitment: i.boundsCommit, Allowed: i.Allowed}
}

// proposedLimits is what an intent note asks for beyond the limits in force,
// and whether it asks for more at all: a higher limit, or an action the
// limits in force do not allow. A note without a limit or a list keeps the
// one in force.
func proposedLimits(cur dealLimitSet, note dealIntent) (dealLimitSet, bool) {
	out, more := cur, false
	if note.MaxTotalMinor != nil {
		if cur.MaxTotalMinor != nil && *note.MaxTotalMinor > *cur.MaxTotalMinor {
			more = true
		}
		limit := *note.MaxTotalMinor
		out.MaxTotalMinor = &limit
	}
	// The floor runs the other way: a lower floor asks for more.
	if note.MinTotalMinor != nil {
		if cur.MinTotalMinor != nil && *note.MinTotalMinor < *cur.MinTotalMinor {
			more = true
		}
		floor := *note.MinTotalMinor
		out.MinTotalMinor, out.BoundsCommitment = &floor, note.boundsCommit
	}
	if note.Allowed != nil {
		for _, a := range note.Allowed {
			if cur.Allowed != nil && !slices.Contains(cur.Allowed, a) {
				more = true
			}
		}
		out.Allowed = slices.Clone(note.Allowed)
	}
	return out, more
}

type dealAct struct {
	Action       string `json:"action"`
	Description  string `json:"description,omitempty"`
	AmountMinor  *int64 `json:"amount_minor,omitempty"`
	Currency     string `json:"currency,omitempty"`
	Payee        string `json:"payee,omitempty"`
	Rail         string `json:"rail,omitempty"`
	Reference    string `json:"reference,omitempty"`
	AuthorizedBy string `json:"authorized_by,omitempty"`
	Unchecked    bool   `json:"unchecked"`
	Reason       string `json:"reason,omitempty"`
	Rule         string `json:"rule,omitempty"`
	// Direction is which way the amount moved: "out" (paid by the user) or
	// "in" (back to the user). Reverses is the sealed act whose money this
	// one returns. capsulectl sets both when the act is sealed
	// (actDirection), never from the agent's input. Older acts carry
	// neither; a pay among them moved money out.
	Direction string `json:"direction,omitempty"`
	Reverses  string `json:"reverses,omitempty"`
	// Accepted is, on a seller's commit, the sealed acceptance of the offer
	// it rests on; the record cites it (rel source).
	Accepted string `json:"accepted,omitempty"`
	// FeeMinor is, on a cancel only, a fee the cancellation costs, recorded
	// as its own amount: never the amount a spend cap evaluates (spend_minor
	// is 0 on every cancel).
	FeeMinor *int64 `json:"fee_minor,omitempty"`
}

// actDirection is which way an act's amount moved, and the act it reverses.
// A pay with an amount moved money out. A cancel with an amount returns
// money when it undoes a payment: the latest sealed pay, not already
// reversed, with the same amount and currency; then it moved money in and
// reverses that pay. Anything else states no direction.
func actDirection(events []sealedEvent, a dealAct, currency string) (string, string) {
	if a.AmountMinor == nil {
		return "", ""
	}
	cur := func(c string) string {
		if c == "" {
			return strings.ToUpper(currency)
		}
		return strings.ToUpper(c)
	}
	switch a.Action {
	case "pay":
		return "out", ""
	case "cancel":
		reversed := map[string]bool{}
		for _, se := range events {
			if se.Event.Act != nil && se.Event.Act.Reverses != "" {
				reversed[se.Event.Act.Reverses] = true
			}
		}
		for i := len(events) - 1; i >= 0; i-- {
			p := events[i].Event.Act
			if p == nil || p.Action != "pay" || p.AmountMinor == nil || reversed[events[i].CapsuleID] {
				continue
			}
			if *p.AmountMinor == *a.AmountMinor && cur(p.Currency) == cur(a.Currency) {
				return "in", events[i].CapsuleID
			}
		}
	}
	return "", ""
}

// actOut reports whether an act's amount was paid out by the user: its
// direction says so, or it is a pay sealed before acts carried a direction.
func actOut(a dealAct) bool {
	return a.Direction == "out" || a.Direction == "" && a.Action == "pay"
}

type dealCloseInput struct {
	Status    string     `json:"status"`
	Delivered *dealTerms `json:"delivered,omitempty"`
	Note      string     `json:"note,omitempty"`
}

type dealCloseResult struct {
	dealCloseInput
	Outcome          string           `json:"outcome"`
	Differences      []dealDifference `json:"differences"`
	UncheckedActions int              `json:"unchecked_actions"`
	// Carried are the cancel-by obligations still open when the deal was
	// closed with --carry-open-obligations: they stay open after the close.
	Carried []dealCarried `json:"carried,omitempty"`
}

// dealEvent is one step as the local store keeps it, with raw values. Exactly
// one body field is set, matching Kind. What is sealed is the x-deal-v0 record
// derived from it (deal_profile.go): fingerprints and commitments, never the
// raw values. Prev is the previous step's record digest. Nonces are the
// commitment nonces; they never leave the store.
type dealEvent struct {
	DealID   string           `json:"deal_id"`
	N        int64            `json:"n"`
	Kind     string           `json:"kind"`
	Prev     string           `json:"prev,omitempty"`
	At       string           `json:"at"`
	Open     *dealOpen        `json:"open,omitempty"`
	Intent   *dealIntent      `json:"intent,omitempty"`
	Message  *dealMessage     `json:"message,omitempty"`
	Claim    *dealClaim       `json:"claim,omitempty"`
	Evidence *dealEvidence    `json:"evidence,omitempty"`
	Change   *dealChange      `json:"change,omitempty"`
	Snapshot *dealSnapshot    `json:"snapshot,omitempty"`
	Check    *dealCheckResult `json:"check,omitempty"`
	Approval *dealApproval    `json:"approval,omitempty"`
	Act      *dealAct         `json:"act,omitempty"`
	Outcome  *dealCloseResult `json:"outcome,omitempty"`
	Close    *dealCloseResult `json:"close,omitempty"`
	// Disclosure is something the agent told someone about the user.
	Disclosure *dealDisclosure `json:"disclosure,omitempty"`
	// TaskAuthority is the user's task authority as opened (typed records):
	// sealed as its own task-authority/v0 step right after the baseline.
	TaskAuthority *dealIntent `json:"task_authority,omitempty"`
	// Platform is an observation of another platform's own approval interaction,
	// as observed (typed records). It never answers a check.
	Platform *dealPlatformApproval `json:"platform,omitempty"`
	// Acceptance is an observation of the counterparty accepting one exact
	// offer (typed records). It authorizes nothing by itself.
	Acceptance *dealAcceptance `json:"acceptance,omitempty"`
	// Confirms is set on a record sealed after the deal was closed: the
	// capsule id of that close. Its Capsule chains to the close with the
	// registered relation `confirms`, never `follows`, and its record
	// commits to the close record's digest.
	Confirms string            `json:"confirms,omitempty"`
	Nonces   map[string]string `json:"nonces,omitempty"`
	// Producer is the capsulectl build that sealed the step, kept with the
	// step so that a later build re-derives the same record.
	Producer *dealProducer `json:"producer,omitempty"`
	// TaxonomyVersion is the action-class taxonomy (dealTaxonomyVersion) the
	// step's record classes its action by, kept with the step like Producer:
	// a step sealed before records carried an action_class has none and
	// re-derives without one.
	TaxonomyVersion string `json:"taxonomy_version,omitempty"`
	// CommitAlg is the construction (dealCommitAlg) the step's record's
	// commitments use, kept with the step like Producer: a step sealed
	// before records declared it has none and re-derives without one.
	CommitAlg string `json:"commit_alg,omitempty"`
	// RuleInputs (dealRuleInputsVersion) marks a step whose record carries
	// the scalars a rules checker reads (deal_rule_inputs.go), kept with the
	// step like Producer: a step sealed before has none and re-derives
	// without them.
	RuleInputs string `json:"rule_inputs,omitempty"`
	// ClaimCommit (dealClaimCommitVersion) marks a step whose claims are
	// sealed as commitments with a visible source_kind (deal_claims.go),
	// kept with the step like Producer: a step sealed before has none and
	// re-derives its claims in clear, unchanged.
	ClaimCommit string `json:"claim_commit,omitempty"`
}

// dealPlatformApproval is an observation of another platform's own approval
// interaction for a checked action, as the caller saw it: the platform and
// mechanism as the caller names them, the text the platform displayed and
// the user's text it returned (each committed, never stored in the record),
// when it was observed, and the amount the displayed text stated. It records
// that the interaction happened with these bytes; it says nothing about
// whether the platform authorized anything or any rule is satisfied.
type dealPlatformApproval struct {
	Check         string `json:"check"`
	Platform      string `json:"platform"`
	Mechanism     string `json:"mechanism"`
	DisplayedText string `json:"displayed_text"`
	UserText      string `json:"user_text,omitempty"`
	ObservedAt    string `json:"observed_at"`
	AmountMinor   *int64 `json:"amount_minor,omitempty"`
	Currency      string `json:"currency,omitempty"`
}

// dealAcceptance is the counterparty accepting an offer, as the agent
// observed it: which exact offer (the capsule id of its checked snapshot, the
// proposed action), on which channel, when, and the counterparty's words
// (committed, never stored in the record). It binds the acceptance to that
// offer's digest; an offer made after it, or a change of details, leaves
// that acceptance behind.
type dealAcceptance struct {
	Offer      string `json:"offer"`
	Channel    string `json:"channel,omitempty"`
	ObservedAt string `json:"observed_at"`
	Words      string `json:"words"`
}

// dealProducer names the build that sealed a step. A development build says
// so: its version is cliVersion's default ("0.1.0-dev") and its commit
// "unknown" unless the build sets them.
type dealProducer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// dealUnrecordedProducer names the build of a step sealed before builds
// were recorded.
const dealUnrecordedProducer = "an earlier capsulectl that did not record its version"

// dealProducers lists, in order of first use, the builds that sealed a
// deal's steps: usually one, more when the deal spanned an upgrade.
func dealProducers(events []sealedEvent) []string {
	var out []string
	for _, se := range events {
		name := dealUnrecordedProducer
		if se.Event.Producer != nil {
			name = se.Event.Producer.String()
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func currentProducer() *dealProducer {
	return &dealProducer{Name: "capsulectl", Version: cliVersion, Commit: cliCommit}
}

func (p *dealProducer) String() string {
	return p.Name + " " + p.Version + " (" + p.Commit + ")"
}

// sealedEvent is an event as read back from the store: the capsule that holds
// it and its position in the log.
type sealedEvent struct {
	CapsuleID string
	Digest    string // the x-deal-v0 record digest
	Sequence  uint64
	Event     dealEvent
}

func (o dealOpen) validate() error {
	if _, ok := dealPointsOfNoReturn[o.Type]; !ok {
		return inputError("open.type must be purchase, rental, booking or service")
	}
	if strings.TrimSpace(o.Intent.Verbatim) == "" {
		return inputError("intent.verbatim (the user's own words) is required")
	}
	if err := validPartyRole(o.Intent.PartyRole); err != nil {
		return err
	}
	if err := o.Intent.validBounds(); err != nil {
		return err
	}
	if o.Who == (dealWho{}) {
		return inputError("who needs at least one identifying field")
	}
	for _, a := range o.Intent.Allowed {
		if !slices.Contains(dealPointsOfNoReturn[o.Type], a) {
			return inputError("intent.allowed names an action that is not a point of no return for this deal type: " + a)
		}
	}
	for _, c := range o.Claims {
		if err := c.validate(); err != nil {
			return err
		}
	}
	if o.Recourse.Rail == "" || o.Recourse.Refundable == nil {
		return inputError("recourse needs the payment rail and whether it is refundable")
	}
	return nil
}

// commercialBoundsKind names the private document a floor lives in: sealed
// records carry only its salted commitment (bounds_commitment).
const commercialBoundsKind = "commercial-bounds/v0"

// commercialBoundsText is the text a bounds_commitment binds: the JCS bytes
// of the commercial-bounds/v0 document holding the floor.
func commercialBoundsText(min int64) string {
	doc, err := bundleJSON(map[string]interface{}{"type": commercialBoundsKind, "min_total_minor": min})
	if err != nil {
		return ""
	}
	b, err := canonical.JCS(doc)
	if err != nil {
		return ""
	}
	return string(b)
}

// withBoundsCommit is an intent with the commitment its step sealed to its
// floor, under that step's own "bounds" nonce.
func withBoundsCommit(i dealIntent, nonces map[string]string) dealIntent {
	if i.MinTotalMinor != nil && nonces["bounds"] != "" {
		i.boundsCommit, _ = commitText(nonces["bounds"], commercialBoundsText(*i.MinTotalMinor))
	}
	return i
}

func (i dealIntent) validBounds() error {
	switch {
	case i.MinTotalMinor != nil && *i.MinTotalMinor < 0:
		return inputError("intent.min_total_minor must not be negative")
	case i.MinTotalMinor != nil && i.MaxTotalMinor != nil && *i.MinTotalMinor > *i.MaxTotalMinor:
		return inputError("intent.min_total_minor must not be more than max_total_minor")
	}
	return nil
}

// Party roles: the side of the deal the user is on (dealIntent.PartyRole).
const (
	dealRoleBuyer  = "buyer"
	dealRoleSeller = "seller"
)

func validPartyRole(role string) error {
	if role != "" && role != dealRoleBuyer && role != dealRoleSeller {
		return inputError("intent.party_role must be buyer or seller")
	}
	return nil
}

// dealRole is the side of the deal the user is on, from the deal's opening:
// buyer when the deal records none.
func dealRole(events []sealedEvent) string {
	if len(events) > 0 && events[0].Event.Open != nil && events[0].Event.Open.Intent.PartyRole != "" {
		return events[0].Event.Open.Intent.PartyRole
	}
	return dealRoleBuyer
}

func (c dealClaim) validate() error {
	if strings.TrimSpace(c.Text) == "" || (strings.TrimSpace(c.Source) == "" && c.SourceKind == "") {
		return inputError("every claim needs its text, and its source_kind or source")
	}
	return nil
}

// dealState folds a deal's sealed steps into what a check compares.
type dealState struct {
	// materiality is the predicate the check evaluates on the agent's
	// picks; nil when none is configured (every pick is material).
	materiality *materialityPredicate
	open        dealOpen
	intent      dealIntent // the latest: the baseline's, or a later intent step
	agreed      dealTerms
	who         dealWho
	whoSource   map[string]string
	terms       dealTerms
	recourse    dealRecourse
	// agreedRecourse is the way back as agreed: the baseline's, updated only
	// by an approved check.
	agreedRecourse dealRecourse
	claims         []dealClaim
	messages       []dealMessage
	// memory is the cross-deal counterparty memory, when the check has one.
	memory *dealCounterpartyMemory
}

func foldDeal(events []sealedEvent) (dealState, error) {
	if len(events) == 0 || events[0].Event.Kind != "open" || events[0].Event.Open == nil {
		return dealState{}, inputError("this deal has no sealed opening step to check against: start a new deal with `deal open`")
	}
	o := *events[0].Event.Open
	o.Intent = withBoundsCommit(o.Intent, events[0].Event.Nonces)
	s := dealState{open: o, intent: o.Intent, agreed: o.Terms, who: o.Who, terms: o.Terms, recourse: o.Recourse, agreedRecourse: o.Recourse, claims: slices.Clone(o.Claims), whoSource: map[string]string{}}
	var lastSnapshot *dealSnapshot
	for _, se := range events[1:] {
		e := se.Event
		switch e.Kind {
		case "intent":
			s.intent = laterIntent(s.intent, withBoundsCommit(*e.Intent, se.Event.Nonces))
		case "message":
			s.messages = append(s.messages, *e.Message)
			if e.Message.Who != nil && e.Message.From == "counterparty" {
				s.overlayWho(*e.Message.Who, "a message")
			}
		case "claim":
			s.claims = append(s.claims, *e.Claim)
		case "evidence":
			if e.Evidence.Verified {
				for i := range s.claims {
					if strings.EqualFold(s.claims[i].Text, e.Evidence.About) {
						s.claims[i].Verified = true
					}
				}
			}
			if e.Evidence.Who != nil {
				s.overlayWho(*e.Evidence.Who, e.Evidence.Source)
			}
		case "change":
			if e.Change.Who != nil {
				s.overlayWho(*e.Change.Who, e.Change.Source)
			}
			if e.Change.Terms != nil {
				s.terms = overlayTerms(s.terms, *e.Change.Terms)
			}
			if e.Change.Recourse != nil {
				s.recourse = overlayRecourse(s.recourse, *e.Change.Recourse)
			}
		case "snapshot":
			lastSnapshot = e.Snapshot
		case "approval":
			// The user's confirmation of proposed limits is the new version of
			// them; the note it answers stays sealed as the proposal.
			if e.Approval.Limits != nil {
				if e.Approval.Proceed && e.Approval.Reason == "" {
					s.intent.MaxTotalMinor = e.Approval.Limits.New.MaxTotalMinor
					s.intent.MinTotalMinor = e.Approval.Limits.New.MinTotalMinor
					s.intent.boundsCommit = e.Approval.Limits.New.BoundsCommitment
					s.intent.Allowed = slices.Clone(e.Approval.Limits.New.Allowed)
				}
				continue
			}
			// An approved proceed adopts the checked terms as the new agreement.
			// Who is never adopted: it is always compared with first contact.
			if e.Approval.Proceed && e.Approval.Reason == "" && lastSnapshot != nil {
				if lastSnapshot.Terms != nil {
					s.agreed = overlayTerms(s.agreed, *lastSnapshot.Terms)
				}
				s.agreed = overlayTerms(s.agreed, s.terms)
				s.agreedRecourse = overlayRecourse(s.agreedRecourse, s.recourse)
				if lastSnapshot.Recourse != nil {
					s.agreedRecourse = overlayRecourse(s.agreedRecourse, *lastSnapshot.Recourse)
				}
			}
		}
	}
	return s, nil
}

// laterIntent folds a later intent note over the one in force. The note is
// written by the agent, so it may narrow what the user set but never widen
// it: the limit is the lower of the two (a note without one keeps the one in
// force), and the allowed actions are those both allow (a note without a list
// keeps the list in force). What the user picked (verbatim, asked) is the
// note's. A note that asks for more is a proposal: it applies only once the
// user confirms it (a confirm_limits approval, a new version of the limits),
// or for one step when the user approves that step's paused check.
func laterIntent(cur, next dealIntent) dealIntent {
	out := next
	// The role is the deal's, set when it opened: a note never changes it
	// (one that names another role is refused before it is sealed).
	out.PartyRole = cur.PartyRole
	// A note that names no terms ("cancel this ticket") changes nothing the
	// user asked to buy: the terms in force stay.
	if reflect.DeepEqual(next.Asked, dealTerms{}) {
		out.Asked = cur.Asked
	}
	switch {
	case cur.MaxTotalMinor == nil:
	case next.MaxTotalMinor == nil || *next.MaxTotalMinor > *cur.MaxTotalMinor:
		limit := *cur.MaxTotalMinor
		out.MaxTotalMinor = &limit
	}
	// A higher floor narrows and applies; a lower one, or none, keeps the
	// floor in force (lowering it is a proposal the user must confirm).
	switch {
	case cur.MinTotalMinor == nil:
	case next.MinTotalMinor == nil || *next.MinTotalMinor < *cur.MinTotalMinor:
		floor := *cur.MinTotalMinor
		out.MinTotalMinor, out.boundsCommit = &floor, cur.boundsCommit
	}
	switch {
	case cur.Allowed == nil:
	case next.Allowed == nil:
		out.Allowed = slices.Clone(cur.Allowed)
	default:
		out.Allowed = []string{}
		for _, a := range next.Allowed {
			if slices.Contains(cur.Allowed, a) {
				out.Allowed = append(out.Allowed, a)
			}
		}
	}
	return out
}

func (s *dealState) overlayWho(w dealWho, source string) {
	for _, f := range whoFields(w) {
		if f.value != "" {
			s.whoSource[f.key] = source
		}
	}
	s.who = overlayWho(s.who, w)
}

type whoField struct{ key, label, value string }

// whoFields lists the identifying fields in card order. domain_age_days is
// evidence about the counterparty, not an identity field, so it is absent.
func whoFields(w dealWho) []whoField {
	return []whoField{
		{"payee", "Payee", w.Payee},
		{"name", "Name", w.Name},
		{"domain", "Website", w.Domain},
		{"phone", "Phone", w.Phone},
		{"email", "Email", w.Email},
		{"relay_address", "Reply address", w.RelayAddress},
		{"profile_id", "Profile", w.ProfileID},
	}
}

func overlayWho(base, top dealWho) dealWho {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&base.Name, top.Name)
	set(&base.Domain, top.Domain)
	set(&base.Phone, top.Phone)
	set(&base.Email, top.Email)
	set(&base.Payee, top.Payee)
	set(&base.RelayAddress, top.RelayAddress)
	set(&base.ProfileID, top.ProfileID)
	if top.DomainAgeDays != nil {
		base.DomainAgeDays = top.DomainAgeDays
	}
	return base
}

func overlayTerms(base, top dealTerms) dealTerms {
	if top.Item != "" {
		base.Item = top.Item
	}
	if top.Quantity != 0 {
		base.Quantity = top.Quantity
	}
	if top.PriceMinor != nil {
		base.PriceMinor = top.PriceMinor
	}
	if top.DepositMinor != nil {
		base.DepositMinor = top.DepositMinor
	}
	if top.Currency != "" {
		base.Currency = top.Currency
	}
	if top.When != "" {
		base.When = top.When
	}
	if top.Place != "" {
		base.Place = top.Place
	}
	if len(top.Conditions) > 0 {
		merged := make(map[string]string, len(base.Conditions)+len(top.Conditions))
		for k, v := range base.Conditions {
			merged[k] = v
		}
		for k, v := range top.Conditions {
			merged[k] = v
		}
		base.Conditions = merged
	}
	return base
}

func overlayRecourse(base, top dealRecourse) dealRecourse {
	if top.Rail != "" {
		base.Rail = top.Rail
	}
	if top.Refundable != nil {
		base.Refundable = top.Refundable
	}
	return base
}

type termField struct{ key, label, a, b string }

// termFieldDiffs compares every field set in ref with the same field in got.
// A field got leaves empty is not a difference: it is not being changed.
func termFieldDiffs(ref, got dealTerms) []termField {
	currency := got.Currency
	if currency == "" {
		currency = ref.Currency
	}
	var out []termField
	add := func(key, label, a, b string) {
		if a != "" && b != "" && !strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
			out = append(out, termField{key, label, a, b})
		}
	}
	money := func(v *int64) string {
		if v == nil {
			return ""
		}
		return formatMoney(*v, currency)
	}
	qty := func(v int64) string {
		if v == 0 {
			return ""
		}
		return fmt.Sprint(v)
	}
	add("item", "Item", ref.Item, got.Item)
	add("quantity", "Quantity", qty(ref.Quantity), qty(got.Quantity))
	add("price", "Price", money(ref.PriceMinor), money(got.PriceMinor))
	add("deposit", "Deposit", money(ref.DepositMinor), money(got.DepositMinor))
	add("currency", "Currency", ref.Currency, got.Currency)
	add("when", "Dates", ref.When, got.When)
	add("place", "Place", ref.Place, got.Place)
	keys := make([]string, 0, len(ref.Conditions))
	for k := range ref.Conditions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("conditions."+k, k, ref.Conditions[k], got.Conditions[k])
	}
	return out
}

var railNames = map[string]string{
	"card": "card", "credit_card": "credit card", "debit_card": "debit card", "paypal": "PayPal",
	"zelle": "Zelle", "wire": "wire transfer", "gift_card": "gift card", "crypto": "crypto",
	"cash_app": "Cash App", "venmo": "Venmo", "western_union": "Western Union", "moneygram": "MoneyGram",
	"cash": "cash", "bank_transfer": "bank transfer",
}

// railsWithoutRecourse are payment rails that give the payer no dispute or
// chargeback: money sent is gone. Safety rule: deposit by Zelle, wire, gift
// card or crypto (and their equivalents) pauses.
var railsWithoutRecourse = map[string]bool{
	"zelle": true, "wire": true, "gift_card": true, "crypto": true, "cash_app": true,
	"venmo": true, "western_union": true, "moneygram": true, "cash": true, "bank_transfer": true,
}

func normRail(r string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(r)), " ", "_")
}

func railName(r string) string {
	if n, ok := railNames[normRail(r)]; ok {
		return n
	}
	return r
}

var (
	codeRequest = regexp.MustCompile(`(?i)(verification|security|confirmation|one[- ]time|2fa|6[- ]digit|login)\s+code|code (we|i) (just )?(sent|texted)|send (me )?the code`)
	offPlatform = regexp.MustCompile(`(?i)\b(whatsapp|telegram|signal|wechat|text me|email me|call me at|message me at|contact me (at|on))\b`)
)

var currencySymbols = map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "CAD": "CA$", "AUD": "A$", "JPY": "¥"}

func formatMoney(minor int64, currency string) string {
	cur := strings.ToUpper(currency)
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	if cur == "JPY" {
		return fmt.Sprintf("%s¥%d", sign, minor)
	}
	amount := fmt.Sprintf("%d.%02d", minor/100, minor%100)
	if sym, ok := currencySymbols[cur]; ok {
		return sign + sym + amount
	}
	if cur == "" {
		return sign + amount
	}
	return sign + amount + " " + cur
}

func ageText(days int64) string {
	switch {
	case days < 14:
		return fmt.Sprintf("%d days ago", days)
	case days < 60:
		return fmt.Sprintf("%d weeks ago", days/7)
	default:
		return fmt.Sprintf("%d months ago", days/30)
	}
}

const recentDomainDays = 90

var proceedPlainLabels = map[string]string{
	"pay": "Pay", "commit": "Confirm", "sign": "Sign", "cancel": "Cancel",
	"share_contact": "Share", "share_credentials": "Share",
}

var proceedLabels = map[string]string{
	"pay": "Pay anyway", "commit": "Confirm anyway", "sign": "Sign anyway", "cancel": "Cancel anyway",
	"share_contact": "Share anyway", "share_credentials": "Share anyway",
}

var actionNames = map[string]string{
	"pay": "paying", "commit": "confirming a commitment", "sign": "signing", "cancel": "cancelling",
	"share_contact": "sharing your contact details", "share_credentials": "sharing a login or code",
	"offer": "making an offer",
}

// evaluateDeal answers the four questions for snap against the sealed state:
// Asked? Same who? Same terms? Checked claims, and a way back? Differences
// from the first three and from the safety rules pause; unverified claims and
// weak recourse are listed on a pause card but never pause on their own.
func evaluateDeal(s dealState, snap dealSnapshot) dealCheckResult {
	r := dealCheckResult{Action: snap.Action, Differences: []dealDifference{}, Unverified: []string{}, Notes: []string{}}
	who := s.who
	if snap.Who != nil {
		who = overlayWho(who, *snap.Who)
	}
	proposed := s.terms
	if snap.Terms != nil {
		proposed = overlayTerms(proposed, *snap.Terms)
	}
	recourse := s.recourse
	if snap.Recourse != nil {
		recourse = overlayRecourse(recourse, *snap.Recourse)
	}
	rail := normRail(recourse.Rail)
	add := func(q, rule, field, text string) {
		r.Differences = append(r.Differences, dealDifference{Question: q, Rule: rule, Field: field, Text: text})
	}

	// 1. Asked?
	intent := s.intent
	r.Asked, r.Picked = attributeProvenance(intent.Asked, proposed)
	// The predicate deciding these pauses is the one pinned when the deal
	// opened, or the check says it is not: a re-pin mid-deal is never
	// silent.
	if opened, now := s.open.Materiality.Digest, s.materiality.ref(); opened != "" && opened != now.Digest {
		add("safety", "materiality_changed", "", fmt.Sprintf("The rule for which of the agent's picks need your answer changed since this deal opened (%s → %s)", materialityLabel(s.open.Materiality), materialityLabel(now)))
	}
	// A material attribute the agent picked needs the user's nod: it pauses
	// the check, so "proceed" is never true on an empty card. Which picks are
	// material is the materiality predicate's to say, not this code's; with
	// none configured, every pick is (fail safe).
	for _, a := range r.Picked {
		if s.materiality.material(a) {
			add("asked", "agent_picked", a.Field, fmt.Sprintf("I picked %s %s; you didn't choose it", a.Label, a.Value))
		}
	}
	// A share that names what it gives is judged by its recipient (below),
	// not by whether the user named the action: giving a merchant you have
	// given your address before needs no new nod, and giving a stranger it
	// always does.
	byRecipient := len(snap.Disclosing) > 0 && s.memory != nil
	if intent.Allowed != nil && !slices.Contains(intent.Allowed, snap.Action) && !byRecipient {
		add("asked", "not_asked", "action", "You didn't ask for this: "+actionNames[snap.Action])
	}
	askedFields := map[string]bool{}
	for _, d := range termFieldDiffs(intent.Asked, proposed) {
		askedFields[d.key] = true
		add("asked", "not_asked", d.key, fmt.Sprintf("Not what you asked: %s %s → %s", strings.ToLower(d.label), d.a, d.b))
	}
	if intent.MaxTotalMinor != nil {
		total := proposed.PriceMinor
		if total == nil {
			total = snap.AmountMinor
		}
		// A limit binds the most that may be taken: the authorized maximum,
		// when it is more than the price or the expected charge.
		field := "price"
		if m := snap.AuthorizedMaxMinor; m != nil && (total == nil || *m > *total) {
			total, field = m, "authorized_max_minor"
		}
		if total != nil && *total > *intent.MaxTotalMinor {
			limit, amount := formatMoney(*intent.MaxTotalMinor, proposed.Currency), formatMoney(*total, proposed.Currency)
			text := fmt.Sprintf("Over your limit of %s (%s)", limit, amount)
			if field == "authorized_max_minor" {
				expected := ""
				if snap.AmountMinor != nil {
					expected = "; the expected charge is " + formatMoney(*snap.AmountMinor, proposed.Currency)
				}
				text = fmt.Sprintf("Over your limit of %s: up to %s may be taken (the authorized maximum%s)", limit, amount, expected)
			}
			// The user's own words named this very purchase, the item and its
			// price: the rule still asks, and says why this is an exception to
			// it rather than skipping the ask.
			if a := intent.Asked; a.Item != "" && a.PriceMinor != nil && *a.PriceMinor == *total &&
				strings.EqualFold(strings.TrimSpace(a.Item), strings.TrimSpace(proposed.Item)) {
				text = fmt.Sprintf("Your rules normally ask above %s. You asked for this %s purchase specifically. Approve this exception?", limit, amount)
			}
			add("asked", "over_limit", field, text)
			askedFields["price"] = true
		}
	}
	// The floor: the lowest total the user will take. Below it, ask. Only
	// the card shows the amount; records seal the rule (under_floor).
	if intent.MinTotalMinor != nil {
		total := proposed.PriceMinor
		if total == nil {
			total = snap.AmountMinor
		}
		if total != nil && *total < *intent.MinTotalMinor {
			add("asked", "under_floor", "price", fmt.Sprintf("Below the lowest price you will take, %s (%s)",
				formatMoney(*intent.MinTotalMinor, proposed.Currency), formatMoney(*total, proposed.Currency)))
			askedFields["price"] = true
		}
	}

	// 2. Same who? Always against first contact, never against a later change.
	first := s.open.Who
	if first.Payee == "" {
		first.Payee = first.Name
	}
	for i, f := range whoFields(first) {
		now := whoFields(who)[i].value
		if f.value == "" || now == "" || sameID(f.key, f.value, now) {
			continue
		}
		detail := f.value + " → " + now
		if f.key == "payee" && rail != "" {
			detail += ", " + railName(recourse.Rail)
		}
		add("who", "payee_or_contact_changed", f.key, fmt.Sprintf("%s changed since first contact (%s)", f.label, detail))
	}

	// Who is this, to the user? The first dealing with a counterparty is a
	// line on the card; the first time a class of the user's data goes to
	// them is a pause, naming them and the class; a repeat is a note.
	if s.memory != nil {
		receiver := who
		if snap.DisclosingTo == "other" && snap.Recipient != nil {
			receiver = *snap.Recipient
		}
		keys := counterpartyKeys(receiver, s.open.Channel)
		r.Recipient = &dealRecipient{Name: recipientName(receiver), FirstTime: s.memory.firstTime(keys)}
		r.Recipient.NewProfile = s.memory.newProfile() && !s.memory.newProfileNoted
		for _, class := range sortedClasses(snap.Disclosing) {
			if s.memory.toldBefore(keys, class) != "" {
				r.Recipient.Repeat = append(r.Recipient.Repeat, class)
				continue
			}
			r.Recipient.First = append(r.Recipient.First, class)
			add("who", "first_disclosure", class, fmt.Sprintf("First time telling %s your %s", r.Recipient.Name, classWord(class)))
		}
	}

	// 3. Same terms? Against what was agreed and approved.
	for _, d := range termFieldDiffs(s.agreed, proposed) {
		if askedFields[d.key] {
			continue
		}
		add("terms", "terms_changed", d.key, fmt.Sprintf("%s changed since it was agreed (%s → %s)", d.label, d.a, d.b))
	}

	// Same way back? Any change of rail or refundability from what was
	// agreed is a difference, for every action. Rail and refundability read
	// as one line on the card when both changed.
	agreedRail := normRail(s.agreedRecourse.Rail)
	railChanged := agreedRail != "" && rail != "" && agreedRail != rail
	refundChanged := s.agreedRecourse.Refundable != nil && recourse.Refundable != nil && *s.agreedRecourse.Refundable != *recourse.Refundable
	refundWord := func(r bool) string {
		if r {
			return "refundable"
		}
		return "not refundable"
	}
	if railChanged {
		text := "Payment changed since it was agreed (" + railName(s.agreedRecourse.Rail) + " → " + railName(recourse.Rail)
		if refundChanged {
			text += ", " + refundWord(*recourse.Refundable)
		}
		add("recourse", "recourse_changed", "rail", text+")")
	}
	if refundChanged {
		text := ""
		if !railChanged && !*recourse.Refundable {
			text = "No longer refundable (agreed as refundable)"
		} else if !railChanged {
			text = "Now refundable (agreed as not refundable)"
		}
		add("recourse", "recourse_changed", "refundable", text)
	}

	// Safety rules.
	if snap.Action == "pay" && railsWithoutRecourse[rail] {
		add("recourse", "irreversible_rail", "rail", railName(recourse.Rail)+" = no card protection")
	}
	if snap.Action == "pay" && s.open.Type == "purchase" && snap.SeenItem != nil && !*snap.SeenItem {
		add("safety", "pay_before_seeing", "", "Paying before you've seen the item")
	}
	if snap.Action == "share_credentials" {
		add("safety", "credentials_requested", "", "Sharing a login or code gives away access")
	}
	var askedCode, movedOff bool
	for _, m := range s.messages {
		if m.From != "counterparty" {
			continue
		}
		askedCode = askedCode || codeRequest.MatchString(m.Text)
		movedOff = movedOff || offPlatform.MatchString(m.Text) || channelHop(s.open.Channel, m.Channel)
	}
	if askedCode {
		add("safety", "verification_code_request", "", "They asked for a verification code — never share it")
	}
	if movedOff {
		add("safety", "off_platform_early", "", "They asked to move off the platform")
	}
	if who.DomainAgeDays != nil && *who.DomainAgeDays < recentDomainDays {
		add("safety", "domain_recent", "domain", "Site registered "+ageText(*who.DomainAgeDays))
	}

	// 4. Checked claims, and a way back?
	for _, c := range s.claims {
		if !c.Verified {
			r.Unverified = append(r.Unverified, c.Text)
		}
	}
	if recourse.Refundable != nil && !*recourse.Refundable && !refundChanged {
		r.Notes = append(r.Notes, "not refundable")
	}
	settleCheck(&r, s)
	return r
}

// settleCheck sets the verdict and the one-tap options from the differences.
// It is re-run after a remote checker adds differences.
func settleCheck(r *dealCheckResult, s dealState) {
	if len(r.Differences) == 0 {
		r.Verdict = "pass"
		r.Options = []dealOption{}
		return
	}
	r.Options = []dealOption{{ID: "hold", Label: "Hold"}}
	// A deny from the user's rules is a deny: no way to proceed.
	if slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Rule == "rules_deny" }) {
		r.Verdict = "deny"
		return
	}
	r.Verdict = "pause"
	if s.open.Who.Phone != "" && slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Question == "who" }) {
		r.Options = append(r.Options, dealOption{ID: "verify_contact", Label: "Call the number I found (" + s.open.Who.Phone + ")"})
	}
	// "Pay anyway" overrides a finding. A check paused only because the
	// rules could not be checked has none to override: "Pay".
	label := proceedLabels[r.Action]
	if !slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Rule != "rules_not_checked" }) {
		label = proceedPlainLabels[r.Action]
	}
	r.Options = append(r.Options, dealOption{ID: "proceed", Label: label})
}

// renderCard writes the difference card: the differences, not a summary, then
// what is unverified, then the one-tap options. A passing check has no card.
func renderCard(r dealCheckResult, demo bool) string {
	if r.Verdict == "pass" {
		return ""
	}
	parts := make([]string, 0, len(r.Differences)+len(r.Notes)+2)
	if r.Recipient != nil && r.Recipient.NewProfile {
		parts = append(parts, newProfileLine)
	}
	if r.Recipient != nil && r.Recipient.FirstTime {
		parts = append(parts, "First time dealing with "+r.Recipient.Name)
	}
	for _, d := range r.Differences {
		if d.Text != "" { // a difference folded into another line
			parts = append(parts, d.Text)
		}
	}
	parts = append(parts, r.Notes...)
	// The agent's other picks: the ones the check did not pause on.
	paused := map[string]bool{}
	for _, d := range r.Differences {
		if d.Rule == "agent_picked" {
			paused[d.Field] = true
		}
	}
	var minor []dealAttribute
	for _, a := range r.Picked {
		if !paused[a.Field] {
			minor = append(minor, a)
		}
	}
	if len(minor) > 0 {
		parts = append(parts, "picked by the agent, not by you: "+attributeText(minor))
	}
	if len(r.Unverified) > 0 {
		parts = append(parts, "unverified: "+strings.Join(r.Unverified, ", "))
	}
	opts := make([]string, len(r.Options))
	for i, o := range r.Options {
		opts[i] = "[" + o.Label + "]"
	}
	card := "⚠️ " + strings.Join(parts, " · ") + " · " + strings.Join(opts, " ")
	if demo {
		card = "DEMO · " + card
	}
	return card
}

// authorizeAct finds the sealed approval that authorizes an action: the
// latest check (verdict) of the same action with no detail change sealed
// after it, answered by a proceed approval (the user's, or standing intent on
// a pass) that has not already authorized an earlier action, and whose checked
// amount, rail and payee match what was done. Anything else is an unchecked
// action: it is sealed anyway, as an outcome, and shown in the report. rule is
// the token the sealed outcome carries.
// In typed records (the check contract) the rules are stricter: a DO
// evaluation authorizes the step on the task authority alone (no approval is
// sealed), an ASK is answered only by the user's own approval given on the
// card shown, a platform's approval never answers it, and an evaluation
// covers the step only until its valid_until (now is when the step is being
// sealed) and only under the terms and refund terms it was made under.
func authorizeAct(events []sealedEvent, act dealAct, now time.Time) (approval, reason, rule string) {
	typed := dealRecordSet(dealEvent{}, events) == recordsTyped
	if typed && act.Action == "commit" && dealRole(events) == dealRoleSeller {
		if _, reason, rule := offerAccepted(events); rule != "" {
			return "", reason, rule
		}
	}
	// One approval (or, typed, one DO evaluation) covers at most one step:
	// an action or a disclosure.
	used := map[string]bool{}
	for _, se := range events {
		if se.Event.Kind == "act" && !se.Event.Act.Unchecked {
			used[se.Event.Act.AuthorizedBy] = true
		}
		if d := se.Event.Disclosure; d != nil && d.AuthorizedBy != "" {
			used[d.AuthorizedBy] = true
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i].Event
		if changesDetails(e) {
			return "", "details changed after the last check", "changed_after_check"
		}
		if e.Kind != "check" || e.Check.Action != act.Action {
			continue
		}
		verdict := events[i]
		if typed && verdict.Event.Check.ValidUntil != "" {
			if until, err := time.Parse(time.RFC3339, verdict.Event.Check.ValidUntil); err == nil && now.After(until) {
				return "", "the check went stale at " + verdict.Event.Check.ValidUntil + ": check again before acting", "stale_check"
			}
		}
		if mismatch := actMismatch(events, verdict.Event.Check.Snapshot, act); mismatch != "" {
			return "", mismatch, "differs_from_check"
		}
		if typed {
			// Only a change step moves the terms in force, and
			// changed_after_check catches that first; this keeps the scope
			// explicit for any other path.
			atCheck, err1 := foldDeal(events[:i])
			current, err2 := foldDeal(events)
			if err1 == nil && err2 == nil {
				if mismatch := scopeMismatch(atCheck, current); mismatch != "" {
					return "", mismatch, "differs_from_check"
				}
			}
			if verdict.Event.Check.Verdict == "pass" {
				if used[verdict.CapsuleID] {
					return "", "that check already covered an earlier step", "approval_already_used"
				}
				return verdict.CapsuleID, "", ""
			}
		}
		for _, later := range events[i+1:] {
			a := later.Event.Approval
			if later.Event.Kind != "approval" || a.Check != verdict.CapsuleID {
				continue
			}
			if !a.Proceed {
				return "", "the check paused and the answer was " + a.Choice, "answer_was_not_proceed"
			}
			if used[later.CapsuleID] {
				return "", "that approval already covered an earlier step", "approval_already_used"
			}
			if typed && a.Approver != "user" {
				return "", "the card was answered, but the deal check asked, and an ask needs your own approval, in your words", "ask_needs_your_words"
			}
			return later.CapsuleID, "", ""
		}
		reason = "the check paused and there is no sealed approval"
		if typed && platformObserved(events[i+1:], verdict.CapsuleID) {
			reason += " of yours: a platform's approval does not answer the deal check"
		}
		return "", reason, "no_sealed_approval"
	}
	return "", "no check before this action", "no_check"
}

// offerAccepted is the sealed acceptance a seller's commit rests on, or why
// there is none:
// the latest offer (only it is live; each later one supersedes the one
// before) has a recorded acceptance, and no change of details came after
// that acceptance. A change leaves the acceptance behind: the offer is made
// again and accepted again.
func offerAccepted(events []sealedEvent) (acceptance, reason, rule string) {
	latest := ""
	for _, se := range events {
		if p := se.Event.Snapshot; se.Event.Kind == "snapshot" && p != nil && p.Action == offerAction {
			latest = se.CapsuleID
		}
	}
	if latest == "" {
		return "", "no offer is on record: a seller commits to an offer the other party accepted", "no_accepted_offer"
	}
	accepted := -1
	for i, se := range events {
		if a := se.Event.Acceptance; se.Event.Kind == "acceptance" && a != nil && a.Offer == latest {
			accepted = i
		}
	}
	if accepted < 0 {
		return "", "the latest offer has no recorded acceptance: an earlier offer's acceptance does not carry over", "offer_not_accepted"
	}
	for _, se := range events[accepted+1:] {
		if changesDetails(se.Event) {
			return "", "details changed after the other party accepted: make the offer again and record its acceptance", "changed_after_acceptance"
		}
	}
	return events[accepted].CapsuleID, "", ""
}

// actMismatch compares what was done with the snapshot that was checked.
func actMismatch(events []sealedEvent, snapshotID string, act dealAct) string {
	for _, se := range events {
		if se.CapsuleID != snapshotID || se.Event.Snapshot == nil {
			continue
		}
		snap := se.Event.Snapshot
		// What was taken is held to what may be taken: above the authorized
		// maximum it differs from the check; below it, though not the
		// estimate, is what a hold is for.
		if act.AmountMinor != nil && snap.AuthorizedMaxMinor != nil {
			if *act.AmountMinor > *snap.AuthorizedMaxMinor {
				return "the amount is more than the authorized maximum checked"
			}
		} else if act.AmountMinor != nil && snap.AmountMinor != nil && *act.AmountMinor != *snap.AmountMinor {
			return "the amount differs from the one checked"
		}
		if act.Payee != "" && snap.Who != nil && snap.Who.Payee != "" && !sameID("payee", act.Payee, snap.Who.Payee) {
			return "the payee differs from the one checked"
		}
		if act.Rail != "" && snap.Recourse != nil && snap.Recourse.Rail != "" && normRail(act.Rail) != normRail(snap.Recourse.Rail) {
			return "the payment method differs from the one checked"
		}
		return ""
	}
	return "the checked snapshot is missing"
}

// closeDeal compares delivered with agreed: completed, mismatch or open.
func closeDeal(s dealState, in dealCloseInput) dealCloseResult {
	r := dealCloseResult{dealCloseInput: in, Differences: []dealDifference{}}
	switch in.Status {
	case "pending", "":
		r.Outcome = "open"
		return r
	case "not_received":
		r.Outcome = "mismatch"
		r.Differences = append(r.Differences, dealDifference{Question: "delivered", Rule: "not_delivered", Text: "Nothing was delivered"})
		return r
	case "not_selected":
		// The other side was not chosen (one buyer of several, say): the
		// deal ends with nothing taken or delivered. Final, like completed.
		r.Outcome = "not_selected"
		return r
	}
	if in.Delivered != nil {
		for _, d := range termFieldDiffs(s.agreed, *in.Delivered) {
			r.Differences = append(r.Differences, dealDifference{Question: "delivered", Rule: "delivered_differs", Field: d.key, Text: fmt.Sprintf("%s: agreed %s, delivered %s", d.label, d.a, d.b)})
		}
	}
	r.Outcome = "completed"
	if len(r.Differences) > 0 {
		r.Outcome = "mismatch"
	}
	return r
}

// restatedIntents marks each intent note whose words are the ones already in
// force (the opening's, or the latest earlier note's): the user said it
// again, they did not change what they asked.
func restatedIntents(events []sealedEvent) map[string]bool {
	out := map[string]bool{}
	if len(events) == 0 || events[0].Event.Open == nil {
		return out
	}
	current := events[0].Event.Open.Intent.Verbatim
	for _, se := range events[1:] {
		if i := se.Event.Intent; i != nil {
			out[se.CapsuleID] = i.Verbatim == current
			current = i.Verbatim
		}
	}
	return out
}

// trailLineIn is trailLine for events[i], which a restated intent changes:
// the same words again are not a change of what was asked.
func trailLineIn(events []sealedEvent, restated map[string]bool, i int) string {
	if e := events[i].Event; e.Kind == "intent" && restated[events[i].CapsuleID] {
		return fmt.Sprintf("you said again what you asked, unchanged: %q", e.Intent.Verbatim)
	}
	if e := events[i].Event; e.Kind == "act" && !e.Act.Unchecked {
		if basis := typedActBasis(events[:i], e.Act.AuthorizedBy); basis != "" {
			return e.Act.Action + " done, as checked: " + basis
		}
	}
	return trailLine(events[i].Event)
}

// typedActBasis is, in a deal sealed in typed records, what an action went
// ahead on: the user's own approval, or what they had already asked for (a
// check that needed no approval). "" otherwise.
func typedActBasis(events []sealedEvent, authorizedBy string) string {
	if dealRecordSet(dealEvent{}, events) != recordsTyped {
		return ""
	}
	for _, se := range events {
		if se.CapsuleID != authorizedBy {
			continue
		}
		switch {
		case se.Event.Kind == "check":
			return "I went ahead based on what you had already asked me to do"
		case se.Event.Approval != nil && se.Event.Approval.Approver == "user":
			return "you approved this"
		}
	}
	return ""
}

// trailLine is the one-line, plain-words account of a step for the report.
func trailLine(e dealEvent) string {
	switch e.Kind {
	case "open":
		line := fmt.Sprintf("opened %s deal: %q", e.Open.Type, e.Open.Intent.Verbatim)
		if e.Open.Demo {
			line = "DEMO · " + line
		}
		return line
	case "intent":
		return fmt.Sprintf("you changed what you asked: %q", e.Intent.Verbatim)
	case "message":
		return "message from " + e.Message.From
	case "claim":
		return "claim recorded (" + e.Claim.Source + "): " + e.Claim.Text
	case "evidence":
		if e.Confirms != "" {
			c := e
			c.Confirms = ""
			return "after the close (confirms it): " + trailLine(c)
		}
		line := ""
		if m := e.Evidence.Email; m != nil {
			line = "merchant's email sealed; " + emailVerdictWords(m.DKIM)
		}
		if o := e.Evidence.Obligation; o != nil {
			if line != "" {
				line += "; "
			}
			what := "cancel-by date"
			if o.due() {
				what = "due date"
			}
			line += what + " recorded (" + e.Evidence.Source + "): " + o.sentence("")
		}
		if line != "" {
			return line
		}
		state := "unverified"
		if e.Evidence.Verified {
			state = "checked"
		}
		return "evidence (" + e.Evidence.Source + "), " + state + ": " + e.Evidence.About
	case "change":
		return "counterparty changed details (" + e.Change.Source + ")"
	case "snapshot":
		return "snapshot before " + actionNames[e.Snapshot.Action]
	case "task_authority":
		return "your request recorded as what the agent may do for you"
	case "platform_approval":
		return e.Platform.Platform + " separately asked you for approval (recorded as observed; it does not answer your rules)"
	case "acceptance":
		return "the other party accepted the offer (recorded as observed)"
	case "check":
		if e.Check.Verdict == "pass" {
			return "checked " + e.Check.Action + ": no differences"
		}
		return "checked " + e.Check.Action + ": flagged — " + e.Check.Card
	case "approval":
		if e.Approval.Approver == "standing_intent" {
			return "went ahead on what you already allowed"
		}
		if e.Approval.Approver == "agent_card" {
			return "the card was answered: " + e.Approval.Choice + " (you approved this card; no words of yours are on record)"
		}
		if l := e.Approval.Limits; l != nil && e.Approval.Proceed {
			return "you confirmed new limits: " + l.Previous.String() + " → " + l.New.String()
		}
		return "your answer: " + e.Approval.Choice
	case "act":
		if e.Act.Unchecked {
			return "⚠️ SKIPPED CHECK: " + e.Act.Action + " done without a passing check or your approval (" + e.Act.Reason + ")"
		}
		return e.Act.Action + " done, as checked"
	case "outcome":
		return "observed: " + e.Outcome.Status + " (" + e.Outcome.Outcome + ")"
	case "close":
		return "closed: " + e.Close.Outcome
	case "disclosure":
		line := "told " + e.Disclosure.recipientWord() + ": " + e.Disclosure.classList()
		if e.Disclosure.AuthorizedBy == "" {
			line = "⚠️ " + line + " without your approval (" + e.Disclosure.Reason + ")"
		}
		return line
	default:
		return e.Kind
	}
}

// deadlinePressure matches a counterparty pushing the user to act fast.
var deadlinePressure = regexp.MustCompile(`(?i)\b(today only|right now|asap|urgent(ly)?|last chance|expires?|deadline|within (the |an |\d+ )?(hour|hours|minutes)|before (it'?s|they'?re) gone|someone else (is|wants)|other buyers?|first come)\b`)

// dealReportItem is one line of the report. Steps are the capsule_ids it
// expands to, in log order.
type dealReportItem struct {
	Side  string   `json:"side,omitempty"`
	Kind  string   `json:"kind"`
	Text  string   `json:"text"`
	Steps []string `json:"steps"`
	// At is when the step this item reports was sealed, for the "did"
	// items: two cycles in the same words are told apart by their times.
	At string `json:"at,omitempty"`
	// Shared is the text for a counterparty's copy, when it differs: a
	// check's line without the user's and the agent's choices (the items go
	// only to an adjudicator).
	Shared string `json:"-"`
}

// dealReport is the three-part report: what the user asked, what the agent
// did, and the anomalies on either side.
type dealReport struct {
	// Instructions says which skill instructions were present when the
	// deal opened (see deal_skill.go), or that this was not recorded.
	Instructions string           `json:"instructions,omitempty"`
	Asked        string           `json:"asked"`
	AskedStep    string           `json:"asked_step"`
	Did          []dealReportItem `json:"did"`
	// Told is what the agent told whom about the user, in order: each
	// disclosure, its recipient, its time and the approval that covered it.
	Told      []dealToldItem   `json:"told"`
	Anomalies []dealReportItem `json:"anomalies"`
	// Merchant is each sealed merchant email set beside what was approved.
	Merchant []dealMerchantRow `json:"merchant"`
	// Money is what the sealed acts moved: paid out, returned, and the net.
	// Absent when no act carried an amount.
	Money *dealMoney `json:"money,omitempty"`
	// Authority is the AUTHORITY block of each action of a deal sealed in
	// typed records: every authority layer, in order, with its time.
	Authority []dealAuthorityBlock `json:"authority,omitempty"`
}

// dealMoney sums the sealed acts' amounts by direction, in minor units.
// Acts in another currency than the first are not summed and are counted
// in Other.
type dealMoney struct {
	Currency      string `json:"currency"`
	PaidMinor     int64  `json:"paid_minor"`
	ReturnedMinor int64  `json:"returned_minor"`
	NetMinor      int64  `json:"net_minor"`
	Other         int    `json:"other_currency_acts,omitempty"`
	Text          string `json:"text"`
}

func buildDealMoney(events []sealedEvent, currency string) *dealMoney {
	var m *dealMoney
	for _, se := range events {
		a := se.Event.Act
		if a == nil || a.AmountMinor == nil || !actOut(*a) && a.Direction != "in" {
			continue
		}
		cur := strings.ToUpper(a.Currency)
		if cur == "" {
			cur = strings.ToUpper(currency)
		}
		if m == nil {
			m = &dealMoney{Currency: cur}
		}
		switch {
		case cur != m.Currency:
			m.Other++
		case a.Direction == "in":
			m.ReturnedMinor += *a.AmountMinor
		default:
			m.PaidMinor += *a.AmountMinor
		}
	}
	if m == nil {
		return nil
	}
	m.NetMinor = m.PaidMinor - m.ReturnedMinor
	m.Text = fmt.Sprintf("Paid %s, returned %s: net %s.", formatMoney(m.PaidMinor, m.Currency), formatMoney(m.ReturnedMinor, m.Currency), formatMoney(m.NetMinor, m.Currency))
	return m
}

func actText(a dealAct, currency string) string {
	text := a.Action
	if a.AmountMinor != nil {
		cur := a.Currency
		if cur == "" {
			cur = currency
		}
		if a.Direction == "in" {
			// Money coming back reads as a return, never as a second charge.
			text = a.Action + ": " + formatMoney(*a.AmountMinor, cur) + " back to you"
			if a.Rail != "" {
				text += " on the " + railName(a.Rail)
			}
			if a.Reverses != "" {
				text += ", reversing the payment"
			}
			return text
		}
		text += " " + formatMoney(*a.AmountMinor, cur)
	}
	if a.Payee != "" {
		text += " to " + a.Payee
	}
	if a.Rail != "" {
		text += " by " + railName(a.Rail)
	}
	return text
}

// buildDealReport reads the sealed steps into the three parts. It invents
// nothing: every item points at the steps it was read from.
func buildDealReport(events []sealedEvent) dealReport {
	open := events[0].Event.Open
	openID := events[0].CapsuleID
	currency := open.Terms.Currency
	r := dealReport{Asked: open.Intent.Verbatim, AskedStep: openID, Did: []dealReportItem{}, Told: []dealToldItem{}, Anomalies: []dealReportItem{}, Money: buildDealMoney(events, currency)}
	checkItem := map[string]int{}
	// The checks the user answered with a sealed, valid proceed. A "you
	// didn't ask for this" on such a check's action was answered by the
	// user's own yes: it is not an anomaly.
	userApproved := map[string]bool{}
	for _, se := range events {
		if a := se.Event.Approval; a != nil && a.Approver == "user" && a.Proceed && a.Reason == "" && a.Limits == nil {
			userApproved[a.Check] = true
		}
	}
	agent := func(kind, text string, steps ...string) {
		r.Anomalies = append(r.Anomalies, dealReportItem{Side: "agent", Kind: kind, Text: text, Steps: steps})
	}
	counterparty := func(kind, text string, steps ...string) {
		r.Anomalies = append(r.Anomalies, dealReportItem{Side: "counterparty", Kind: kind, Text: text, Steps: steps})
	}
	// A paused check lists every cause of the pause in the card's own words
	// (its sealed differences), so the card and the anomalies cannot diverge.
	// An item read from an earlier step that the check states again folds
	// into the check's item: pending holds those items by the check rule that
	// covers them; folded marks them for removal.
	pending := map[string][]int{}
	folded := map[int]bool{}
	causes := map[string]int{}
	watch := func(rule, field string) {
		key := rule + "/" + field
		pending[key] = append(pending[key], len(r.Anomalies)-1)
	}
	first := open.Who
	if first.Payee == "" {
		first.Payee = first.Name
	}
	young := false
	changedWho := func(w dealWho, step string) {
		if !young && w.DomainAgeDays != nil && *w.DomainAgeDays < recentDomainDays {
			young = true
			counterparty("domain_recent", "Website registered "+ageText(*w.DomainAgeDays), step)
			watch("domain_recent", "domain")
		}
		now := whoFields(w)
		for i, f := range whoFields(first) {
			if f.value != "" && now[i].value != "" && !strings.EqualFold(strings.TrimSpace(f.value), strings.TrimSpace(now[i].value)) {
				counterparty("changed_identifier", fmt.Sprintf("%s changed: %s → %s", f.label, f.value, now[i].value), openID, step)
				watch("payee_or_contact_changed", f.key)
			}
		}
	}
	changedWho(open.Who, openID)
	// The user's own words so far: the opening, then each sealed intent.
	userWords := open.Intent.Verbatim
	for _, se := range events {
		e := se.Event
		if e.Intent != nil {
			userWords += " " + e.Intent.Verbatim
		}
		switch e.Kind {
		case "message":
			if e.Message.From != "counterparty" {
				continue
			}
			if e.Message.Who != nil {
				changedWho(*e.Message.Who, se.CapsuleID)
			}
			if channelHop(open.Channel, e.Message.Channel) {
				counterparty("channel_hop", fmt.Sprintf("Moved from %s to %s", open.Channel, e.Message.Channel), openID, se.CapsuleID)
				watch("off_platform_early", "")
			} else if offPlatform.MatchString(e.Message.Text) {
				counterparty("channel_hop", "Asked to move off the platform", se.CapsuleID)
				watch("off_platform_early", "")
			}
			if deadlinePressure.MatchString(e.Message.Text) {
				counterparty("deadline_pressure", "Pushed you to decide fast", se.CapsuleID)
			}
			if codeRequest.MatchString(e.Message.Text) {
				counterparty("code_request", "Asked for a verification code", se.CapsuleID)
				watch("verification_code_request", "")
			}
		case "change":
			if e.Change.Who != nil {
				changedWho(*e.Change.Who, se.CapsuleID)
			}
		case "evidence":
			if e.Evidence.Who != nil {
				changedWho(*e.Evidence.Who, se.CapsuleID)
			}
		case "check":
			verdict := map[string]string{"pass": "no differences", "pause": "flagged", "deny": "not allowed by your rules"}[e.Check.Verdict]
			checkItem[se.CapsuleID] = len(r.Did)
			text := fmt.Sprintf("Checked before %s: %s", actionNames[e.Check.Action], verdict)
			shared := text
			// What the user chose and what the agent chose for them, told
			// apart as the approval text tells them, so a choice the agent
			// made never reads as the user's.
			// The check sealed what it judged; the report only re-reads, in
			// the user's own words so far, what the agent picked: an
			// attribute those words name (dates, a place, a fare in the
			// opening sentence) is shown as the user's. The verdict, and any
			// pause for a material pick, stay as the check sealed them.
			yours, picked := slices.Clone(e.Check.Asked), []dealAttribute{}
			for _, a := range e.Check.Picked {
				if namedInWords(a.Value, userWords) {
					yours = append(yours, a)
				} else {
					picked = append(picked, a)
				}
			}
			var who []string
			if len(yours) > 0 {
				who = append(who, "you asked for: "+attributeText(yours))
			}
			if len(picked) > 0 {
				who = append(who, "the agent picked, not you: "+attributeText(picked))
			}
			if len(who) > 0 {
				text += " (" + strings.Join(who, "; ") + ")"
			}
			r.Did = append(r.Did, dealReportItem{Kind: "check", Text: text, Steps: []string{e.Check.Snapshot, se.CapsuleID}, Shared: shared, At: e.At})
			var asked []string
			for _, d := range e.Check.Differences {
				if d.Question == "asked" && d.Rule != "agent_picked" && !(d.Rule == "not_asked" && d.Field == "action" && userApproved[se.CapsuleID]) {
					asked = append(asked, d.Text)
				}
			}
			if len(asked) > 0 {
				agent("asked_vs_did", fmt.Sprintf("Tried %s: %s", actionNames[e.Check.Action], strings.Join(asked, " · ")), openID, e.Check.Snapshot, se.CapsuleID)
			}
			for _, d := range e.Check.Differences {
				if d.Question == "asked" && d.Rule != "agent_picked" || d.Text == "" {
					continue
				}
				var steps []string
				key := d.Rule + "/" + d.Field
				for _, i := range pending[key] {
					steps = append(steps, r.Anomalies[i].Steps...)
					folded[i] = true
				}
				delete(pending, key)
				steps = append(steps, e.Check.Snapshot, se.CapsuleID)
				if i, ok := causes[d.Text]; ok {
					r.Anomalies[i].Steps = appendNew(r.Anomalies[i].Steps, steps...)
					continue
				}
				causes[d.Text] = len(r.Anomalies)
				side, kind := pauseCauseKind(d.Rule)
				r.Anomalies = append(r.Anomalies, dealReportItem{Side: side, Kind: kind, Text: d.Text, Steps: appendNew(nil, steps...)})
			}
		case "approval":
			if i, ok := checkItem[e.Approval.Check]; ok {
				r.Did[i].Steps = append(r.Did[i].Steps, se.CapsuleID)
				if e.Approval.Approver == "standing_intent" {
					r.Did[i].Text += "; went ahead on what you already allowed"
				} else if e.Approval.Approver == "agent_card" {
					r.Did[i].Text += "; the card was answered " + e.Approval.Choice
				} else {
					r.Did[i].Text += "; you chose " + optionLabel(events, e.Approval.Check, e.Approval.Choice)
				}
			}
		case "acceptance":
			r.Did = append(r.Did, dealReportItem{Kind: "acceptance", At: e.Acceptance.ObservedAt, Steps: []string{e.Acceptance.Offer, se.CapsuleID},
				Text:   fmt.Sprintf("The other party accepted the offer: %q (recorded as observed)", e.Acceptance.Words),
				Shared: "The other party accepted the offer (recorded as observed)"})
		case "platform_approval":
			p := e.Platform
			line := p.Platform + " separately asked you for approval"
			r.Did = append(r.Did, dealReportItem{Kind: "platform_approval", At: p.ObservedAt, Steps: []string{se.CapsuleID},
				Text:   fmt.Sprintf("%s: %q (recorded as observed; it does not answer your rules)", line, p.DisplayedText),
				Shared: line + " (recorded as observed)"})
		case "act":
			a := e.Act
			steps := []string{se.CapsuleID}
			if a.AuthorizedBy != "" {
				steps = append([]string{a.AuthorizedBy}, steps...)
			}
			text := "Did: " + actText(*a, currency)
			if a.Unchecked {
				text += " ⚠️"
			} else if basis := typedActBasis(events, a.AuthorizedBy); basis != "" {
				text += " (" + basis + ")"
			}
			r.Did = append(r.Did, dealReportItem{Kind: "act", Text: text, Steps: steps, At: e.At})
			if !a.Unchecked {
				continue
			}
			did := actText(*a, currency)
			switch a.Rule {
			case "differs_from_check":
				agent("asked_vs_did", "Did something other than what was checked: "+did+" ("+a.Reason+")", se.CapsuleID)
			case "answer_was_not_proceed", "no_sealed_approval", "approval_already_used":
				agent("unsealed_approval", "Went ahead without your approval: "+did+" ("+a.Reason+")", se.CapsuleID)
			default:
				agent("skipped_check", "Skipped the check: "+did+" ("+a.Reason+")", se.CapsuleID)
			}
		case "outcome":
			for _, d := range e.Outcome.Differences {
				counterparty("delivered_differs", d.Text, openID, se.CapsuleID)
			}
		case "close":
			r.Did = append(r.Did, dealReportItem{Kind: "close", Text: "Closed: " + e.Close.Outcome, Steps: []string{se.CapsuleID}, At: e.At})
		case "disclosure":
			d := e.Disclosure
			item := dealToldItem{At: e.At, To: d.recipient(open.Who), ToKind: d.To, Fields: d.Fields, Authority: "approval", Steps: []string{se.CapsuleID}}
			if d.AuthorizedBy != "" {
				item.Steps = []string{d.AuthorizedBy, se.CapsuleID}
			} else {
				item.Authority, item.Reason = "none", d.Reason
				agent("unapproved_disclosure", fmt.Sprintf("Told %s your %s without your approval (%s)", item.To, d.classList(), d.Reason), se.CapsuleID)
			}
			r.Told = append(r.Told, item)
		}
	}
	kept := r.Anomalies[:0]
	for i, item := range r.Anomalies {
		if !folded[i] {
			kept = append(kept, item)
		}
	}
	r.Anomalies = kept
	// Unverified claims, as they stand at the end, each with where it came from.
	state, _ := foldDeal(events)
	for _, c := range state.claims {
		if c.Verified {
			continue
		}
		step := openID
		for _, se := range events {
			if se.Event.Kind == "claim" && se.Event.Claim.Text == c.Text {
				step = se.CapsuleID
			}
		}
		counterparty("unverified_claim", "Unverified: "+c.Text+" (from "+strings.ReplaceAll(c.Source, "_", " ")+")", step)
	}
	var merchant []dealReportItem
	r.Merchant, merchant = merchantReport(events, state)
	r.Anomalies = append(r.Anomalies, merchant...)
	return r
}

// pauseCauseKind names a check rule as a report anomaly: the rules that match
// an anomaly read from the steps keep that anomaly's kind; a check stopping
// something the agent was about to do is on the agent side.
func pauseCauseKind(rule string) (side, kind string) {
	switch rule {
	case "payee_or_contact_changed":
		return "counterparty", "changed_identifier"
	case "verification_code_request":
		return "counterparty", "code_request"
	case "off_platform_early":
		return "counterparty", "channel_hop"
	case "pay_before_seeing", "credentials_requested", "agent_picked", "first_disclosure", "materiality_changed",
		"rules_deny", "rules_escalate", "rules_not_evaluable", "rules_not_checked":
		// The user's rules judged what the agent proposed.
		return "agent", rule
	}
	return "counterparty", rule
}

// optionLabel is the label the user saw for a choice on a check's card
// ("Pay anyway" over a finding, "Pay" over none, "Hold"); the choice's id
// otherwise. Only hold and proceed: another option's label can carry a
// contact detail ("Call the number I found (…)").
func optionLabel(events []sealedEvent, check, choice string) string {
	if choice != "hold" && choice != "proceed" {
		return choice
	}
	for _, se := range events {
		if se.CapsuleID != check || se.Event.Check == nil {
			continue
		}
		for _, o := range se.Event.Check.Options {
			if o.ID == choice && o.Label != "" {
				return o.Label
			}
		}
	}
	return choice
}

// appendNew appends the steps not already in list, keeping order.
func appendNew(list []string, steps ...string) []string {
	for _, s := range steps {
		if !slices.Contains(list, s) {
			list = append(list, s)
		}
	}
	return list
}

// channelHop reports a message on a different channel from first contact. An
// unknown channel ("other", or none) is never a hop.
func channelHop(first, now string) bool {
	known := func(c string) bool { return c != "" && c != "other" }
	return known(first) && known(now) && !strings.EqualFold(first, now)
}

// changesDetails reports a step that can change what a check compared: a
// detail change, or a message or evidence step carrying counterparty
// identifiers or facts (a new payee in a message is still a new payee). A
// check sealed before such a step no longer covers an action.
func changesDetails(e dealEvent) bool {
	switch e.Kind {
	case "change":
		return true
	case "message":
		return e.Message.Who != nil
	case "evidence":
		return e.Evidence.Who != nil
	}
	return false
}

// dealDisclosure is something the agent told someone about the user: the
// fields it gave (each a class and, in the local store only, the value), to
// whom, on what channel, and the approval that covered it, or why none did.
// The sealed record carries the classes and a commitment to each value,
// never a value: a ledger of disclosures must never itself be one.
type dealDisclosure struct {
	// To is "counterparty" (the deal's other side, the default) or "other"
	// (anyone else: a courier, a platform, a third person).
	To      string                `json:"to"`
	Who     *dealWho              `json:"who,omitempty"`
	Channel string                `json:"channel,omitempty"`
	Fields  []dealDisclosureField `json:"fields"`
	// AuthorizedBy is the sealed approval that covered it; empty when none
	// did, with Reason and Rule saying why.
	AuthorizedBy string `json:"authorized_by,omitempty"`
	// Accepted is, on a seller's address, the sealed acceptance of the offer
	// it rests on; the record cites it (rel source).
	Accepted string `json:"accepted,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Rule         string `json:"rule,omitempty"`
}

type dealDisclosureField struct {
	Class string `json:"class"`
	Value string `json:"value"`
}

// dealToldItem is one disclosure as the report states it.
type dealToldItem struct {
	At string `json:"at"`
	To string `json:"to"`
	// ToKind is counterparty or other, all a shared copy may say of whom.
	ToKind    string                `json:"to_kind"`
	Fields    []dealDisclosureField `json:"fields"`
	Authority string                `json:"authority"` // approval | none
	Reason    string                `json:"reason,omitempty"`
	Steps     []string              `json:"steps"`
}

// disclosureClasses are the kinds of thing an agent tells someone about the
// user, each with the point of no return that covers it.
var disclosureClasses = map[string]string{
	"name": "share_contact", "phone": "share_contact", "email": "share_contact",
	"home_address": "share_contact", "address": "share_contact", "pickup_location": "share_contact",
	"other_contact": "share_contact",
	"credential":    "share_credentials", "verification_code": "share_credentials",
	"payment_card": "share_credentials", "id_document": "share_credentials",
}

var disclosureClassWords = map[string]string{
	"home_address": "home address", "address": "street address", "pickup_location": "pickup location", "other_contact": "contact details",
	"verification_code": "verification code", "payment_card": "payment card", "id_document": "ID document",
}

func classWord(c string) string {
	if w, ok := disclosureClassWords[c]; ok {
		return w
	}
	return strings.ReplaceAll(c, "_", " ")
}

// action is the point of no return that covers the disclosure.
func (d dealDisclosure) action() string {
	return disclosureClasses[d.Fields[0].Class]
}

// addressed is whether the disclosure gives a place: a street or home
// address, or a pickup location.
func (d dealDisclosure) addressed() bool {
	for _, f := range d.Fields {
		switch f.Class {
		case "home_address", "address", "pickup_location":
			return true
		}
	}
	return false
}

func (d dealDisclosure) classList() string {
	words := make([]string, len(d.Fields))
	for i, f := range d.Fields {
		words[i] = classWord(f.Class)
	}
	return strings.Join(words, ", ")
}

func (d dealDisclosure) recipientWord() string {
	if d.To == "other" {
		return "someone other than the counterparty"
	}
	return "the counterparty"
}

// recipient names who was told, as the local store knows them.
func (d dealDisclosure) recipient(first dealWho) string {
	if d.Who != nil {
		for _, f := range whoFields(*d.Who) {
			if f.value != "" {
				return f.value
			}
		}
	}
	if d.To == "counterparty" {
		for _, f := range whoFields(first) {
			if f.value != "" {
				return f.value
			}
		}
	}
	return d.recipientWord()
}

func (d dealDisclosure) validate() error {
	if d.To != "counterparty" && d.To != "other" {
		return inputError("disclosure.to must be counterparty or other")
	}
	if d.To == "other" && (d.Who == nil || *d.Who == (dealWho{})) {
		return inputError("a disclosure to someone other than the counterparty needs who (who they are)")
	}
	if len(d.Fields) == 0 {
		return inputError("disclosure.fields needs at least one {class, value}")
	}
	action := ""
	for _, f := range d.Fields {
		a, ok := disclosureClasses[f.Class]
		if !ok {
			return inputError("disclosure field class must be one of name, phone, email, home_address, address, pickup_location, other_contact, credential, verification_code, payment_card, id_document: " + f.Class)
		}
		if strings.TrimSpace(f.Value) == "" {
			return inputError("every disclosed field needs the value that was given (kept on this device only)")
		}
		if action != "" && a != action {
			return inputError("seal contact details and credentials as separate disclosures: each is covered by its own check")
		}
		action = a
	}
	return nil
}

// recordsTyped is the record set of a deal sealed in the typed action
// records (dealOpen.Records).
const recordsTyped = "typed/v0"

// dealRecordSet is the record set of the deal ev belongs to: the one its
// opening step names, "" (x-deal-v0 throughout) when it names none.
func dealRecordSet(ev dealEvent, events []sealedEvent) string {
	open := ev.Open
	if open == nil && len(events) > 0 {
		open = events[0].Event.Open
	}
	if open == nil {
		return ""
	}
	return open.Records
}

// scopeMismatch compares the deal's terms and refund terms in force when a
// check was made (atCheck) with those in force now: an evaluation covers the
// action only as it stood when it was checked.
func scopeMismatch(atCheck, now dealState) string {
	if !reflect.DeepEqual(termsBody(atCheck.terms), termsBody(now.terms)) {
		return "the terms differ from the ones checked"
	}
	if !reflect.DeepEqual(atCheck.recourse.Refundable, now.recourse.Refundable) {
		return "the refund terms differ from the ones checked"
	}
	return ""
}

// platformObserved reports a platform approval sealed for the check.
func platformObserved(events []sealedEvent, check string) bool {
	for _, se := range events {
		if p := se.Event.Platform; se.Event.Kind == "platform_approval" && p != nil && p.Check == check {
			return true
		}
	}
	return false
}

// dealBuiltinRules is the rule table capsulectl's own deal check evaluates, by
// question, in the order evaluateDeal asks them. A test keeps it equal to the
// rules evaluateDeal emits.
var dealBuiltinRules = []struct {
	Question string
	Rules    []string
}{
	{"asked", []string{"agent_picked", "not_asked", "over_limit", "under_floor"}},
	{"who", []string{"payee_or_contact_changed", "first_disclosure"}},
	{"terms", []string{"terms_changed"}},
	{"recourse", []string{"recourse_changed", "irreversible_rail"}},
	{"safety", []string{"pay_before_seeing", "credentials_requested", "verification_code_request", "off_platform_early", "domain_recent", "materiality_changed"}},
}

// rulesetDigest is the digest of what evaluated: the built-in rule table and
// the materiality predicate (by its digest; "" when none was configured), so
// a change to either changes it.
func rulesetDigest(materialityDigest string) (string, error) {
	table := make([]interface{}, len(dealBuiltinRules))
	for i, q := range dealBuiltinRules {
		rules := make([]interface{}, len(q.Rules))
		for j, r := range q.Rules {
			rules[j] = r
		}
		table[i] = map[string]interface{}{"question": q.Question, "rules": rules}
	}
	// No predicate configured (every agent pick material, fail safe) is null.
	var materiality interface{}
	if materialityDigest != "" {
		materiality = materialityDigest
	}
	return canonical.JSONDigest(map[string]interface{}{
		"evaluator": "capsulectl deal check", "rules": table, "materiality_digest": materiality,
	})
}
