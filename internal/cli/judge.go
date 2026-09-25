// Package cli: the deterministic half of the retired capsule-judge harness,
// ported as three verbs -- `judge pin`, `judge drift`, `calibration
// summarize` -- per [capsulectl-book-verbs-v0]. These need no EvidenceBook:
// each reads plain JSON files the caller supplies and prints a result: no
// `--profile`, no store, no signing. The prompting/scoring half of
// capsule-judge stays a skill (an LLM call), never ported here -- "nothing
// that judges lives in Go" (evidencebook-skills-v0 design doc).
//
// The pin's own identity intentionally excludes point-in-time
// measured/policy fields (adjudication sampling rate, measured agreement
// rate existed in the old capsule_judge shape; this surface has no
// equivalent yet) -- same discipline as capsule_judge.capsules.judge_pin_digest:
// two runs of the SAME judge configuration must produce the SAME pin even as
// its measured stats move, which is what makes `judge drift` meaningful.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/spf13/cobra"
)

func judgeCommands() *cobra.Command {
	judge := &cobra.Command{Use: "judge", Short: "Deterministic judge-pin identity and drift comparison (no book required)"}
	judge.AddCommand(judgePinCommand(), judgeDriftCommands())
	return judge
}

func calibrationCommands() *cobra.Command {
	calibration := &cobra.Command{Use: "calibration", Short: "Deterministic calibration statistics (no book required)"}
	calibration.AddCommand(calibrationSummarizeCommand())
	return calibration
}

// decodeJSONPreserveNumbers is decodeJSON (seal.go) plus UseNumber: sampling
// params must distinguish an integer literal (digest-safe) from a float
// literal (rejected, same rule canonical.JSONDigest already enforces) by the
// exact text that was written, which plain float64 decoding cannot do --
// 700000 and 700000.0 are indistinguishable as a Go float64 but not as JSON
// text. Same convention as decodeBundleJSON/getCapsule elsewhere in this
// package.
func decodeJSONPreserveNumbers(raw []byte, v any) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrInput, err)
		}
	}()
	if len(raw) > maxInput {
		return inputError("input exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return inputError("invalid JSON input or unknown field")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return inputError("input must contain one JSON value")
	}
	return nil
}

func twoArgs(_ *cobra.Command, args []string) error {
	if len(args) != 2 {
		return ErrInput
	}
	return nil
}

// -- judge pin ----------------------------------------------------------

// judgePinInput is the caller-supplied reproducible call shape: model id +
// version, sampling params, prompt digest, axes digest. Never the
// point-in-time measured/policy fields -- those change across runs of the
// SAME judge and would make the pin move when nothing about the judge
// actually changed.
type judgePinInput struct {
	ModelID        string         `json:"model_id"`
	ModelVersion   *string        `json:"model_version,omitempty"`
	PromptDigest   string         `json:"prompt_digest"`
	AxesDigest     string         `json:"axes_digest"`
	SamplingParams map[string]any `json:"sampling_params,omitempty"`
}

// validateSamplingParams enforces the same digest-safety discipline as
// capsule_judge.capsules._validate_sampling_params: only string/bool/
// digest-safe-integer values are allowed. canonical.JSONDigest would itself
// reject a float or an out-of-range integer, but a nested object/array is
// legal JSON that JSONDigest would silently canonicalize -- rejecting it here
// keeps a sampling param to the flat, digest-safe shape the pin's identity
// depends on, same as the retired Python.
func validateSamplingParams(params map[string]any) error {
	for key, value := range params {
		switch v := value.(type) {
		case string, bool:
			continue
		case json.Number:
			if canonical.IsFloat(v) {
				return inputError("sampling_params[" + key + "] is not digest-safe: floats are forbidden (pre-scale to an integer, e.g. temperature_micros)")
			}
			if canonical.IsUnsafeInt(v) {
				return inputError("sampling_params[" + key + "] is not digest-safe: integer exceeds the safe range")
			}
		default:
			return inputError("sampling_params[" + key + "] is not digest-safe: only string, bool, or integer values are allowed")
		}
	}
	return nil
}

