package cli

import "fmt"

// dealTaxonomyVersion is the action-class taxonomy a new deal record's
// action_class is drawn from: version 4 of capsule-engine's
// capsule_engine/guards/action_taxonomy.json
// (github.com/action-state-group/capsule-engine at
// 5300e0be262fa2d878b18eca630cbf539f785752, file sha256
// 9e10156647792a0e3fe6cf24e5211ebb8757cb8362c1e868dcfe6db6e8d891a7, JCS sha256
// 1ddce1235c3445b4ec92890d903934b08831c993f1b562e7f00082f87ca4c859). A
// record sealed with an action_class also seals this version, so a reader
// resolves the class against the table that was live when it was sealed.
// Each step keeps the version it was sealed with: a step sealed under
// version 2 (at 2521ee6) or version 3 (at a5e9e30) re-derives with its own.
//
// Versions 2, 3 and 4 name the same classes, each with the same trigger
// class. Version 3 adds approver_role account_holder to money.purchase,
// money.subscription, booking.create and booking.modify; version 4 adds it to
// booking.cancel, data.delete, communication.publish and disclosure.personal.
// So the mapping below is the same under all three.
const dealTaxonomyVersion = "4"

// The taxonomy-v4 classes a deal record can carry. Each is a row of
// action_taxonomy.json at 5300e0b; none is invented here.
const (
	classMoneyPurchase       = "money.purchase"
	classMoneyRefund         = "money.refund"
	classBookingCreate       = "booking.create"
	classBookingCancel       = "booking.cancel"
	classAgreementAccept     = "agreement.accept"
	classDisclosurePersonal  = "disclosure.personal"
	classDisclosureSecret    = "disclosure.secret"
	classExternalCommitOther = "external_commitment.other"
)

// dealActionClasses maps a deal action, per deal type, to its taxonomy-v4
// class. A cancel is mapped here only for a cancel that returns no money; one
// that returns money is classed by dealActionClass. A pair absent from the
// table (a deal type or action added later) is classed
// external_commitment.other, never left without a class: a record with no
// class is one a cap cannot select.
var dealActionClasses = map[string]map[string]string{
	"purchase": {
		"pay":    classMoneyPurchase,
		"commit": classMoneyPurchase, // placing an order commits to paying for it
		"cancel": classExternalCommitOther,
	},
	"rental": {
		"pay":    classMoneyPurchase,
		"commit": classAgreementAccept,
		"sign":   classAgreementAccept,
		"cancel": classExternalCommitOther,
	},
	"booking": {
		"pay":    classBookingCreate,
		"commit": classBookingCreate,
		"cancel": classBookingCancel,
	},
	"service": {
		"pay":    classMoneyPurchase,
		"commit": classAgreementAccept,
		"sign":   classAgreementAccept,
		"cancel": classExternalCommitOther,
	},
}

// dealActionClass is the taxonomy-v4 class of a deal action. direction is the
// way its amount moved (actDirection): a cancel whose amount returns a sealed
// payment ("in") is a refund, money arriving rather than leaving, whatever the
// deal type. The two disclosures are classed by what is given, in every deal
// type.
func dealActionClass(dealType, action, direction string) string {
	switch {
	case action == "cancel" && direction == "in":
		return classMoneyRefund
	case action == "share_contact":
		return classDisclosurePersonal
	case action == "share_credentials":
		return classDisclosureSecret
	}
	if class, ok := dealActionClasses[dealType][action]; ok {
		return class
	}
	return classExternalCommitOther
}

// dealSpendMinor is the amount of an action a spend cap evaluates, sealed as
// spend_minor beside its class: what the action pays out. Stopping a
// commitment is never spend, so every cancel is 0, whether it returns a
// payment, returns part of one, or costs a fee (a fee is recorded as its own
// fee_minor, and no cap evaluates it). Any other action spends its amount,
// unless it moved money in. With no amount and no cancel, there is none.
func dealSpendMinor(action, direction string, amount *int64) (int64, bool) {
	switch {
	case action == "cancel":
		return 0, true
	case amount == nil || direction == "in":
		return 0, false
	}
	return *amount, true
}

// checkAuthorizedMax holds a check's authorized maximum to its shape, and
// fails safe: a pay on a rail that can hold more than it charges (a card, a
// wallet, PayPal, or any rail not known to be holdless) must state the most
// it may take, since a limit binds that, not the expected charge.
func checkAuthorizedMax(snap dealSnapshot, dealRail string) error {
	if m := snap.AuthorizedMaxMinor; m != nil {
		switch {
		case snap.Action != "pay":
			return inputError("authorized_max_minor is the most a payment may take: only a pay carries one")
		case *m < 0:
			return inputError("authorized_max_minor must not be negative")
		case snap.AmountMinor != nil && *m < *snap.AmountMinor:
			return inputError(fmt.Sprintf("authorized_max_minor (%d) is less than amount_minor (%d): it is the most the payment may take, so at least the expected charge", *m, *snap.AmountMinor))
		}
		return nil
	}
	if snap.Action != "pay" {
		return nil
	}
	rail := dealRail
	if snap.Recourse != nil && snap.Recourse.Rail != "" {
		rail = snap.Recourse.Rail
	}
	if railsWithoutRecourse[normRail(rail)] {
		return nil
	}
	name := railName(rail)
	if name == "" {
		name = "an unstated rail"
	}
	return inputError("a pay by " + name + " must state authorized_max_minor: the most the payment may take, as the approval or the card hold shows it (the same as amount_minor when no larger maximum is shown)")
}

// checkFee refuses a fee on anything but a cancel, or a negative one.
func checkFee(action string, fee *int64) error {
	if fee == nil {
		return nil
	}
	if action != "cancel" {
		return inputError("fee_minor is a cancellation fee: only a cancel carries one")
	}
	if *fee < 0 {
		return inputError("fee_minor must not be negative")
	}
	return nil
}

// dealReversesRefVersion marks a step whose refund check carries
// reverses_ref: the pay act the refund reverses, by its record digest, in the
// typed reference shape (typedRef). It is the same act, by the same digest,
// as the refund act's refs[rel=reverses] (a deal-record reference), so a
// reader can key a refund on what it reverses before the act is sealed. A
// partial cancel reverses no sealed payment and carries none.
const dealReversesRefVersion = "1"

// sealCancelAmount states a cancel's amount in a sealed record so that nothing
// reads it as money paid out. A cancel with an amount carries direction "in".
// An amount that returns a sealed payment stays amount_minor: the refund, the
// pay it reverses named by reverses. Any other amount (part of a payment, or
// one that matches none) moves to cancelled_amount_minor, which states what
// the cancel was about and is neither money moved nor spend.
func sealCancelAmount(m map[string]interface{}, direction string) {
	amount, ok := m["amount_minor"]
	if !ok {
		return
	}
	if direction != "in" {
		delete(m, "amount_minor")
		m["cancelled_amount_minor"] = amount
	}
	m["direction"] = "in"
}
