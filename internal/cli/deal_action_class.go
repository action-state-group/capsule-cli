package cli

// dealTaxonomyVersion is the action-class taxonomy a deal record's
// action_class is drawn from: version 2 of capsule-engine's
// capsule_engine/guards/action_taxonomy.json
// (github.com/action-state-group/capsule-engine at 2521ee6). A record sealed
// with an action_class also seals this version, so a reader resolves the
// class against the table that was live when it was sealed.
const dealTaxonomyVersion = "2"

// The taxonomy-v2 classes a deal record can carry. Each is a row of
// action_taxonomy.json at 2521ee6; none is invented here.
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

// dealActionClasses maps a deal action, per deal type, to its taxonomy-v2
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

// dealActionClass is the taxonomy-v2 class of a deal action. direction is the
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
