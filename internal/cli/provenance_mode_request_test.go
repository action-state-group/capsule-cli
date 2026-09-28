package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"

	"github.com/action-state-group/agent-action-capsule/go/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Vector capsule_ids and field values are copied by hand from
// capsule-emit-go's testdata/provenance-mode-vectors/ (itself a byte-for-byte
// copy of the AAC provenance-mode-vectors corpus, agent-action-capsule,
// branch aac-external-review-followups, commit 8def04c5). Those released
// vectors carry spec_version -04 and are never rewritten. capsulectl now
// stamps -05, which the capsule_id commits to, so a positive case is proved
// byte-identical to its committed vector with only spec_version (and, for a
// chained child, the parent_capsule_id that follows from it) moved to -05.
const (
	posBackfilledCapsuleID     = "52bd075279c3528c2e08962a6e5948ad82e68fd079d8198e7a2a3c27d0883bd4"
	negTimeRungOverclaimID     = "ec8bb8fed1f318cb20e606df4c5d700e5d5a766eb8b49601e1a4cd01277b1376"
	negTimeLaunderingID        = "9dbb97d6c1f7caf9b57a3a672ad82a294509376960920ed5c705adf3f99b9027"
	posChainDuplicatesParentID = "ed3cdeca453ee37e40bbeeee0f582f579873fa690a6af2d729578595b287d378"
	posChainDuplicatesChildID  = "5d7ee9fe6af6391a0a905b3297316bc483d781098d9ceca4fa697fa591f929dd"
	sourceRefDigestAllThrees   = "3333333333333333333333333333333333333333333333333333333333333333"

	// -05 twins of the positive vectors above, as capsulectl seals them.
	posBackfilledCapsuleIDV05     = "60ccc31abd4d364aa3c6497f296b3546d67843045c6eb499a5f8196002aaf167"
	posChainDuplicatesParentIDV05 = "33309c97203d757cfc6afd58e4183323962b5053d21fa69942661f6dc501baf5"
	posChainDuplicatesChildIDV05  = "f3ca2ead12836bb3221ab1168d94cdc8a942f7162e8ba31a7e0c803a10e7d20a"
)

const provenanceModeVectorDir = "testdata/provenance-mode-vectors"

func provenanceVectorDisposition() string {
	return `"Disposition":{"Decision":"accept","Approver":"policy","HumanDisposed":false,"VerdictClass":"executed"}`
}

// requireCommittedCapsuleID proves a committed vector capsule still carries,
// and recomputes to, its pinned capsule_id.
func requireCommittedCapsuleID(t *testing.T, capsule map[string]any, capsuleID string) {
	t.Helper()
	require.Equal(t, capsuleID, capsule["capsule_id"], "vector fixture drifted from its pinned capsule_id")
	recomputed, err := canonical.ComputeCapsuleID(capsule)
	require.NoError(t, err)
	require.Equal(t, capsuleID, recomputed)
}

// requireV05TwinBytes proves sealed is byte-for-byte the committed -04
// vector capsule with spec_version moved to -05 (and any overrides applied),
// capsule_id recomputed.
func requireV05TwinBytes(t *testing.T, committed map[string]any, overrides map[string]any, sealed []byte, sealedID string) {
	t.Helper()
	twin := make(map[string]any, len(committed))
	for k, v := range committed {
		twin[k] = v
	}
	for k, v := range overrides {
		twin[k] = v
	}
	twin["spec_version"] = emit.SpecVersion
	delete(twin, "capsule_id")
	id, err := canonical.ComputeCapsuleID(twin)
	require.NoError(t, err)
	twin["capsule_id"] = id
	want, err := canonical.JCS(twin)
	require.NoError(t, err)
	assert.Equal(t, id, sealedID)
	assert.Equal(t, string(want), string(sealed))
}

func loadProvenanceVector(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(emitModuleDir(t), provenanceModeVectorDir, name, "input.json"))
	require.NoError(t, err)
	payload, err := emit.DecodePayload(raw)
	require.NoError(t, err)
	return payload
}

