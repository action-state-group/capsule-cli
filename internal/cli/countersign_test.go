package cli

import (
	"crypto/ed25519"
	"crypto/rand"
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

// countersignerEntry signs a bundle digest with a fresh countersigner key and
// returns the wire entry plus that key's hex public key.
func countersignerEntry(t *testing.T, digest string, checks []CountersignCheck) (CountersignatureEntry, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	entry := CountersignatureEntry{
		Type:      countersignAPI,
		Signer:    CountersignSigner{ID: "countersign.example", KeyID: hex.EncodeToString(public)},
		Over:      digest,
		Statement: CountersignStatement{Checks: checks, RecomputedAt: "2026-09-16T00:00:00Z", Scope: CountersignScope{LedgerID: "test-log", ClosureDepth: json.Number("2")}},
		Signature: signBundleDigest(private, digest),
	}
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
		Signature: signBundleDigest(key, digest),
	}
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

// TestCountersignAnchorEntryInterop is the wire-shape reconciliation's
// acceptance proof: a countersignatures[] entry PRODUCED by the real
// capsule-anchor countersign engine (Python) VERIFIES GREEN here and
// resolves against the countersigner directory -- a genuine cross-language
// round trip, not a same-language self-check. The fixture is COMMITTED
// (testdata/countersign_anchor_interop.json, generated once from the real
// capsule_anchor.countersign package after the reconciliation fixes: sign
// over the bundle digest, not the statement; key_id is the full 64-hex
// Ed25519 public key, not a truncated hash) rather than invoked live at test
// time -- capsule-anchor is a separate, unpublished repo this CLI's CI has
// no toolchain for, so a live invocation would only ever skip, exactly the
// failure mode that let the two implementations diverge unnoticed (see the
// wire-shape reconciliation brief). Regenerate the fixture whenever the
// anchor's countersign wire shape changes.
func TestCountersignAnchorEntryInterop(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "countersign_anchor_interop.json"))
	require.NoError(t, err)
	fixture, err := decodeBundleJSON(raw)
	require.NoError(t, err)

	bundle, ok := fixture["bundle_for_digest"].(map[string]interface{})
	require.True(t, ok)
	bundle["countersignatures"] = []interface{}{fixture["countersignature_entry"]}

	expectedDigest, ok := fixture["bundle_digest"].(string)
	require.True(t, ok)
	gotDigest, err := aacbundle.BundleDigest(bundle)
	require.NoError(t, err, "Go's JCS canonicalization must agree with the anchor's own digest over the same content")
	require.Equal(t, expectedDigest, gotDigest)

	directoryRaw, err := json.Marshal(fixture["directory"])
	require.NoError(t, err)
	dirServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(directoryRaw)
	}))
	defer dirServer.Close()
	client := &http.Client{Transport: dirServer.Client().Transport, Timeout: 5 * time.Second}

	producerPubkeyHex, ok := fixture["producer_pubkey_hex"].(string)
	require.True(t, ok)
	trusted, err := parseKeys([]string{producerPubkeyHex})
	require.NoError(t, err)

	digest, reports, summary, err := verifyCountersignatures(t.Context(), client, dirServer.URL, bundle, trusted)
	require.NoError(t, err, "a genuine anchor-produced countersignature must verify without error")
	assert.Equal(t, expectedDigest, digest)
	require.Len(t, reports, 1)
	assert.Equal(t, "resolved", reports[0].State)
	assert.Equal(t, "Countersign Test Operator", reports[0].SignerName)
	require.NotNil(t, reports[0].Independent)
	assert.True(t, *reports[0].Independent)
	assert.Equal(t, "resolved", summary)
}
