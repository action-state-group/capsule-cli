package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/action-state-group/capsule-cli/internal/settlement"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/spf13/cobra"
)

var publicKeyHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// settlementCommands is `capsulectl settlement`: the settlement leg records of
// draft-mih-agent-settlement-records-00, read and checked offline.
func settlementCommands() *cobra.Command {
	group := &cobra.Command{Use: "settlement", Short: "Check settlement leg records and derive their payment and delivery states"}
	status := &cobra.Command{
		Use:   "status",
		Short: "Verify settlement leg records offline and derive each settlement's payment and delivery state",
		Long: "Reads leg records (each an Agent Action Capsule with a settlement member), checks each one " +
			"(Capsule identity, Producer Envelope, settlement member, amounts, wrapped objects, sealer rules) " +
			"and derives, per terms leg, the payment state and the delivery state from the legs present. " +
			"No state is read from a record. A one-sided state is a stated claim, not an agreement; a delivery " +
			"backed by the seller's own leg alone is the seller's statement of a handoff, not proof of receipt.",
		Args: noArgs,
		RunE: runSettlementStatus,
	}
	addSettlementStatusFlags(status)
	group.AddCommand(status)
	return group
}

func runSettlementStatus(c *cobra.Command, _ []string) error {
	paths, _ := c.Flags().GetStringArray("leg")
	if len(paths) == 0 {
		return inputError("settlement status needs at least one --leg file")
	}
	in, err := settlementInput(c, nil, nil)
	if err != nil {
		return err
	}
	return writeSettlementResult(c, settlement.Verify(in))
}

func addSettlementStatusFlags(cmd *cobra.Command) {
	cmd.Flags().StringArray("leg", nil, "A leg record file (repeatable): an artifact.Record or a bare Capsule with an inline signature and key_id")
	cmd.Flags().StringArray("object", nil, "A wrapped object's exact octets (repeatable), matched to wrapped entries by SHA-256; used to read the x402 scheme and to detect re-signing")
	cmd.Flags().StringArray("payer-key", nil, "An Ed25519 public key (hex) accepted for the payer (repeatable)")
	cmd.Flags().StringArray("payee-key", nil, "An Ed25519 public key (hex) accepted for the payee (repeatable)")
}

// settlementInput is the verifier's input: the given legs and objects, then
// the --leg and --object files, under the --payer-key/--payee-key policy.
func settlementInput(c *cobra.Command, legs []settlement.Leg, held map[string][]byte) (settlement.Input, error) {
	in := settlement.Input{Legs: legs, Objects: map[string][]byte{}}
	for digest, octets := range held {
		in.Objects[digest] = octets
	}
	paths, _ := c.Flags().GetStringArray("leg")
	for _, path := range paths {
		l, err := readSettlementLeg(path)
		if err != nil {
			return settlement.Input{}, err
		}
		in.Legs = append(in.Legs, l)
	}
	objects, _ := c.Flags().GetStringArray("object")
	for _, path := range objects {
		octets, err := readInput(path)
		if err != nil {
			return settlement.Input{}, err
		}
		sum := sha256.Sum256(octets)
		in.Objects[hex.EncodeToString(sum[:])] = octets
	}
	payerKeys, _ := c.Flags().GetStringArray("payer-key")
	payeeKeys, _ := c.Flags().GetStringArray("payee-key")
	for flag, keys := range map[string][]string{"--payer-key": payerKeys, "--payee-key": payeeKeys} {
		for _, key := range keys {
			if !publicKeyHexPattern.MatchString(key) {
				return settlement.Input{}, inputError(flag + " must be a 32-byte Ed25519 public key in lowercase hex (64 characters)")
			}
		}
	}
	if len(payerKeys) > 0 || len(payeeKeys) > 0 {
		in.Policy = map[string][]string{settlement.RolePayer: payerKeys, settlement.RolePayee: payeeKeys}
	}
	return in, nil
}

// writeSettlementResult prints the result with its readings, and fails when a
// leg failed.
func writeSettlementResult(c *cobra.Command, result settlement.Result) error {
	readings := make([]map[string]any, 0, len(result.Settlements))
	for _, s := range result.Settlements {
		readings = append(readings, map[string]any{"terms": s.Terms, "payment": paymentReading(s), "delivery": deliveryReading(s)})
	}
	if err := output(c, map[string]any{"settlement": result, "readings": readings}); err != nil {
		return err
	}
	if !result.Conforming {
		return fmt.Errorf("settlement verification failed: %d failure(s)", len(result.Failures))
	}
	return nil
}