func posBackfilledRequest() string {
	return `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-backfilled","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
		`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`
}

// TestBackfilledSealIsCommittedVectorWithSpecVersion05 seals the
// pos-provenance-mode-backfilled request through parseRequest/seal and proves
// the result is the committed -04 vector's bytes with only spec_version
// moved to -05.
func TestBackfilledSealIsCommittedVectorWithSpecVersion05(t *testing.T) {
	committed := loadProvenanceVector(t, "pos-provenance-mode-backfilled")
	requireCommittedCapsuleID(t, committed, posBackfilledCapsuleID)

	r, err := parseRequest([]byte(posBackfilledRequest()))
	require.NoError(t, err)
	_, key := profileFixture(t)
	record, err := seal(r, key)
	require.NoError(t, err)
	assert.Equal(t, posBackfilledCapsuleIDV05, record.CapsuleID)
	requireV05TwinBytes(t, committed, nil, record.Capsule, record.CapsuleID)
}

// TestSealRefusesProvenanceModeTimingNegatives: the two timing-negative
// vectors are Class 1 check 9 findings, and emit.Seal self-checks every
// record, so capsulectl refuses to seal either shape. Their committed -04
// bytes still recompute to the pinned capsule_id.
func TestSealRefusesProvenanceModeTimingNegatives(t *testing.T) {
	cases := []struct {
		name        string
		capsuleID   string
		requestBody string
	}{
		{
			// -05 §5.3(bis) time_rung="witnessed" with no corroborating reference.
			name:      "neg-provenance-mode-time-rung-overclaim",
			capsuleID: negTimeRungOverclaimID,
			requestBody: `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-witnessed-overclaim","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
				`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z","time_rung":"witnessed"}}`,
		},
		{
			// imported_at == source_asserted_at: the laundering shape.
			name:      "neg-provenance-mode-time-laundering",
			capsuleID: negTimeLaunderingID,
			requestBody: `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-laundering","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
				`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-09-22T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireCommittedCapsuleID(t, loadProvenanceVector(t, tc.name), tc.capsuleID)

			r, err := parseRequest([]byte(tc.requestBody))
			require.NoError(t, err)
			_, key := profileFixture(t)
			record, err := seal(r, key)
			require.ErrorIs(t, err, ErrInput)
			assert.ErrorContains(t, err, "emit rejected sealing input")
			assert.Empty(t, record.CapsuleID)
			assert.Empty(t, record.Capsule)
			assert.Empty(t, record.ProducerEnvelope)
		})
	}
}

// TestProvenanceModeMissingFieldsRejectedProducerSide covers the 5th vector,
// neg-provenance-mode-backfilled-missing-fields: mode="backfilled" with none
// of the four required companion fields. Like the two timing negatives
// above, this shape can never come out of our own producer -- it is rejected
// before Build ever computes a capsule_id; the vector exists to test the Class 1 verifier (check 9)
// against an already-malformed capsule that arrived by some other path.
func TestProvenanceModeMissingFieldsRejectedProducerSide(t *testing.T) {
	requestBody := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-missing-fields","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
		`"provenance_mode":{"mode":"backfilled"}}`
	r, err := parseRequest([]byte(requestBody))
	require.NoError(t, err)
	_, key := profileFixture(t)
	_, err = seal(r, key)
	require.Error(t, err)
}

// TestChainDuplicatesSealIsCommittedVectorWithSpecVersion05 covers
// pos-chain-duplicates-collapsed-once: a contemporaneous parent published
// first, then a backfilled child chained to it with relation "duplicates",
// citing the parent's real capsule_id. Both are the committed ledger entries
// with spec_version moved to -05 (the child's parent_capsule_id follows).
func TestChainDuplicatesSealIsCommittedVectorWithSpecVersion05(t *testing.T) {
	committed := loadProvenanceVector(t, "pos-chain-duplicates-collapsed-once")
	ledger, ok := committed["ledger"].([]any)
	require.True(t, ok, "store vector must carry a ledger array")
	require.Len(t, ledger, 2)
	committedParent, ok := ledger[0].(map[string]any)
	require.True(t, ok)
	committedChild, ok := ledger[1].(map[string]any)
	require.True(t, ok)
	requireCommittedCapsuleID(t, committedParent, posChainDuplicatesParentID)
	requireCommittedCapsuleID(t, committedChild, posChainDuplicatesChildID)

	_, key := profileFixture(t)

	parentRequest := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-contemporaneous-parent","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `}}`
	pr, err := parseRequest([]byte(parentRequest))
	require.NoError(t, err)
	parent, err := seal(pr, key)
	require.NoError(t, err)
	assert.Equal(t, posChainDuplicatesParentIDV05, parent.CapsuleID)
	requireV05TwinBytes(t, committedParent, nil, parent.Capsule, parent.CapsuleID)

	childRequest := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-backfilled-duplicate","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
		`"chain":{"parent_capsule_id":"` + parent.CapsuleID + `","relation":"duplicates"},` +
		`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`
	cr, err := parseRequest([]byte(childRequest))
	require.NoError(t, err)
	child, err := seal(cr, key)
	require.NoError(t, err)
	assert.Equal(t, posChainDuplicatesChildIDV05, child.CapsuleID)
	committedChain, ok := committedChild["chain"].(map[string]any)
	require.True(t, ok)
	twinChain := make(map[string]any, len(committedChain))
	for k, v := range committedChain {
		twinChain[k] = v
	}
	twinChain["parent_capsule_id"] = parent.CapsuleID
	requireV05TwinBytes(t, committedChild, map[string]any{"chain": twinChain}, child.Capsule, child.CapsuleID)
}

