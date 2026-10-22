package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/capsule-cli/internal/settlement"
	"github.com/spf13/cobra"
)

// What each deal command tells the user (capsule-cli#345): its voice and
// the words, from the profile's wording pack, with values filled from the
// sealed steps. The table of which command speaks how is in the design note
// on #345 and in TestEveryDealCommandSpeaks.

// dealErrorPhase says, for a failed deal command, whether the user's step
// had happened yet: before it (check, open, intent, approval, a disclosure
// noted before it is sent), after it (it happened outside: act, payment,
// delivery, close, evidence), or neither.
func dealErrorPhase(c *cobra.Command) string {
	for p := c.Parent(); p != nil; p = p.Parent() {
		if p.Name() == "sale" {
			// A sale's commands record no step of the user's own.
			return "other"
		}
	}
	switch c.Name() {
	case "check", "open":
		return "before"
	case "close":
		return "after"
	case "note":
		kind, _ := c.Flags().GetString("kind")
		switch kind {
		case "intent", "approval", "disclosure":
			return "before"
		case "act", "evidence", noteKindPaymentReceived, noteKindDelivered:
			return "after"
		}
	}
	return "other"
}

// silent is the narration of a step that is on the record and says nothing.
func silent(cli.Wording) cli.Narration { return cli.Narration{Voice: cli.VoiceSilent} }

// line is a narration of one or more lines.
func line(lines ...string) cli.Narration {
	return cli.Narration{Voice: cli.VoiceLine, Say: cli.Lines(lines...)}
}

// approvalReasonKeys name the line for an answer that was sealed but not
// applied, by the reason the answer carries (judgeApproval,
// judgeLimitsConfirmation).
var approvalReasonKeys = map[string]string{
	approvalAnswered:  "answered",
	approvalChanged:   "changed",
	proposalReplaced:  "replaced",
	proposalConfirmed: "confirmed",
}

// approvalNarration is the line after the user answers: what their answer
// does now, never "you OK'd" for an answer that was not applied.
func approvalNarration(w cli.Wording, a dealApproval, events []sealedEvent, state dealState) cli.Narration {
	if a.Reason != "" {
		key, ok := approvalReasonKeys[a.Reason]
		if !ok {
			key = "other"
		}
		return line(w.Line("say.approval.not_applied."+key, nil))
	}
	switch a.Choice {
	case "confirm_limits":
		limit := ""
		if a.Limits != nil && a.Limits.New.MaxTotalMinor != nil {
			limit = spokenMoney(*a.Limits.New.MaxTotalMinor, state.terms.Currency)
		}
		if limit == "" {
			return line(w.Line("say.approval.confirm_limits.other", nil))
		}
		return line(w.Line("say.approval.confirm_limits", cli.V{"limit": limit}))
	case "proceed":
		action, amount := "", ""
		if check, _, ok := checkedCard(events, a.Check); ok {
			action = check.Event.Check.Action
			for _, se := range events {
				if se.CapsuleID == check.Event.Check.Snapshot && se.Event.Snapshot != nil && se.Event.Snapshot.AmountMinor != nil {
					amount = spokenMoney(*se.Event.Snapshot.AmountMinor, state.terms.Currency)
				}
			}
		}
		if action == "pay" && amount == "" {
			action = "other"
		}
		if text := w.Line("say.approval.proceed."+action, cli.V{"amount": amount}); text != "" {
			return line(text)
		}
		return line(w.Line("say.approval.proceed.other", nil))
	case "verify_contact":
		return line(w.Line("say.approval.verify_contact", nil))
	default:
		return line(w.Line("say.approval.hold", nil))
	}
}

