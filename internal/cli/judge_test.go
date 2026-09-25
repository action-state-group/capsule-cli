package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeJSONFile(t *testing.T, name string, value any) string {
	t.Helper()
	b, e := json.Marshal(value)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

// asObject and asObjectList decode a result field with comma-ok: the field
// was populated by this package's own output() call one line above, so a
// failed assertion here means the command's JSON shape changed, and the
// require.True below reports that plainly instead of panicking.
func asObject(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "expected a JSON object, got %T", v)
	return m
}
func asObjectList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	require.True(t, ok, "expected a JSON array, got %T", v)
	return l
}

// -- cross-implementation parity ------------------------------------------

// TestJSONDigestParityWithRetiredPythonJudgePin cross-checks
// canonical.JSONDigest -- the exact primitive judgePinDigest is built on --
// against the ACTUAL capsule_judge.capsules.judge_pin_digest function (the
// code this verb ports and retires), not a hand reconstruction of its
// canonicalization. Fixtures were computed at authoring time by calling the
// real, still-present dependency directly:
//
//	cd capsule-judge && python3 -c "from capsule_judge.capsules import judge_pin_digest; \
//	  print(judge_pin_digest(model_id=..., model_version=..., \
//	  sampling_params=..., prompt_digest=...))"
//
// This is the parity test [capsulectl-book-verbs-v0] requires before the
// Python original may be considered retired: it proves the Go
// canonicalization (map key ordering, None -> null, missing
// sampling_params -> {}) reproduces judge_pin_digest's OWN output byte for
// byte on the 4-field shape it actually builds
// (model_id/model_version/sampling_params/prompt_digest). It does not and
// cannot cover axes_digest, which is new to this port and has no Python
// precedent -- TestJudgePinDigestChangesWithEachReproducibleField below
// covers axes_digest's own behavior (it moves the pin), just not against a
// Python oracle that never had the field.
func TestJSONDigestParityWithRetiredPythonJudgePin(t *testing.T) {
	v2026 := "2026-09-01"
	cases := []struct {
		name           string
		modelID        string
		modelVersion   *string
		samplingParams map[string]any
		promptDigest   string
		want           string
	}{
		{
			name:         "no model_version, no sampling_params",
			modelID:      "gpt-eval/1.0",
			modelVersion: nil,
			promptDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want:         "66331e5d9f55c227b2df32e50b9abc29f9398751502ea0d03aeb891543a498e3",
		},
		{
			name:           "model_version and sampling_params",
			modelID:        "gpt-eval/1.0",
			modelVersion:   &v2026,
			samplingParams: map[string]any{"temperature_x100": json.Number("0"), "seed": json.Number("7")},
			promptDigest:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			want:           "9a5bb73f3728a362aaf24b51c69162ff82969847cd32b296ad220a54a821c873",
		},
		{
			name:           "different model, one sampling param",
			modelID:        "claude-judge/2.1",
			modelVersion:   nil,
			samplingParams: map[string]any{"top_p_x1000": json.Number("950")},
			promptDigest:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			want:           "c96c00f6de951e79254d824c19684d493a22ac82169af3924f55381b096dc1f2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var modelVersion any
			if tc.modelVersion != nil {
				modelVersion = *tc.modelVersion
			}
			samplingParams := tc.samplingParams
			if samplingParams == nil {
				samplingParams = map[string]any{}
			}
			// Exactly the 4-key shape capsule_judge.capsules.judge_pin_digest
			// builds -- no axes_digest, which did not exist in that shape.
			pythonShape := map[string]any{
				"model_id":        tc.modelID,
				"model_version":   modelVersion,
				"sampling_params": samplingParams,
				"prompt_digest":   tc.promptDigest,
			}
			got, e := canonical.JSONDigest(pythonShape)
			require.NoError(t, e)
			assert.Equal(t, tc.want, got, "Go JSONDigest must match the Python json_digest fixture byte-for-byte")
		})
	}
}

// -- judge pin --------------------------------------------------------------

func pinInputFixture() map[string]any {
	return map[string]any{
		"model_id":      "gpt-eval/1.0",
		"prompt_digest": "a" + repeat("a", 63),
		"axes_digest":   "b" + repeat("b", 63),
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestJudgePinDeterministic(t *testing.T) {
	path := writeJSONFile(t, "pin.json", pinInputFixture())
	out1, e := invoke(t, "", "judge", "pin", path)
	require.NoError(t, e)
	out2, e := invoke(t, "", "judge", "pin", path)
	require.NoError(t, e)
	assert.Equal(t, out1, out2)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out1), &result))
	digest, ok := result["judge_pin_digest"].(string)
	require.True(t, ok)
	assert.Len(t, digest, 64)
}

