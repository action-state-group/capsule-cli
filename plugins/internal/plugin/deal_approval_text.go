package plugin

import (
	"slices"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// approvalVerbs name an action at the start of the approval text.
var approvalVerbs = map[string]string{
	"pay": "Pay", "commit": "Confirm", "sign": "Sign", "cancel": "Cancel",
	"share_contact": "Share your contact details", "share_credentials": "Share a login or code",
}

// dealApprovalText is the one block an agent shows the user when it asks to
// go ahead, and on a pause the card the user's answer is sealed against. It
// says each fact once: what, how much, to whom and how it is paid; on a
// share before a payment, that it is the first of two steps; then one
// bullet for whether the user's rules were checked and one per thing the
// check found.
// It is built from the check's own fields (differences, picks, unverified
// claims, rules, recipient), so the agent's request carries the check's
// words. The one-tap options are the host's to render, never text here. When
// the check goes stale stays in the sealed record and the check's output
// (checked_at, stale_after_minutes), out of the prompt.
func dealApprovalText(state dealState, snap dealSnapshot, result dealCheckResult, events []sealedEvent) string {
	headline := approvalHeadline(state, snap)
	lines := []string{headline}
	if result.Verdict != "pass" {
		if step := shareBeforePay(state, snap, events); step != "" {
			lines = append(lines, step)
		}
	}
	if rules := rulesStatusLine(result.Rules); rules != "" {
		lines = append(lines, "• "+rules)
	}
	for _, f := range approvalFindings(result, headline) {
		lines = append(lines, "• "+f)
	}
	return strings.Join(lines, "\n")
}

// shareBeforePay is, on a share in a deal that will still pay, the line
// saying the share is the first of two steps: the deal allows a pay and no
// pay is sealed yet. The amount is the deal's total as it stands, when it
// has one.
func shareBeforePay(state dealState, snap dealSnapshot, events []sealedEvent) string {
	if snap.Action != "share_contact" && snap.Action != "share_credentials" || !slices.Contains(state.intent.Allowed, "pay") {
		return ""
	}
	for _, se := range events {
		if se.Event.Kind == "act" && se.Event.Act != nil && se.Event.Act.Action == "pay" {
			return ""
		}
	}
	terms := state.terms
	if snap.Terms != nil {
		terms = overlayTerms(terms, *snap.Terms)
	}
	if terms.PriceMinor == nil {
		return "Step 1 of 2: I will ask again before paying."
	}
	return "Step 1 of 2: I will ask again before paying " + formatMoney(*terms.PriceMinor, terms.Currency) + "."
}

// approvalHeadline is the block's first line: what, how much, to whom, by
// what rail, and whether it is refundable.
func approvalHeadline(state dealState, snap dealSnapshot) string {
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
	// A representation: the exact words, and to whom, are what the user
	// approves.
	if rp := snap.Representing; snap.Action == representAction && rp != nil {
		line := "Tell " + strings.TrimSpace(to) + ", about the " + classWord(rp.Class) + ": \"" + rp.Text + "\""
		if state.open.Demo {
			line = "DEMO · " + line
		}
		return line
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
	if state.open.Demo {
		line = "DEMO · " + line
	}
	return line
}

// approvalFindings are the block's lines after the headline and the rules,
// one per distinct finding. A pass leaves out the recipient line: a first
// dealing that did not pause is not news. Findings the block already says
// another way are folded: the first dealing with a recipient and every
// detail told to them for the first time make one line; the rules' own
// not-checked difference is the rules line; a note the headline already
// carries is not repeated.
func approvalFindings(r dealCheckResult, headline string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(text string) {
		if text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	if r.Verdict != "pass" {
		add(recipientFinding(r.Recipient))
	}
	for _, line := range picksFindings(r) {
		add(line)
	}
	for _, d := range r.Differences {
		switch d.Rule {
		case "first_disclosure", "rules_not_checked":
			continue
		}
		add(d.Text)
	}
	for _, n := range r.Notes {
		if !strings.Contains(headline, n) {
			add(n)
		}
	}
	if len(r.Unverified) > 0 {
		add("Unverified: " + strings.Join(r.Unverified, " · "))
	}
	return out
}

// picksFindings tell what the user chose and what the agent chose for them
// apart, one line each, so a choice the agent made is never presented as the
// user's. A pick the check paused on is its own difference line, so it is
// not said here.
func picksFindings(r dealCheckResult) []string {
	if len(r.Picked) == 0 {
		return nil
	}
	paused := map[string]bool{}
	for _, d := range r.Differences {
		if d.Rule == "agent_picked" {
			paused[d.Field] = true
		}
	}
	var minor []cli.DealAttribute
	for _, a := range r.Picked {
		if !paused[a.Field] {
			minor = append(minor, a)
		}
	}
	var said []string
	if len(r.Asked) > 0 {
		said = append(said, "You asked for: "+attributeText(r.Asked))
	}
	if len(minor) > 0 {
		said = append(said, "The agent picked, not you: "+attributeText(minor))
	}
	return said
}

// recipientFinding is the one line about who is being dealt with: a first
// dealing, the details they have never had, and, on a profile with no
// counterparty history yet, why everyone reads as first-time.
func recipientFinding(rc *dealRecipient) string {
	if rc == nil || (!rc.FirstTime && len(rc.First) == 0) {
		return ""
	}
	words := make([]string, len(rc.First))
	for i, c := range rc.First {
		words[i] = classWord(c)
	}
	var line string
	switch {
	case rc.FirstTime && len(words) > 0:
		line = "First time dealing with " + rc.Name + ", and they have never had your " + listWords(words, "or")
	case rc.FirstTime:
		line = "First time dealing with " + rc.Name
	default:
		line = "First time giving " + rc.Name + " your " + listWords(words, "and")
	}
	if rc.NewProfile {
		line += " (" + newProfileNote + ")"
	}
	return line
}

// listWords joins words as a sentence does: "a", "a and b", "a, b and c".
func listWords(words []string, conj string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}