// judgePinDigest computes the pin's own identity: JSON-DIGEST (lowercase-hex
// SHA-256 of plain RFC 8785 JCS) over exactly the reproducible call shape.
// canonical.JSONDigest is the same primitive `bundle`/`publish` already use
// for capsule_id and BundleDigest -- this is not a second digest scheme.
func judgePinDigest(input judgePinInput) (string, error) {
	if input.ModelID == "" {
		return "", inputError("model_id is required")
	}
	if input.PromptDigest == "" {
		return "", inputError("prompt_digest is required")
	}
	if input.AxesDigest == "" {
		return "", inputError("axes_digest is required")
	}
	if err := validateSamplingParams(input.SamplingParams); err != nil {
		return "", err
	}
	var modelVersion any
	if input.ModelVersion != nil {
		modelVersion = *input.ModelVersion
	}
	canonicalDict := map[string]any{
		"model_id":        input.ModelID,
		"model_version":   modelVersion,
		"sampling_params": input.SamplingParams,
		"prompt_digest":   input.PromptDigest,
		"axes_digest":     input.AxesDigest,
	}
	digest, e := canonical.JSONDigest(canonicalDict)
	if e != nil {
		return "", errors.Join(ErrInput, e)
	}
	return digest, nil
}

type judgePinResult struct {
	JudgePinDigest string `json:"judge_pin_digest"`
}

func judgePinCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pin FILE",
		Short: "Compute a judge_pin_digest: JSON-DIGEST over model id, model version, sampling params, prompt digest, axes digest",
		Args:  oneArg,
		RunE: func(c *cobra.Command, args []string) error {
			raw, e := readInput(args[0])
			if e != nil {
				return e
			}
			var input judgePinInput
			if e := decodeJSONPreserveNumbers(raw, &input); e != nil {
				return e
			}
			digest, e := judgePinDigest(input)
			if e != nil {
				return e
			}
			return output(c, judgePinResult{JudgePinDigest: digest})
		},
	}
}

// -- judge drift ----------------------------------------------------------

func judgeDriftCommands() *cobra.Command {
	drift := &cobra.Command{Use: "drift", Short: "Compare two pins, or two evaluation-report/v1 sets, for drift"}
	drift.AddCommand(judgeDriftPinCommand(), judgeDriftReportsCommand())
	return drift
}

type judgeDriftPinResult struct {
	AJudgePinDigest string `json:"a_judge_pin_digest"`
	BJudgePinDigest string `json:"b_judge_pin_digest"`
	PinMatches      bool   `json:"pin_matches"`
}

func judgeDriftPinCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pin FILE_A FILE_B",
		Short: "Compare two judge-pin inputs: same reproducible judge, or a silent config change",
		Args:  twoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			rawA, e := readInput(args[0])
			if e != nil {
				return e
			}
			rawB, e := readInput(args[1])
			if e != nil {
				return e
			}
			var a, b judgePinInput
			if e := decodeJSONPreserveNumbers(rawA, &a); e != nil {
				return e
			}
			if e := decodeJSONPreserveNumbers(rawB, &b); e != nil {
				return e
			}
			digestA, e := judgePinDigest(a)
			if e != nil {
				return e
			}
			digestB, e := judgePinDigest(b)
			if e != nil {
				return e
			}
			return output(c, judgeDriftPinResult{
				AJudgePinDigest: digestA, BJudgePinDigest: digestB, PinMatches: digestA == digestB,
			})
		},
	}
}

// evaluationReportRecord is the minimal evaluation-report/v1 shape `judge
// drift reports` and `calibration summarize` read: case identity, the pin
// that produced it, and its verdict. The full record schema is
// [spec-judge-record-family-v1], which has not landed as of this writing --
// these three fields are exactly what the deterministic comparisons below
// need and nothing this verb invents ahead of that schema.
type evaluationReportRecord struct {
	CaseID         string `json:"case_id"`
	JudgePinDigest string `json:"judge_pin_digest"`
	Verdict        string `json:"verdict"`
}

func readReportSet(path string) ([]evaluationReportRecord, error) {
	raw, e := readInput(path)
	if e != nil {
		return nil, e
	}
	var records []evaluationReportRecord
	if e := decodeJSONPreserveNumbers(raw, &records); e != nil {
		return nil, e
	}
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if r.CaseID == "" {
			return nil, inputError("every report record requires a non-empty case_id")
		}
		if r.JudgePinDigest == "" {
			return nil, inputError("report record " + r.CaseID + " requires a non-empty judge_pin_digest")
		}
		if r.Verdict == "" {
			return nil, inputError("report record " + r.CaseID + " requires a non-empty verdict")
		}
		if seen[r.CaseID] {
			return nil, inputError("duplicate case_id in report set: " + r.CaseID)
		}
		seen[r.CaseID] = true
	}
	return records, nil
}

