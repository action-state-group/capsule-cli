package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/settlement"
	"github.com/spf13/cobra"
)

// Settlement legs on a deal (draft-mih-agent-settlement-records-00). Where
// the user sells, the user is the payee, and two of the four legs are theirs
// to seal:
//
//   - payment_received: the payee-observed leg. It wraps the rail's own
//     confirmation by digest and reads every value from it; it never requests
//     or moves money.
//   - delivered: what the seller handed over or performed (direction sent).
//     It is the seller's statement of a handoff, never proof of receipt; it
//     may resolve a deliver_by or perform_by obligation.
//
// Each is one evidence step whose Capsule carries the leg as its top-level
// settlement member. The terms leg and the payer's legs are the buyer's: a
// leg names the buyer's terms leg (--terms) and never restates it.

// Note kinds that seal a settlement leg.
const (
	noteKindPaymentReceived = "payment_received"
	noteKindDelivered       = "delivered"
)

// settlementNoteEvent builds the evidence event for a settlement note from
// the files named on the command line, before the deal is opened.
func settlementNoteEvent(c *cobra.Command, kind string) (dealEvent, error) {
	flag := func(name string) string { v, _ := c.Flags().GetString(name); return strings.TrimSpace(v) }
	if flag("input") != "" {
		return dealEvent{}, inputError(kind + " is built from the rail's and the terms' files, not --input")
	}
	termsPath := flag("terms")
	if termsPath == "" {
		return dealEvent{}, inputError(kind + " needs --terms: the terms leg it answers (the buyer's, as a capsule file)")
	}
	termsRef, err := termsLegID(termsPath)
	if err != nil {
		return dealEvent{}, err
	}
	observedAt := flag("observed-at")
	if observedAt == "" {
		observedAt = dealClock().UTC().Format(time.RFC3339)
	} else if at, err := time.Parse(time.RFC3339, observedAt); err != nil || at.UTC().Format(time.RFC3339) != observedAt || at.After(dealClock()) {
		return dealEvent{}, inputError("--observed-at is when your system reported it, in UTC (2026-10-06T09:14:00Z), not in the future")
	}
	read := func(name string, required bool) ([]byte, error) {
		path := flag(name)
		if path == "" {
			if required {
				return nil, inputError(kind + " needs --" + name)
			}
			return nil, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, inputError("cannot read --" + name + " " + path)
		}
		return data, nil
	}

	ev := dealEvent{Kind: "evidence"}
	var built settlement.Built
	switch kind {
	case noteKindPaymentReceived:
		in := settlement.X402Receipt{TermsRef: termsRef, ObservedAt: observedAt}
		if in.SettleResponse, err = read("settle-response", true); err != nil {
			return dealEvent{}, err
		}
		if in.Requirements, err = read("requirements", true); err != nil {
			return dealEvent{}, err
		}
		if in.Receipt, err = read("receipt", false); err != nil {
			return dealEvent{}, err
		}
		if c.Flags().Changed("asset-scale") {
			scale, _ := c.Flags().GetInt("asset-scale")
			in.AssetScale = &scale
		}
		if c.Flags().Changed("receive-fee") {
			fee := flag("receive-fee")
			in.ReceiveFee = &fee
		}
		if built, err = settlement.PayeeObservedX402(in); err != nil {
			return dealEvent{}, inputError("payment_received: " + err.Error())
		}
		ev.Evidence = &dealEvidence{About: "payment received", Source: "x402_settle_response",
			SettlementAccounts: built.AccountIDs, ReceivedTo: built.AccountIDs[0]}
	case noteKindDelivered:
		in := settlement.Delivery{TermsRef: termsRef, ObservedAt: observedAt, Carrier: flag("carrier"), Status: flag("delivery-status"),
			ShippedAt: flag("shipped-at"), DeliveredAt: flag("delivered-at")}
		content, err := read("content", false)
		if err != nil {
			return dealEvent{}, err
		}
		digest := flag("content-digest")
		switch {
		case content != nil && digest != "":
			return dealEvent{}, inputError("delivered takes --content or --content-digest, not both")
		case content != nil:
			sum := sha256.Sum256(content)
			in.ContentDigest = hex.EncodeToString(sum[:])
		default:
			in.ContentDigest = digest
		}
		if in.ContentDigest == "" && in.Carrier == "" {
			return dealEvent{}, inputError("delivered needs what was handed over: --content FILE (its exact octets) or --content-digest HEX, or --carrier for goods shipped")
		}
		if in.Proof, err = read("proof", false); err != nil {
			return dealEvent{}, err
		}
		ev.Evidence = &dealEvidence{About: "delivered", Source: "seller_handoff"}
		if tracking := flag("tracking-number"); tracking != "" {
			// The tracking number is carried only as a salted commitment
			// (§15); its nonce stays on the device to open it to a party
			// entitled to check it.
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return dealEvent{}, err
			}
			nonce := hex.EncodeToString(raw)
			if in.TrackingDigest, err = commitText(nonce, tracking); err != nil {
				return dealEvent{}, err
			}
			ev.Evidence.SettlementNonces = map[string]string{"tracking": nonce}
			ev.Evidence.SettlementAccounts = []string{tracking}
		}
		if built, err = settlement.DeliveredSent(in); err != nil {
			return dealEvent{}, inputError("delivered: " + err.Error())
		}
		if step, _ := c.Flags().GetInt64("resolves"); step > 0 {
			ev.Evidence.ResolvesStep = step
		}
	}
	member, err := built.JSON()
	if err != nil {
		return dealEvent{}, err
	}
	ev.Evidence.Settlement = member
	if len(built.Objects) > 0 {
		ev.Evidence.SettlementObjects = built.Objects
	}
	return ev, nil
}

