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
//   - PROVING A NEGATIVE: "I cancelled on the 4th". A sealed cancel action
//     plus the merchant's own cancellation email, bound to the deal that
//     created the obligation. The report says exactly what that proves and
//     what it does not.

// dealObligation is a commitment that takes effect when a date passes.
type dealObligation struct {
	// Kind: trial_conversion (a trial becomes paid), renewal (a
	// subscription renews), cancel_window (free cancellation ends) or
	// payment_due (a payment is taken on a date).
	Kind string `json:"kind"`
	// CancelBy is the last day to cancel (YYYY-MM-DD).
	CancelBy string `json:"cancel_by"`
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

var obligationKinds = []string{"trial_conversion", "renewal", "cancel_window", "payment_due"}

var obligationWords = map[string]string{
	"trial_conversion": "the trial becomes paid",
	"renewal":          "the subscription renews",
	"cancel_window":    "free cancellation ends",
	"payment_due":      "a payment is taken",
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
	case !validDate(o.CancelBy):
		return inputError("obligation.cancel_by must be a date, YYYY-MM-DD")
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
	CancelBy  string `json:"cancel_by"`
	// Status: open, passed (the date went by with no cancel sealed) or
	// cancelled (a cancel was sealed after it; see the cancellation proof
	// for what that shows).
	Status      string `json:"status"`
	DaysLeft    *int64 `json:"days_left,omitempty"`
	RemindOn    string `json:"remind_on,omitempty"`
	Text        string `json:"text"`
	Source      string `json:"source"`
	Confirmed   bool   `json:"source_merchant_confirmed"`
	CancelledAt string `json:"cancelled_at,omitempty"`
	Note        string `json:"note"`
}

// dealDeadlines lists a deal's obligations as of today. A cancel act sealed
// after an obligation cancels it in the deal's record; a date that passed
// with none is "passed".
func dealDeadlines(events []sealedEvent, today time.Time, remindDays int) []dealDeadline {
	currency := events[0].Event.Open.Terms.Currency
	day := today.UTC().Format("2006-01-02")
	var out []dealDeadline
	for i, se := range events {
		ev := se.Event.Evidence
		if se.Event.Kind != "evidence" || ev == nil || ev.Obligation == nil {
			continue
		}
		o := ev.Obligation
		d := dealDeadline{
			DealID: se.Event.DealID, Step: se.Event.N, CapsuleID: se.CapsuleID, Kind: o.Kind, CancelBy: o.CancelBy,
			Text: o.sentence(currency), Source: ev.Source, Confirmed: ev.Email != nil && ev.Verified, Note: deadlineNotEnforced,
		}
		for _, later := range events[i+1:] {
			if a := later.Event.Act; a != nil && a.Action == "cancel" {
				d.Status, d.CancelledAt = "cancelled", later.Event.At
				break
			}
		}
		if d.Status == "" {
			if day > o.CancelBy {
				d.Status = "passed"
			} else {
				d.Status = "open"
				by, _ := time.Parse("2006-01-02", o.CancelBy)
				now, _ := time.Parse("2006-01-02", day)
				left := int64(by.Sub(now).Hours() / 24)
				d.DaysLeft = &left
				remind := by.AddDate(0, 0, -remindDays)
				if remind.Before(now) {
					remind = now
				}
				d.RemindOn = remind.Format("2006-01-02")
			}
		}
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

// deadlinesICS renders open deadlines as an iCalendar file with an alarm,
// for the host's calendar or scheduler. No daemon runs here.
func deadlinesICS(ds []dealDeadline, remindDays int, now time.Time) string {
	esc := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`)
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//capsulectl//deal deadlines//EN\r\nCALSCALE:GREGORIAN\r\n")
	for _, d := range ds {
		if d.Status != "open" {
			continue
		}
		by, _ := time.Parse("2006-01-02", d.CancelBy)
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:%s-%d@capsulectl.deal\r\nDTSTAMP:%s\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\n",
			d.DealID, d.Step, now.UTC().Format("20060102T150405Z"), by.Format("20060102"), by.AddDate(0, 0, 1).Format("20060102"))
		fmt.Fprintf(&b, "SUMMARY:%s\r\nDESCRIPTION:%s\r\n", esc.Replace("Last day to cancel: "+d.Text), esc.Replace(d.Note+" Deal "+d.DealID+", step "+fmt.Sprint(d.Step)+"."))
		fmt.Fprintf(&b, "BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:%s\r\nTRIGGER;RELATED=START:-P%dD\r\nEND:VALARM\r\nEND:VEVENT\r\n", esc.Replace("Cancel-by date: "+d.CancelBy), remindDays)
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
			if ev := events[j].Event.Evidence; ev != nil && ev.Obligation != nil {
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
	cmd := &cobra.Command{Use: "deadlines", Short: "List cancel-by dates (one deal, or every deal in the profile) as JSON, and optionally as a calendar file with reminders", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
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
		for _, id := range ids {
			events, err := s.loadOther(ctx, id)
			if err != nil {
				return err
			}
			for _, d := range dealDeadlines(events, now, remind) {
				if all || d.Status == "open" {
					list = append(list, d)
				}
			}
		}
		slices.SortStableFunc(list, func(a, b dealDeadline) int { return strings.Compare(a.CancelBy, b.CancelBy) })
		out := map[string]any{"deadlines": list, "enforced": false, "note": deadlineNotEnforced, "as_of": now.UTC().Format("2006-01-02")}
		if icsPath != "" {
			if err := atomicFile(icsPath, []byte(deadlinesICS(list, remind, now)), false); err != nil {
				return err
			}
			out["ics"] = icsPath
		}
		return output(c, out)
	}}
	cmd.Flags().String("deal", "", "Only this deal (default: every deal in the profile)")
	cmd.Flags().String("ics", "", "Also write the open dates to this new iCalendar file, each with a reminder")
	cmd.Flags().Int("remind-days", 2, "Remind this many days before each cancel-by date")
	cmd.Flags().Bool("all", false, "Include dates that passed or were cancelled")
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
