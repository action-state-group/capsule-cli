package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
)

// Versioning and diff rules for Evidence Contracts. This is a port of
// capsule-engine's capsule_engine/packs/contract_diff.py, which documents
// each rule; the two are held together by the shared case library in
// testdata/contract/cases (copied from capsule-engine's
// tests/fixtures/contract-cases), which both implementations must pass.
//
// A change from A to B is non-breaking only when every evidence set that
// satisfied A still satisfies B and every claim made against A still names a
// requirement of B. Removing a requirement, tightening, identity changes (id,
// profile, clause) and changes whose direction cannot be determined are
// breaking; loosening is non-breaking but reported. A field rule applies only
// where the schema defines the field for the requirement's shape, and a field
// with no rule is breaking.

// embeddedContractSchema is capsule-engine's schemas/evidence-contract-v0.json,
// byte for byte (TestEmbeddedContractSchemaMatchesTestdata keeps it equal to
// the testdata copy the contract tests use). `contract diff` validates both
// inputs against it unless --schema names another; `contract validate` still
// embeds nothing and always takes --schema.
//
//go:embed schemas/evidence-contract-v0.json
var embeddedContractSchema []byte

const embeddedContractSchemaURL = "capsulectl-embedded:evidence-contract-v0.json"

func loadEmbeddedContractSchema() (*jsonschema.Schema, any, error) {
	var schemaRaw any
	if e := json.Unmarshal(embeddedContractSchema, &schemaRaw); e != nil {
		return nil, nil, e
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(embeddedContractSchema))
	if e != nil {
		return nil, nil, e
	}
	compiler := jsonschema.NewCompiler()
	if e := compiler.AddResource(embeddedContractSchemaURL, doc); e != nil {
		return nil, nil, e
	}
	sch, e := compiler.Compile(embeddedContractSchemaURL)
	if e != nil {
		return nil, nil, e
	}
	return sch, schemaRaw, nil
}

// notA reports whether any present value fails isType: defence in depth for
// a diff of unvalidated input, where a value of the wrong JSON type is never
// ranked, only reported as changed.
func notA(isType func(any) bool, values ...any) bool {
	for _, v := range values {
		if v != missing && !isType(v) {
			return true
		}
	}
	return false
}

func isList(v any) bool   { _, ok := v.([]any); return ok }
func isString(v any) bool { _, ok := v.(string); return ok }

// ErrBreaking reports that `contract diff` found at least one breaking change
// -- an ordinary, expected outcome (exit code 1), like ErrSchemaInvalid.
var ErrBreaking = errors.New("contract change is breaking")

const (
	severityBreaking    = "breaking"
	severityNonBreaking = "non_breaking"
)

var changeSeverity = map[string]string{
	"contract_id_changed":    severityBreaking,
	"version_reused":         severityBreaking,
	"version_changed":        severityNonBreaking,
	"requirement_added":      severityBreaking,
	"requirement_removed":    severityBreaking,
	"requirement_reid":       severityBreaking,
	"requirements_reordered": severityNonBreaking,
	"tightened":              severityBreaking,
	"loosened":               severityNonBreaking,
	"changed":                severityBreaking,
	"editorial":              severityNonBreaking,
}

// assuranceLadder is the Evidence Result's Grade vocabulary, lowest first.
var assuranceLadder = []string{"self-attested", "witnessed", "countersigned"}

var tierRank = map[string]int{"informational": 0, "must_have": 1}

type contractChange struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func (c contractChange) severity() string { return changeSeverity[c.Kind] }

type contractPin struct {
	Ref    string
	Digest string
}

func (p contractPin) toJSON() map[string]any {
	return map[string]any{
		"contract_ref":    p.Ref,
		"contract_digest": map[string]any{"digest_alg": "SHA-256", "digest": p.Digest},
	}
}

type contractDiff struct {
	A, B    contractPin
	Changes []contractChange
}

func (d contractDiff) breaking() bool {
	for _, c := range d.Changes {
		if c.severity() == severityBreaking {
			return true
		}
	}
	return false
}

// errDuplicateRequirementID: requirements are matched to claims by id, so a
// contract with two requirements of one id cannot be diffed.
var errDuplicateRequirementID = errors.New("duplicate requirement id")

// missing stands for an absent member, distinct from an explicit JSON null.
var missing = &struct{}{}

func member(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return missing
}

func same(a, b any) bool { return reflect.DeepEqual(a, b) }

