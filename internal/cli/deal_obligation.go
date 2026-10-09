package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Two kinds of accountable step are easy to miss because nothing is "done"
// at them:
//
//   - a CANCEL-BY DATE. The point of no return is a date passing, not an
//     action: "the trial converts to $24.00/month on Oct 17 unless cancelled
//     by Oct 16". The deal records the obligation when it is created, with
//     its source (the merchant's email, or a snapshot of the page), lists
//     open dates on every check and report, and emits them in a form the
//     host's own scheduler or calendar can use. It records the deadline; it
//     does not enforce it, and nothing is cancelled for the user.
//
//   - A DUE DATE. Something the user owes the other side by a date: a seller's
//     delivery (deliver_by) or a service performed (perform_by). Recorded,
//     listed and emitted the same way; a later record resolves it. A cancel
//     does not end it.
//
//   - PROVING A NEGATIVE: "I cancelled on the 4th". A sealed cancel action
//     plus the merchant's own cancellation email, bound to the deal that
//     created the obligation. The report says exactly what that proves and
//     what it does not.

// dealObligation is a commitment that takes effect when a date passes.
type dealObligation struct {
	// Kind: trial_conversion (a trial becomes paid), renewal (a
	// subscription renews), cancel_window (free cancellation ends) or
	// payment_due (a payment is taken on a date), each with a cancel-by date;
	// or deliver_by (the user delivers) or perform_by (the user performs a
	// service), each with a due date.
	Kind string `json:"kind"`
	// CancelBy is the last day to cancel (YYYY-MM-DD), on a cancel-by kind.
	CancelBy string `json:"cancel_by,omitempty"`
	// DueBy is the day it is due (YYYY-MM-DD), on a due kind.
	DueBy string `json:"due_by,omitempty"`
	// TakesEffect is the day the commitment takes effect, when stated.
	TakesEffect string `json:"takes_effect,omitempty"`
	AmountMinor *int64 `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	// Period is week, month or year for a recurring charge, once otherwise.
	Period string `json:"period,omitempty"`
	// Terms is the merchant's own wording, kept on the device; the record
	// carries a commitment to it.
	Terms string `json:"terms,omitempty"`
}

var obligationKinds = []string{"trial_conversion", "renewal", "cancel_window", "payment_due", "deliver_by", "perform_by"}

// dueKinds are the obligations with a due date rather than a cancel-by date.
var dueKinds = []string{"deliver_by", "perform_by"}

var obligationWords = map[string]string{
	"trial_conversion": "the trial becomes paid",
	"renewal":          "the subscription renews",
	"cancel_window":    "free cancellation ends",
	"payment_due":      "a payment is taken",
	"deliver_by":       "delivery is due",
	"perform_by":       "the service is due",
}

// due is whether the obligation has a due date (deliver_by, perform_by)
// rather than a cancel-by date.
func (o dealObligation) due() bool { return slices.Contains(dueKinds, o.Kind) }

// date is the obligation's date: its due date or its cancel-by date.
func (o dealObligation) date() string {
	if o.due() {
		return o.DueBy
	}
	return o.CancelBy
}

const deadlineNotEnforced = "We record this date; we do not enforce it. Nothing is cancelled for you: cancel with the merchant before the date."

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (o *dealObligation) normalize() error {
	o.Kind = strings.TrimSpace(strings.ToLower(o.Kind))
	o.Currency = strings.ToUpper(strings.TrimSpace(o.Currency))
	o.Period = strings.TrimSpace(strings.ToLower(o.Period))
	switch {
	case !slices.Contains(obligationKinds, o.Kind):
		return inputError("obligation.kind must be one of " + strings.Join(obligationKinds, ", "))
	case o.due() && (!validDate(o.DueBy) || o.CancelBy != ""):
		return inputError("obligation." + o.Kind + " needs due_by, a date (YYYY-MM-DD), and no cancel_by")
	case !o.due() && (!validDate(o.CancelBy) || o.DueBy != ""):
		return inputError("obligation." + o.Kind + " needs cancel_by, a date (YYYY-MM-DD), and no due_by")
	case o.TakesEffect != "" && !validDate(o.TakesEffect):
		return inputError("obligation.takes_effect must be a date, YYYY-MM-DD")
	case o.Period != "" && !slices.Contains([]string{"week", "month", "year", "once"}, o.Period):
		return inputError("obligation.period must be week, month, year or once")
	case o.AmountMinor != nil && *o.AmountMinor < 0:
		return inputError("obligation.amount_minor must not be negative")
	}
	return nil
}

// sentence is the obligation in one plain line.
func (o dealObligation) sentence(currency string) string {
	what := obligationWords[o.Kind]
	if o.AmountMinor != nil {
		cur := o.Currency
		if cur == "" {
			cur = currency
		}
		price := formatMoney(*o.AmountMinor, cur)
		if o.Period != "" && o.Period != "once" {
			price += "/" + o.Period
		}
		what += " (" + price + ")"
	}
	if o.TakesEffect != "" {
		what += " on " + o.TakesEffect
	}
	if o.due() {
		return what + " by " + o.DueBy
	}
	return what + " unless cancelled by " + o.CancelBy
}

// obligationHint proposes an obligation from a merchant email that says no
// charge comes before its cancel-by date. The agent confirms it against the
// email and seals it with `--kind evidence` (it is never sealed by itself).
func obligationHint(p merchantEmailParsed) *dealObligation {
	if p.CancelBy == "" || !p.ChargeAfterCancelBy {
		return nil
	}
	o := &dealObligation{Kind: "trial_conversion", CancelBy: p.CancelBy, Period: p.Period, Currency: p.RecurringCur, AmountMinor: p.RecurringMinor}
	return o
}

// dealDeadline is one obligation as it stands now.
type dealDeadline struct {
	DealID    string `json:"deal_id"`
	Step      int64  `json:"step"`
	CapsuleID string `json:"capsule_id"`
	Kind      string `json:"kind"`
	CancelBy  string `json:"cancel_by,omitempty"`
	DueBy     string `json:"due_by,omitempty"`
	// Status: open (on an open deal), carried_at_close (on a closed deal
	// that carried it), resolved (a later record resolves it), cancelled (a
	// cancel is sealed after it), or passed (the date went by and nothing
	// resolving it is sealed). Only open and carried_at_close are listed by
	// default; both stay open until resolved or the date passes.
	Status string `json:"status"`
	// Marking is the status as a listing and a calendar title show it.
	Marking string `json:"marking"`
	// Carried is true for an obligation the deal's close carried.
	Carried     bool   `json:"carried_at_close"`
	DealState   string `json:"deal_state"`
	DaysLeft    *int64 `json:"days_left,omitempty"`
	RemindOn    string `json:"remind_on,omitempty"`
	Text        string `json:"text"`
	Source      string `json:"source"`
	Confirmed   bool   `json:"source_merchant_confirmed"`
	CancelledAt string `json:"cancelled_at,omitempty"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
	// Holds says what the deal holds about this obligation, as of AsOf.
	// It never says what anyone did or did not do.
	Holds string `json:"holds"`
	AsOf  string `json:"as_of"`
	Note  string `json:"note"`
}

