package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mapArtifactStore map[string]artifact.Record

func (s mapArtifactStore) Get(_ context.Context, id string) (artifact.Record, error) {
	record, ok := s[id]
	if !ok {
		return artifact.Record{}, artifact.ErrNotFound
	}
	return record, nil
}

func TestAssembleDiscloseAndPermalink(t *testing.T) {
	profile, key := profileFixture(t)
	store := mapArtifactStore{}
	first := bundleRecord(t, key, nil, nil)
	// second chains to first; a reference back to first would duplicate the chain
	// parent, which §5.5.5 forbids. root exercises the non-chain citation edge.
	second := bundleRecord(t, key, &emit.Chain{ParentCapsuleID: first.CapsuleID, Relation: emit.ChainConfirms}, nil)
	root := bundleRecord(t, key, &emit.Chain{ParentCapsuleID: second.CapsuleID, Relation: emit.ChainConfirms}, []emit.Reference{{Type: "agent-action-capsule", DigestAlg: "SHA-256", Digest: first.CapsuleID}})
	for _, record := range []artifact.Record{first, second, root} {
		store[record.CapsuleID] = record
	}
	log := memory.New()
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	for _, record := range []artifact.Record{first, second, root} {
		value, err := hex.DecodeString(record.CapsuleID)
		require.NoError(t, err)
		_, err = log.Append(t.Context(), cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
		require.NoError(t, err)
	}
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	config := checkpoint.DefaultRunnerConfig(profile.LogID)
	config.Cadence.CadenceEntries = 1
	runner, err := checkpoint.NewRunner(config, log, signer)
	require.NoError(t, err)
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)

	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root.CapsuleID, ClosureDepth: 2, Payloads: "all"})
	require.NoError(t, err)
	verified := aacbundle.VerifyBundle(bundle)
	require.Equal(t, "pass", verified.GraphClosure.Status)
	require.Equal(t, "pass", verified.IntervalCoverage.Status)
	require.Equal(t, "pass", verified.PerRecordMembership.Status)

	disclosed, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root.CapsuleID, ClosureDepth: 2, Payloads: "all", WithDisclosure: true, Suppress: map[string]bool{"agent_input": true}})
	require.NoError(t, err)
	verified = aacbundle.VerifyBundle(disclosed)
	matched, withheld := false, false
	for _, result := range verified.Disclosures {
		matched = matched || result.Member == "agent_output" && result.Status == disclosure.Match
		withheld = withheld || result.Member == "agent_input" && result.Status == "withheld"
	}
	require.True(t, matched, "stored nested agent output must pass DE-3")
	require.True(t, withheld, "suppressed input must remain withheld")

	fragment, err := aacbundle.EncodeFragment(disclosed)
	require.NoError(t, err)
	roundTripped, err := aacbundle.DecodeFragment(fragment)
	require.NoError(t, err)
	require.IsType(t, map[string]interface{}{}, roundTripped)
}

func TestPayloadsAllRequiresEveryOriginalAtDepthZero(t *testing.T) {
	profile, key := profileFixture(t)
	store := mapArtifactStore{}
	rec := bundleRecord(t, key, nil, nil)
	// Keep the committed agent_output_digest but drop the retained original, so a
	// payloads=all claim cannot be honored.
	kept := make([]artifact.Artifact, 0, len(rec.Artifacts))
	for _, a := range rec.Artifacts {
		if a.Binding != artifact.AgentOutputDigest {
			kept = append(kept, a)
		}
	}
	rec.Artifacts = kept
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

	// ClosureDepth 0 is honored (root-only); payloads=all must refuse the missing original.
	_, err = AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: rec.CapsuleID, ClosureDepth: 0, Payloads: "all", WithDisclosure: true})
	require.ErrorContains(t, err, "not retained")
}

// TestAppendDisclosureRecordSealsDiscloseActOnLog is the mutant-catching test
// for the "every act is on record" requirement: it recomputes the expected
// disclosure_record digest independently of production code and asserts the
// CLL actually grew by exactly one entry carrying that exact value. A mutant
// that drops the log.Append (or appends the wrong bytes) turns this red.
func TestAppendDisclosureRecordSealsDiscloseActOnLog(t *testing.T) {
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

	before, err := log.ScanEntries(t.Context(), 0, cll.MaxScanLimit)
	require.NoError(t, err)
	require.Len(t, before, 1, "only the capsule is on the log before disclose")

	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: rec.CapsuleID, ClosureDepth: 2, Payloads: "all", WithDisclosure: true})
	require.NoError(t, err)

	entry, err := appendDisclosureRecord(t.Context(), log, bundle)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), entry.Seq, "the disclosure_record must be the NEXT log entry, not a substitute for it")

	after, err := log.ScanEntries(t.Context(), 0, cll.MaxScanLimit)
	require.NoError(t, err)
	require.Len(t, after, 2, "disclose must append to the log, not merely emit the bundle")

	// Recompute the expected digest independently of disclosureRecord()'s
	// implementation, straight from the disclosed overlay and completeness block.
	overlay := bundle["disclosures"].(map[string]interface{})
	members := overlay[rec.CapsuleID].(map[string]interface{})
	inputDigest, err := canonical.JSONDigest(members["agent_input"])
	require.NoError(t, err)
	outputDigest, err := canonical.JSONDigest(members["agent_output"])
	require.NoError(t, err)
	expected := map[string]interface{}{
		"type":              "disclosure_record",
		"root":              rec.CapsuleID,
		"payloads_mode":     "all",
		"suppressed_fields": []interface{}{},
		"revealed": map[string]interface{}{
			rec.CapsuleID: map[string]interface{}{"agent_input": inputDigest, "agent_output": outputDigest},
		},
	}
	expectedDigest, err := canonical.JSONDigest(expected)
	require.NoError(t, err)
	assert.Equal(t, expectedDigest, hex.EncodeToString(after[1].Value), "the appended entry must commit to exactly what was disclosed")
}

// TestAppendDisclosureRecordRejectsUnrecordedSuppression is the negative on the
// trusted path: a disclose that suppresses a member must not silently drop it
// from the sealed record -- the commitment must name every suppressed field.
func TestAppendDisclosureRecordRejectsUnrecordedSuppression(t *testing.T) {
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

	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: rec.CapsuleID, ClosureDepth: 2, Payloads: "selected", Suppress: map[string]bool{"agent_input": true}, WithDisclosure: true})
	require.NoError(t, err)

	entry, err := appendDisclosureRecord(t.Context(), log, bundle)
	require.NoError(t, err)

	record, err := disclosureRecord(bundle)
	require.NoError(t, err)
	assert.Equal(t, []interface{}{"agent_input"}, record["suppressed_fields"])
	revealed := record["revealed"].(map[string]interface{})[rec.CapsuleID].(map[string]interface{})
	_, stillRevealed := revealed["agent_input"]
	assert.False(t, stillRevealed, "a suppressed member must not appear as revealed in the sealed record")
	digest, err := canonical.JSONDigest(record)
	require.NoError(t, err)
	assert.Equal(t, digest, hex.EncodeToString(entry.Value))
}

func bundleRecord(t *testing.T, key ed25519.PrivateKey, chain *emit.Chain, references []emit.Reference) artifact.Record {
	t.Helper()
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Capsule.Chain = chain
	request.Capsule.References = references
	record, err := seal(request, key)
	require.NoError(t, err)
	return record
}