// intentAsk is the card for an intent note that asks for more than the
// limits in force: no check stands behind it and no button says yes, as
// confirm_limits takes the user's own words.
func intentAsk(w cli.Wording, inForce, proposed dealLimitSet, currency string) cli.Narration {
	// Every change the note asks for is on the card: confirming it applies
	// them all.
	var changes []string
	if p, f := proposed.MaxTotalMinor, inForce.MaxTotalMinor; p != nil && f != nil && *p > *f {
		changes = append(changes, w.Line("card.change.limit", cli.V{"from": spokenMoney(*f, currency), "to": spokenMoney(*p, currency)}))
	}
	if p, f := proposed.MinTotalMinor, inForce.MinTotalMinor; p != nil && f != nil && *p < *f {
		changes = append(changes, w.Line("card.change.floor", cli.V{"from": spokenMoney(*f, currency), "to": spokenMoney(*p, currency)}))
	}
	var actions []string
	for _, a := range proposed.Allowed {
		if inForce.Allowed != nil && !containsString(inForce.Allowed, a) {
			if word := w.Line("action."+a, nil); word != "" {
				actions = append(actions, word)
			} else {
				actions = append(actions, strings.ReplaceAll(a, "_", " "))
			}
		}
	}
	if len(actions) > 0 {
		changes = append(changes, w.Line("card.change.permission", cli.V{"action": listWords(actions, "and")}))
	}
	// What the buyer must be told is a permission and an obligation: each
	// one added or dropped is said (proposedLimits).
	if p, f := proposed.OrderedRepresentations, inForce.OrderedRepresentations; p != nil && f != nil {
		var added, dropped []string
		for _, c := range *p {
			if !containsString(*f, c) {
				added = append(added, classWord(c))
			}
		}
		for _, c := range *f {
			if !containsString(*p, c) {
				dropped = append(dropped, classWord(c))
			}
		}
		if len(added) > 0 {
			changes = append(changes, w.Line("card.change.tell_more", cli.V{"what": listWords(added, "and")}))
		}
		if len(dropped) > 0 {
			changes = append(changes, w.Line("card.change.tell_less", cli.V{"what": listWords(dropped, "and")}))
		}
	}
	if len(changes) == 0 {
		// More than the limits in force, in no way named above (what the
		// buyer is told, say).
		changes = append(changes, w.Line("card.change.other", nil))
	}
	question := w.Line("card.question.changes", cli.V{"changes": capitalize(listWords(changes, "and"))})
	ask := cli.Ask{Tag: w.Line("card.tag.ask", nil), Question: question, Reply: "words", HoldSay: w.Line("say.approval.limit_hold", nil),
		Options: []cli.AskOption{{Label: w.Line("card.button.hold", nil), Choice: "hold"}}}
	return cli.Narration{Voice: cli.VoiceAsk, Ask: &ask, Say: cli.AskText(w, ask)}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// receiptLines are the receipt's chat lines after a payment, a close or a
// report: what was done for how much against the limit, what the user OK'd
// on the way, and that the receipt is saved. No postmark until the page can
// show the same witness (capsule-cli#341).
func receiptLines(w cli.Wording, events []sealedEvent) []string {
	state, err := foldDeal(events)
	if err != nil {
		return nil
	}
	var lines []string
	item := shortTitle(state.terms.Item)
	// Money as the receipt states it (buildDealMoney): the net paid in the
	// deal's currency. An act in another currency is never added to it, and
	// then the limit, set in the deal's currency, is not compared.
	if m := buildDealMoney(events, state.terms.Currency); m != nil && m.PaidMinor > 0 {
		amount := spokenMoney(m.NetMinor, m.Currency)
		limit := state.intent.MaxTotalMinor
		sameCurrency := m.Other == 0 && strings.EqualFold(m.Currency, state.terms.Currency)
		switch {
		case item != "" && limit != nil && sameCurrency && m.NetMinor <= *limit:
			lines = append(lines, w.Line("say.receipt.done.within", cli.V{"item": item, "amount": amount, "limit": spokenMoney(*limit, m.Currency)}))
		case item != "":
			lines = append(lines, w.Line("say.receipt.done", cli.V{"item": item, "amount": amount}))
		default:
			lines = append(lines, w.Line("say.receipt.done.no_item", cli.V{"amount": amount}))
		}
	}
	var what []string
	for _, se := range events {
		a := se.Event.Approval
		if se.Event.Kind != "approval" || a == nil || !a.Proceed || a.Reason != "" || a.Approver == "standing_intent" {
			continue
		}
		key, amount := "say.receipt.what.other", ""
		if check, _, ok := checkedCard(events, a.Check); ok {
			switch check.Event.Check.Action {
			case "pay":
				key = "say.receipt.what.pay"
				for _, se := range events {
					if snap := se.Event.Snapshot; se.CapsuleID == check.Event.Check.Snapshot && snap != nil && snap.AmountMinor != nil {
						amount = spokenMoney(*snap.AmountMinor, state.terms.Currency)
					}
				}
				if amount == "" {
					key = "say.receipt.what.pay.no_amount"
				}
			case "share_contact", "share_credentials":
				key = "say.receipt.what.share"
			}
		}
		// Each thing OK'd is said once: two answers that read the same are
		// one phrase.
		if text := w.Line(key, cli.V{"amount": amount}); text != "" && !containsString(what, text) {
			what = append(what, text)
		}
	}
	switch len(what) {
	case 0:
	case 1:
		lines = append(lines, w.Line("say.receipt.approvals.one", cli.V{"what": what[0]}))
	default:
		lines = append(lines, w.Line("say.receipt.approvals.other", cli.V{"count": fmt.Sprint(len(what)), "what": listWords(what, "and")}))
	}
	return append(lines, w.Line("say.receipt.saved", nil))
}

// firstReceiptLines are what the first receipt adds, once: what this one
// covers next to the merchant's (a paid purchase from a merchant with a
// sealed name), and why it is worth keeping, or, for an errand called off,
// that it is a record of what was not done (actionstate-muse#208).
func firstReceiptLines(w cli.Wording, events []sealedEvent) []string {
	state, err := foldDeal(events)
	if err != nil {
		return nil
	}
	if fc := finalClose(events); fc >= 0 {
		if o := events[fc].Event.Close.Outcome; o == "called_off" || o == "not_selected" {
			return []string{w.Line("say.receipt.first.keep.called_off", nil)}
		}
	}
	if dealRole(events) == dealRoleSeller {
		return []string{w.Line("say.receipt.first.keep.sold", nil)}
	}
	var lines []string
	if m := buildDealMoney(events, state.terms.Currency); m != nil && m.PaidMinor > 0 && state.open.Type == "purchase" {
		// Only a sealed name that reads as one: never one from an email or
		// a website field, and never an id or a domain.
		if name := strings.TrimSpace(legWho(events).Name); name != "" && !idLike.MatchString(name) {
			lines = append(lines, w.Line("say.receipt.first.contrast", cli.V{"merchant": name}))
		}
	}
	return append(lines, w.Line("say.receipt.first.keep", nil))
}

// idLike is a name that is an id, a handle or a domain rather than what
// people call a merchant: "sticker_shop_example", "shop@example.com",
// "marketplace.example".
var idLike = regexp.MustCompile(`[_@/]|^[^\s]+\.[A-Za-z]{2,}$`)

// isFirstReceipt is whether the step just sealed (the last of events) is the
// profile's first receipt moment: the deal's only act or final close, and no
// other deal has a buyer's act or a close. It is read from the sealed steps
// alone; nothing records that a receipt was shown. Another deal's close
// counts even when it left the deal open, so the lines are never said twice.
// A seller's acts (an offer, accepting one) are not receipt moments, here or
// in another deal: a seller's first receipt is its thread's close.
func (s *dealSession) isFirstReceipt(ctx context.Context, dealID string, events []sealedEvent) bool {
	seller := dealRole(events) == dealRoleSeller
	if seller && events[len(events)-1].Event.Kind == "act" {
		return false
	}
	n := 0
	for _, se := range events {
		if se.Event.Kind == "act" && !seller {
			n++
		}
		if c := se.Event.Close; se.Event.Kind == "close" && c != nil {
			if _, final := closeKeys[c.Outcome]; final {
				n++
			}
		}
	}
	if n != 1 {
		return false
	}
	others, err := s.index.steps(ctx, stepFilter{Kinds: []string{"open", "act", "close"}, ExceptDeal: dealID})
	if err != nil {
		return false
	}
	sellers := map[string]bool{}
	for _, st := range others {
		var ev dealEvent
		if st.Kind == "open" && json.Unmarshal([]byte(st.Local), &ev) == nil && ev.Open != nil && ev.Open.Intent.PartyRole == dealRoleSeller {
			sellers[st.DealID] = true
		}
	}
	for _, st := range others {
		if st.Kind == "close" || st.Kind == "act" && !sellers[st.DealID] {
			return false
		}
	}
	return true
}

// nothingDoneLine is a called-off purchase's proof line: the receipt shows
// nothing paid (and nothing shared). `deal close` refuses called_off and
// not_selected on a deal with an act, so only a disclosure can have
// happened. A seller's thread says neither: its payments are evidence
// notes, not acts.
func nothingDoneLine(w cli.Wording, events []sealedEvent) string {
	if dealRole(events) == dealRoleSeller {
		return ""
	}
	for _, se := range events {
		if se.Event.Kind == "disclosure" {
			return w.Line("say.close.nothing_paid", nil)
		}
	}
	return w.Line("say.close.nothing_done", nil)
}

// closeLines are a final close's receipt lines: the outcome, and, when the
// deal was called off, what the receipt shows was not done.
func closeLines(w cli.Wording, key string, events []sealedEvent) []string {
	lines := []string{w.Line(key, nil)}
	if key == "say.close.called_off" {
		lines = append(lines, nothingDoneLine(w, events))
	}
	return lines
}

// listingKeys name a listing's chat line by how the deal ended: its final
// close's outcome, and "open" while it has none.
var listingKeys = map[string]string{"completed": "say.list.deal.bought", "mismatch": "say.list.deal.mismatch", "called_off": "say.list.deal.called_off", "not_selected": "say.list.deal.not_chosen", "open": "say.list.deal.open"}

// listingSay is a listing's line for "show my receipts": the item as the
// deal last agreed it, how it ended, the amount paid when the deal paid it
// in one currency, and the local date it ended (or opened, while open). No
// deal id; a demo says so.
func listingSay(w cli.Wording, l dealListing, events []sealedEvent) string {
	item := ""
	if state, err := foldDeal(events); err == nil {
		item = shortTitle(state.agreed.Item)
	} else if open := events[0].Event.Open; open != nil {
		// A sale's log opens with the sale, not a deal.
		item = shortTitle(open.Terms.Item)
	}
	if item == "" && l.Summary.What != dealNotRecorded {
		item = shortTitle(l.Summary.What)
	}
	if item == "" {
		item = w.Line("say.list.untitled", nil)
	}
	if open := events[0].Event.Open; open != nil && open.Demo {
		item = w.Line("say.list.demo", cli.V{"item": item})
	}
	outcome, date := "open", l.OpenedAt
	if fc := finalClose(events); fc >= 0 {
		outcome, date = events[fc].Event.Close.Outcome, events[fc].Event.At
	}
	vals := cli.V{"item": item, "date": listingDate(date)}
	if l.Kind == "sale" {
		for _, se := range events {
			if se.Event.Kind == "sale_close" {
				vals["date"] = listingDate(se.Event.At)
				return w.Line("say.list.sale.closed", vals)
			}
		}
		return w.Line("say.list.sale", vals)
	}
	key, ok := listingKeys[outcome]
	if !ok {
		return ""
	}
	if outcome == "completed" || outcome == "mismatch" {
		if outcome == "completed" && l.Result == dealResultSold {
			key = "say.list.deal.sold"
		}
		if a := l.Summary.Amount; a != nil && a.Basis == "paid" && a.OtherCurrencyActs == 0 && a.AmountMinor > 0 {
			vals["amount"] = spokenMoney(a.AmountMinor, a.Currency)
		} else {
			key += ".no_amount"
		}
	}
	return w.Line(key, vals)
}

// listingDate is a listing's local date: "Oct 20", with the year when it is
// not this year's.
func listingDate(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return localDate(at)
	}
	t = t.In(time.Local)
	if t.Year() != cli.DealClock().In(time.Local).Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}