// paymentReading says, in words, what a payment state does and does not show.
func paymentReading(s settlement.Settlement) string {
	switch s.PaymentState {
	case settlement.PaymentTermsOnly:
		return "terms only: no observed leg answers these terms"
	case settlement.PaymentPayerStated:
		return "a stated claim by the payer alone, not an agreement; the absence of the payee's leg is not evidence the payee disagrees"
	case settlement.PaymentPayeeStated:
		return "a stated claim by the payee alone, not an agreement; the absence of the payer's leg is not evidence the payer disagrees"
	case settlement.PaymentAgreed:
		agreed := "both sides, under distinct keys, report the same payment reference, amount and status (" + s.AgreedStatus + ")"
		if s.TermsAmount != "equal" {
			// §9.2: agreed is about the payment only. Two sides can agree on
			// what moved and both differ from the terms; that is never a
			// clean agreement and is said first.
			return "the amount paid DIFFERS FROM THE TERMS: " + agreed + ", but that amount is not the amount the terms state"
		}
		return agreed + ", and the amount equals the terms"
	case settlement.PaymentMismatch:
		return "both sides report this payment and differ"
	}
	return "both sides report a payment, but their legs cannot be joined"
}

// deliveryReading keeps the three delivery sources distinct: the seller's own
// leg is a stated handoff, never proof of receipt.
func deliveryReading(s settlement.Settlement) string {
	switch s.DeliveryState {
	case settlement.DeliveryNone:
		return "no delivered leg"
	case settlement.DeliveryMismatch:
		return "the delivered content digests differ"
	case settlement.DeliveryMatched:
		return "the seller's handoff and the buyer's receipt name the same content"
	}
	var parts []string
	for _, source := range s.DeliveryEvidence {
		switch source {
		case settlement.EvidenceSellerHandoff:
			parts = append(parts, "the seller states it handed over this content (a claim, not proof of receipt)")
		case settlement.EvidenceCarrierConfirmation:
			parts = append(parts, "a carrier or proof-of-delivery document is carried by digest (not verified here)")
		case settlement.EvidenceBuyerReceipt:
			parts = append(parts, "the buyer states it received this content")
		}
	}
	reading := "stated by one side"
	for i, part := range parts {
		if i == 0 {
			reading += ": " + part
		} else {
			reading += "; " + part
		}
	}
	return reading
}

// readSettlementLeg reads a leg in either capsule file shape `verify` accepts.
func readSettlementLeg(path string) (settlement.Leg, error) {
	raw, err := readInput(path)
	if err != nil {
		return settlement.Leg{}, err
	}
	shape, err := capsuleFileShape(path, raw)
	if err != nil {
		return settlement.Leg{}, err
	}
	l := settlement.Leg{Label: path}
	if shape == capsuleShapeRecord {
		var r artifact.Record
		if err := decodeJSONAs("--leg "+path, raw, &r); err != nil {
			return settlement.Leg{}, err
		}
		if l.Capsule, err = decodeLegCapsule(path, r.Capsule); err != nil {
			return settlement.Leg{}, err
		}
		l.Envelope = r.ProducerEnvelope
		return l, nil
	}
	if l.Capsule, err = decodeLegCapsule(path, raw); err != nil {
		return settlement.Leg{}, err
	}
	signature, hasSignature := l.Capsule["signature"].(string)
	delete(l.Capsule, "signature")
	delete(l.Capsule, "key_id")
	if hasSignature {
		if l.Envelope, err = hex.DecodeString(signature); err != nil {
			return settlement.Leg{}, inputError("--leg " + path + ": signature must be the hex Producer Envelope")
		}
	}
	return l, nil
}

func decodeLegCapsule(path string, raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var capsule map[string]any
	if err := decoder.Decode(&capsule); err != nil || capsule == nil {
		return nil, inputError("--leg " + path + ": the capsule is not a JSON object")
	}
	return capsule, nil
}
