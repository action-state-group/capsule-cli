package cli

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
)

// dealOutcomeSealVersion marks a step sealed by a build that seals
// outcome_seal records (dealEvent.OutcomeSealing). Only a deal whose opening
// step carries it is given one: a deal opened before stays without, and its
// outcome stays not_proven in a History.
const dealOutcomeSealVersion = "1"

// dealOutcomeSeal is an outcome_seal record's body, the whole of it: the
// producer's own statement of how one act came out. Step is the capsule id of
// the deal's last act sealed as an action (never an unchecked one), and
// ActionClass is that act's sealed class, so the record repeats no taxonomy
// version: the act's own applies. It is an assertion, not corroboration: no
// one but the producer stands behind it.
type dealOutcomeSeal struct {
	Step        string `json:"step"`
	Outcome     string `json:"outcome"`
	ActionClass string `json:"action_class"`
}

// outcomeSealValues is the closed set an outcome_seal's outcome is drawn
// from. partial and disputed are reserved: nothing seals them yet.
var outcomeSealValues = []string{"delivered", "not_delivered", "partial", "cancelled", "refunded", "disputed", "unknown"}

// outcomeSealBodyKeys and outcomeSealBlockKeys are every key an outcome_seal
// record may carry, in its body and in its x-deal-v0 block. A history copy
// discloses the record whole, so verify fails one carrying anything else.
var (
	outcomeSealBodyKeys  = []string{"action_class", "outcome", "step"}
	outcomeSealBlockKeys = []string{"at", "baseline_ref", "canonicalization", "commit_alg", "deal_id", "prev", "producer", "profile", "record_type", "seq"}
)

// dealStepClass is the taxonomy class a step's action is sealed with: the
// one place a record's action_class is derived, so an outcome_seal repeats
// exactly what its act sealed.
func dealStepClass(role, dealType string, ev dealEvent, action, direction string) string {
	if action == offerAction && ev.OfferClass != "" {
		return classMarketplaceOffer
	}
	return dealRoleActionClass(role, dealType, action, direction)
}

// lastSealedAct is the deal's last act sealed as an action, and whether it
// has one. An unchecked act is sealed as an outcome, never as an action, so
// it is never the step an outcome_seal names.
func lastSealedAct(events []sealedEvent) (sealedEvent, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if e := events[i].Event; e.Kind == "act" && e.Act != nil && !e.Act.Unchecked {
			return events[i], true
		}
	}
	return sealedEvent{}, false
}

// outcomeSealFor is the outcome_seal for outcome on the deal as events
// stand, naming its last sealed act and that act's class; nil for a deal
// opened before outcome seals, or with no act sealed as an action.
func outcomeSealFor(events []sealedEvent, outcome string) *dealOutcomeSeal {
	if len(events) == 0 || events[0].Event.OutcomeSealing == "" {
		return nil
	}
	act, ok := lastSealedAct(events)
	if !ok || act.Event.TaxonomyVersion == "" {
		return nil
	}
	a := act.Event.Act
	class := dealStepClass(dealRole(events), events[0].Event.Open.Type, act.Event, a.Action, a.Direction)
	return &dealOutcomeSeal{Step: act.CapsuleID, Outcome: outcome, ActionClass: class}
}

// closeOutcome is the outcome_seal value of a close: received as agreed is
// delivered; not received is not_delivered; received with a difference, or
// still pending, is unknown (a difference is not a measure of how much
// arrived, so it is never partial). A deal closed not_selected has no act, so
// it gets no seal.
func closeOutcome(r dealCloseResult) string {
	switch {
	case r.Status == "received" && r.Outcome == "completed":
		return "delivered"
	case r.Status == "not_received":
		return "not_delivered"
	default:
		return "unknown"
	}
}

// cancelOutcome is the outcome_seal value of a cancel act: refunded when it
// returns money (classed money.refund), else cancelled.
func cancelOutcome(class string) string {
	if class == classMoneyRefund {
		return "refunded"
	}
	return "cancelled"
}

// sealOutcome seals seal as the deal's next step, when there is one.
func (s *dealSession) sealOutcome(ctx context.Context, dealID string, events []sealedEvent, seal *dealOutcomeSeal) ([]sealedEvent, map[string]any, error) {
	if seal == nil {
		return events, nil, nil
	}
	se, err := s.seal(ctx, dealID, events, dealEvent{Kind: "outcome_seal", OutcomeSeal: seal})
	if err != nil {
		return nil, nil, err
	}
	return append(events, se), map[string]any{"capsule_id": se.CapsuleID, "outcome": seal.Outcome, "step": seal.Step}, nil
}