// shortTitle shortens an item's title for a chat line: its first eight
// words, and an ellipsis when it had more.
func shortTitle(title string) string {
	words := strings.Fields(title)
	if len(words) <= 8 {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:8], " ") + "…"
}

// report is a report's narration: its lines, then, when a page was written,
// "View details" and the page to send in its own message, labelled as the
// user's own copy. A narrated page is always that copy: a copy for someone
// else is silent.
func report(w cli.Wording, lines []string, htmlPath string) cli.Narration {
	n := cli.Narration{Voice: cli.VoiceReport}
	if htmlPath != "" {
		lines = append(lines, w.Line("report.view_details", nil))
		n.Attach = &cli.Attach{Path: htmlPath, MediaType: "text/html", Label: w.Line("report.private", nil)}
	}
	n.Say = cli.Lines(lines...)
	return n
}

// writeReceiptPage writes the deal's receipt page (your own copy) to
// htmlPath, as `deal report --html` does, after a step that changes it.
func (s *dealSession) writeReceiptPage(ctx context.Context, events []sealedEvent, htmlPath string) error {
	rep, err := s.dealReportFor(ctx, events)
	if err != nil {
		return err
	}
	b, err := s.dealReportBundle(ctx, events, rep, cli.DealAudienceKeep, dealVerifyCommand(htmlPath), true)
	if err != nil {
		return err
	}
	words, _ := cli.ActiveWording(s.p)
	page, err := dealReportHTML(b, dealNotCountersigned(), dealVerifyCommand(htmlPath), words)
	if err != nil {
		return err
	}
	return cli.AtomicFile(htmlPath, []byte(page), false)
}

