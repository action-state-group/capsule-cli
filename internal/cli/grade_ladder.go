package cli

import (
	"slices"
	"strings"
)

// gradeLadder is the evidence grade ladder, lowest first, as capsule-emit's
// TRANSLATION.md defines it ("The ladder" table) at
// action-state-group/capsule-emit@502402f14c0bb40c6550aeb98ea80fea37345375:
// unsigned entry → self-attested → witnessed → self-countersigned /
// unresolved-signer → countersigned. self-countersigned and
// unresolved-signer share a rung: a countersignature exists, but it is the
// producer's own key, or a key no directory resolves.
//
// It is the one list of rung names. A grade capsulectl shows, as a field
// value or in its own words, names one of these and nothing else
// (TestGradeStringsNameOnlyLadderRungs): when the ladder is renamed, this
// list moves first and every string that no longer names a rung fails.
var gradeLadder = []string{"unsigned-entry", "self-attested", "witnessed", "self-countersigned", "unresolved-signer", "countersigned"}

// gradeInPart qualifies a rung that covers only some of a record's steps
// (witnessed_in_part: a receipt covers steps 1 to k of n; the rest are
// self-attested until a later checkpoint covers them).
const gradeInPart = "_in_part"

// gradeNoRung are the values a rung field takes when no rung on its axis is
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

// isGradeValue reports whether value, a rung field's value, names a rung of
// the ladder (or a rung in part), or one of the values that name no rung.
func isGradeValue(value string) bool {
	if slices.Contains(gradeNoRung, value) {
		return true
	}
	return slices.Contains(gradeLadder, rungName(strings.TrimSuffix(value, gradeInPart)))
}
