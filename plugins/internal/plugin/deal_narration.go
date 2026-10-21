package plugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
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
			limit = formatMoney(*a.Limits.New.MaxTotalMinor, state.terms.Currency)
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
					amount = formatMoney(*se.Event.Snapshot.AmountMinor, state.terms.Currency)
				}
			}
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
	var question string
	switch {
	case proposed.MaxTotalMinor != nil && inForce.MaxTotalMinor != nil && *proposed.MaxTotalMinor > *inForce.MaxTotalMinor:
		question = w.Line("card.question.limit", cli.V{"from": formatMoney(*inForce.MaxTotalMinor, currency), "to": formatMoney(*proposed.MaxTotalMinor, currency)})
	case proposed.MinTotalMinor != nil && inForce.MinTotalMinor != nil && *proposed.MinTotalMinor < *inForce.MinTotalMinor:
		question = w.Line("card.question.floor", cli.V{"from": formatMoney(*inForce.MinTotalMinor, currency), "to": formatMoney(*proposed.MinTotalMinor, currency)})
	default:
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
		question = w.Line("card.question.permission", cli.V{"action": listWords(actions, "and")})
	}
	ask := cli.Ask{Tag: w.Line("card.tag.ask", nil), Question: question, Reply: "words",
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
	currency := state.terms.Currency
	var paid *int64
	for _, se := range events {
		if a := se.Event.Act; se.Event.Kind == "act" && a != nil && a.Action == "pay" && a.AmountMinor != nil && a.Direction != "in" {
			total := *a.AmountMinor
			if paid != nil {
				total += *paid
			}
			paid = &total
			if a.Currency != "" {
				currency = a.Currency
			}
		}
	}
	var lines []string
	item := shortTitle(state.terms.Item)
	if paid != nil {
		amount := formatMoney(*paid, currency)
		switch limit := state.intent.MaxTotalMinor; {
		case item != "" && limit != nil && *paid <= *limit:
			lines = append(lines, w.Line("say.receipt.done.within", cli.V{"item": item, "amount": amount, "limit": formatMoney(*limit, currency)}))
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
		key := "say.receipt.what.other"
		if check, _, ok := checkedCard(events, a.Check); ok {
			switch check.Event.Check.Action {
			case "pay":
				key = "say.receipt.what.pay"
			case "share_contact", "share_credentials":
				key = "say.receipt.what.share"
			}
		}
		if text := w.Line(key, nil); !containsString(what, text) || key == "say.receipt.what.pay" {
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
// "View details" and the page to send.
func report(w cli.Wording, lines []string, htmlPath string) cli.Narration {
	n := cli.Narration{Voice: cli.VoiceReport}
	if htmlPath != "" {
		lines = append(lines, w.Line("report.view_details", nil))
		n.Attach = &cli.Attach{Path: htmlPath, MediaType: "text/html", Label: w.Line("report.view_details", nil)}
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
	page, err := dealReportHTML(b, dealNotCountersigned(), dealVerifyCommand(htmlPath))
	if err != nil {
		return err
	}
	return cli.AtomicFile(htmlPath, []byte(page), false)
}

// stepReport is a step's report, with its receipt page when htmlPath is
// given. The step is already sealed: a page that fails is said in
// attach_error, never as a failed step.
func (s *dealSession) stepReport(ctx context.Context, out map[string]any, events []sealedEvent, htmlPath string, lines func(cli.Wording) []string) map[string]any {
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
		return report(w, said, written)
	})
}

// closeKeys name a close's final-receipt line by its outcome; an open
// outcome (pending) has none: the deal goes on.
var closeKeys = map[string]string{"completed": "say.close.received", "not_selected": "say.close.called_off", "mismatch": "say.close.mismatch"}

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
					lines = append([]string{w.Line(key, nil)}, lines...)
				}
			}
		}
		return report(w, lines, htmlPath)
	})
}

// speakFor narrates out with the selected profile's wording, or the
// built-in pack when no profile is selected.
func speakFor(c *cobra.Command, out map[string]any, build func(cli.Wording) cli.Narration) map[string]any {
	p, err := cli.Selected(c)
	if err != nil {
		return cli.Narrate(out, build(cli.BuiltinWording()))
	}
	return cli.Speak(out, p, build)
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