// stepReport is a step's narration (voice: a report, or a line), with its receipt page when htmlPath is
// given. The step is already sealed: a page that fails is said in
// attach_error, never as a failed step.
func (s *dealSession) stepReport(ctx context.Context, out map[string]any, events []sealedEvent, htmlPath, voice string, lines func(cli.Wording) []string) map[string]any {
	written := ""
	if htmlPath != "" {
		if err := s.writeReceiptPage(ctx, events, htmlPath); err != nil {
			out["attach_error"] = cli.SafeError(err)
		} else {
			written = htmlPath
		}
	}
	return cli.Speak(out, s.p, func(w cli.Wording) cli.Narration {
		said := lines(w)
		if _, failed := out["attach_error"]; failed {
			said = append(said, w.Line("report.attach_failed", nil))
		}
		n := report(w, said, written)
		n.Voice = voice
		return n
	})
}

// closeKeys name a close's final-receipt line by its outcome; an open
// outcome (pending) has none: the deal goes on.
var closeKeys = map[string]string{"completed": "say.close.received", "not_selected": "say.close.called_off", "called_off": "say.close.called_off", "mismatch": "say.close.mismatch"}

// emailLines are the lines for a merchant email added to the receipt:
// whose email it is, and whether its signature checks out.
func emailLines(w cli.Wording, m *merchantEmail, state dealState) []string {
	sender := recipientName(state.who)
	if sender == "" {
		sender = m.DKIM.FromDomain
	}
	lines := []string{w.Line("say.email.added", cli.V{"sender": sender})}
	if m.DKIM.Result == "pass" && m.DKIM.Merchant {
		return append(lines, w.Line("say.verify_email.ok", cli.V{"sender": sender, "domain": m.DKIM.signer()}))
	}
	return append(lines, w.Line("say.verify_email.failed", cli.V{"sender": sender}))
}