// driftCase mirrors capsule_judge.capsules.build_judge_drift_check_capsule's
// detail shape (pin_matches, label_matches, drifted = not(pin_matches AND
// label_matches) -- a pin mismatch alone counts as drift even when the label
// happens to agree, same as the retired Python's own acceptance test
// (test_pin_mismatch_alone_counts_as_drift_even_with_the_same_label).
type driftCase struct {
	CaseID       string `json:"case_id"`
	AVerdict     string `json:"a_verdict"`
	BVerdict     string `json:"b_verdict"`
	PinMatches   bool   `json:"pin_matches"`
	LabelMatches bool   `json:"label_matches"`
	Drifted      bool   `json:"drifted"`
}

// reportsDriftResult is the `judge drift reports` output. DriftRateMicros is
// omitted (never a fabricated 0) when Compared is 0 -- there is nothing to
// have measured a rate over.
type reportsDriftResult struct {
	Compared        int         `json:"compared"`
	Drifted         int         `json:"drifted"`
	DriftRateMicros *int64      `json:"drift_rate_micros,omitempty"`
	Cases           []driftCase `json:"cases"`
	UnmatchedA      []string    `json:"unmatched_a"`
	UnmatchedB      []string    `json:"unmatched_b"`
}

func judgeDriftReportsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reports FILE_A FILE_B",
		Short: "Compare two evaluation-report/v1 sets (JSON arrays) by case_id and report drift, never a silent disagreement",
		Args:  twoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			a, e := readReportSet(args[0])
			if e != nil {
				return e
			}
			b, e := readReportSet(args[1])
			if e != nil {
				return e
			}
			bByCase := make(map[string]evaluationReportRecord, len(b))
			for _, r := range b {
				bByCase[r.CaseID] = r
			}
			matchedB := make(map[string]bool, len(b))
			var cases []driftCase
			var drifted int
			for _, ra := range a {
				rb, ok := bByCase[ra.CaseID]
				if !ok {
					continue
				}
				matchedB[ra.CaseID] = true
				pinMatches := ra.JudgePinDigest == rb.JudgePinDigest
				labelMatches := ra.Verdict == rb.Verdict
				isDrift := !(pinMatches && labelMatches)
				if isDrift {
					drifted++
				}
				cases = append(cases, driftCase{
					CaseID: ra.CaseID, AVerdict: ra.Verdict, BVerdict: rb.Verdict,
					PinMatches: pinMatches, LabelMatches: labelMatches, Drifted: isDrift,
				})
			}
			sort.Slice(cases, func(i, j int) bool { return cases[i].CaseID < cases[j].CaseID })
			var unmatchedA, unmatchedB []string
			for _, ra := range a {
				if _, ok := bByCase[ra.CaseID]; !ok {
					unmatchedA = append(unmatchedA, ra.CaseID)
				}
			}
			for _, rb := range b {
				if !matchedB[rb.CaseID] {
					unmatchedB = append(unmatchedB, rb.CaseID)
				}
			}
			sort.Strings(unmatchedA)
			sort.Strings(unmatchedB)
			result := reportsDriftResult{
				Compared: len(cases), Drifted: drifted, Cases: cases,
				UnmatchedA: unmatchedA, UnmatchedB: unmatchedB,
			}
			// Never a fabricated 0% drift rate for an empty comparison --
			// same "unmeasured is not zero" discipline as capsule_judge's
			// compute_judge_calibration_stats.
			if len(cases) > 0 {
				rate := rateMicros(drifted, len(cases))
				result.DriftRateMicros = &rate
			}
			return output(c, result)
		},
	}
}

// rateMicros scales k/n to an integer in [0, 1_000_000] -- the same
// "digest-bearing fields never carry a float" discipline capsule_judge's
// _rate_micros/_confidence_micros apply to money and probabilities. This
// output is not itself digest-bearing (it is command text, not a sealed
// record field) but a value later cited into calibration-summary/v1 must
// already be in this shape, so it is produced in it from the start rather
// than converted at the point of sealing.
func rateMicros(k, n int) int64 {
	return int64((float64(k) / float64(n) * 1_000_000) + 0.5)
}

// -- calibration summarize -------------------------------------------------

