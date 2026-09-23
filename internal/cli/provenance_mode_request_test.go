package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Vector capsule_ids and field values are copied by hand from
// capsule-emit-go's testdata/provenance-mode-vectors/ (itself a byte-for-byte
// copy of the AAC -05 provenance-mode-vectors corpus, agent-action-capsule,
// branch aac-external-review-followups, commit 8def04c5, SHA256SUMS
// confirmed unchanged there). This test proves the same 5 cases byte-match
// when built from a capsule-seal-request/v1 file through this package's
// parseRequest/seal, not just through emit.Build directly.
const (
	posBackfilledCapsuleID     = "52bd075279c3528c2e08962a6e5948ad82e68fd079d8198e7a2a3c27d0883bd4"
	negTimeRungOverclaimID     = "ec8bb8fed1f318cb20e606df4c5d700e5d5a766eb8b49601e1a4cd01277b1376"
	negTimeLaunderingID        = "9dbb97d6c1f7caf9b57a3a672ad82a294509376960920ed5c705adf3f99b9027"
	posChainDuplicatesParentID = "ed3cdeca453ee37e40bbeeee0f582f579873fa690a6af2d729578595b287d378"
	posChainDuplicatesChildID  = "5d7ee9fe6af6391a0a905b3297316bc483d781098d9ceca4fa697fa591f929dd"
	sourceRefDigestAllThrees   = "3333333333333333333333333333333333333333333333333333333333333333"
)

func provenanceVectorDisposition() string {
	return `"Disposition":{"Decision":"accept","Approver":"policy","HumanDisposed":false,"VerdictClass":"executed"}`
}

// TestProvenanceModeVectorsCapsuleIDByteParity covers 3 of the 5 vector
// directories that are producer-buildable standalone (the two that involve a
// parent/child chain are covered by TestChainDuplicatesVectorByteParity
// below).
func TestProvenanceModeVectorsCapsuleIDByteParity(t *testing.T) {
	cases := []struct {
		name        string
		capsuleID   string
		requestBody string
	}{
		{
			name:      "pos-provenance-mode-backfilled",
			capsuleID: posBackfilledCapsuleID,
			requestBody: `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-backfilled","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
				`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`,
		},
		{
			// -05 §5.3(bis) time_rung="witnessed" with no corroborating
			// reference: our producer-side validateProvenanceMode has no
			// opinion on this (it is a Class 1 check 9 finding, out of this
			// package's scope per [capsulectl-publish-accept-provenance-mode]
			// item 3) -- the request still seals, byte-identical to the
			// vector's capsule_id.
			name:      "neg-provenance-mode-time-rung-overclaim",
			capsuleID: negTimeRungOverclaimID,
			requestBody: `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-witnessed-overclaim","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
				`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z","time_rung":"witnessed"}}`,
		},
		{
			// imported_at == source_asserted_at (the laundering shape): also
			// a check 9 finding, not a producer-side structural rejection --
			// still seals, byte-identical to the vector's capsule_id.
			name:      "neg-provenance-mode-time-laundering",
			capsuleID: negTimeLaunderingID,
			requestBody: `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-laundering","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
				`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-09-22T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseRequest([]byte(tc.requestBody))
			require.NoError(t, err)
			_, key := profileFixture(t)
			record, err := seal(r, key)
			require.NoError(t, err)
			assert.Equal(t, tc.capsuleID, record.CapsuleID, "vector fixture drifted from its pinned capsule_id")
		})
	}
}

// TestProvenanceModeMissingFieldsRejectedProducerSide covers the 5th vector,
// neg-provenance-mode-backfilled-missing-fields: mode="backfilled" with none
// of the four required companion fields. Unlike the other two negative
// vectors above, this shape can never come out of our own producer -- it is
// rejected before Build ever computes a capsule_id, so there is no id to
// byte-match; the vector exists to test the Class 1 verifier (check 9)
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

// TestChainDuplicatesVectorByteParity covers pos-chain-duplicates-collapsed-once:
// a contemporaneous parent published first, then a backfilled child chained
// to it with relation "duplicates", citing the parent's real capsule_id.
func TestChainDuplicatesVectorByteParity(t *testing.T) {
	_, key := profileFixture(t)

	parentRequest := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-contemporaneous-parent","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `}}`
	pr, err := parseRequest([]byte(parentRequest))
	require.NoError(t, err)
	parent, err := seal(pr, key)
	require.NoError(t, err)
	assert.Equal(t, posChainDuplicatesParentID, parent.CapsuleID)

	childRequest := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-backfilled-duplicate","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
		`"chain":{"parent_capsule_id":"` + parent.CapsuleID + `","relation":"duplicates"},` +
		`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`
	cr, err := parseRequest([]byte(childRequest))
	require.NoError(t, err)
	child, err := seal(cr, key)
	require.NoError(t, err)
	assert.Equal(t, posChainDuplicatesChildID, child.CapsuleID)
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
// through a jsonl test profile, its capsule_id matching the AAC Python
// provenance-mode-vectors corpus.
func TestBackfilledSealRequestPublishesAgainstJSONLProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, saveProfile(p, false))

	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)

	requestBody := `{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"prov-backfilled","ActionType":"decide","Operator":"ACME-CO","Developer":"agent@v1","Timestamp":"2026-08-24T00:00:00Z",` + provenanceVectorDisposition() + `},` +
		`"provenance_mode":{"mode":"backfilled","source_ref":{"type":"x-external-ledger-entry","digest_alg":"SHA-256","digest":"` + sourceRefDigestAllThrees + `"},"source_asserted_at":"2026-01-01T00:00:00Z","import_batch":"import-2026-09","imported_at":"2026-09-22T00:00:00Z"}}`
	path := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(path, []byte(requestBody), 0600))

	out, err := invoke(t, "", "publish", "--profile", p.Name, "--request", path)
	require.NoError(t, err)
	assert.Contains(t, out, posBackfilledCapsuleID)
}