// termsLegID reads the buyer's terms leg and returns its Capsule ID, after the
// verifier's own checks: a terms leg that would not verify is not answered.
func termsLegID(path string) (string, error) {
	leg, err := readSettlementLeg(path)
	if err != nil {
		return "", err
	}
	if s, _ := leg.Capsule["settlement"].(map[string]any); s["leg"] != settlement.LegTerms {
		return "", inputError("--terms " + path + " is not a terms leg: a leg answers the terms leg it names")
	}
	result := settlement.Verify(settlement.Input{Legs: []settlement.Leg{leg}})
	if !result.Conforming || len(result.Legs) != 1 {
		var codes []string
		for _, f := range result.Failures {
			codes = append(codes, f.Code)
		}
		return "", inputError("--terms " + path + " is not a valid settlement leg: " + strings.Join(codes, ", "))
	}
	return result.Legs[0].CapsuleID, nil
}

// checkSettlementNote holds a settlement note to the deal it is sealed on.
func checkSettlementNote(events []sealedEvent, ev *dealEvent, kind string) error {
	if dealRole(events) != dealRoleSeller {
		return inputError(kind + " is the payee's leg: it is recorded on a deal where the user sells (intent.party_role seller)")
	}
	if dealRecordSet(dealEvent{}, events) != recordsTyped {
		return inputError(kind + " is recorded in typed deals (deal open --records typed)")
	}
	switch kind {
	case noteKindPaymentReceived:
		// The payment binds to the account this deal was first paid to, by
		// its per-deal fingerprint: a later payment to another account is
		// refused, never recorded as the same payee.
		for _, se := range events {
			if e := se.Event.Evidence; e != nil && e.ReceivedTo != "" && !sameID("payee", e.ReceivedTo, ev.Evidence.ReceivedTo) {
				return inputError("refusing to seal: this payment was received to another account than the one this deal recorded at step " + fmt.Sprint(se.Event.N))
			}
		}
	case noteKindDelivered:
		// Runs after the note's resolves is read: Resolves names the step.
		if ev.Evidence.Resolves == "" {
			return nil
		}
		for _, se := range events {
			if se.CapsuleID == ev.Evidence.Resolves && se.Event.Evidence != nil && se.Event.Evidence.Obligation != nil &&
				!slices.Contains(dueKinds, se.Event.Evidence.Obligation.Kind) {
				return inputError("--resolves names a " + se.Event.Evidence.Obligation.Kind + " obligation: a delivery resolves deliver_by or perform_by")
			}
		}
	}
	return nil
}