// Date is the deadline's date, for the email template.
func (d dealDeadline) Date() string { return d.date() }

// date is the deadline's date: its due date or its cancel-by date.
func (d dealDeadline) date() string {
	if d.DueBy != "" {
		return d.DueBy
	}
	return d.CancelBy
}

var deadlineMarkings = map[string]string{
	"open": "OPEN DEAL", "carried_at_close": "CARRIED AT CLOSE", "resolved": "RESOLVED",
	"cancelled": "CANCEL SEALED", "passed": "DATE PASSED",
}

// dealDeadlines lists a deal's obligations as of now. Whether a date has
// passed is computed here, when the listing or receipt is made, never
// sealed.
func dealDeadlines(events []sealedEvent, now time.Time, remindDays int) []dealDeadline {
	currency := events[0].Event.Open.Terms.Currency
	day := now.UTC().Format("2006-01-02")
	asOf := now.UTC().Format("2006-01-02T15:04:05Z")
	closeAt := finalClose(events)
	carried := map[string]bool{}
	dealState := "open"
	if closeAt >= 0 {
		dealState = "closed"
		for _, c := range events[closeAt].Event.Close.Carried {
			carried[c.CapsuleID] = true
		}
	}
	var out []dealDeadline
	for i, se := range events {
		ev := se.Event.Evidence
		if se.Event.Kind != "evidence" || ev == nil || ev.Obligation == nil {
			continue
		}
		o := ev.Obligation
		d := dealDeadline{
			DealID: se.Event.DealID, Step: se.Event.N, CapsuleID: se.CapsuleID, Kind: o.Kind, CancelBy: o.CancelBy, DueBy: o.DueBy,
			Text: o.sentence(currency), Source: ev.Source, Confirmed: ev.Email != nil && ev.Verified, Note: deadlineNotEnforced,
			Carried: carried[se.CapsuleID], DealState: dealState, AsOf: asOf,
		}
		for _, later := range events[i+1:] {
			if r := later.Event.Evidence; r != nil && r.Resolves == se.CapsuleID {
				d.Status, d.ResolvedBy = "resolved", later.CapsuleID
				how := "recorded by the agent"
				if r.Email != nil && r.Verified {
					how = "the merchant's own email, merchant-confirmed"
				}
				d.Holds = fmt.Sprintf("Resolved: a record sealed at %s resolves it (%s).", later.Event.At, how)
				break
			}
			// A cancel ends a cancel-by obligation, not something the user owes.
			if a := later.Event.Act; a != nil && a.Action == "cancel" && d.Status == "" && !o.due() {
				d.Status, d.CancelledAt = "cancelled", later.Event.At
				d.Holds = "A cancel is sealed on this deal at " + later.Event.At + "."
			}
		}
		when := "the last day to cancel is " + o.CancelBy
		if o.due() {
			when = "it is due by " + o.DueBy
		}
		switch {
		case d.Status != "":
		case day > o.date() && o.due():
			d.Status = "passed"
			d.Holds = fmt.Sprintf("The due date passed (as of %s). Nothing resolving it is sealed on this deal.", asOf)
		case day > o.date():
			d.Status = "passed"
			d.Holds = fmt.Sprintf("The date passed (as of %s). No cancellation confirmation is sealed on this deal.", asOf)
		default:
			d.Status = "open"
			d.Holds = strings.ToUpper(when[:1]) + when[1:] + "; nothing resolving it is sealed on this deal yet."
			if d.Carried {
				d.Status = "carried_at_close"
				d.Holds = "Carried at the close of this deal: " + when + "; nothing resolving it is sealed on this deal yet."
			}
			by, _ := time.Parse("2006-01-02", o.date())
			today, _ := time.Parse("2006-01-02", day)
			left := int64(by.Sub(today).Hours() / 24)
			d.DaysLeft = &left
			remind := by.AddDate(0, 0, -remindDays)
			if remind.Before(today) {
				remind = today
			}
			d.RemindOn = remind.Format("2006-01-02")
		}
		d.Marking = deadlineMarkings[d.Status]
		out = append(out, d)
	}
	return out
}