func TestJudgePinRequiresModelID(t *testing.T) {
	input := pinInputFixture()
	delete(input, "model_id")
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.ErrorIs(t, e, ErrInput)
}

func TestJudgePinRequiresPromptDigest(t *testing.T) {
	input := pinInputFixture()
	delete(input, "prompt_digest")
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.ErrorIs(t, e, ErrInput)
}

func TestJudgePinRequiresAxesDigest(t *testing.T) {
	input := pinInputFixture()
	delete(input, "axes_digest")
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.ErrorIs(t, e, ErrInput)
}

// TestJudgePinRejectsFloatSamplingParam: the float rejection here is
// defense in depth -- canonical.JSONDigest's own jcsValue already rejects a
// float json.Number, so this test cannot by itself distinguish
// validateSamplingParams's float branch from that downstream enforcement.
// TestJudgePinRejectsNestedSamplingParam below is the one that isolates a
// check only validateSamplingParams performs.
func TestJudgePinRejectsFloatSamplingParam(t *testing.T) {
	input := pinInputFixture()
	input["sampling_params"] = map[string]any{"temperature": 0.7}
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.ErrorIs(t, e, ErrInput)
}

// TestJudgePinRejectsNestedSamplingParam proves validateSamplingParams's
// structural check bites on its own: canonical.JSONDigest happily digests a
// nested map/array value with no error (verified by hand -- JSONDigest(
// map[string]any{"sampling_params": map[string]any{"nested": map[string]any{
// "bar": "baz"}}}) returns a digest, not an error), so a nested sampling
// param would silently pass if validateSamplingParams's default case were
// removed. This is the check the float/unsafe-int tests above cannot
// isolate.
func TestJudgePinRejectsNestedSamplingParam(t *testing.T) {
	input := pinInputFixture()
	input["sampling_params"] = map[string]any{"nested": map[string]any{"bar": "baz"}}
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.ErrorIs(t, e, ErrInput)
}

func TestJudgePinAcceptsIntegerSamplingParam(t *testing.T) {
	input := pinInputFixture()
	input["sampling_params"] = map[string]any{"seed": 7}
	path := writeJSONFile(t, "pin.json", input)
	_, e := invoke(t, "", "judge", "pin", path)
	require.NoError(t, e)
}

// TestJudgePinDigestChangesWithEachReproducibleField mirrors
// capsule_judge's own test_judge_pin_digest_changes_when_model_version_changes
// / _when_sampling_params_change: any change to the reproducible call shape
// must move the pin. Includes axes_digest, new to this port.
func TestJudgePinDigestChangesWithEachReproducibleField(t *testing.T) {
	base := pinInputFixture()
	basePath := writeJSONFile(t, "base.json", base)
	baseOut, e := invoke(t, "", "judge", "pin", basePath)
	require.NoError(t, e)

	variants := map[string]map[string]any{
		"model_id":      {"model_id": "different-model/1.0"},
		"model_version": {"model_version": "v2"},
		"prompt_digest": {"prompt_digest": "c" + repeat("c", 63)},
		"axes_digest":   {"axes_digest": "d" + repeat("d", 63)},
	}
	for field, override := range variants {
		t.Run(field, func(t *testing.T) {
			variant := pinInputFixture()
			for k, v := range override {
				variant[k] = v
			}
			path := writeJSONFile(t, field+".json", variant)
			out, e := invoke(t, "", "judge", "pin", path)
			require.NoError(t, e)
			assert.NotEqual(t, baseOut, out)
		})
	}
}

// -- judge drift pin ----------------------------------------------------

func TestJudgeDriftPinSameJudge(t *testing.T) {
	pathA := writeJSONFile(t, "a.json", pinInputFixture())
	pathB := writeJSONFile(t, "b.json", pinInputFixture())
	out, e := invoke(t, "", "judge", "drift", "pin", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, true, result["pin_matches"])
}

func TestJudgeDriftPinSilentModelUpgrade(t *testing.T) {
	a := pinInputFixture()
	b := pinInputFixture()
	b["model_version"] = "v2"
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "pin", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, false, result["pin_matches"])
}

// -- judge drift reports --------------------------------------------------

