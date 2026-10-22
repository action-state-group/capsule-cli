package plugin

import (
	"context"
	"errors"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// The proposal an offer answers (capsule-cli#364, capsule-engine#193). A
// seller's offer either answers a message the counterparty sent, named by
// `answering` and sealed as the check's `source` ref, or, when no
// counterparty message is sealed before it, carries the rule input
// no_proposal: there is no proposal to go stale. An offer that names none on
// a deal with counterparty messages carries neither, and a rule on the
// proposal's age cannot establish it.

// messageSentAt normalizes a message's sent_at: the platform's own time for
// it, RFC 3339 with any offset, kept in UTC at whole seconds and never later
// than now. Empty stays empty.
func messageSentAt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil || at.After(cli.DealClock()) {
		return "", cli.InputError("sent_at is when the message was sent, as the platform shows it, in RFC 3339 (2026-10-06T09:14:00Z), not in the future")
	}
	return at.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), nil
}

// checkAnswering checks what a check states about the proposal it answers,
// and decides no_proposal: `answering` goes with an offer only, and names a
// message the counterparty sent in this deal, sealed before the check. An
// offer naming none, on a deal with no counterparty message sealed, answers
// no proposal. NoProposal is never taken from the input file.
func checkAnswering(events []sealedEvent, snap *dealSnapshot) error {
	snap.NoProposal = false
	if snap.Answering != "" && snap.Action != offerAction {
		return cli.InputError("answering goes with an offer: it names the buyer's message the offer answers. A commit rests on the buyer's acceptance of your offer")
	}
	if snap.Action != offerAction {
		return nil
	}
	if snap.Answering == "" {
		snap.NoProposal = counterpartyMessage(events, "") == nil
		return nil
	}
	if counterpartyMessage(events, snap.Answering) == nil {
		return cli.InputError("answering names no message the buyer sent in this deal: give the capsule_id `deal note --kind message` returned for the buyer's message (from counterparty), noted before this check")
	}
	return nil
}

// counterpartyMessage is the sealed message from the counterparty with
// capsule id id (any one, when id is empty), or nil.
func counterpartyMessage(events []sealedEvent, id string) *sealedEvent {
	for i := range events {
		m := events[i].Event.Message
		if events[i].Event.Kind == "message" && m != nil && m.From == "counterparty" && (id == "" || events[i].CapsuleID == id) {
			return &events[i]
		}
	}
	return nil
}

// answeredProposalAt is, for the check of an offer that answers a
// counterparty's message, when that message was sent: its sealed sent_at,
// else its sealed at (when it was noted), read from the message's own sealed
// record. Empty for any other check.
func (s *dealSession) answeredProposalAt(ctx context.Context, checked string, events []sealedEvent) (string, error) {
	answering := ""
	for _, se := range events {
		if sn := se.Event.Snapshot; se.CapsuleID == checked && sn != nil && sn.Action == offerAction {
			answering = sn.Answering
		}
	}
	if answering == "" {
		return "", nil
	}
	sealed, err := s.capsuleWithInput(ctx, answering)
	if err != nil {
		return "", err
	}
	record, _ := sealed["agent_input"].(map[string]interface{})
	body, _ := record["body"].(map[string]interface{})
	block, _ := record[cli.DealProfile].(map[string]interface{})
	if at, _ := body["sent_at"].(string); at != "" {
		return at, nil
	}
	at, _ := block["at"].(string)
	if at == "" {
		return "", errors.New("refusing to check: the answered message's sealed record is not on this device, so the rules checker cannot be told when it was sent")
	}
	return at, nil
}
