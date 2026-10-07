package cli

import (
	"fmt"
	"strings"
)

// dealAuthorityLayer is one authority layer an action relied on (or that was
// present when it was done without one), in the user's words, with its time
// and the step it was read from.
type dealAuthorityLayer struct {
	// Layer is task_authority, evaluation, user_approval, card_answer,
	// platform_approval or action.
	Layer string `json:"layer"`
	At    string `json:"at"`
	Text  string `json:"text"`
	Note  string `json:"note,omitempty"`
	Step  string `json:"step"`
}

// dealAuthorityBlock is the AUTHORITY block for one action of a typed deal:
// every layer, in order, each with its own time. The layers are never
// collapsed into "you approved": the task authority, the rules check, the
// user's own answer and a platform's own approval are separate facts.
type dealAuthorityBlock struct {
	Action  string               `json:"action"`
	Step    string               `json:"step"`
	Covered bool                 `json:"covered"`
	Layers  []dealAuthorityLayer `json:"layers"`
}

// buildDealAuthority reads the AUTHORITY block of every action (and every
// disclosure that needed approval) of a deal sealed in typed records, from
// the sealed steps alone. A deal sealed in x-deal-v0 records has none: its
// report keeps its own wording.
func buildDealAuthority(events []sealedEvent) []dealAuthorityBlock {
	if len(events) == 0 || dealRecordSet(dealEvent{}, events) != recordsTyped {
		return nil
	}
	byID := map[string]int{}
	for i, se := range events {
		byID[se.CapsuleID] = i
	}
	currency := events[0].Event.Open.Terms.Currency
	var blocks []dealAuthorityBlock
	for i, se := range events {
		e := se.Event
		var action, authorizedBy, reason string
		var amount *int64
		covered := false
		switch {
		case e.Kind == "act" && e.Act != nil:
			action, authorizedBy, amount = e.Act.Action, e.Act.AuthorizedBy, e.Act.AmountMinor
			covered, reason = !e.Act.Unchecked, e.Act.Reason
		case e.Kind == "disclosure" && e.Disclosure != nil && e.Disclosure.AuthorizedBy != "":
			action, authorizedBy, covered = "share", e.Disclosure.AuthorizedBy, true
		default:
			continue
		}
		// The evaluation the step relied on: itself on a DO, the one the
		// user's answer was to on an ASK; for a step done without either,
		// the latest check of the same action before it.
		evaluation, approval := "", ""
		if j, ok := byID[authorizedBy]; ok && authorizedBy != "" {
			if a := events[j].Event.Approval; a != nil {
				evaluation, approval = a.Check, authorizedBy
			} else {
				evaluation = authorizedBy
			}
		} else if e.Kind == "act" {
			for k := i - 1; k >= 0; k-- {
				if c := events[k].Event.Check; events[k].Event.Kind == "check" && c != nil && c.Action == action {
					evaluation = events[k].CapsuleID
					break
				}
			}
		}
		b := dealAuthorityBlock{Step: se.CapsuleID, Covered: covered}
		if ev, ok := byID[evaluation]; ok && evaluation != "" {
			if layer, ok := taskAuthorityLayer(events[:ev]); ok {
				b.Layers = append(b.Layers, layer)
			}
			b.Layers = append(b.Layers, evaluationLayer(events[ev]))
			if !covered {
				// What answered the check, if anything, when it did not count.
				for _, later := range events[ev+1 : i] {
					if a := later.Event.Approval; later.Event.Kind == "approval" && a != nil && a.Check == evaluation && a.Approver == "agent_card" {
						b.Layers = append(b.Layers, dealAuthorityLayer{Layer: "card_answer", At: later.Event.At, Step: later.CapsuleID,
							Text: "The card was answered (" + a.Choice + ") with no words of yours", Note: "That does not answer the check: it needs your own words."})
					}
				}
			}
			if j, ok := byID[approval]; ok && approval != "" {
				b.Layers = append(b.Layers, userApprovalLayer(events[j]))
			}
			for _, later := range events[ev+1 : i] {
				if p := later.Event.Platform; later.Event.Kind == "platform_approval" && p != nil && p.Check == evaluation {
					b.Layers = append(b.Layers, platformLayer(later, events[ev].Event.Check.Verdict == "pass", amount, currency))
				}
			}
		}
		if e.Kind == "disclosure" {
			b.Action = "told " + e.Disclosure.recipientWord() + ": " + e.Disclosure.classList()
		} else {
			b.Action = actText(*e.Act, currency)
		}
		done := dealAuthorityLayer{Layer: "action", At: e.At, Step: se.CapsuleID, Text: "Done: " + b.Action}
		if !covered {
			done.Text, done.Note = "Done without a passing check or your approval: "+b.Action, reason
		}
		b.Layers = append(b.Layers, done)
		blocks = append(blocks, b)
	}
	return blocks
}

