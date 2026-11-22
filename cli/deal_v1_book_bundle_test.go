package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v1BookBundle reads testdata/deal-v1-book-bundle/bundle.json: a bundle the
// deal-book library's Book.Bundle wrote from a real x-deal-v1 deal book (see
// the README beside it). Its step record carries no evidence_record_header;
// the header is the record's disclosed agent_input. No test helper copies it
// into the record (capsule-cli#604).
func v1BookBundle(t *testing.T) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile("testdata/deal-v1-book-bundle/bundle.json")
	require.NoError(t, err)
	value, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	records := value["records"].([]interface{})
	require.Len(t, records, 1)
	_, embedded := records[0].(map[string]interface{})["evidence_record_header"]
	require.False(t, embedded, "the fixture must be a bundle as a book writes it")
	return value
}

// disclosedHeader is the fixture's one disclosed agent_input, the step header.
func disclosedHeader(value map[string]interface{}) map[string]interface{} {
	for _, d := range value["disclosures"].(map[string]interface{}) {
		return d.(map[string]interface{})["agent_input"].(map[string]interface{})
	}
	return nil
}

func TestV1BookBundleStandaloneReportFindsItsLastStep(t *testing.T) {
	value := v1BookBundle(t)
	assert.True(t, holdsDealSteps(value))
	assert.NotNil(t, lastStepRecord(value))
	assert.NotEmpty(t, DealLastStepKey(value))

	SetDealV1ReportVerifier(nil)
	entry := DealReportEntry(value, aacbundle.VerificationResult{})
	assert.Equal(t, "pass", entry["status"], entry)
	assert.Equal(t, []string{DealV1ReportRuleNotChecked}, entry["findings"])
	assert.NoError(t, CheckDealStandaloneReport(value))
	assert.Contains(t, entry["as_of"], "as of 2026-")
}

// A disclosed header is read only when it matches what the record's Capsule
// commits, so a header that was edited, or one that is no longer disclosed,
// finds no step and the report is refused, never taken on trust.
func TestV1BookBundleRefusesAHeaderThatDoesNotMatchItsCommitment(t *testing.T) {
	for name, edit := range map[string]func(value map[string]interface{}){
		"seq edited":         func(v map[string]interface{}) { disclosedHeader(v)["seq"] = float64(9) },
		"committed_at":       func(v map[string]interface{}) { disclosedHeader(v)["committed_at"] = "2030-01-01T00:00:00Z" },
		"a member added":     func(v map[string]interface{}) { disclosedHeader(v)["extra"] = "x" },
		"record_type edited": func(v map[string]interface{}) { disclosedHeader(v)["record_type"] = "step_completed" },
		"not disclosed":      func(v map[string]interface{}) { delete(v, "disclosures") },
	} {
		t.Run(name, func(t *testing.T) {
			value := v1BookBundle(t)
			edit(value)
			assert.Nil(t, lastStepRecord(value))
			assert.False(t, holdsDealSteps(value))
			assert.Empty(t, DealLastStepKey(value))
			entry := DealReportEntry(value, aacbundle.VerificationResult{})
			assert.Equal(t, "fail", entry["status"], entry)
			assert.Equal(t, []string{"sealed_report_unverified"}, entry["findings"])
			assert.ErrorIs(t, CheckDealStandaloneReport(value), ErrBundleInvalid)
		})
	}
}

// A disclosed agent_input that matches its commitment but is not a step's
// (any other record's payload) is not a deal step.
func TestStepHeaderNeedsAStepRecordType(t *testing.T) {
	value := v1BookBundle(t)
	record := value["records"].([]interface{})[0].(map[string]interface{})
	require.NotNil(t, stepHeader(value, record))
	disclosedHeader(value)["record_type"] = "deal_note"
	assert.Nil(t, stepHeader(value, record))
}

