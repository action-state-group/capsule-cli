package cli

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// This file is the deterministic half of `capsulectl deal`: the deal record
// shapes and the four questions asked at a point of no return. It performs no
// I/O. Every input it reads was sealed before it is read (see deal.go), and
// every result it returns is sealed before it is shown.

// dealPointsOfNoReturn lists, per deal type, the actions that cannot easily be
// undone. `deal check` refuses any other action, so an agent cannot reach an
// irreversible step through a name the rules do not cover.
var dealPointsOfNoReturn = map[string][]string{
	"purchase": {"pay", "commit", "share_contact", "share_credentials"},
	"rental":   {"pay", "commit", "sign", "share_contact", "share_credentials"},
	"booking":  {"pay", "commit", "cancel", "share_contact", "share_credentials"},
	"service":  {"pay", "commit", "sign", "share_contact", "share_credentials"},
}

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
	Text     string `json:"text"`
	Source   string `json:"source"`
	Verified bool   `json:"verified,omitempty"`
}

type dealRecourse struct {
	Rail       string `json:"rail,omitempty"`
	Refundable *bool  `json:"refundable,omitempty"`
}

// dealIntent is what the user asked for: their verbatim words plus the parts
// of the request the agent could make exact.
type dealIntent struct {
	Verbatim      string    `json:"verbatim"`
	Asked         dealTerms `json:"asked,omitempty"`
	MaxTotalMinor *int64    `json:"max_total_minor,omitempty"`
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
}

// dealApproval answers one check (a verdict). Approver is "user" for the
// user's own answer, or "standing_intent" when a passing check is covered by
// what the user already allowed.
type dealApproval struct {
	Check    string `json:"check"`
	Choice   string `json:"choice"`
	Approver string `json:"approver"`
	Said     string `json:"said,omitempty"`
	Proceed  bool   `json:"proceed"`
	Reason   string `json:"reason,omitempty"`
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
}

