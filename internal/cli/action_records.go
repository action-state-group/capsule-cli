package cli

// The typed action records carry the check contract: one record per step of a
// consequential action, each naming its own type, in the same hash-linked
// chain as the x-deal-v0 evidence records (messages, claims, evidence, detail
// changes, proposals, unapproved disclosures and the close):
//
//	task-authority/v0     what the user asked for and the limits they set
//	proposed-action/v0    the normalized ProposedAction, exactly as checked
//	action-evaluation/v0  one evaluation of it (the check response)
//	action-approval/v0    one approval artifact, of a stated authority
//	action-record/v0      the action taken, with every authority it relied on
//	action-outcome/v0     what was observed, or an action taken without authority
//	action-report/v0      a report a person reads (defined; sealed later)
//
// A deal opened with --records typed is sealed in them (dealOpen.Records). Each
// typed record is derived from the step's x-deal-v0 record, so the two record
// sets commit to the same facts: only the header, the names and the check
// contract's fields differ.

const (
	TypeTaskAuthority    = "task-authority/v0"
	TypeProposedAction   = "proposed-action/v0"
	TypeActionEvaluation = "action-evaluation/v0"
	TypeActionApproval   = "action-approval/v0"
	TypeActionRecord     = "action-record/v0"
	TypeActionOutcome    = "action-outcome/v0"
	TypeActionReport     = "action-report/v0"
	TypeCheckRequest     = "check-request/v0"
	TypeCheckResponse    = "check-response/v0"
	TypeCommercialBounds = CommercialBoundsKind
	TypedRecordRef       = "record"
	TypedFPAlg           = "hmac-sha256-chain-key"
)

// Materiality modes an evaluation states, so that no predicate is told apart
// from a missing field.
const (
	MaterialityModePredicate = "predicate"
	MaterialityModeNone      = "none_fail_safe"
)

// predicateDigest is the digest of the predicate that applied, "" when none
// was configured (digest "none": every pick the agent made alone paused).
func (m DealMateriality) PredicateDigest() string {
	if m.Digest == "none" {
		return ""
	}
	return m.Digest
}

// mode is how typed records state the materiality a check ran under, from
// the check's Materiality, the same value its output shows: the predicate's
// digest, or, with none configured, null and the fail-safe mode.
func (m DealMateriality) Mode() (string, *string) {
	if d := m.PredicateDigest(); d != "" {
		return MaterialityModePredicate, &d
	}
	return MaterialityModeNone, nil
}
