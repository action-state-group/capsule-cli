package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withheldBundleFixture builds a checkpointed, single-record, payloads=none
// Evidence Bundle -- the exact shape countersign request submits.
func withheldBundleFixture(t *testing.T) (map[string]interface{}, Profile, ed25519.PrivateKey) {
	t.Helper()
	profile, key := profileFixture(t)
	store := mapArtifactStore{}
	rec := bundleRecord(t, key, nil, nil)
	store[rec.CapsuleID] = rec
	log := memory.New()
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	value, err := hex.DecodeString(rec.CapsuleID)
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
	require.NoError(t, err)
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	config := checkpoint.DefaultRunnerConfig(profile.LogID)
	config.Cadence.CadenceEntries = 1
	runner, err := checkpoint.NewRunner(config, log, signer)
	require.NoError(t, err)
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)

	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: rec.CapsuleID, ClosureDepth: 2, Payloads: "none"})
	require.NoError(t, err)
	_, hasDisclosures := bundle["disclosures"]
	require.False(t, hasDisclosures, "payloads=none must never carry a disclosures overlay")
	assert.Equal(t, "none", bundle["completeness"].(map[string]interface{})["payloads_mode"])
	return bundle, profile, key
}

// signCountersignEntry sets entry.Signature to key's signature over the
// entry's countersign/v1 signing input.
func signCountersignEntry(t *testing.T, key ed25519.PrivateKey, entry *CountersignatureEntry) {
	t.Helper()
	statement, err := jsonValue(entry.Statement)
	require.NoError(t, err)
	message, err := countersignSigningInput(entry.Over, statement, entry.Type)
	require.NoError(t, err)
	entry.Signature = hex.EncodeToString(ed25519.Sign(key, message))
}

// countersignerEntry signs a bundle digest and statement with a fresh
// countersigner key and returns the wire entry plus that key's hex public key.
func countersignerEntry(t *testing.T, digest string, checks []CountersignCheck) (CountersignatureEntry, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := CountersignatureEntry{
		Type:      countersignAPI,
		Signer:    CountersignSigner{ID: "countersign.example", KeyID: hex.EncodeToString(public)},
		Over:      digest,
		Statement: CountersignStatement{Checks: checks, RecomputedAt: "2026-09-16T00:00:00Z", Scope: CountersignScope{LedgerID: "test-log", ClosureDepth: json.Number("2")}},
	}
	signCountersignEntry(t, private, &entry)
	return entry, hex.EncodeToString(public)
}

func TestPayloadsNoneRejectsDisclosure(t *testing.T) {
	profile, key := profileFixture(t)
	store := mapArtifactStore{}
	rec := bundleRecord(t, key, nil, nil)
	store[rec.CapsuleID] = rec
	log := memory.New()
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	value, err := hex.DecodeString(rec.CapsuleID)
	require.NoError(t, err)
	_, err = log.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
	require.NoError(t, err)
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	config := checkpoint.DefaultRunnerConfig(profile.LogID)
	config.Cadence.CadenceEntries = 1
	runner, err := checkpoint.NewRunner(config, log, signer)
	require.NoError(t, err)
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)

	_, err = AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: rec.CapsuleID, ClosureDepth: 2, Payloads: "none", WithDisclosure: true})
	require.ErrorContains(t, err, "cannot be combined")
}