// refuseSettlementInput keeps the settlement fields out of an evidence note's
// --input: only the settlement kinds build them, from the rail's files.
func refuseSettlementInput(e *dealEvidence) error {
	if e != nil && (len(e.Settlement) > 0 || len(e.SettlementObjects) > 0 || len(e.SettlementAccounts) > 0 || len(e.SettlementNonces) > 0 || e.ReceivedTo != "") {
		return inputError("evidence --input cannot set a settlement leg: use deal note --kind payment_received or delivered")
	}
	return nil
}

func addSettlementNoteFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("terms", "", "payment_received, delivered: the terms leg answered (the buyer's settlement terms leg, as a capsule file)")
	f.String("settle-response", "", "payment_received: the x402 settlement response, the body exactly as the facilitator returned it")
	f.String("requirements", "", "payment_received: your own x402 payment requirements for the request (the amount you were paid)")
	f.String("receipt", "", "payment_received: the x402 signed receipt you issued, if any (wrapped by digest)")
	f.Int("asset-scale", 0, "payment_received: the asset's decimals, when it is not a known asset")
	f.String("receive-fee", "", "payment_received: the fee your system reported deducted on receipt, in atomic units (any scheme but exact)")
	f.String("content", "", "delivered: a file holding exactly what you handed over (only its SHA-256 is sealed)")
	f.String("content-digest", "", "delivered: the SHA-256 of what you handed over, when you hold only the digest")
	f.String("carrier", "", "delivered: the carrier, for goods shipped")
	f.String("delivery-status", "", "delivered: pending, in_transit, delivered, failed or returned, as the carrier reported it")
	f.String("shipped-at", "", "delivered: when the goods shipped, UTC RFC 3339")
	f.String("delivered-at", "", "delivered: when the carrier reported them delivered, UTC RFC 3339")
	f.String("tracking-number", "", "delivered: the carrier's tracking number (sealed only as a salted commitment)")
	f.String("proof", "", "delivered: a carrier's proof-of-delivery document (wrapped by digest, not verified)")
	f.Int64("resolves", 0, "delivered: the step holding the deliver_by or perform_by obligation this delivery resolves")
}

// dealSettlementLegs are the deal's own settlement legs as the verifier reads
// them: each step's sealed Capsule and Producer Envelope, with the octets of
// the objects it wraps.
func (s *dealSession) dealSettlementLegs(ctx context.Context, events []sealedEvent) ([]settlement.Leg, map[string][]byte, error) {
	var legs []settlement.Leg
	objects := map[string][]byte{}
	for _, se := range events {
		e := se.Event.Evidence
		if e == nil || len(e.Settlement) == 0 {
			continue
		}
		record, err := s.t.artifacts.Get(ctx, se.CapsuleID)
		if err != nil {
			return nil, nil, err
		}
		capsule, err := decodeLegCapsule("step "+fmt.Sprint(se.Event.N), record.Capsule)
		if err != nil {
			return nil, nil, err
		}
		legs = append(legs, settlement.Leg{Label: fmt.Sprintf("step %d", se.Event.N), Capsule: capsule, Envelope: record.ProducerEnvelope})
		for digest, octets := range e.SettlementObjects {
			objects[digest] = octets
		}
	}
	return legs, objects, nil
}

// dealSettlementCommand is `deal settlement`: the deal's own legs, with the
// other side's legs the user holds, checked and read like `settlement status`.
func dealSettlementCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "settlement", Short: "Derive the payment and delivery state of a deal's settlement legs, with the other side's legs you hold", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			own, objects, err := s.dealSettlementLegs(ctx, events)
			if err != nil {
				return err
			}
			in, err := settlementInput(c, own, objects)
			if err != nil {
				return err
			}
			return writeSettlementResult(c, settlement.Verify(in))
		})
	}}
	addSettlementStatusFlags(cmd)
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	return cmd
}