// The x-deal-v0 shape still reads its embedded header, with no disclosure.
func TestStepHeaderPrefersTheEmbeddedHeader(t *testing.T) {
	embedded := map[string]interface{}{"record_type": "deal_step"}
	record := map[string]interface{}{"capsule_id": "a", "evidence_record_header": embedded}
	assert.Equal(t, embedded, stepHeader(map[string]interface{}{}, record))
	record["evidence_record_header"] = map[string]interface{}{"record_type": "other"}
	assert.Nil(t, stepHeader(map[string]interface{}{}, record))
}

// withSecondStep adds a later deal step signed by another key, its header
// disclosed (when disclose is true) and committed by its Capsule, as a book
// carries a step.
func withSecondStep(t *testing.T, value map[string]interface{}, disclose bool) map[string]interface{} {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	header := map[string]interface{}{"record_type": "deal_step", "seq": float64(2), "committed_at": "2026-09-27T19:00:00Z", "v": float64(1)}
	step := sealedMember(t, key, "step2", header)
	id := step["capsule_id"].(string)
	value["records"] = append(value["records"].([]interface{}), step)
	value["completeness_certificate"].(map[string]interface{})["memberships"].(map[string]interface{})[id] = map[string]interface{}{"log_coordinates": map[string]interface{}{"seq": float64(2)}}
	if disclose {
		value["disclosures"].(map[string]interface{})[id] = map[string]interface{}{"agent_input": header}
	}
	return value
}

// The report must be signed by the last step's key. With a later step by
// another signer disclosed, the first signer's report is refused; with that
// step's header withheld, the last step cannot be proved, so the report is
// refused too rather than read as signed by the last step it can see.
func TestV1BookBundleReportMustBeSignedByTheLastStep(t *testing.T) {
	for name, disclose := range map[string]bool{"later step disclosed": true, "later step's header withheld": false} {
		t.Run(name, func(t *testing.T) {
			value := withSecondStep(t, v1BookBundle(t), disclose)
			entry := DealReportEntry(value, aacbundle.VerificationResult{})
			assert.Equal(t, "fail", entry["status"], entry)
			assert.Equal(t, []string{"sealed_report_unverified"}, entry["findings"])
			assert.ErrorIs(t, CheckDealStandaloneReport(value), ErrBundleInvalid)
		})
	}
}

// A v1 copy of a book's steps whose section names no report is a stripped
// copy, now that its steps are found through their disclosed headers.
func TestV1BookBundleWithoutItsReportIsAStrippedCopy(t *testing.T) {
	value := v1BookBundle(t)
	exts := value["extensions"].(map[string]interface{})
	delete(exts, DealReportExtension)
	delete(exts[DealProfileV1].(map[string]interface{}), DealReportPointer)
	entry := DealReportEntry(value, aacbundle.VerificationResult{})
	assert.Equal(t, "fail", entry["status"], entry)
	assert.Equal(t, []string{"sealed_report_not_named"}, entry["findings"])
	assert.ErrorIs(t, CheckDealStandaloneReport(value), ErrBundleInvalid)
}

// The whole verify --bundle verdict on the real bytes: with no report rule
// registered the neutral checks pass and the bundle is INCOMPLETE on the
// rule it did not run; with a rule registered that accepts, it is VALID.
func TestV1BookBundleVerdict(t *testing.T) {
	SetDealV1ReportVerifier(nil)
	verdict, _ := BundleVerdict(v1BookBundle(t), nil)
	assert.Equal(t, "INCOMPLETE", verdict)

	var gotReport, gotExt []byte
	SetDealV1ReportVerifier(func(report, ext []byte) error { gotReport, gotExt = report, ext; return nil })
	t.Cleanup(func() { SetDealV1ReportVerifier(nil) })
	verdict, _ = BundleVerdict(v1BookBundle(t), nil)
	assert.Equal(t, "VALID", verdict)
	assert.NotEmpty(t, gotReport)
	assert.NotEmpty(t, gotExt)
}