// TestCountersignRequestAgainstMockService drives the real request path: sign
// the withheld bundle's digest with the requester's ledger key, submit to a
// mock countersign service over HTTPS, and attach the returned entry.
func TestCountersignRequestAgainstMockService(t *testing.T) {
	bundle, profile, key := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)

	var observedRequesterKeyID string
	entry, counterKeyHex := countersignerEntry(t, digest, []CountersignCheck{{Name: "chain_consistency", Result: "pass"}, {Name: "range_membership", Result: "pass"}})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var submission countersignSubmission
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&submission))
		observedRequesterKeyID = submission.Requester.KeyID
		assert.Equal(t, "30d", submission.Window)
		assert.Equal(t, digest, mustDigest(t, submission.Bundle))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(countersignSubmissionResponse{Countersignatures: []CountersignatureEntry{entry}}))
	}))
	defer server.Close()
	client := &http.Client{Transport: server.Client().Transport, Timeout: 5 * time.Second}

	submission, gotDigest, err := buildCountersignSubmission(bundle, "30d", profile.LogID, key)
	require.NoError(t, err)
	require.Equal(t, digest, gotDigest)
	entries, err := requestCountersignatures(t.Context(), client, server.URL, submission, gotDigest)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	requesterPublic, ok := key.Public().(ed25519.PublicKey)
	require.True(t, ok)
	assert.Equal(t, hex.EncodeToString(requesterPublic), observedRequesterKeyID)

	require.NoError(t, attachCountersignatures(bundle, entries))
	countersignatures, ok := bundle["countersignatures"].([]interface{})
	require.True(t, ok)
	require.Len(t, countersignatures, 1)

	// The attached entry must itself still verify offline, independent of the
	// service that produced it.
	directory := countersignerDirectory{Countersigners: []countersignerDirectoryRow{{Name: "Countersign Test Operator", KeyIDs: []string{counterKeyHex}}}}
	dirServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(directory))
	}))
	defer dirServer.Close()
	dirClient := &http.Client{Transport: dirServer.Client().Transport, Timeout: 5 * time.Second}
	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	gotDigest2, reports, summary, err := verifyCountersignatures(t.Context(), dirClient, dirServer.URL, bundle, trusted)
	require.NoError(t, err)
	assert.Equal(t, digest, gotDigest2)
	require.Len(t, reports, 1)
	assert.Equal(t, "resolved", reports[0].State)
	assert.Equal(t, "Countersign Test Operator", reports[0].SignerName)
	assert.Equal(t, "resolved", summary)
	assert.True(t, *reports[0].Independent)
	assert.Equal(t, receiptAbsent, reports[0].Receipt, "an entry without a receipt is still valid; its receipt is reported absent")
}

// TestCountersignVerifyTamperedEntryFails is the mutant-catching negative:
// flipping a byte of the signature must flip verification to failure, not
// silently pass.
func TestCountersignVerifyTamperedEntryFails(t *testing.T) {
	bundle, profile, _ := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	entry, _ := countersignerEntry(t, digest, []CountersignCheck{{Name: "chain_consistency", Result: "pass"}})

	sigBytes, err := hex.DecodeString(entry.Signature)
	require.NoError(t, err)
	sigBytes[0] ^= 1
	entry.Signature = hex.EncodeToString(sigBytes)
	require.NoError(t, attachCountersignatures(bundle, []CountersignatureEntry{entry}))

	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	_, _, _, err = verifyCountersignatures(t.Context(), client, "https://directory.invalid/witnesses.json", bundle, trusted)
	require.Error(t, err, "a tampered countersignature must fail verification, not pass silently")
	assert.ErrorContains(t, err, "does not verify")
}

// TestCountersignVerifyTamperedOverFails covers the other tamper vector: an
// entry's "over" no longer names the bundle it is attached to.
func TestCountersignVerifyTamperedOverFails(t *testing.T) {
	bundle, profile, _ := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	entry, _ := countersignerEntry(t, digest, nil)
	entry.Over = "0000000000000000000000000000000000000000000000000000000000000000"
	require.NoError(t, attachCountersignatures(bundle, []CountersignatureEntry{entry}))

	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	_, _, _, err = verifyCountersignatures(t.Context(), client, "https://directory.invalid/witnesses.json", bundle, trusted)
	require.Error(t, err)
	assert.ErrorContains(t, err, "different bundle digest")
}

// TestCountersignVerifyUnresolvedSigner is the directory-miss case: a
// well-formed, correctly-signed entry whose signer is simply absent from the
// directory must be reported (not silently dropped) and must not be
// confused with a resolved, independent countersignature.
func TestCountersignVerifyUnresolvedSigner(t *testing.T) {
	bundle, profile, _ := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	entry, _ := countersignerEntry(t, digest, []CountersignCheck{{Name: "key_hygiene", Result: "pass"}})
	require.NoError(t, attachCountersignatures(bundle, []CountersignatureEntry{entry}))

	emptyDirectory := countersignerDirectory{}
	dirServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(emptyDirectory))
	}))
	defer dirServer.Close()
	client := &http.Client{Transport: dirServer.Client().Transport, Timeout: 5 * time.Second}

	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	_, reports, summary, err := verifyCountersignatures(t.Context(), client, dirServer.URL, bundle, trusted)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "unresolved_signer", reports[0].State)
	assert.Equal(t, "unresolved_signer", summary)
	assert.Empty(t, reports[0].SignerName, "an unresolved signer must never surface a directory name")
}

