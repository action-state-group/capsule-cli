package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedMember seals one capsule over agent_input and returns it as a bundle
// record carries it: the capsule's members plus capsule_id, signature, key_id.
func sealedMember(t *testing.T, key ed25519.PrivateKey, actionID string, agentInput map[string]interface{}) map[string]interface{} {
	t.Helper()
	request, err := ParseRequest([]byte(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"` + actionID + `","ActionType":"fyi","Operator":"op","Developer":"dev","Timestamp":"2026-10-20T00:00:00Z"}}`))
	require.NoError(t, err)
	identity, err := emit.NewEd25519SigningIdentity(key)
	require.NoError(t, err)
	result, err := emit.Seal(emit.SealInput{Capsule: request.Capsule, Payload: agentInput, Identity: identity})
	require.NoError(t, err)
	record, err := emit.DecodePayload(result.Payload)
	require.NoError(t, err)
	record["capsule_id"] = result.CapsuleID
	record["signature"] = hex.EncodeToString(result.Envelope)
	record["key_id"] = hex.EncodeToString(key.Public().(ed25519.PublicKey))
	return record
}

// v1ReportCopy is a copy's bundle as far as the standalone report checks read
// it: one deal step signed by key, the checkpoint, and the x-deal-v1 section
// naming a report sealed by reportKey over input.
func v1ReportCopy(t *testing.T, reportKey ed25519.PrivateKey, edit func(input map[string]interface{}), ext string) map[string]interface{} {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	if reportKey == nil {
		reportKey = key
	}
	step := sealedMember(t, key, "step", map[string]interface{}{"n": float64(1)})
	step["evidence_record_header"] = map[string]interface{}{"record_type": "deal_step", "committed_at": "2026-10-20T00:00:00Z"}
	// A v1 book's log id is random, not derived from the deal id; v0's is.
	logID := "deal/d1"
	if ext == DealProfileV1 {
		logID = "deal/5c1e0f7a9b3d44e2a6c8f0b1d2e3a4b5"
	}
	input := map[string]interface{}{
		"type": "deal_report", "profile": "x-deal-v1", "deal_id": "d1", "audience": "counterparty",
		"as_of":           "2026-10-21T08:30:00Z",
		"deal_checkpoint": map[string]interface{}{"log_id": logID, "mmr_size": float64(1), "root": "ab"},
		"rests_on":        map[string]interface{}{"own": map[string]interface{}{"log_id": logID, "size": float64(1), "last": "ab"}},
	}
	if edit != nil {
		edit(input)
	}
	report := sealedMember(t, reportKey, DealReportActionID, input)
	id := report["capsule_id"].(string)
	// The report is signed by the last step's key unless reportKey says otherwise.
	return map[string]interface{}{
		"checkpoint":               map[string]interface{}{"mmr_size": float64(1), "root": "ab"},
		"completeness_certificate": map[string]interface{}{"log_id": logID, "memberships": map[string]interface{}{step["capsule_id"].(string): map[string]interface{}{"log_coordinates": map[string]interface{}{"seq": float64(1)}}}},
		"records":                  []interface{}{step},
		"extensions": map[string]interface{}{
			ext:                 map[string]interface{}{DealReportPointer: id},
			DealReportExtension: map[string]interface{}{"record": report, "agent_input": input},
		},
	}
}

// asFileRead makes the copy what verify --bundle reads: the file's JSON, decoded as the verifier does.
func asFileRead(t *testing.T, v map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	out, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	return out
}

func TestDealV1StandaloneReportPasses(t *testing.T) {
	entry := DealReportEntry(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfileV1)), aacbundle.VerificationResult{})
	assert.Equal(t, "pass", entry["status"], entry)
	assert.Equal(t, DealProfileV1, entry["kind"])
	assert.Equal(t, "standalone", entry["report"])
	// The profile's report rule is the plugin's: never a clean pass here.
	assert.Equal(t, []string{DealV1ReportRuleNotChecked}, entry["findings"])
	assert.Contains(t, entry["note"], "capsulectl-deal")
	assert.Equal(t, "as of 2026-10-21T08:30:00Z (the holder's time, when it computed the report; no witness attests it)", entry["as_of"])
}

func TestDealV1StandaloneReportFails(t *testing.T) {
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cases := map[string]struct {
		key  ed25519.PrivateKey
		edit func(map[string]interface{})
	}{
		"a report signed by a key that is not the last step's": {key: other},
		"a v0 profile in a v1 report":                          {edit: func(i map[string]interface{}) { i["profile"] = DealProfile }},
		"no as_of":                                             {edit: func(i map[string]interface{}) { delete(i, "as_of") }},
		"as_of not UTC":                                        {edit: func(i map[string]interface{}) { i["as_of"] = "2026-10-21T03:30:00-05:00" }},
		"as_of with fractional seconds":                        {edit: func(i map[string]interface{}) { i["as_of"] = "2026-10-21T08:30:00.5Z" }},
		"as_of not a time":                                     {edit: func(i map[string]interface{}) { i["as_of"] = "soon" }},
		"no deal_id":                                           {edit: func(i map[string]interface{}) { i["deal_id"] = "" }},
		"resting on another book's log": {edit: func(i map[string]interface{}) {
			i["rests_on"].(map[string]interface{})["own"].(map[string]interface{})["log_id"] = "deal/other"
		}},
		"a non-copy audience": {edit: func(i map[string]interface{}) { i["audience"] = "public" }},
		"another checkpoint root": {edit: func(i map[string]interface{}) {
			i["deal_checkpoint"].(map[string]interface{})["root"] = "cd"
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			entry := DealReportEntry(asFileRead(t, v1ReportCopy(t, c.key, c.edit, DealProfileV1)), aacbundle.VerificationResult{})
			assert.Equal(t, "fail", entry["status"])
			assert.Equal(t, []string{"sealed_report_unverified"}, entry["findings"])
		})
	}
	t.Run("an edited report text under the signed digest", func(t *testing.T) {
		value := v1ReportCopy(t, nil, nil, DealProfileV1)
		value["extensions"].(map[string]interface{})[DealReportExtension].(map[string]interface{})["agent_input"].(map[string]interface{})["as_of"] = "2026-10-22T00:00:00Z"
		entry := DealReportEntry(asFileRead(t, value), aacbundle.VerificationResult{})
		assert.Equal(t, "fail", entry["status"])
	})
	t.Run("x-deal-v1 beside x-deal-v0", func(t *testing.T) {
		value := v1ReportCopy(t, nil, nil, DealProfileV1)
		value["extensions"].(map[string]interface{})[DealProfile] = map[string]interface{}{}
		entry := DealReportEntry(asFileRead(t, value), aacbundle.VerificationResult{})
		assert.Equal(t, "fail", entry["status"])
	})
	t.Run("the pointer without the carrier", func(t *testing.T) {
		value := v1ReportCopy(t, nil, nil, DealProfileV1)
		delete(value["extensions"].(map[string]interface{}), DealReportExtension)
		entry := DealReportEntry(asFileRead(t, value), aacbundle.VerificationResult{})
		assert.Equal(t, "fail", entry["status"])
	})
	t.Run("the carrier without the pointer", func(t *testing.T) {
		value := v1ReportCopy(t, nil, nil, DealProfileV1)
		value["extensions"].(map[string]interface{})[DealProfileV1] = map[string]interface{}{}
		entry := DealReportEntry(asFileRead(t, value), aacbundle.VerificationResult{})
		assert.Equal(t, "fail", entry["status"])
	})
	t.Run("a stripped copy names no report", func(t *testing.T) {
		value := v1ReportCopy(t, nil, nil, DealProfileV1)
		delete(value["extensions"].(map[string]interface{}), DealReportExtension)
		value["extensions"].(map[string]interface{})[DealProfileV1] = map[string]interface{}{}
		entry := DealReportEntry(asFileRead(t, value), aacbundle.VerificationResult{})
		assert.Equal(t, "fail", entry["status"])
		assert.Equal(t, []string{"sealed_report_not_named"}, entry["findings"])
	})
}

// A v1 report is named for a deal whose book log id is unrelated to it.
func TestDealV1ReportBindsToTheBookByItsLogNotTheDealID(t *testing.T) {
	value := asFileRead(t, v1ReportCopy(t, nil, func(i map[string]interface{}) { i["deal_id"] = "some-other-name" }, DealProfileV1))
	entry := DealReportEntry(value, aacbundle.VerificationResult{})
	assert.Equal(t, "pass", entry["status"], entry)
}

func TestDealV0StandaloneReportIsUnchangedByV1(t *testing.T) {
	// A v1-shaped payload under the v0 extension is not a v0 report.
	entry := DealReportEntry(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfile)), aacbundle.VerificationResult{})
	assert.Equal(t, "fail", entry["status"])
	assert.Equal(t, DealProfile, entry["kind"])
}

func TestCheckDealStandaloneReportReadsV1(t *testing.T) {
	assert.NoError(t, CheckDealStandaloneReport(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfileV1))))
	value := v1ReportCopy(t, nil, func(i map[string]interface{}) { i["as_of"] = "soon" }, DealProfileV1)
	assert.ErrorIs(t, CheckDealStandaloneReport(asFileRead(t, value)), ErrBundleInvalid)
}

// verify --bundle's checks for the failures a helper reader also sees: a v1
// pointer with no carrier, or both extensions, never read as passing.
func TestCheckDealStandaloneReportRefusesV1FailuresWithoutACarrier(t *testing.T) {
	noCarrier := v1ReportCopy(t, nil, nil, DealProfileV1)
	delete(noCarrier["extensions"].(map[string]interface{}), DealReportExtension)
	assert.ErrorIs(t, CheckDealStandaloneReport(asFileRead(t, noCarrier)), ErrBundleInvalid)

	both := v1ReportCopy(t, nil, nil, DealProfileV1)
	both["extensions"].(map[string]interface{})[DealProfile] = map[string]interface{}{}
	delete(both["extensions"].(map[string]interface{}), DealReportExtension)
	assert.ErrorIs(t, CheckDealStandaloneReport(asFileRead(t, both)), ErrBundleInvalid)
}

// The report verify --bundle prints for a v1 copy: the extension entry shows
// the holder's time, never the checkpoint wording, and a bundle naming both
// versions lists each extension once under its own name with one failure.
func TestAssessBundleShowsTheV1ReportEntry(t *testing.T) {
	entries := func(value map[string]interface{}) []map[string]any {
		_, _, report := bundleAssessment(value, nil)
		out, _ := report["extensions"].([]map[string]any)
		return out
	}
	got := entries(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfileV1)))
	var v1 map[string]any
	for _, e := range got {
		if e["kind"] == DealProfileV1 {
			v1 = e
		}
	}
	require.NotNil(t, v1, got)
	assert.Equal(t, "pass", v1["status"])
	assert.Equal(t, []string{DealV1ReportRuleNotChecked}, v1["findings"])
	assert.Equal(t, "as of 2026-10-21T08:30:00Z (the holder's time, when it computed the report; no witness attests it)", v1["as_of"])

	both := v1ReportCopy(t, nil, nil, DealProfileV1)
	both["extensions"].(map[string]interface{})[DealProfile] = map[string]interface{}{}
	value := asFileRead(t, both)
	verdict, _, report := bundleAssessment(value, nil)
	assert.Equal(t, "INVALID", verdict)
	kinds, fails := map[any]int{}, 0
	for _, e := range report["extensions"].([]map[string]any) {
		kinds[e["kind"]]++
		if e["status"] == "fail" {
			fails++
		}
	}
	assert.Equal(t, map[any]int{DealProfile: 1, DealProfileV1: 1, DealReportExtension: 1}, kinds)
	assert.Equal(t, 1, fails)
}

func TestAPassingV1ReportKeepsTheVerdictFromReadingValid(t *testing.T) {
	entry := DealReportEntry(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfileV1)), aacbundle.VerificationResult{})
	claim := aacbundle.ClaimResult{Status: "pass", Findings: entry["findings"].([]string)}
	assert.Equal(t, "INCOMPLETE", claimsVerdict([]aacbundle.ClaimResult{claim}))
}

// withBook adds the x-deal-v1 extension's book to the copy's section, as a
// host that cuts a counterparty copy does: the core carries it to the rule
// and reads none of it.
func withBook(copy map[string]interface{}) map[string]interface{} {
	section := copy["extensions"].(map[string]interface{})[DealProfileV1].(map[string]interface{})
	section["book"] = map[string]interface{}{
		"agreement": map[string]interface{}{"record": map[string]interface{}{"n": float64(1)}},
		"relied_on": map[string]interface{}{},
	}
	return copy
}

func TestDealV1ReportRuleHook(t *testing.T) {
	t.Cleanup(func() { SetDealV1ReportVerifier(nil) })
	var gotReport, gotExt map[string]interface{}
	var refuse error
	SetDealV1ReportVerifier(func(report, ext []byte) error {
		require.NoError(t, json.Unmarshal(report, &gotReport))
		require.NoError(t, json.Unmarshal(ext, &gotExt))
		return refuse
	})
	copy := func() map[string]interface{} {
		return asFileRead(t, withBook(v1ReportCopy(t, nil, nil, DealProfileV1)))
	}

	entry := DealReportEntry(copy(), aacbundle.VerificationResult{})
	assert.Equal(t, "pass", entry["status"], entry)
	assert.NotContains(t, entry, "findings", "an accepted report no longer reads report_rule_not_checked")
	assert.Equal(t, DealReportActionID, gotReport["record"].(map[string]interface{})["action_id"])
	assert.Equal(t, "deal_report", gotReport["agent_input"].(map[string]interface{})["type"])
	// The rule gets the extension whole: the pointer and the book, unread by the core.
	assert.Equal(t, gotReport["record"].(map[string]interface{})["capsule_id"], gotExt[DealReportPointer])
	assert.Equal(t, map[string]interface{}{
		"agreement": map[string]interface{}{"record": map[string]interface{}{"n": float64(1)}},
		"relied_on": map[string]interface{}{},
	}, gotExt["book"])

	refuse = errors.New("the first record is not the agreement")
	entry = DealReportEntry(copy(), aacbundle.VerificationResult{})
	assert.Equal(t, "fail", entry["status"])
	assert.Equal(t, []string{DealV1ReportRuleFailed}, entry["findings"])
	assert.Contains(t, entry["note"], "not the agreement")

	// A section with no book still reaches the rule, which refuses it: the
	// core does not decide whether a copy must carry one.
	gotReport, gotExt = nil, nil
	entry = DealReportEntry(asFileRead(t, v1ReportCopy(t, nil, nil, DealProfileV1)), aacbundle.VerificationResult{})
	assert.Equal(t, "fail", entry["status"])
	require.NotNil(t, gotExt)
	assert.NotContains(t, gotExt, "book")

	// Neutral failures stay failures when the rule would accept.
	refuse = nil
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	gotReport = nil
	entry = DealReportEntry(asFileRead(t, withBook(v1ReportCopy(t, other, nil, DealProfileV1))), aacbundle.VerificationResult{})
	assert.Equal(t, "fail", entry["status"])
	assert.Nil(t, gotReport, "the rule runs only after the neutral checks")
}

// Without a registered rule, a section with a book is neutral-checked only:
// the report passes and stays report_rule_not_checked.
func TestDealV1BookWithoutARuleIsNotChecked(t *testing.T) {
	entry := DealReportEntry(asFileRead(t, withBook(v1ReportCopy(t, nil, nil, DealProfileV1))), aacbundle.VerificationResult{})
	assert.Equal(t, "pass", entry["status"], entry)
	assert.Equal(t, []string{DealV1ReportRuleNotChecked}, entry["findings"])
}

// What the bundle verdict reads from the entry: an accepted report adds no
// claim (so no INCOMPLETE finding), a refused one a failing claim (INVALID).
func TestDealV1ReportRuleVerdict(t *testing.T) {
	t.Cleanup(func() { SetDealV1ReportVerifier(nil) })
	var refuse error
	SetDealV1ReportVerifier(func(_, _ []byte) error { return refuse })
	read := func() map[string]any {
		return DealReportEntry(asFileRead(t, withBook(v1ReportCopy(t, nil, nil, DealProfileV1))), aacbundle.VerificationResult{})
	}
	entry := read()
	_, hasFindings := entry["findings"].([]string)
	assert.False(t, hasFindings, "an accepted report adds no claim, as the verdict loop in assessBundle reads findings")
	refuse = errors.New("no")
	entry = read()
	claim := aacbundle.ClaimResult{Status: entry["status"].(string), Findings: entry["findings"].([]string)}
	assert.Equal(t, "INVALID", claimsVerdict([]aacbundle.ClaimResult{claim}))
}

// CheckDealStandaloneReport reports the rule's refusal as such.
func TestDealV1ReportRuleCheck(t *testing.T) {
	t.Cleanup(func() { SetDealV1ReportVerifier(nil) })
	var refuse error
	SetDealV1ReportVerifier(func(_, _ []byte) error { return refuse })
	good := func() map[string]interface{} {
		return asFileRead(t, withBook(v1ReportCopy(t, nil, nil, DealProfileV1)))
	}
	assert.NoError(t, CheckDealStandaloneReport(good()))
	refuse = errors.New("the first record is not the agreement")
	err := CheckDealStandaloneReport(good())
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.ErrorContains(t, err, DealV1ReportRuleFailed)
	assert.ErrorContains(t, err, "not the agreement")
}