// dealEvent is one step as the local store keeps it, with raw values. Exactly
// one body field is set, matching Kind. What is sealed is the x-deal-v0 record
// derived from it (deal_profile.go): fingerprints and commitments, never the
// raw values. Prev is the previous step's record digest. Nonces are the
// commitment nonces; they never leave the store.
type dealEvent struct {
	DealID   string            `json:"deal_id"`
	N        int64             `json:"n"`
	Kind     string            `json:"kind"`
	Prev     string            `json:"prev,omitempty"`
	At       string            `json:"at"`
	Open     *dealOpen         `json:"open,omitempty"`
	Intent   *dealIntent       `json:"intent,omitempty"`
	Message  *dealMessage      `json:"message,omitempty"`
	Claim    *dealClaim        `json:"claim,omitempty"`
	Evidence *dealEvidence     `json:"evidence,omitempty"`
	Change   *dealChange       `json:"change,omitempty"`
	Snapshot *dealSnapshot     `json:"snapshot,omitempty"`
	Check    *dealCheckResult  `json:"check,omitempty"`
	Approval *dealApproval     `json:"approval,omitempty"`
	Act      *dealAct          `json:"act,omitempty"`
	Outcome  *dealCloseResult  `json:"outcome,omitempty"`
	Close    *dealCloseResult  `json:"close,omitempty"`
	Nonces   map[string]string `json:"nonces,omitempty"`
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

func (c dealClaim) validate() error {
	if strings.TrimSpace(c.Text) == "" || strings.TrimSpace(c.Source) == "" {
		return inputError("every claim needs its text and its source")
	}
	return nil
}

// dealState folds a deal's sealed steps into what a check compares.
type dealState struct {
	open      dealOpen
	intent    dealIntent // the latest: the baseline's, or a later intent step
	agreed    dealTerms
	who       dealWho
	whoSource map[string]string
	terms     dealTerms
	recourse  dealRecourse
	// agreedRecourse is the way back as agreed: the baseline's, updated only
	// by an approved check.
	agreedRecourse dealRecourse
	claims         []dealClaim
	messages       []dealMessage
}

func foldDeal(events []sealedEvent) (dealState, error) {
	if len(events) == 0 || events[0].Event.Kind != "open" || events[0].Event.Open == nil {
		return dealState{}, inputError("deal has no sealed baseline")
	}
	o := *events[0].Event.Open
	s := dealState{open: o, intent: o.Intent, agreed: o.Terms, who: o.Who, terms: o.Terms, recourse: o.Recourse, agreedRecourse: o.Recourse, claims: slices.Clone(o.Claims), whoSource: map[string]string{}}
	var lastSnapshot *dealSnapshot
	for _, se := range events[1:] {
		e := se.Event
		switch e.Kind {
		case "intent":
			s.intent = *e.Intent
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

var proceedLabels = map[string]string{
	"pay": "Pay anyway", "commit": "Confirm anyway", "sign": "Sign anyway", "cancel": "Cancel anyway",
	"share_contact": "Share anyway", "share_credentials": "Share anyway",
}

var actionNames = map[string]string{
	"pay": "paying", "commit": "confirming a commitment", "sign": "signing", "cancel": "cancelling",
	"share_contact": "sharing your contact details", "share_credentials": "sharing a login or code",
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
	if intent.Allowed != nil && !slices.Contains(intent.Allowed, snap.Action) {
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
		if total != nil && *total > *intent.MaxTotalMinor {
			add("asked", "over_limit", "price", fmt.Sprintf("Over your limit of %s (%s)", formatMoney(*intent.MaxTotalMinor, proposed.Currency), formatMoney(*total, proposed.Currency)))
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
	r.Verdict = "pause"
	r.Options = []dealOption{{ID: "hold", Label: "Hold"}}
	if s.open.Who.Phone != "" && slices.ContainsFunc(r.Differences, func(d dealDifference) bool { return d.Question == "who" }) {
		r.Options = append(r.Options, dealOption{ID: "verify_contact", Label: "Call the number I found (" + s.open.Who.Phone + ")"})
	}
	r.Options = append(r.Options, dealOption{ID: "proceed", Label: proceedLabels[r.Action]})
}

// renderCard writes the difference card: the differences, not a summary, then
// what is unverified, then the one-tap options. A passing check has no card.
func renderCard(r dealCheckResult, demo bool) string {
	if r.Verdict == "pass" {
		return ""
	}
	parts := make([]string, 0, len(r.Differences)+len(r.Notes)+1)
	for _, d := range r.Differences {
		if d.Text != "" { // a difference folded into another line
			parts = append(parts, d.Text)
		}
	}
	parts = append(parts, r.Notes...)
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
func authorizeAct(events []sealedEvent, act dealAct) (approval, reason, rule string) {
	used := map[string]bool{}
	for _, se := range events {
		if se.Event.Kind == "act" && !se.Event.Act.Unchecked {
			used[se.Event.Act.AuthorizedBy] = true
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
		if mismatch := actMismatch(events, verdict.Event.Check.Snapshot, act); mismatch != "" {
			return "", mismatch, "differs_from_check"
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
				return "", "that approval already covered an earlier action", "approval_already_used"
			}
			return later.CapsuleID, "", ""
		}
		return "", "the check paused and there is no sealed approval", "no_sealed_approval"
	}
	return "", "no check before this action", "no_check"
}

// actMismatch compares what was done with the snapshot that was checked.
func actMismatch(events []sealedEvent, snapshotID string, act dealAct) string {
	for _, se := range events {
		if se.CapsuleID != snapshotID || se.Event.Snapshot == nil {
			continue
		}
		snap := se.Event.Snapshot
		if act.AmountMinor != nil && snap.AmountMinor != nil && *act.AmountMinor != *snap.AmountMinor {
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
		state := "unverified"
		if e.Evidence.Verified {
			state = "checked"
		}
		return "evidence (" + e.Evidence.Source + "), " + state + ": " + e.Evidence.About
	case "change":
		return "counterparty changed details (" + e.Change.Source + ")"
	case "snapshot":
		return "snapshot before " + actionNames[e.Snapshot.Action]
	case "check":
		if e.Check.Verdict == "pass" {
			return "checked " + e.Check.Action + ": no differences"
		}
		return "checked " + e.Check.Action + ": flagged — " + e.Check.Card
	case "approval":
		if e.Approval.Approver == "standing_intent" {
			return "went ahead on what you already allowed"
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
}

// dealReport is the three-part report: what the user asked, what the agent
// did, and the anomalies on either side.
type dealReport struct {
	Asked     string           `json:"asked"`
	AskedStep string           `json:"asked_step"`
	Did       []dealReportItem `json:"did"`
	Anomalies []dealReportItem `json:"anomalies"`
}

func actText(a dealAct, currency string) string {
	text := a.Action
	if a.AmountMinor != nil {
		cur := a.Currency
		if cur == "" {
			cur = currency
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
	r := dealReport{Asked: open.Intent.Verbatim, AskedStep: openID, Did: []dealReportItem{}, Anomalies: []dealReportItem{}}
	checkItem := map[string]int{}
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
	for _, se := range events {
		e := se.Event
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
			verdict := "no differences"
			if e.Check.Verdict == "pause" {
				verdict = "flagged"
			}
			checkItem[se.CapsuleID] = len(r.Did)
			r.Did = append(r.Did, dealReportItem{Kind: "check", Text: fmt.Sprintf("Checked before %s: %s", actionNames[e.Check.Action], verdict), Steps: []string{e.Check.Snapshot, se.CapsuleID}})
			var asked []string
			for _, d := range e.Check.Differences {
				if d.Question == "asked" {
					asked = append(asked, d.Text)
				}
			}
			if len(asked) > 0 {
				agent("asked_vs_did", fmt.Sprintf("Tried %s: %s", actionNames[e.Check.Action], strings.Join(asked, " · ")), openID, e.Check.Snapshot, se.CapsuleID)
			}
			for _, d := range e.Check.Differences {
				if d.Question == "asked" || d.Text == "" {
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
				} else {
					r.Did[i].Text += "; you chose " + e.Approval.Choice
				}
			}
		case "act":
			a := e.Act
			steps := []string{se.CapsuleID}
			if a.AuthorizedBy != "" {
				steps = append([]string{a.AuthorizedBy}, steps...)
			}
			text := "Did: " + actText(*a, currency)
			if a.Unchecked {
				text += " ⚠️"
			}
			r.Did = append(r.Did, dealReportItem{Kind: "act", Text: text, Steps: steps})
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
			r.Did = append(r.Did, dealReportItem{Kind: "close", Text: "Closed: " + e.Close.Outcome, Steps: []string{se.CapsuleID}})
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
	case "pay_before_seeing", "credentials_requested":
		return "agent", rule
	}
	return "counterparty", rule
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