// computeContractPin names the exact contract a Result was evaluated against:
// <id>@<version>, plus the SHA-256 of the contract's RFC 8785 (JCS) bytes.
func computeContractPin(doc map[string]any) (contractPin, error) {
	id, _ := doc["id"].(string)
	version, _ := doc["version"].(string)
	digest, e := canonical.JSONDigest(doc)
	if e != nil {
		return contractPin{}, e
	}
	return contractPin{Ref: id + "@" + version, Digest: digest}, nil
}

// -- field rules -------------------------------------------------------------

type fieldRule func(path string, a, b any) []contractChange

func one(path, kind, reason string) []contractChange {
	return []contractChange{{Path: path, Kind: kind, Reason: reason}}
}

// stringSet returns nil for an absent member (distinct from an empty set).
func stringSet(v any) map[string]bool {
	if v == missing {
		return nil
	}
	out := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, x := range list {
			out[fmt.Sprint(x)] = true
		}
	}
	return out
}

func names(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func setMinus(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func compareSets(moreIsTighter bool, what string) fieldRule {
	return func(path string, a, b any) []contractChange {
		if notA(isList, a, b) {
			return one(path, "changed", what+": not a list; direction cannot be determined")
		}
		sa, sb := stringSet(a), stringSet(b)
		if moreIsTighter {
			if sa == nil {
				sa = map[string]bool{}
			}
			if sb == nil {
				sb = map[string]bool{}
			}
		}
		if (sa == nil) == (sb == nil) && same(sa, sb) {
			return nil
		}
		if !moreIsTighter {
			if sa == nil {
				return one(path, "tightened", fmt.Sprintf("%s: was unrestricted, now only %s", what, names(sb)))
			}
			if sb == nil {
				return one(path, "loosened", fmt.Sprintf("%s: was %s, now unrestricted", what, names(sa)))
			}
		}
		added, removed := setMinus(sb, sa), setMinus(sa, sb)
		if len(added) > 0 && len(removed) > 0 {
			return one(path, "changed", fmt.Sprintf("%s: %s replaced by %s; evidence for the old members may not count", what, names(removed), names(added)))
		}
		grew := len(added) > 0
		tighter := grew == moreIsTighter
		detail := "removed " + names(removed)
		if grew {
			detail = "added " + names(added)
		}
		kind := "loosened"
		if tighter {
			kind = "tightened"
		}
		return one(path, kind, what+": "+detail)
	}
}

// gradeFloor: rank of the lowest grade named, -1 for no floor, ok=false if a
// grade is not on the ladder.
func gradeFloor(v any) (int, bool) {
	if v == missing {
		return -1, true
	}
	var grades []any
	switch t := v.(type) {
	case string:
		grades = []any{t}
	case []any:
		grades = t
	default:
		return 0, false
	}
	if len(grades) == 0 {
		return -1, true
	}
	floor := len(assuranceLadder)
	for _, g := range grades {
		s, _ := g.(string)
		rank := -1
		for i, name := range assuranceLadder {
			if name == s {
				rank = i
			}
		}
		if rank < 0 {
			return 0, false
		}
		floor = min(floor, rank)
	}
	return floor, true
}

func compareGrades(path string, a, b any) []contractChange {
	if same(a, b) {
		return nil
	}
	fa, okA := gradeFloor(a)
	fb, okB := gradeFloor(b)
	switch {
	case !okA || !okB:
		return one(path, "changed", "assurance grade not on the ladder; direction cannot be determined")
	case fa == fb:
		// What a list of several grades means is not yet defined, so a change
		// in its members at the same floor is not known to be safe.
		return one(path, "changed", "grades listed changed at the same floor; multi-grade lists are not yet defined")
	case fb > fa:
		return one(path, "tightened", "assurance floor raised")
	default:
		return one(path, "loosened", "assurance floor lowered")
	}
}

// Each component is at most 9 digits so the comparison fits an int64; a
// longer component does not parse. RE2 has no lookahead, so the two shapes
// the pattern alone admits -- a bare "P" and a "T" with no time component --
// are rejected in durationParts.
var durationPattern = regexp.MustCompile(`^P(?:(\d{1,9})Y)?(?:(\d{1,9})M)?(?:(\d{1,9})W)?(?:(\d{1,9})D)?(?:T(?:(\d{1,9})H)?(?:(\d{1,9})M)?(?:(\d{1,9})S)?)?$`)

// duration is (months, seconds): years and months are calendar units of
// varying length, so they are never converted into seconds.
type duration struct {
	unbounded       bool
	months, seconds int64
}

var (
	unboundedDuration = duration{unbounded: true}
	zeroDuration      = duration{}
)

func durationParts(v any) (duration, bool) {
	s, ok := v.(string)
	if !ok || s == "P" || strings.HasSuffix(s, "T") {
		return duration{}, false
	}
	m := durationPattern.FindStringSubmatch(s)
	if m == nil {
		return duration{}, false
	}
	var n [7]int64
	for i, g := range m[1:] {
		if g == "" {
			continue
		}
		x, e := strconv.ParseInt(g, 10, 64)
		if e != nil {
			return duration{}, false
		}
		n[i] = x
	}
	y, mo, w, d, h, mi, sec := n[0], n[1], n[2], n[3], n[4], n[5], n[6]
	return duration{months: 12*y + mo, seconds: (((7*w+d)*24+h)*60+mi)*60 + sec}, true
}

func sign(n int64) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

// order: -1 if b is shorter, 0 if equal, 1 if longer; ok=false if the two
// cannot be ordered (one longer in months, the other in seconds).
func order(a, b duration) (int, bool) {
	if a.unbounded || b.unbounded {
		switch {
		case a.unbounded && b.unbounded:
			return 0, true
		case b.unbounded:
			return 1, true
		default:
			return -1, true
		}
	}
	dm, ds := sign(b.months-a.months), sign(b.seconds-a.seconds)
	switch {
	case dm == 0:
		return ds, true
	case ds == 0, dm == ds:
		return dm, true
	}
	return 0, false
}

// compareDurations: a longer duration is looser. absent is what an absent
// (or null) value stands for: unbounded for a limit, zero for a grace.
func compareDurations(absent duration, what string) fieldRule {
	return func(path string, a, b any) []contractChange {
		if same(a, b) {
			return nil
		}
		parts := func(v any) (duration, bool) {
			if v == missing || v == nil {
				return absent, true
			}
			return durationParts(v)
		}
		da, okA := parts(a)
		db, okB := parts(b)
		if !okA || !okB {
			return one(path, "changed", what+": not an ISO-8601 duration; direction cannot be determined")
		}
		o, ok := order(da, db)
		switch {
		case !ok:
			return one(path, "changed", what+": months and days are not exactly comparable")
		case o == 0:
			return one(path, "editorial", what+": same length, spelled differently")
		case o < 0:
			return one(path, "tightened", what+": shortened")
		default:
			return one(path, "loosened", what+": lengthened")
		}
	}
}

func compareWindow(path string, a, b any) []contractChange {
	if same(a, b) {
		return nil
	}
	if a == missing {
		return one(path, "tightened", "window added: evidence must now fall inside it")
	}
	if b == missing {
		return one(path, "loosened", "window removed")
	}
	wa, okA := a.(map[string]any)
	wb, okB := b.(map[string]any)
	if !okA || !okB {
		return one(path, "changed", "value changed; no rule says this is safe")
	}
	out := compareDurations(unboundedDuration, "window duration")(path+"/duration", member(wa, "duration"), member(wb, "duration"))
	for _, key := range []string{"cure", "grace"} {
		out = append(out, compareDurations(zeroDuration, "window "+key)(path+"/"+key, member(wa, key), member(wb, key))...)
	}
	for _, key := range sortedUnion(wa, wb, "duration", "cure", "grace") {
		out = append(out, compareField(path+"/"+key, key, member(wa, key), member(wb, key), nil)...)
	}
	return out
}

func compareTier(path string, a, b any) []contractChange {
	// An absent tier is "informational" (the native shape's default).
	ta, tb := a, b
	if ta == missing {
		ta = "informational"
	}
	if tb == missing {
		tb = "informational"
	}
	if same(ta, tb) {
		return nil
	}
	sa, _ := ta.(string)
	sb, _ := tb.(string)
	ra, okA := tierRank[sa]
	rb, okB := tierRank[sb]
	if !okA || !okB {
		return one(path, "changed", "tier changed")
	}
	kind := "loosened"
	if rb > ra {
		kind = "tightened"
	}
	return one(path, kind, fmt.Sprintf("tier %s -> %s", sa, sb))
}

func isSubsequence(short, long []any) bool {
	i := 0
	for _, x := range long {
		if i < len(short) && same(short[i], x) {
			i++
		}
	}
	return i == len(short)
}

func compareSequence(path string, a, b any) []contractChange {
	if !same(a, b) && notA(isList, a, b) {
		return one(path, "changed", "required sequence is not a list; direction cannot be determined")
	}
	la, _ := a.([]any)
	lb, _ := b.([]any)
	if len(la) == len(lb) && (len(la) == 0 || same(la, lb)) {
		return nil
	}
	switch {
	case isSubsequence(lb, la):
		return one(path, "loosened", "required sequence has fewer steps")
	case isSubsequence(la, lb):
		return one(path, "tightened", "required sequence has more steps")
	default:
		return one(path, "changed", "required sequence reordered or replaced")
	}
}

func editorial(path string, a, b any) []contractChange {
	if same(a, b) {
		return nil
	}
	if notA(isString, a, b) {
		return one(path, "changed", "not a string; no rule says this is safe")
	}
	return one(path, "editorial", "not read when sufficiency is decided")
}

// withEvidenceRequirements returns extra plus the rules for the closed
// evidence_requirements block, which the abstract outcome and obligation
// shapes, process and human_role all define.
func withEvidenceRequirements(extra map[string]fieldRule) map[string]fieldRule {
	rules := map[string]fieldRule{
		"evidence_requirements/accepted_epistemic_types": compareSets(false, "accepted epistemic types"),
		"evidence_requirements/required_sources":         compareSets(true, "required sources"),
		"evidence_requirements/minimum_assurance":        compareGrades,
		"evidence_requirements/freshness":                compareDurations(unboundedDuration, "freshness"),
	}
	for k, v := range extra {
		rules[k] = v
	}
	return rules
}

// shapeRules holds the field rules by requirement shape, keyed by the path
// relative to the requirement: a rule applies only where the schema defines
// that field. An extension field on an open profile that shares a rule's
// name gets no rule, so any change to it is breaking.
var shapeRules = map[string]map[string]fieldRule{
	"native": {
		"window":                   compareWindow,
		"tier":                     compareTier,
		"required_assurance_grade": compareGrades,
		"clause/source_url":        editorial,
	},
	"outcome":    withEvidenceRequirements(map[string]fieldRule{"authority/escalation_path": editorial}),
	"obligation": withEvidenceRequirements(nil),
	"process": withEvidenceRequirements(map[string]fieldRule{
		"required_sequence": compareSequence,
		"approvals":         compareSets(true, "approvals"),
		"escalation_path":   editorial,
	}),
	"human_role": withEvidenceRequirements(nil),
}

// shape is "native" for the pack-declared shape (outcome or obligation with
// an evidence_rule), otherwise the profile ("" when there is none).
func shape(req map[string]any) string {
	profile, _ := req["profile"].(string)
	if _, native := req["evidence_rule"]; native && (profile == "" || profile == "outcome" || profile == "obligation") {
		return "native"
	}
	return profile
}

func sortedUnion(a, b map[string]any, skip ...string) []string {
	seen := map[string]bool{}
	for _, k := range skip {
		seen[k] = true
	}
	var keys []string
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// compareField compares one member. rel is path relative to the requirement
// (or the root): the key rules is looked up by.
func compareField(path, rel string, a, b any, rules map[string]fieldRule) []contractChange {
	if rule, ok := rules[rel]; ok {
		return rule(path, a, b)
	}
	if same(a, b) {
		return nil
	}
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	if okA && okB {
		var out []contractChange
		for _, k := range sortedUnion(ma, mb) {
			out = append(out, compareField(path+"/"+k, rel+"/"+k, member(ma, k), member(mb, k), rules)...)
		}
		return out
	}
	switch {
	case a == missing:
		return one(path, "changed", "field added; no rule says this is safe")
	case b == missing:
		return one(path, "changed", "field removed; no rule says this is safe")
	default:
		return one(path, "changed", "value changed; no rule says this is safe")
	}
}

// deterministic: positively evaluated without a judge -- the only case where
// rewording the statement cannot move a result.
func deterministic(req map[string]any) bool {
	switch shape(req) {
	case "outcome":
		adjudication, ok := req["adjudication"].(map[string]any)
		return ok && adjudication["mode"] == "deterministic"
	case "native":
		return req["backward_verdict"] == "DETERMINISTIC" && req["mode"] != "judged"
	}
	return false
}

func compareRequirement(rid string, a, b map[string]any) []contractChange {
	base := "requirements[" + rid + "]"
	var rules map[string]fieldRule
	if s := shape(a); s != "" && s == shape(b) {
		rules = shapeRules[s]
	}
	var out []contractChange
	for _, key := range sortedUnion(a, b, "id") {
		va, vb := member(a, key), member(b, key)
		path := base + "/" + key
		switch {
		case key == "statement" && !same(va, vb):
			if deterministic(a) && deterministic(b) {
				out = append(out, one(path, "editorial", "statement reworded; the requirement is evaluated deterministically")...)
			} else {
				out = append(out, one(path, "changed", "statement reworded; a judge or human evaluates this wording")...)
			}
		case key == "profile" && !same(va, vb):
			out = append(out, one(path, "changed", "profile changed: a different kind of requirement")...)
		default:
			out = append(out, compareField(path, key, va, vb, rules)...)
		}
	}
	return out
}

// requirementsByID returns the requirements keyed by id and the ids in
// document order.
func requirementsByID(doc map[string]any, side string) (map[string]map[string]any, []string, error) {
	list, ok := doc["requirements"].([]any)
	if !ok {
		return nil, nil, hint(ErrInput, "contract "+side+" has no requirements array")
	}
	byID := map[string]map[string]any{}
	var order []string
	for _, r := range list {
		req, ok := r.(map[string]any)
		if !ok {
			return nil, nil, hint(ErrInput, "contract "+side+" has a requirement that is not an object")
		}
		id, ok := req["id"].(string)
		if !ok {
			return nil, nil, hint(ErrInput, "contract "+side+" has a requirement with no string id")
		}
		if _, dup := byID[id]; dup {
			return nil, nil, errors.Join(errDuplicateRequirementID, hint(ErrInput, "contract "+side+" has two requirements with id "+id))
		}
		byID[id] = req
		order = append(order, id)
	}
	return byID, order, nil
}

func withoutID(req map[string]any) map[string]any {
	out := make(map[string]any, len(req))
	for k, v := range req {
		if k != "id" {
			out[k] = v
		}
	}
	return out
}

func diffContracts(a, b map[string]any) (contractDiff, error) {
	reqsA, orderA, e := requirementsByID(a, "A")
	if e != nil {
		return contractDiff{}, e
	}
	reqsB, orderB, e := requirementsByID(b, "B")
	if e != nil {
		return contractDiff{}, e
	}
	pinA, e := computeContractPin(a)
	if e != nil {
		return contractDiff{}, hint(ErrInput, "contract A cannot be canonicalized: "+e.Error())
	}
	pinB, e := computeContractPin(b)
	if e != nil {
		return contractDiff{}, hint(ErrInput, "contract B cannot be canonicalized: "+e.Error())
	}
	var changes []contractChange
	add := func(cs []contractChange) { changes = append(changes, cs...) }

	if !same(a["id"], b["id"]) {
		add(one("id", "contract_id_changed", "claims name their contract by id; claims against A do not name B"))
	} else if same(a["version"], b["version"]) && pinA.Digest != pinB.Digest {
		add(one("version", "version_reused", "one version label now names two different contracts"))
	}
	if !same(a["version"], b["version"]) {
		add(one("version", "version_changed", fmt.Sprintf("version %v -> %v", a["version"], b["version"])))
	}
	for _, key := range sortedUnion(a, b, "id", "version", "requirements") {
		add(compareField(key, key, member(a, key), member(b, key), nil))
	}

	var removed, added []string
	for _, rid := range orderA {
		if _, ok := reqsB[rid]; !ok {
			removed = append(removed, rid)
		}
	}
	for _, rid := range orderB {
		if _, ok := reqsA[rid]; !ok {
			added = append(added, rid)
		}
	}
	var stillRemoved []string
	for _, old := range removed {
		match := -1
		for i, nw := range added {
			if same(withoutID(reqsA[old]), withoutID(reqsB[nw])) {
				match = i
				break
			}
		}
		if match < 0 {
			stillRemoved = append(stillRemoved, old)
			continue
		}
		nw := added[match]
		added = append(added[:match:match], added[match+1:]...)
		add(one("requirements["+old+"]", "requirement_reid", fmt.Sprintf("re-identified as %s; claims citing %s no longer resolve", nw, old)))
	}
	for _, rid := range stillRemoved {
		add(one("requirements["+rid+"]", "requirement_removed", "no longer required: claims against A that cite it do not resolve in B"))
	}
	for _, rid := range added {
		add(one("requirements["+rid+"]", "requirement_added", "new requirement: evidence that satisfied A says nothing about it"))
	}

	var commonA, commonB []string
	for _, rid := range orderA {
		if _, ok := reqsB[rid]; ok {
			commonA = append(commonA, rid)
		}
	}
	for _, rid := range orderB {
		if _, ok := reqsA[rid]; ok {
			commonB = append(commonB, rid)
		}
	}
	if !same(commonA, commonB) {
		add(one("requirements", "requirements_reordered", "requirements are keyed by id; order carries no meaning"))
	}
	for _, rid := range commonA {
		add(compareRequirement(rid, reqsA[rid], reqsB[rid]))
	}

	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Kind < changes[j].Kind
	})
	return contractDiff{A: pinA, B: pinB, Changes: changes}, nil
}

// -- command -----------------------------------------------------------------

func decodeContract(path string) (map[string]any, any, error) {
	raw, e := readInput(path)
	if e != nil {
		return nil, nil, e
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if e := dec.Decode(&doc); e != nil || doc == nil {
		return nil, nil, hint(ErrInput, "malformed JSON in "+path+" (expected an object)")
	}
	// The schema validator wants its own decoding of the same bytes.
	generic, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if e != nil {
		return nil, nil, hint(ErrInput, "malformed JSON in "+path)
	}
	return doc, generic, nil
}

func contractDiffCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff A B",
		Short: "Classify every change from contract A to contract B as breaking or non-breaking",
		Long: "Classify every change from Evidence Contract A to B as breaking or non-breaking, with a reason,\n" +
			"and name both by <id>@<version> and the SHA-256 of their JCS bytes. Both are validated first, against\n" +
			"the embedded Evidence Contract v0 schema or --schema. Exit 0: identical or non-breaking; 1: breaking;\n" +
			"2: an input is unreadable or invalid.",
		Args: func(c *cobra.Command, args []string) error {
			if len(args) != 2 {
				return inputError(fmt.Sprintf("%s takes exactly two contract files, OLD and NEW (got %d)", c.CommandPath(), len(args)))
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			schemaLoc, _ := c.Flags().GetString("schema")
			asJSON, _ := c.Flags().GetBool("json")
			docA, genericA, e := decodeContract(args[0])
			if e != nil {
				return e
			}
			docB, genericB, e := decodeContract(args[1])
			if e != nil {
				return e
			}
			schema, schemaRaw, e := loadEmbeddedContractSchema()
			against := "the embedded Evidence Contract v0 schema"
			if schemaLoc != "" {
				schema, schemaRaw, e = loadSchema(schemaLoc)
				against = "--schema"
				if e != nil {
					return errors.Join(ErrInput, &schemaLoadError{loc: schemaLoc, err: e})
				}
			}
			if e != nil {
				return e
			}
			for i, generic := range []any{genericA, genericB} {
				if report := validateContract(schema, schemaRaw, generic); !report.valid {
					if !asJSON {
						printReport(c, args[i], report)
					}
					return hint(ErrInput, args[i]+" is not a valid contract under "+against)
				}
			}
			d, e := diffContracts(docA, docB)
			if e != nil {
				return e
			}
			if asJSON {
				changes := make([]map[string]any, len(d.Changes))
				for i, ch := range d.Changes {
					changes[i] = map[string]any{"path": ch.Path, "kind": ch.Kind, "severity": ch.severity(), "reason": ch.Reason}
				}
				if oe := output(c, map[string]any{"a": d.A.toJSON(), "b": d.B.toJSON(), "breaking": d.breaking(), "changes": changes}); oe != nil {
					return oe
				}
			} else {
				printDiff(c, d)
			}
			if d.breaking() {
				return ErrBreaking
			}
			return nil
		},
	}
	cmd.Flags().String("schema", "", "Path or URL to a JSON Schema to validate both contracts against instead of the embedded Evidence Contract v0 schema")
	cmd.Flags().Bool("json", false, "Emit a capsule-cli-result/v1 report instead of readable text")
	return cmd
}

func printDiff(c *cobra.Command, d contractDiff) {
	w := c.OutOrStdout()
	fmt.Fprintf(w, "A: %s sha256:%s\n", d.A.Ref, d.A.Digest)
	fmt.Fprintf(w, "B: %s sha256:%s\n", d.B.Ref, d.B.Digest)
	if len(d.Changes) == 0 {
		fmt.Fprintln(w, "identical")
	}
	for _, ch := range d.Changes {
		fmt.Fprintf(w, "  %-12s %-22s %s: %s\n", ch.severity(), ch.Kind, ch.Path, ch.Reason)
	}
	if d.breaking() {
		fmt.Fprintln(w, "BREAKING")
	} else {
		fmt.Fprintln(w, "non-breaking")
	}
}
