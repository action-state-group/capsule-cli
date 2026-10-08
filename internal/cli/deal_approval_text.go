package cli

import (
	"strings"
	"time"
)

// approvalVerbs name an action at the start of the approval text.
var approvalVerbs = map[string]string{
	"pay": "Pay", "commit": "Confirm", "sign": "Sign", "cancel": "Cancel",
	"share_contact": "Share your contact details", "share_credentials": "Share a login or code",
}

// dealApprovalText is the message an agent shows the user when it asks to go
// ahead: whether the user's rules were checked (and by which ruleset), what,
// who, how much, how it is paid, and what the check found. It is generated
// from the sealed check so that the agent's request for approval carries the
// check's own words. When the check goes stale stays in the sealed record and
// the check's output (checked_at, stale_after_minutes), out of the prompt.
func dealApprovalText(state dealState, snap dealSnapshot, result dealCheckResult, checkedAt string, staleAfter time.Duration) string {
	what := approvalVerbs[snap.Action]
	if snap.Description != "" {
		what += ": " + snap.Description
	} else if state.terms.Item != "" {
		what += ": " + state.terms.Item
	}
	who := state.who
	if snap.Who != nil {
		if snap.Who.Name != "" {
			who.Name = snap.Who.Name
		}
		if snap.Who.Domain != "" {
			who.Domain = snap.Who.Domain
		}
		if snap.Who.Payee != "" {
			who.Payee = snap.Who.Payee
		}
	}
	to := who.Payee
	if to == "" {
		to = who.Name
	}
	if who.Domain != "" {
		to += " (" + who.Domain + ")"
	}
	currency := state.terms.Currency
	if snap.Terms != nil && snap.Terms.Currency != "" {
		currency = snap.Terms.Currency
	}
	recourse := state.recourse
	if snap.Recourse != nil {
		if snap.Recourse.Rail != "" {
			recourse.Rail = snap.Recourse.Rail
		}
		if snap.Recourse.Refundable != nil {
			recourse.Refundable = snap.Recourse.Refundable
		}
	}
	var parts []string
	if snap.AmountMinor != nil {
		parts = append(parts, formatMoney(*snap.AmountMinor, currency))
	}
	if strings.TrimSpace(to) != "" {
		parts = append(parts, "to "+to)
	}
	if recourse.Rail != "" {
		parts = append(parts, "by "+railName(recourse.Rail))
	}
	if recourse.Refundable != nil {
		if *recourse.Refundable {
			parts = append(parts, "refundable")
		} else {
			parts = append(parts, "not refundable")
		}
	}
	line := what
	if len(parts) > 0 {
		line += " · " + strings.Join(parts, " · ")
	}
	// What the user chose and what the agent chose for them, told apart, so
	// a choice the agent made is never presented as the user's.
	if len(result.Picked) > 0 {
		chosen := "The agent picked, not you: " + attributeText(result.Picked) + "."
		if len(result.Asked) > 0 {
			chosen = "You asked for: " + attributeText(result.Asked) + ". " + chosen
		}
		line += "\n" + chosen
	}
	finding := "Before you go ahead: no differences."
	if result.Verdict != "pass" {
		var texts []string
		for _, d := range result.Differences {
			if d.Text != "" {
				texts = append(texts, d.Text)
			}
		}
		finding = "Before you go ahead: flagged: " + strings.Join(texts, " · ") + "."
	}
	if len(result.Unverified) > 0 {
		finding += " Unverified: " + strings.Join(result.Unverified, " · ") + "."
	}
	if state.open.Demo {
		line = "DEMO · " + line
	}
	text := line + "\n" + finding
	if rules := rulesStatusLine(result.Rules); rules != "" {
		text = rules + "\n" + text
	}
	return text
}