// taskAuthorityLayer is the user's task authority in force after the given
// steps: what they asked when the deal opened, or the new limits they last
// confirmed in their own words.
func taskAuthorityLayer(events []sealedEvent) (dealAuthorityLayer, bool) {
	for k := len(events) - 1; k >= 0; k-- {
		se := events[k]
		if ta := se.Event.TaskAuthority; se.Event.Kind == "task_authority" && ta != nil {
			return dealAuthorityLayer{Layer: "task_authority", At: se.Event.At, Step: se.CapsuleID,
				Text: fmt.Sprintf("Your request recorded: %q", ta.Verbatim)}, true
		}
		if a := se.Event.Approval; a != nil && a.Choice == "confirm_limits" && a.Proceed && a.Limits != nil {
			return dealAuthorityLayer{Layer: "task_authority", At: se.Event.At, Step: se.CapsuleID,
				Text: fmt.Sprintf("You confirmed new limits: %s → %s (%q)", a.Limits.Previous.String(), a.Limits.New.String(), a.Said)}, true
		}
	}
	return dealAuthorityLayer{}, false
}

// evaluationLayer is the rules check: DO, within the user's rules, or ASK
// with every reason it asked.
func evaluationLayer(se sealedEvent) dealAuthorityLayer {
	c := se.Event.Check
	l := dealAuthorityLayer{Layer: "evaluation", At: se.Event.At, Step: se.CapsuleID}
	if c.Verdict == "pass" {
		l.Text = "Your rules: DO, within your rules (no approval needed)"
		return l
	}
	var reasons []string
	for _, d := range c.Differences {
		if d.Text != "" {
			reasons = append(reasons, d.Text)
		}
	}
	l.Text = "Your rules: ASK, because " + strings.Join(reasons, " · ")
	return l
}

// userApprovalLayer is the user's own answer to the check, in their words,
// given on the card the check showed.
func userApprovalLayer(se sealedEvent) dealAuthorityLayer {
	a := se.Event.Approval
	l := dealAuthorityLayer{Layer: "user_approval", At: se.Event.At, Step: se.CapsuleID, Text: fmt.Sprintf("Approved by you: %q", a.Said)}
	if a.ShownCard != "" {
		l.Note = "On the card the check showed you."
	}
	return l
}

// platformLayer is a platform's own approval prompt, as observed: what it
// displayed and the reply it returned. It is that platform's own check and
// never the answer to the user's rules.
func platformLayer(se sealedEvent, do bool, amount *int64, currency string) dealAuthorityLayer {
	p := se.Event.Platform
	text := fmt.Sprintf("%s asked separately (%s): %q", p.Platform, p.Mechanism, p.DisplayedText)
	if p.UserText != "" {
		text += fmt.Sprintf("; the reply it returned: %q", p.UserText)
	}
	note := "This is " + p.Platform + "'s own check, recorded as observed; it does not answer your rules."
	if do {
		note = "This is " + p.Platform + "'s own check, recorded as observed. Your rules needed no approval: DO, within your rules."
	}
	if amount != nil && p.AmountMinor != nil && *amount != *p.AmountMinor {
		cur := p.Currency
		if cur == "" {
			cur = currency
		}
		note += fmt.Sprintf(" It stated %s, not the %s done.", formatMoney(*p.AmountMinor, cur), formatMoney(*amount, currency))
	}
	return dealAuthorityLayer{Layer: "platform_approval", At: p.ObservedAt, Step: se.CapsuleID, Text: text, Note: note}
}
