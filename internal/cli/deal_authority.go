package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// dealAuthorityOrder says how an AUTHORITY block is ordered, wherever it is
// shown.
const dealAuthorityOrder = "Each action's layers are listed in the order it relied on them (its sealed authority basis), each with its own time."

// dealAuthorityLayer is one authority layer an action relied on, in the
// user's words, with its time and the step it was read from.
type dealAuthorityLayer struct {
	// Layer is task_authority, evaluation, user_approval,
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
// disclosure that needed approval) of a deal sealed in typed records from
// the sealed records themselves: records[i] is step i's record as sealed
// (the log's own bytes; loading a deal refuses any step whose record
// re-derives differently). A covered action's block is its
// action-record/v0's evaluation_ref and authority_basis, in that order,
// scope included; nothing is re-decided here. An action done without
// authority is sealed as an action-outcome/v0 with no basis: its block is
// the action and the reason it was not covered. A deal sealed in x-deal-v0
// records has none: its report keeps its own wording.
func buildDealAuthority(events []sealedEvent, records [][]byte) []dealAuthorityBlock {
	if len(events) == 0 || len(records) != len(events) || dealRecordSet(dealEvent{}, events) != recordsTyped {
		return nil
	}
	byDigest := map[string]sealedEvent{}
	for _, se := range events {
		byDigest[se.Digest] = se
	}
	currency := events[0].Event.Open.Terms.Currency
	var blocks []dealAuthorityBlock
	for i, se := range events {
		var rec struct {
			Type string `json:"type"`
			Body struct {
				AmountMinor   *int64    `json:"amount_minor"`
				EvaluationRef sealedRef `json:"evaluation_ref"`
				Basis         []struct {
					Type  string    `json:"type"`
					Ref   sealedRef `json:"ref"`
					Scope string    `json:"scope"`
				} `json:"authority_basis"`
			} `json:"body"`
		}
		if json.Unmarshal(records[i], &rec) != nil {
			continue
		}
		e := se.Event
		switch {
		case rec.Type == typeActionRecord:
		case rec.Type == typeActionOutcome && e.Kind == "act" && e.Act != nil && e.Act.Unchecked:
			what := actText(*e.Act, currency)
			blocks = append(blocks, dealAuthorityBlock{Action: what, Step: se.CapsuleID, Layers: []dealAuthorityLayer{{
				Layer: "action", At: e.At, Step: se.CapsuleID, Text: "Done without a passing check or your approval: " + what, Note: e.Act.Reason}}})
			continue
		default:
			continue
		}
		b := dealAuthorityBlock{Step: se.CapsuleID, Covered: true}
		if e.Kind == "disclosure" {
			b.Action = "told " + e.Disclosure.recipientWord() + ": " + e.Disclosure.classList()
		} else {
			b.Action = actText(*e.Act, currency)
		}
		evaluation, haveEvaluation := byDigest[rec.Body.EvaluationRef.Digest]
		for j, entry := range rec.Body.Basis {
			ref, ok := byDigest[entry.Ref.Digest]
			if !ok {
				continue
			}
			switch entry.Type {
			case "task_authority":
				b.Layers = append(b.Layers, taskAuthorityLayer(ref))
			case "user_approval":
				b.Layers = append(b.Layers, userApprovalLayer(ref))
			case "platform_approval":
				do := haveEvaluation && evaluation.Event.Check != nil && evaluation.Event.Check.Verdict == "pass"
				b.Layers = append(b.Layers, platformLayer(ref, do, entry.Scope == "mismatch", rec.Body.AmountMinor, currency))
			}
			// The evaluation the action relied on follows the task authority
			// it was made under.
			if j == 0 && haveEvaluation {
				b.Layers = append(b.Layers, evaluationLayer(evaluation))
			}
		}
		b.Layers = append(b.Layers, dealAuthorityLayer{Layer: "action", At: e.At, Step: se.CapsuleID, Text: "Done: " + b.Action})
		blocks = append(blocks, b)
	}
	return blocks
}

// sealedRef is a typed record ref, as sealed.
type sealedRef struct {
	Digest string `json:"digest"`
}

// taskAuthorityLayer is the task authority a basis names: what the user
// asked when the deal opened, or the new limits they confirmed in their own
// words.
func taskAuthorityLayer(se sealedEvent) dealAuthorityLayer {
	l := dealAuthorityLayer{Layer: "task_authority", At: se.Event.At, Step: se.CapsuleID}
	switch a := se.Event.Approval; {
	case se.Event.TaskAuthority != nil:
		l.Text = fmt.Sprintf("Your request recorded: %q", se.Event.TaskAuthority.Verbatim)
	case a != nil && a.Limits != nil:
		l.Text = fmt.Sprintf("You confirmed new limits: %s → %s (%q)", a.Limits.Previous.String(), a.Limits.New.String(), a.Said)
	default:
		l.Text = "Your request recorded"
	}
	return l
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
// never the answer to the user's rules. mismatch is the sealed basis entry's
// scope: the amount it stated is not the amount done.
func platformLayer(se sealedEvent, do, mismatch bool, amount *int64, currency string) dealAuthorityLayer {
	p := se.Event.Platform
	text := fmt.Sprintf("%s asked separately (%s): %q", p.Platform, p.Mechanism, p.DisplayedText)
	if p.UserText != "" {
		text += fmt.Sprintf("; the reply it returned: %q", p.UserText)
	}
	note := "This is " + p.Platform + "'s own check, recorded as observed; it does not answer your rules."
	if do {
		note = "This is " + p.Platform + "'s own check, recorded as observed. Your rules needed no approval: DO, within your rules."
	}
	if mismatch {
		note += " It stated another amount than the one done"
		if amount != nil && p.AmountMinor != nil {
			cur := p.Currency
			if cur == "" {
				cur = currency
			}
			note += fmt.Sprintf(" (%s, not %s)", formatMoney(*p.AmountMinor, cur), formatMoney(*amount, currency))
		}
		note += "."
	}
	return dealAuthorityLayer{Layer: "platform_approval", At: p.ObservedAt, Step: se.CapsuleID, Text: text, Note: note}
}