// reportSay is `deal report`'s narration: the receipt's lines, and the page
// when one was written.
func reportSay(out map[string]any, p cli.Profile, events []sealedEvent, htmlPath string) map[string]any {
	return cli.Speak(out, p, func(w cli.Wording) cli.Narration {
		lines := receiptLines(w, events)
		for _, se := range events {
			if se.Event.Kind == "close" && se.Event.Close != nil {
				if key, final := closeKeys[se.Event.Close.Outcome]; final {
					lines = append(closeLines(w, key, events), lines...)
				}
			}
		}
		return report(w, lines, htmlPath)
	})
}

// speakFor narrates out with the selected profile's wording, or the
// built-in pack when no profile is selected.
func speakFor(c *cobra.Command, out map[string]any, build func(cli.Wording) cli.Narration) map[string]any {
	return cli.Narrate(out, cli.NarrateWith(c, build))
}

// localDate is a YYYY-MM-DD date as a chat line says it: "Nov 2".
func localDate(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, date); err != nil {
			return date
		}
		t = t.In(time.Local)
	}
	return t.Format("Jan 2")
}

// countersignerName is who countersigned, as the directory names them.
func countersignerName(v dealCountersignView) string {
	for _, e := range v.Entries {
		if e.SignerName != "" {
			return e.SignerName
		}
	}
	return ""
}

