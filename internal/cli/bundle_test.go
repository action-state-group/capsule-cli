package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/store/memory"
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

	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root.CapsuleID, Payloads: "all"})
	require.NoError(t, err)
	verified := aacbundle.VerifyBundle(bundle)
	require.Equal(t, "pass", verified.GraphClosure.Status)
	require.Equal(t, "pass", verified.IntervalCoverage.Status)
	require.Equal(t, "pass", verified.PerRecordMembership.Status)

	disclosed, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root.CapsuleID, Payloads: "all", WithDisclosure: true, Suppress: map[string]bool{"agent_input": true}})
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