// historyDiscloseOutcomeSeals takes each outcome_seal of covered out of
// withhold: in a history copy it is the one record whose payload is
// disclosed, whole.
func historyDiscloseOutcomeSeals(covered []sealedEvent, withhold map[string]bool) {
	for _, se := range covered {
		if se.Event.Kind == "outcome_seal" {
			delete(withhold, se.CapsuleID)
		}
	}
}

var actionClassPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}\.[a-z][a-z0-9_]{0,63}$`)

// historyOutcomeSeals reads one history copy's disclosed payloads onto
// member, and returns its findings. Every disclosed payload must be an
// outcome_seal of this deal holding exactly its three fields, naming a record
// the copy carries (deal_payload_disclosed for any other record,
// outcome_seal_malformed for a seal with anything more, less or else; each
// at most once, naming the deal). The
// disclosure's match to its record is the bundle's own check (deal_invalid).
//
// member gets outcome (the latest seal's, else not_proven) and, with any
// seal, outcome_seals, oldest first, each with its three times apart: the
// act's (step_at), the seal's (sealed_at), and none witnessed, since a
// checkpoint covers the copy, not one record. The act's payload is withheld,
// so its class is never compared here: action_class_match is unchecked.
func historyOutcomeSeals(dealID string, b map[string]interface{}, member map[string]any) []string {
	times := map[string]string{}
	records, _ := b["records"].([]interface{})
	for _, raw := range records {
		r, _ := raw.(map[string]interface{})
		if id, _ := r["capsule_id"].(string); id != "" {
			times[id], _ = r["timestamp"].(string)
		}
	}
	type sealRead struct {
		seq  int64
		read map[string]any
	}
	var other, malformed bool
	var seals []sealRead
	disclosures, _ := b["disclosures"].(map[string]interface{})
	ids := make([]string, 0, len(disclosures))
	for id := range disclosures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d, _ := disclosures[id].(map[string]interface{})
		if len(d) == 0 {
			continue
		}
		payload, _ := d["agent_input"].(map[string]interface{})
		block, _ := payload[dealProfile].(map[string]interface{})
		if block["record_type"] != "outcome_seal" {
			other = true
			continue
		}
		body, _ := payload["body"].(map[string]interface{})
		step, _ := body["step"].(string)
		outcome, _ := body["outcome"].(string)
		class, _ := body["action_class"].(string)
		_, carried := times[step]
		seq, seqErr := jsonInt(block["seq"])
		if len(d) != 1 || len(payload) != 2 || !onlyKeys(body, outcomeSealBodyKeys, true) || !onlyKeys(block, outcomeSealBlockKeys, false) ||
			block["deal_id"] != dealID || seqErr != nil || !hexDigestPattern.MatchString(step) || !carried || step == id ||
			!slices.Contains(outcomeSealValues, outcome) || !actionClassPattern.MatchString(class) {
			malformed = true
			continue
		}
		seals = append(seals, sealRead{seq, map[string]any{
			"capsule_id": id, "step": step, "outcome": outcome, "action_class": class, "action_class_match": "unchecked",
			"step_at": times[step], "sealed_at": times[id],
		}})
	}
	var failed []string
	if other {
		failed = append(failed, "deal_payload_disclosed:"+dealID)
	}
	if malformed {
		failed = append(failed, "outcome_seal_malformed:"+dealID)
	}
	member["outcome"] = "not_proven"
	if len(seals) > 0 && len(failed) == 0 {
		sort.Slice(seals, func(i, j int) bool { return seals[i].seq < seals[j].seq })
		reads := make([]map[string]any, len(seals))
		for i, s := range seals {
			reads[i] = s.read
		}
		member["outcome"], member["outcome_seals"] = reads[len(reads)-1]["outcome"], reads
	}
	return failed
}

// historyOutcomeCounts counts a History's deals by their latest seal's
// outcome, and those with none as not_proven. Every value is listed, zeros
// too. The counts are of producer statements, not of what happened.
func historyOutcomeCounts(deals []map[string]any) map[string]int {
	counts := map[string]int{"not_proven": 0}
	for _, v := range outcomeSealValues {
		counts[v] = 0
	}
	for _, d := range deals {
		if o, ok := d["outcome"].(string); ok {
			counts[o]++
		}
	}
	return counts
}

// onlyKeys is whether m has no key outside keys and, when all is set, every
// one of them.
func onlyKeys(m map[string]interface{}, keys []string, all bool) bool {
	for k := range m {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return !all || len(m) == len(keys)
}

// jsonInt reads a decoded JSON whole number.
func jsonInt(v interface{}) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("seq is not a JSON number")
	}
	return n.Int64()
}
