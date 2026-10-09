package settlement

// The closed sets and initial registry contents of
// draft-mih-agent-settlement-records-00. TestRegistryMatchesTheVectors holds
// them to the registry.json the draft's conformance vectors pin.

// Version is the only settlement member version this package reads (§4.2).
const Version = "0"

// The four legs (§4.2), a closed set.
const (
	LegTerms         = "terms"
	LegPayerObserved = "payer_observed"
	LegPayeeObserved = "payee_observed"
	LegDelivered     = "delivered"
)

// The two sealer roles (§3).
const (
	RolePayer = "payer"
	RolePayee = "payee"
)

var legs = map[string]bool{LegTerms: true, LegPayerObserved: true, LegPayeeObserved: true, LegDelivered: true}

var sealerRoles = map[string]bool{RolePayer: true, RolePayee: true}

// statuses are an observed leg's status values (§10).
var statuses = map[string]bool{"pending": true, "settled": true, "failed": true, "reversed": true}

// deliveryDirections maps a delivered leg's direction to the only sealer role
// that may seal it (§5.4).
var deliveryDirections = map[string]string{"sent": RolePayee, "received": RolePayer}

// physicalDeliveryStatuses are delivery.status values for physical goods (§5.4).
var physicalDeliveryStatuses = map[string]bool{"pending": true, "in_transit": true, "delivered": true, "failed": true, "returned": true}

// legMembers lists, per leg, the settlement members it requires and the ones
// it may carry; any other member is forbidden (§4.2).
var legMembers = map[string]struct{ required, optional []string }{
	LegTerms:         {required: []string{"version", "leg", "sealer_role", "amount"}, optional: []string{"payment_ref", "deliverable", "valid_until", "wrapped"}},
	LegPayerObserved: {required: []string{"version", "leg", "sealer_role", "terms_ref", "amount", "payment_ref", "status", "observed_at"}, optional: []string{"routing_fee", "wrapped"}},
	LegPayeeObserved: {required: []string{"version", "leg", "sealer_role", "terms_ref", "received", "payment_ref", "status", "observed_at"}, optional: []string{"receive_fee", "wrapped"}},
	LegDelivered:     {required: []string{"version", "leg", "sealer_role", "terms_ref", "observed_at", "delivery"}, optional: []string{"wrapped"}},
}

// paymentRefType is one registered payment reference type (§7.2, §17.1).
type paymentRefType struct {
	qualifiers []string
	// receiveFeeNotApplicable is true when the type declares receive fees not
	// applicable (§6.1 rule 4). For x402.transaction it holds for the
	// "exact" scheme only, which the verifier must establish (§7.2).
	receiveFeeNotApplicable bool
	// lowercase is true when the type's normal form is lowercase (hex or UUID).
	lowercase func(ref map[string]any) bool
}

func always(map[string]any) bool { return true }

func eip155Network(ref map[string]any) bool {
	network, _ := ref["network"].(string)
	return len(network) >= 7 && network[:7] == "eip155:"
}

var paymentRefTypes = map[string]paymentRefType{
	"x402.transaction":               {qualifiers: []string{"network"}, receiveFeeNotApplicable: true, lowercase: eip155Network},
	"ln.payment_hash":                {lowercase: always},
	"bolt12.invoice_payment_hash":    {lowercase: always},
	"ap2.transaction_id":             {},
	"ap2.payment_id":                 {},
	"ap2.network_confirmation_id":    {},
	"acp.order_id":                   {},
	"ucp.order_id":                   {},
	"mpp.reference":                  {qualifiers: []string{"method"}},
	"iso20022.uetr":                  {lowercase: always},
	"iso20022.end_to_end_id":         {qualifiers: []string{"debtor_agent"}},
	"open_payments.incoming_payment": {},
}

// issuerOther is a wrapped object's issuer that is neither payer nor payee
// (a facilitator, a processor, a merchant system, a bank, a carrier).
const issuerOther = "other"

// wrappedTypes maps each registered wrapped object type to its issuer role
// (§17.2), as the vectors' registry records it.
var wrappedTypes = map[string]string{
	"x402.offer":           RolePayee,
	"x402.receipt":         RolePayee,
	"x402.payment-payload": RolePayer,
	"x402.settle-response": issuerOther,
	"ap2.checkout-mandate": RolePayer,
	"ap2.payment-mandate":  RolePayer,
	"ap2.checkout-receipt": issuerOther,
	"ap2.payment-receipt":  issuerOther,
	"bolt12.invoice":       RolePayee,
	"bolt12.payer-proof":   RolePayer,
	"mpp.payment-receipt":  RolePayee,
	"iso20022.message":     issuerOther,
	"delivery.proof":       issuerOther,
}

// Payment states (§9.2).
const (
	PaymentTermsOnly   = "terms_only"
	PaymentPayerStated = "payer_stated"
	PaymentPayeeStated = "payee_stated"
	PaymentAgreed      = "agreed"
	PaymentMismatch    = "mismatch"
	PaymentUnjoined    = "unjoined"
)

// Delivery states (§9.3).
const (
	DeliveryNone     = "none"
	DeliveryStated   = "stated"
	DeliveryMatched  = "matched"
	DeliveryMismatch = "mismatch"
)

// Failure codes (§9.4), plus the base profile's two: a leg is a Capsule, so
// the base profile's failures apply to it too.
const (
	CodeCapsuleInvalid             = "capsule_invalid"
	CodeEnvelopeInvalid            = "envelope_invalid"
	CodeSettlementMalformed        = "settlement_malformed"
	CodeAmountNotExact             = "amount_not_exact"
	CodeTermsRefUnresolved         = "terms_ref_unresolved"
	CodeLegRoleMismatch            = "leg_role_mismatch"
	CodeSealerNotAuthorizedForRole = "sealer_not_authorized_for_role"
	CodeSealerConflation           = "sealer_conflation"
	CodeWrappedDigestMismatch      = "wrapped_digest_mismatch"
	CodeWrappedResigned            = "wrapped_resigned"
)

// Informational codes (§9.4): the leg is not rejected.
const (
	CodePaymentRefTypeUnknown = "payment_ref_type_unknown"
	CodeFeeUnstated           = "fee_unstated"
	CodeFeeAssetDiffers       = "fee_asset_differs"
)

// Informational codes this implementation adds where -00 leaves a gap. Each is
// a finding only: it never changes a state -00 defines.
const (
	// CodeInProgress marks a pair whose statuses are pending on one side and
	// settled on the other. -00 reports it as a status mismatch; -01 is
	// expected to define an in_progress state for it (Open Issues).
	CodeInProgress = "in_progress"
	// CodeObservedLegNotUnique marks a side with more than one observed leg
	// for one terms leg that no supersedes chain orders (§9.2 SHOULD, §14
	// pre-written legs). The pair is then unjoined.
	CodeObservedLegNotUnique = "observed_leg_not_unique"
)