// TestCountersignVerifySelfSignatureNotIndependent covers the producer's-own-
// key case: well-formed and correctly signed, but must render as not
// independent, never as a resolved third-party countersignature.
func TestCountersignVerifySelfSignatureNotIndependent(t *testing.T) {
	bundle, profile, key := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	public, ok := key.Public().(ed25519.PublicKey)
	require.True(t, ok)
	entry := CountersignatureEntry{
		Type:      countersignAPI,
		Signer:    CountersignSigner{ID: profile.LogID, KeyID: hex.EncodeToString(public)},
		Over:      digest,
		Statement: CountersignStatement{RecomputedAt: "2026-09-16T00:00:00Z", Scope: CountersignScope{LedgerID: profile.LogID}},
	}
	signCountersignEntry(t, key, &entry)
	require.NoError(t, attachCountersignatures(bundle, []CountersignatureEntry{entry}))

	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	_, reports, summary, err := verifyCountersignatures(t.Context(), client, "https://directory.invalid/witnesses.json", bundle, trusted)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "not_independent", reports[0].State)
	assert.False(t, *reports[0].Independent)
	assert.Equal(t, "not_independent", summary)
}

func TestCountersignVerifyNoEntriesIsNone(t *testing.T) {
	bundle, profile, _ := withheldBundleFixture(t)
	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	_, reports, summary, err := verifyCountersignatures(t.Context(), client, "https://directory.invalid/witnesses.json", bundle, trusted)
	require.NoError(t, err)
	assert.Empty(t, reports)
	assert.Equal(t, "none", summary)
}

// TestCountersignVerifyUnknownTypeIsUnverifiedNotRejected exercises the -00
// spec's own reserved slot: an entry of a type this CLI does not define (the
// initial registered "cose-sign1") must be reported unverified, never fail
// the bundle and never be silently dropped.
func TestCountersignVerifyUnknownTypeIsUnverifiedNotRejected(t *testing.T) {
	bundle, profile, _ := withheldBundleFixture(t)
	raw := map[string]interface{}{"type": "cose-sign1", "signature": "not-a-real-cose-sign1"}
	bundle["countersignatures"] = []interface{}{raw}

	trusted, err := parseKeys(profile.TrustedKeys)
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second}
	_, reports, summary, err := verifyCountersignatures(t.Context(), client, "https://directory.invalid/witnesses.json", bundle, trusted)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "unverified", reports[0].State)
	assert.Equal(t, "unverified", summary)
}

// TestDefaultCountersignerDirectoryURLPointsAtCapsuleEmit pins the
// --directory fallback to capsule-emit's witnesses.json: capsule-emit is the
// directory's home, not checkpointed-local-log.
// A silent revert back to checkpointed-local-log must fail this test.
func TestDefaultCountersignerDirectoryURLPointsAtCapsuleEmit(t *testing.T) {
	resolved, err := validateDirectoryURL("")
	require.NoError(t, err)
	assert.Equal(t, "https://raw.githubusercontent.com/action-state-group/capsule-emit/main/witnesses.json", resolved)
}

func mustDigest(t *testing.T, bundle map[string]interface{}) string {
	t.Helper()
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	return digest
}