// TestJudgeDriftReportsDeliberateDrift mirrors capsule_judge's acceptance
// test (test_deliberately_drifted_judge_seals_a_delta_not_a_silent_disagreement):
// same pin, different verdict -- a real disagreement, sealed as a delta.
func TestJudgeDriftReportsDeliberateDrift(t *testing.T) {
	a := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "not_met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, float64(1), result["compared"])
	assert.Equal(t, float64(1), result["drifted"])
	cases := asObjectList(t, result["cases"])
	require.Len(t, cases, 1)
	c := asObject(t, cases[0])
	assert.Equal(t, true, c["pin_matches"])
	assert.Equal(t, false, c["label_matches"])
	assert.Equal(t, true, c["drifted"])
}

func TestJudgeDriftReportsNoDrift(t *testing.T) {
	a := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, float64(0), result["drifted"])
	assert.EqualValues(t, 0, result["drift_rate_micros"])
}

// TestJudgeDriftReportsPinMismatchAloneIsDrift mirrors
// test_pin_mismatch_alone_counts_as_drift_even_with_the_same_label: a silent
// model upgrade with the SAME label is still drift.
func TestJudgeDriftReportsPinMismatchAloneIsDrift(t *testing.T) {
	a := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-v1", "verdict": "met"}}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-v2", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	cases := asObjectList(t, result["cases"])
	c := asObject(t, cases[0])
	assert.Equal(t, false, c["pin_matches"])
	assert.Equal(t, true, c["label_matches"])
	assert.Equal(t, true, c["drifted"])
}

func TestJudgeDriftReportsUnmatchedCasesAreListedNotDropped(t *testing.T) {
	a := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-only-a", "judge_pin_digest": "pin-a", "verdict": "met"},
	}
	b := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-only-b", "judge_pin_digest": "pin-a", "verdict": "met"},
	}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, float64(1), result["compared"])
	assert.Equal(t, []any{"case-only-a"}, result["unmatched_a"])
	assert.Equal(t, []any{"case-only-b"}, result["unmatched_b"])
}

func TestJudgeDriftReportsEmptyComparisonOmitsDriftRate(t *testing.T) {
	a := []map[string]any{{"case_id": "case-only-a", "judge_pin_digest": "pin-a", "verdict": "met"}}
	b := []map[string]any{{"case_id": "case-only-b", "judge_pin_digest": "pin-a", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	out, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	_, present := result["drift_rate_micros"]
	assert.False(t, present, "an empty comparison must never fabricate a 0%% drift rate")
}

func TestJudgeDriftReportsRejectsDuplicateCaseID(t *testing.T) {
	a := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "not_met"},
	}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	_, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.ErrorIs(t, e, ErrInput)
}

// TestJudgeDriftReportsRejectsMissingJudgePinDigest: an empty
// judge_pin_digest on both sides must not be silently treated as a matching
// pin ("" == "" would otherwise read as pin_matches=true for two reports
// that never recorded a pin at all).
func TestJudgeDriftReportsRejectsMissingJudgePinDigest(t *testing.T) {
	a := []map[string]any{{"case_id": "case-1", "verdict": "met"}}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	_, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.ErrorIs(t, e, ErrInput)
}

func TestJudgeDriftReportsRejectsMissingVerdict(t *testing.T) {
	a := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a"}}
	b := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	pathA := writeJSONFile(t, "a.json", a)
	pathB := writeJSONFile(t, "b.json", b)
	_, e := invoke(t, "", "judge", "drift", "reports", pathA, pathB)
	require.ErrorIs(t, e, ErrInput)
}

// -- calibration summarize -------------------------------------------------

func TestCalibrationSummarizeUnmeasuredIsNeverFabricatedZero(t *testing.T) {
	reports := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-2", "judge_pin_digest": "pin-a", "verdict": "not_met"},
	}
	ratings := []map[string]any{}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	out, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	pins := asObjectList(t, result["pins"])
	require.Len(t, pins, 1)
	pin := asObject(t, pins[0])
	assert.EqualValues(t, 2, pin["evaluated_count"])
	assert.EqualValues(t, 0, pin["rated_count"])
	_, present := pin["agreement_rate_micros"]
	assert.False(t, present, "an unmeasured pin must never report a fabricated 0%% agreement rate")
}

