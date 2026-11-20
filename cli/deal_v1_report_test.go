package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	input := map[string]interface{}{
		"type": "deal_report", "profile": "x-deal-v1", "deal_id": "d1", "audience": "counterparty",
		"as_of":           "2026-10-21T08:30:00Z",
		"deal_checkpoint": map[string]interface{}{"log_id": "deal/d1", "mmr_size": float64(1), "root": "ab"},
	}
	if edit != nil {
		edit(input)
	}
	report := sealedMember(t, reportKey, DealReportActionID, input)
	id := report["capsule_id"].(string)
	// The report is signed by the last step's key unless reportKey says otherwise.
	return map[string]interface{}{
		"checkpoint":               map[string]interface{}{"mmr_size": float64(1), "root": "ab"},
		"completeness_certificate": map[string]interface{}{"log_id": "deal/d1", "memberships": map[string]interface{}{step["capsule_id"].(string): map[string]interface{}{"log_coordinates": map[string]interface{}{"seq": float64(1)}}}},
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
		"another deal":                                         {edit: func(i map[string]interface{}) { i["deal_id"] = "d2" }},
		"a non-copy audience":                                  {edit: func(i map[string]interface{}) { i["audience"] = "public" }},
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
