package cli

import (
	"slices"
	"strings"
)

// gradeLadder is the evidence grade ladder, lowest first, as capsule-emit's
// TRANSLATION.md defines it ("The ladder") at
// action-state-group/capsule-emit@9d862d99ef9eb64d3ea8eadea173d4ec17f5ae2f:
// self-attested → witnessed → countersigned.
//
// It is the one list of rung names. A grade capsulectl shows, as a field
// value or in its own words, names one of these, an annotation, or no rung
// (TestGradeStringsNameOnlyLadderRungs): when the ladder is renamed, this
// list moves first and every string that no longer names a rung fails.
var gradeLadder = []string{"self-attested", "witnessed", "countersigned"}

// gradeAnnotations are the countersignature findings that are not rungs:
// a self-countersignature (the producer's own key) or a countersignature
// from a signer no directory resolves. Either adds nothing: the grade stays
// where witnessing put it (self-attested, or witnessed with a receipt), and
// the finding is listed beside it. (TRANSLATION.md, "Countersignature
// annotations are not rungs"; agent-action-capsule PR #187, pending, maps
// them onto an Evidence Result.)
var gradeAnnotations = []string{"self-countersigned", "unresolved-signer"}

// gradeInPart qualifies a rung that covers only some of a record's steps
// (witnessed_in_part: a receipt covers steps 1 to k of n; the rest are
// self-attested until a later checkpoint covers them).
const gradeInPart = "_in_part"

// gradeNoRung are the findings when no rung on the countersign axis is
// reached. They name the absence, never a rung: not_countersigned (no
// countersignature), unverified (a countersignature of a type capsulectl
// does not check: attached, not counted), unchecked (the page carries a
// countersignature capsulectl did not check when it wrote the page).
var gradeNoRung = []string{"not_countersigned", "unverified", "unchecked"}

// rungName is the ladder spelling of a rung field value: field values are
// snake_case (self_attested), the ladder hyphenates (self-attested).
func rungName(value string) string {
	return strings.ReplaceAll(value, "_", "-")
}

// isRungValue reports whether value, a rung field's value, is a rung of the
// ladder, or a rung in part. Nothing else may sit in a rung field.
func isRungValue(value string) bool {
	return slices.Contains(gradeLadder, rungName(strings.TrimSuffix(value, gradeInPart)))
}

// isFindingValue reports whether value, a finding field's value, is a
// countersignature annotation or one of the values that name no rung.
func isFindingValue(value string) bool {
	return slices.Contains(gradeNoRung, value) || slices.Contains(gradeAnnotations, rungName(value))
}

// isGradeValue reports whether a word the receipt shows names a rung, an
// annotation, or no rung: anything else that looks like a grade is off the
// ladder.
func isGradeValue(value string) bool {
	return isRungValue(value) || isFindingValue(value)
}