// TestProvenanceModeConflictBetweenRequestAndCapsuleRejected guards the merge
// in parseRequest: a request must not set provenance_mode (or chain) both at
// the top level and inside "capsule".
func TestProvenanceModeConflictBetweenRequestAndCapsuleRejected(t *testing.T) {
	requestBody := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"a","ActionType":"fyi","Operator":"o","Developer":"d","Timestamp":"2026-09-08T00:00:00Z","ProvenanceMode":{"Mode":"contemporaneous"}},` +
		`"provenance_mode":{"mode":"contemporaneous"}}`
	_, err := parseRequest([]byte(requestBody))
	require.Error(t, err)
	assert.ErrorContains(t, err, "provenance_mode must not be set on both")
}

// TestBackfilledSealRequestPublishesAgainstJSONLProfile is the item's
// regression check (4): a real backfilled seal-request -- not the Python
// acceptance script's local simulate_seal stand-in -- published for real
// through a jsonl test profile, its capsule_id the -05 twin of the AAC
// provenance-mode-vectors corpus entry (see
// TestBackfilledSealIsCommittedVectorWithSpecVersion05).
func TestBackfilledSealRequestPublishesAgainstJSONLProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, saveProfile(p, false))

	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)

	requestBody := posBackfilledRequest()
	path := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(path, []byte(requestBody), 0600))

	out, err := invoke(t, "", "publish", "--profile", p.Name, "--request", path)
	require.NoError(t, err)
	assert.Contains(t, out, posBackfilledCapsuleIDV05)
}

// TestProvenanceModeTimingNegativesHaveExactlyTheirCheck9Finding: the pinned
// capsule_ids above prove the committed bytes; this proves what the neutral
// verifier says about them. Each timing-negative vector must produce exactly
// the one check 9 finding its expected.json names -- no other check 9
// finding, and not none.
func TestProvenanceModeTimingNegativesHaveExactlyTheirCheck9Finding(t *testing.T) {
	for name, id := range map[string]string{
		"neg-provenance-mode-time-rung-overclaim": negTimeRungOverclaimID,
		"neg-provenance-mode-time-laundering":     negTimeLaunderingID,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(emitModuleDir(t), provenanceModeVectorDir, name)
			raw, err := os.ReadFile(filepath.Join(dir, "input.json"))
			require.NoError(t, err)
			capsule, err := verify.DecodeCapsuleJSON(raw)
			require.NoError(t, err)
			var expected struct {
				OK       bool `json:"ok"`
				Findings []struct {
					Check int    `json:"check"`
					Code  string `json:"code"`
				} `json:"findings"`
			}
			raw, err = os.ReadFile(filepath.Join(dir, "expected.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &expected))
			require.Len(t, expected.Findings, 1)
			require.Equal(t, 9, expected.Findings[0].Check)

			result := verify.Verify(capsule, nil, nil)
			require.NotNil(t, result.CapsuleID)
			assert.Equal(t, id, *result.CapsuleID)
			assert.Equal(t, expected.OK, result.OK)
			var check9 []string
			for _, f := range result.Findings {
				if f.Check != nil && *f.Check == 9 {
					check9 = append(check9, f.Code)
				}
			}
			assert.Equal(t, []string{expected.Findings[0].Code}, check9)
		})
	}
}