// humanRatingRecord is the minimal human-rating/v1 shape calibration
// summarize reads. A rating may carry an explicit agrees_with_judge bool
// (the same field capsule_judge's adjudication capsules carried) or a plain
// rating label to compare against the report's verdict -- explicit
// agrees_with_judge wins when both are present, since it is what the human
// actually asserted rather than a derived comparison.
type humanRatingRecord struct {
	CaseID          string `json:"case_id"`
	Rating          string `json:"rating,omitempty"`
	AgreesWithJudge *bool  `json:"agrees_with_judge,omitempty"`
}

func readHumanRatings(path string) ([]humanRatingRecord, error) {
	raw, e := readInput(path)
	if e != nil {
		return nil, e
	}
	var records []humanRatingRecord
	if e := decodeJSONPreserveNumbers(raw, &records); e != nil {
		return nil, e
	}
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if r.CaseID == "" {
			return nil, inputError("every human-rating record requires a non-empty case_id")
		}
		if r.AgreesWithJudge == nil && r.Rating == "" {
			return nil, inputError("human-rating record " + r.CaseID + " needs either agrees_with_judge or rating")
		}
		// One canonical rating per case_id: a second, silently-overwriting
		// rating for the same case is exactly the kind of dropped-without-a-
		// trace input this codebase never accepts (same discipline as
		// readReportSet's duplicate-case_id rejection).
		if seen[r.CaseID] {
			return nil, inputError("duplicate case_id in human-rating set: " + r.CaseID)
		}
		seen[r.CaseID] = true
	}
	return records, nil
}

type calibrationPinSummary struct {
	JudgePinDigest      string `json:"judge_pin_digest"`
	EvaluatedCount      int    `json:"evaluated_count"`
	RatedCount          int    `json:"rated_count"`
	AgreementCount      int    `json:"agreement_count"`
	AgreementRateMicros *int64 `json:"agreement_rate_micros,omitempty"`
}

// summarizeCalibration folds a report set + a human-rating set into k of n
// agreement per judge_pin_digest -- the Go port of
// capsule_judge.calibration.compute_judge_calibration_stats, adapted from
// scanning a ledger (that function's source) to reading two files (this
// verb needs no book). agreement_rate_micros is omitted, never zero, when
// rated_count is 0: an unmeasured judge is a different, honest state from a
// judge measured at 0% agreement -- the same rule the ported function's own
// tests assert (test_no_judgments_yet_is_all_unmeasured,
// test_unadjudicated_judgments_leave_agreement_rate_unmeasured).
func summarizeCalibration(reports []evaluationReportRecord, ratings []humanRatingRecord) []calibrationPinSummary {
	ratingsByCase := make(map[string]humanRatingRecord, len(ratings))
	for _, r := range ratings {
		ratingsByCase[r.CaseID] = r
	}

	type tally struct {
		evaluated, rated, agreement int
	}
	byPin := make(map[string]*tally)
	var order []string
	for _, report := range reports {
		t, ok := byPin[report.JudgePinDigest]
		if !ok {
			t = &tally{}
			byPin[report.JudgePinDigest] = t
			order = append(order, report.JudgePinDigest)
		}
		t.evaluated++
		rating, ok := ratingsByCase[report.CaseID]
		if !ok {
			continue
		}
		t.rated++
		agrees := false
		if rating.AgreesWithJudge != nil {
			agrees = *rating.AgreesWithJudge
		} else {
			agrees = rating.Rating == report.Verdict
		}
		if agrees {
			t.agreement++
		}
	}
	sort.Strings(order)
	summaries := make([]calibrationPinSummary, 0, len(order))
	for _, pin := range order {
		t := byPin[pin]
		summary := calibrationPinSummary{
			JudgePinDigest: pin, EvaluatedCount: t.evaluated, RatedCount: t.rated, AgreementCount: t.agreement,
		}
		if t.rated > 0 {
			rate := rateMicros(t.agreement, t.rated)
			summary.AgreementRateMicros = &rate
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

type calibrationSummarizeResult struct {
	Pins []calibrationPinSummary `json:"pins"`
}

func calibrationSummarizeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "summarize REPORTS_FILE RATINGS_FILE",
		Short: "Fold an evaluation-report/v1 set and a human-rating/v1 set into k-of-n agreement per judge_pin_digest",
		Args:  twoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			reports, e := readReportSet(args[0])
			if e != nil {
				return e
			}
			ratings, e := readHumanRatings(args[1])
			if e != nil {
				return e
			}
			return output(c, calibrationSummarizeResult{Pins: summarizeCalibration(reports, ratings)})
		},
	}
}