func openDeadlines(events []sealedEvent, today time.Time) []dealDeadline {
	var open []dealDeadline
	for _, d := range dealDeadlines(events, today, 2) {
		if d.Status == "open" {
			open = append(open, d)
		}
	}
	return open
}

// deadlinesICS renders the open deadlines (on an open deal, or carried at a
// close) and the open deals' expected close dates as an iCalendar file with
// an alarm, for the host's calendar or scheduler. No daemon runs here. Each
// title says which deal it is and, for a carried obligation, CARRIED AT
// CLOSE, so a reminder for a deal that looks finished is not read as a bug.
func deadlinesICS(ds []dealDeadline, deals []dealOpenListing, remindDays int, now time.Time) string {
	esc := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`)
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//capsulectl//deal deadlines//EN\r\nCALSCALE:GREGORIAN\r\n")
	event := func(uid, date, summary, description, alarm string) {
		day, _ := time.Parse("2006-01-02", date)
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:%s@capsulectl.deal\r\nDTSTAMP:%s\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\n",
			uid, now.UTC().Format("20060102T150405Z"), day.Format("20060102"), day.AddDate(0, 0, 1).Format("20060102"))
		fmt.Fprintf(&b, "SUMMARY:%s\r\nDESCRIPTION:%s\r\n", esc.Replace(summary), esc.Replace(description))
		fmt.Fprintf(&b, "BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:%s\r\nTRIGGER;RELATED=START:-P%dD\r\nEND:VALARM\r\nEND:VEVENT\r\n", esc.Replace(alarm), remindDays)
	}
	for _, d := range ds {
		uid := fmt.Sprintf("%s-%d", d.DealID, d.Step)
		if d.DueBy != "" {
			switch d.Status {
			case "open":
				event(uid, d.DueBy, "Due ("+d.DealID+"): "+d.Text, d.Holds+" "+d.Note, "Due date: "+d.DueBy)
			case "carried_at_close":
				event(uid, d.DueBy, "CARRIED AT CLOSE · "+d.DealID+" · due: "+d.Text,
					"This deal is closed; this due date was carried at its close and stays open until a record resolves it. "+d.Holds+" "+d.Note, "Carried at close, "+d.DealID+": due "+d.DueBy)
			}
			continue
		}
		switch d.Status {
		case "open":
			event(uid, d.CancelBy, "Last day to cancel ("+d.DealID+"): "+d.Text, d.Holds+" "+d.Note, "Cancel-by date: "+d.CancelBy)
		case "carried_at_close":
			event(uid, d.CancelBy, "CARRIED AT CLOSE · "+d.DealID+" · last day to cancel: "+d.Text,
				"This deal is closed; this cancel-by date was carried at its close and stays open until a record resolves it. "+d.Holds+" "+d.Note, "Carried at close, "+d.DealID+": cancel by "+d.CancelBy)
		}
	}
	for _, l := range deals {
		if l.ExpectCloseBy == "" {
			continue
		}
		event(l.DealID+"-close", l.ExpectCloseBy, "Close deal "+l.DealID+"? (open, expected to close by "+l.ExpectCloseBy+")",
			l.Text+" Close it with capsulectl deal close when it is resolved.", "Open deal "+l.DealID)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// dealCancellation is what a sealed cancel shows, and what it does not.
type dealCancellation struct {
	CancelStep  string   `json:"cancel_step"`
	SentAt      string   `json:"sent_at"`
	Authorized  bool     `json:"authorized"`
	CancelBy    string   `json:"cancel_by,omitempty"`
	InTime      *bool    `json:"before_cancel_by,omitempty"`
	Merchant    string   `json:"merchant"`
	ConfirmedAt string   `json:"merchant_confirmed_at,omitempty"`
	Steps       []string `json:"steps"`
	Proven      []string `json:"proven"`
	NotProven   []string `json:"not_proven"`
}

// dealCancellations reads every sealed cancel and the merchant's
// cancellation emails sealed after it into a plain statement of proof.
func dealCancellations(events []sealedEvent) []dealCancellation {
	var out []dealCancellation
	for i, se := range events {
		a := se.Event.Act
		if se.Event.Kind != "act" || a == nil || a.Action != "cancel" {
			continue
		}
		c := dealCancellation{CancelStep: se.CapsuleID, SentAt: se.Event.At, Authorized: !a.Unchecked, Steps: []string{se.CapsuleID}}
		var obligation *sealedEvent
		for j := i - 1; j >= 0; j-- {
			if ev := events[j].Event.Evidence; ev != nil && ev.Obligation != nil && !ev.Obligation.due() {
				obligation = &events[j]
				break
			}
		}
		sent := "Your agent recorded a cancel at " + se.Event.At + " (sealed on this device; the time is this device's clock, fixed by the next witnessed checkpoint when a witness is configured)."
		if !c.Authorized {
			sent = "Your agent recorded a cancel at " + se.Event.At + ", without a passing check or your sealed approval (" + a.Reason + ")."
		}
		c.Proven = append(c.Proven, sent)
		if obligation != nil {
			o := obligation.Event.Evidence.Obligation
			c.CancelBy = o.CancelBy
			in := se.Event.At[:10] <= o.CancelBy
			c.InTime = &in
			c.Steps = append([]string{obligation.CapsuleID}, c.Steps...)
			if in {
				c.Proven = append(c.Proven, "That recorded cancel is dated on or before the cancel-by date ("+o.CancelBy+").")
			} else {
				c.Proven = append(c.Proven, "That recorded cancel is dated AFTER the cancel-by date ("+o.CancelBy+").")
			}
		}
		// The merchant's own cancellation email, sealed after the cancel.
		var confirmed, unconfirmed *sealedEvent
		for j := i + 1; j < len(events); j++ {
			ev := events[j].Event.Evidence
			if ev == nil || ev.Email == nil || ev.Email.Parsed.Kind != "cancellation" {
				continue
			}
			if ev.Verified && confirmed == nil {
				confirmed = &events[j]
			} else if !ev.Verified && unconfirmed == nil {
				unconfirmed = &events[j]
			}
		}
		switch {
		case confirmed != nil:
			m := confirmed.Event.Evidence.Email
			at := m.Parsed.SentAt
			if at == "" {
				at = confirmed.Event.At
			}
			c.ConfirmedAt = at
			c.Merchant = "confirmed"
			c.Steps = append(c.Steps, confirmed.CapsuleID)
			c.Proven = append(c.Proven, "The merchant's own signed email, dated "+at+", says the cancellation went through ("+emailVerdictWords(m.DKIM)+").")
			if c.CancelBy != "" && len(at) >= 10 && at[:10] > c.CancelBy {
				c.NotProven = append(c.NotProven, "The merchant's email is dated after the cancel-by date; it does not show the merchant received the cancel in time.")
			}
		case unconfirmed != nil:
			c.Merchant = "not_confirmed"
			c.Steps = append(c.Steps, unconfirmed.CapsuleID)
			c.NotProven = append(c.NotProven, "A cancellation email was sealed, but it is "+emailVerdictWords(unconfirmed.Event.Evidence.Email.DKIM)+".")
		default:
			c.Merchant = "none"
			c.NotProven = append(c.NotProven, "No cancellation email from the merchant has been sealed, so nothing independent shows the merchant received the cancel.")
		}
		c.NotProven = append(c.NotProven,
			"That you will not be charged again: only the merchant's own records can show that; a later charge would show on your statement.",
			"That the merchant acted on the cancel, beyond what its own email says.")
		out = append(out, c)
	}
	return out
}

func dealDeadlinesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "deadlines", Short: "List cancel-by dates and open deals (one deal, or every deal in the profile) as JSON, and optionally as a calendar file with reminders", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		dealID, _ := c.Flags().GetString("deal")
		icsPath, _ := c.Flags().GetString("ics")
		remind, _ := c.Flags().GetInt("remind-days")
		all, _ := c.Flags().GetBool("all")
		if remind < 0 || remind > 60 {
			return inputError("--remind-days must be between 0 and 60")
		}
		p, err := selected(c)
		if err != nil {
			return err
		}
		ctx := c.Context()
		s, err := openDealSession(ctx, p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		ids := []string{dealID}
		if dealID == "" {
			if ids, err = s.dealIDs(ctx); err != nil {
				return err
			}
		}
		now := dealClock()
		list := []dealDeadline{}
		deals := []dealOpenListing{}
		for _, id := range ids {
			if saleIDPattern.MatchString(id) {
				continue // a sale's log is not a deal: its threads are
			}
			events, err := s.loadOther(ctx, id)
			if err != nil {
				return err
			}
			for _, d := range dealDeadlines(events, now, remind) {
				if all || d.Status == "open" || d.Status == "carried_at_close" {
					list = append(list, d)
				}
			}
			if l := openDealListing(events, now); l != nil {
				deals = append(deals, *l)
			}
		}
		slices.SortStableFunc(list, func(a, b dealDeadline) int { return strings.Compare(a.date(), b.date()) })
		slices.SortStableFunc(deals, func(a, b dealOpenListing) int { return strings.Compare(a.ExpectCloseBy, b.ExpectCloseBy) })
		out := map[string]any{
			"deadlines": list, "open_deals": deals, "enforced": false, "note": deadlineNotEnforced,
			"as_of":  now.UTC().Format("2006-01-02T15:04:05Z"),
			"states": "deadlines: OPEN DEAL (on an open deal), CARRIED AT CLOSE (on a closed deal, still open), RESOLVED, CANCEL SEALED and DATE PASSED (with --all). open_deals: deals with no close sealed, against their expected close date.",
		}
		if icsPath != "" {
			if err := atomicFile(icsPath, []byte(deadlinesICS(list, deals, remind, now)), false); err != nil {
				return err
			}
			out["ics"] = icsPath
		}
		return output(c, out)
	}}
	cmd.Flags().String("deal", "", "Only this deal (default: every deal in the profile)")
	cmd.Flags().String("ics", "", "Also write the open dates (and open deals' expected close dates) to this new iCalendar file, each with a reminder")
	cmd.Flags().Int("remind-days", 2, "Remind this many days before each cancel-by date")
	cmd.Flags().Bool("all", false, "Include dates that were resolved, cancelled or passed")
	return cmd
}

// loadOther opens and verifies one deal of the session, closing the deal
// opened before it.
func (s *dealSession) loadOther(ctx context.Context, dealID string) ([]sealedEvent, error) {
	if s.t != nil {
		if err := s.t.close(); err != nil {
			return nil, err
		}
		s.t = nil
	}
	if err := s.useDeal(ctx, dealID, false); err != nil {
		return nil, err
	}
	return s.load(ctx, dealID)
}