// receivedAmount is what a payee's observed leg says arrived, in the asset's
// own units ("450.00 USDC"), or "" when the leg does not say.
func receivedAmount(leg json.RawMessage) string {
	var m struct {
		Received struct {
			Value      string      `json:"value"`
			AssetCode  string      `json:"assetCode"`
			AssetScale json.Number `json:"assetScale"`
		} `json:"received"`
	}
	var name string
	if json.Unmarshal(leg, &m) != nil || m.Received.Value == "" {
		return ""
	}
	if name = settlement.AssetName(m.Received.AssetCode); name == "" {
		// An asset id is not something to read out.
		return ""
	}
	value, ok := new(big.Int).SetString(m.Received.Value, 10)
	scale, err := m.Received.AssetScale.Int64()
	if !ok || err != nil || scale < 0 || scale > 30 {
		return ""
	}
	amount := new(big.Rat).SetFrac(value, new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	places := int(scale)
	if places > 2 {
		places = 2
	}
	return amount.FloatString(places) + " " + name
}

// capitalize starts a sentence with a capital letter.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// countersignLine is a countersignature's line: by whom, when the directory
// names them.
func countersignLine(w cli.Wording, view dealCountersignView) cli.Narration {
	who := countersignerName(view)
	switch {
	case view.Rung == "countersigned" && who != "":
		return line(w.Line("say.countersign.ok", cli.V{"who": who}))
	case view.Rung == "countersigned":
		return line(w.Line("say.countersign.ok.unnamed", nil))
	case who != "":
		return line(w.Line("say.countersign.failed", cli.V{"who": who}))
	}
	return line(w.Line("say.countersign.failed.unnamed", nil))
}

// saleLine is a new sale's line: what is listed, at its price when it has
// one.
func saleLine(w cli.Wording, terms dealTerms) cli.Narration {
	if terms.PriceMinor == nil {
		return line(w.Line("say.sale.new.no_price", cli.V{"item": shortTitle(terms.Item)}))
	}
	return line(w.Line("say.sale.new", cli.V{"item": shortTitle(terms.Item), "price": spokenMoney(*terms.PriceMinor, terms.Currency)}))
}

// verifyEmailLine is a merchant email's recheck: whose email (the deal's
// counterparty, else the email's From domain) and whether it checks out.
func verifyEmailLine(w cli.Wording, sender, fromDomain string, ok bool, signedBy string) cli.Narration {
	if sender == "" {
		sender = fromDomain
	}
	if ok && signedBy != "" {
		return line(w.Line("say.verify_email.ok", cli.V{"sender": sender, "domain": signedBy}))
	}
	return line(w.Line("say.verify_email.failed", cli.V{"sender": sender}))
}

// spokenMoney is an amount as a person hears it: thousands separated and
// no cents on a whole amount ("$1,700", "$1.90"). Sealed text keeps
// formatMoney's form.
func spokenMoney(minor int64, currency string) string {
	cur := strings.ToUpper(currency)
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	whole, cents := minor/100, minor%100
	if cur == "JPY" {
		whole, cents = minor, 0
	}
	digits := fmt.Sprint(whole)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	if cents != 0 {
		digits += fmt.Sprintf(".%02d", cents)
	}
	if cur == "JPY" {
		return sign + "¥" + digits
	}
	if sym, ok := currencySymbols[cur]; ok {
		return sign + sym + digits
	}
	if cur == "" {
		return sign + digits
	}
	return sign + digits + " " + cur
}