// TestCountersignBundleAgreesWithPythonVerifier is the cross-implementation
// agreement check §7b requires for anything computing a digest or
// canonicalization another one of our implementations also computes: the
// same withheld bundle, verified independently by the Python bundle_verifier,
// must reach the same digest and the same pass/withheld verdicts as the Go
// side already asserts inside AssembleBundle. It skips (loudly) rather than
// failing when python3 or the agent_action_capsule package is unavailable,
// e.g. on a CI runner that has not been given a Python toolchain -- flagged
// in cli-verbs-results.md, not concealed by a silent pass.
func TestCountersignBundleAgreesWithPythonVerifier(t *testing.T) {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; skipping the Python cross-check (see cli-verbs-results.md)")
	}
	if err := exec.Command(pythonPath, "-c", "import agent_action_capsule.bundle").Run(); err != nil {
		t.Skip("agent_action_capsule not importable; skipping the Python cross-check (see cli-verbs-results.md)")
	}

	bundle, _, _ := withheldBundleFixture(t)
	goResult := aacbundle.VerifyBundle(bundle)
	goDigest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)

	encoded, err := json.Marshal(bundle)
	require.NoError(t, err)
	bundleFile := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(bundleFile, encoded, 0o600))

	const script = `
import json, sys
from agent_action_capsule import bundle as b
with open(sys.argv[1]) as f:
    value = json.load(f)
result = b.verify_bundle(value)
print(json.dumps({
    "bundle_digest": b.bundle_digest(value),
    "graph_closure": result.graph_closure.status,
    "interval_coverage": result.interval_coverage.status,
    "per_record_membership": result.per_record_membership.status,
}))
`
	out, err := exec.Command(pythonPath, "-c", script, bundleFile).Output()
	require.NoError(t, err, "python bundle verifier invocation failed")
	var pyResult struct {
		BundleDigest        string `json:"bundle_digest"`
		GraphClosure        string `json:"graph_closure"`
		IntervalCoverage    string `json:"interval_coverage"`
		PerRecordMembership string `json:"per_record_membership"`
	}
	require.NoError(t, json.Unmarshal(out, &pyResult))

	assert.Equal(t, goDigest, pyResult.BundleDigest, "Go and Python must compute the identical bundle digest")
	assert.Equal(t, goResult.GraphClosure.Status, pyResult.GraphClosure)
	assert.Equal(t, goResult.IntervalCoverage.Status, pyResult.IntervalCoverage)
	assert.Equal(t, goResult.PerRecordMembership.Status, pyResult.PerRecordMembership)
	assert.Equal(t, "pass", pyResult.GraphClosure)
	assert.Equal(t, "pass", pyResult.IntervalCoverage)
	assert.Equal(t, "pass", pyResult.PerRecordMembership)
}

// countersignVectorSHA256 pins testdata/countersign-v1.json to the golden
// vector capsule-anchor generates and commits
// (packages/tests/countersign/vectors/countersign-v1.json). The two copies
// must stay byte-identical: regenerate there, copy here, update this pin.
const countersignVectorSHA256 = "3b278a3395ad47326f32987848de7b92eac08c6acbdfbebd71a237f94c197280"

type countersignVector struct {
	SignerPublicKeyHex   string                 `json:"signer_public_key_hex"`
	ProducerPublicKeyHex string                 `json:"producer_public_key_hex"`
	Directory            json.RawMessage        `json:"directory"`
	Bundle               map[string]interface{} `json:"bundle"`
	BundleDigest         string                 `json:"bundle_digest"`
	SigningInput         string                 `json:"signing_input"`
	Entry                interface{}            `json:"entry"`
	Negative             []struct {
		Name   string            `json:"name"`
		Entry  interface{}       `json:"entry"`
		Expect map[string]string `json:"expect"`
	} `json:"negative"`
}

func loadCountersignVector(t *testing.T) countersignVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "countersign-v1.json"))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, countersignVectorSHA256, hex.EncodeToString(sum[:]), "testdata/countersign-v1.json drifted from the pinned golden vector")
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var vector countersignVector
	require.NoError(t, decoder.Decode(&vector))
	return vector
}

// verifyVectorEntry attaches entry to a copy of the vector's bundle and runs
// the real countersign verify core against the vector's directory.
func verifyVectorEntry(t *testing.T, vector countersignVector, entry interface{}) (string, []countersignatureReport, string, error) {
	t.Helper()
	bundle, err := jsonValue(vector.Bundle)
	require.NoError(t, err)
	bundleMap, ok := bundle.(map[string]interface{})
	require.True(t, ok)
	bundleMap["countersignatures"] = []interface{}{entry}
	dirServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(vector.Directory)
	}))
	t.Cleanup(dirServer.Close)
	client := &http.Client{Transport: dirServer.Client().Transport, Timeout: 5 * time.Second}
	trusted, err := parseKeys([]string{vector.ProducerPublicKeyHex})
	require.NoError(t, err)
	return verifyCountersignatures(t.Context(), client, dirServer.URL, bundleMap, trusted)
}