func TestCalibrationSummarizeExplicitAgreesWithJudge(t *testing.T) {
	reports := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-2", "judge_pin_digest": "pin-a", "verdict": "met"},
	}
	ratings := []map[string]any{
		{"case_id": "case-1", "agrees_with_judge": true},
		{"case_id": "case-2", "agrees_with_judge": false},
	}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	out, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	pins := asObjectList(t, result["pins"])
	pin := asObject(t, pins[0])
	assert.EqualValues(t, 2, pin["rated_count"])
	assert.EqualValues(t, 1, pin["agreement_count"])
	assert.EqualValues(t, 500000, pin["agreement_rate_micros"])
}

func TestCalibrationSummarizeRatingLabelComparedToVerdict(t *testing.T) {
	reports := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	ratings := []map[string]any{{"case_id": "case-1", "rating": "met"}}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	out, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	pins := asObjectList(t, result["pins"])
	pin := asObject(t, pins[0])
	assert.EqualValues(t, 1, pin["agreement_count"])
	assert.EqualValues(t, 1000000, pin["agreement_rate_micros"])
}

// TestCalibrationSummarizeScopedPerJudgePinDigest uses deliberately
// asymmetric data per pin -- pin-a has 3 evaluated cases, only 2 rated, 1 of
// those an agreement (partially rated: evaluated_count > rated_count > 0,
// the case symmetric fixtures can't exercise); pin-b has 1 evaluated case,
// fully rated, a disagreement. If a cross-attribution bug folded pin-b's
// case into pin-a's tally (or vice versa), the counts below would not
// match: a fixture where every pin has the same n/k could not tell the two
// apart.
func TestCalibrationSummarizeScopedPerJudgePinDigest(t *testing.T) {
	reports := []map[string]any{
		{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-2", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-3", "judge_pin_digest": "pin-a", "verdict": "met"},
		{"case_id": "case-4", "judge_pin_digest": "pin-b", "verdict": "met"},
	}
	ratings := []map[string]any{
		{"case_id": "case-1", "agrees_with_judge": true},
		{"case_id": "case-2", "agrees_with_judge": false},
		// case-3 deliberately has no rating: pin-a is partially rated.
		{"case_id": "case-4", "agrees_with_judge": false},
	}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	out, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	pins := asObjectList(t, result["pins"])
	require.Len(t, pins, 2)

	pinA := asObject(t, pins[0])
	assert.Equal(t, "pin-a", pinA["judge_pin_digest"])
	assert.EqualValues(t, 3, pinA["evaluated_count"])
	assert.EqualValues(t, 2, pinA["rated_count"])
	assert.EqualValues(t, 1, pinA["agreement_count"])
	assert.EqualValues(t, 500000, pinA["agreement_rate_micros"])

	pinB := asObject(t, pins[1])
	assert.Equal(t, "pin-b", pinB["judge_pin_digest"])
	assert.EqualValues(t, 1, pinB["evaluated_count"])
	assert.EqualValues(t, 1, pinB["rated_count"])
	assert.EqualValues(t, 0, pinB["agreement_count"])
	assert.EqualValues(t, 0, pinB["agreement_rate_micros"])
}

func TestCalibrationSummarizeDeterministic(t *testing.T) {
	reports := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	ratings := []map[string]any{{"case_id": "case-1", "agrees_with_judge": true}}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	out1, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	out2, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.NoError(t, e)
	assert.Equal(t, out1, out2)
}

func TestCalibrationSummarizeRejectsRatingWithNeitherField(t *testing.T) {
	reports := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	ratings := []map[string]any{{"case_id": "case-1"}}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	_, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.ErrorIs(t, e, ErrInput)
}

// TestCalibrationSummarizeRejectsDuplicateCaseIDInRatings mirrors
// TestJudgeDriftReportsRejectsDuplicateCaseID: a second, silently
// overwriting rating for the same case_id must be rejected, not resolved by
// last-write-wins -- same discipline readReportSet already applies, now
// matched in readHumanRatings.
func TestCalibrationSummarizeRejectsDuplicateCaseIDInRatings(t *testing.T) {
	reports := []map[string]any{{"case_id": "case-1", "judge_pin_digest": "pin-a", "verdict": "met"}}
	ratings := []map[string]any{
		{"case_id": "case-1", "agrees_with_judge": true},
		{"case_id": "case-1", "agrees_with_judge": false},
	}
	reportsPath := writeJSONFile(t, "reports.json", reports)
	ratingsPath := writeJSONFile(t, "ratings.json", ratings)
	_, e := invoke(t, "", "calibration", "summarize", reportsPath, ratingsPath)
	require.ErrorIs(t, e, ErrInput)
}