// TestCountersignGoldenVectorSigningInput checks this CLI builds exactly the
// vector's signing input, UTF8(JCS({over, statement, type})), from the entry.
func TestCountersignGoldenVectorSigningInput(t *testing.T) {
	vector := loadCountersignVector(t)
	entry, ok := vector.Entry.(map[string]interface{})
	require.True(t, ok)
	over, _ := entry["over"].(string)
	entryType, _ := entry["type"].(string)
	message, err := countersignSigningInput(over, entry["statement"], entryType)
	require.NoError(t, err)
	assert.Equal(t, vector.SigningInput, string(message))
}

// TestCountersignGoldenVectorVerifies is the cross-language acceptance proof:
// an entry produced by capsule-anchor's real signer and log verifies here,
// signature and receipt, and resolves against the directory.
func TestCountersignGoldenVectorVerifies(t *testing.T) {
	vector := loadCountersignVector(t)
	digest, reports, summary, err := verifyVectorEntry(t, vector, vector.Entry)
	require.NoError(t, err, "the anchor-produced golden entry must verify")
	assert.Equal(t, vector.BundleDigest, digest)
	require.Len(t, reports, 1)
	assert.Equal(t, "resolved", reports[0].State)
	assert.Equal(t, "Countersign Test Operator", reports[0].SignerName)
	assert.Equal(t, receiptVerified, reports[0].Receipt, reports[0].ReceiptDetail)
	require.NotNil(t, reports[0].Independent)
	assert.True(t, *reports[0].Independent)
	assert.Equal(t, "resolved", summary)
	require.Len(t, reports[0].Checks, 5)
	assert.Equal(t, CountersignCheck{Name: "range membership", Result: "failed"}, reports[0].Checks[1])
}

// TestCountersignGoldenVectorNegatives runs every negative case in the
// vector. flipped-result is the one this fix exists for: the genuine
// signature with one result rewritten "failed" -> "established" must fail.
func TestCountersignGoldenVectorNegatives(t *testing.T) {
	vector := loadCountersignVector(t)
	names := map[string]bool{}
	for _, tc := range vector.Negative {
		names[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			_, reports, _, err := verifyVectorEntry(t, vector, tc.Entry)
			if tc.Expect["signature"] == "invalid" {
				require.Error(t, err, "a countersignature whose signing input was altered must fail verification")
				assert.ErrorContains(t, err, "does not verify")
				assert.Nil(t, reports, "checks of an invalid entry must never be displayed")
				return
			}
			require.NoError(t, err)
			require.Len(t, reports, 1)
			assert.Equal(t, tc.Expect["receipt"], reports[0].Receipt)
			assert.NotEmpty(t, reports[0].ReceiptDetail)
			if tc.Name == "receipt-for-other-statement-no-entry-hash" {
				// With no entry_hash to compare, the receipt must fail on
				// the inclusion proof or the receipt signature itself.
				assert.NotContains(t, reports[0].ReceiptDetail, "entry_hash")
			}
			assert.Equal(t, "resolved", reports[0].State, "an unverified receipt does not invalidate the entry")
		})
	}
	for _, name := range []string{"flipped-result", "digest-only-signature", "receipt-for-other-statement", "receipt-for-other-statement-no-entry-hash"} {
		assert.True(t, names[name], "golden vector is missing negative case %q", name)
	}
}

// TestCountersignRequestRefusesFlippedResult covers the request path: a
// service response whose statement was altered after signing is refused
// before it is ever attached to the bundle file.
func TestCountersignRequestRefusesFlippedResult(t *testing.T) {
	bundle, profile, key := withheldBundleFixture(t)
	digest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err)
	entry, _ := countersignerEntry(t, digest, []CountersignCheck{{Name: "range membership", Result: "failed"}})
	entry.Statement.Checks[0].Result = "established"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(countersignSubmissionResponse{Countersignatures: []CountersignatureEntry{entry}}))
	}))
	defer server.Close()
	client := &http.Client{Transport: server.Client().Transport, Timeout: 5 * time.Second}
	submission, gotDigest, err := buildCountersignSubmission(bundle, "30d", profile.LogID, key)
	require.NoError(t, err)
	_, err = requestCountersignatures(t.Context(), client, server.URL, submission, gotDigest)
	require.Error(t, err)
	assert.ErrorContains(t, err, "does not verify")
}
