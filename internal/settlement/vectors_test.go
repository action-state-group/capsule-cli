package settlement

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/vectors is vectors/settlement/ of agent-action-capsule at
// 565d1f0eaab1659b537426b434cc9374a39570f2 (draft-mih-agent-settlement-records-00),
// byte for byte. Every expectation in it is written from the draft text, not
// from any implementation.
const vectorsDir = "testdata/vectors"

type vectorCase struct {
	ID        string            `json:"id"`
	KeyPolicy map[string]string `json:"key_policy"`
	Records   []struct {
		Label       string         `json:"label"`
		Capsule     map[string]any `json:"capsule"`
		CapsuleID   *string        `json:"capsule_id"`
		EnvelopeHex string         `json:"envelope_hex"`
	} `json:"records"`
	WrappedObjects []struct {
		Type       string `json:"type"`
		Digest     string `json:"digest"`
		OctetsB64u string `json:"octets_b64u"`
	} `json:"wrapped_objects"`
	Expect json.RawMessage `json:"expect"`
}

type vectorFile struct {
	Keys map[string]struct {
		SeedHex      string `json:"seed_hex"`
		PublicKeyHex string `json:"public_key_hex"`
	} `json:"keys"`
	Count int          `json:"count"`
	Cases []vectorCase `json:"cases"`
}

func decodeNumbers(t *testing.T, raw []byte, into any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(into))
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vectorsDir, "cases.json"))
	require.NoError(t, err)
	var file vectorFile
	decodeNumbers(t, raw, &file)
	require.Len(t, file.Cases, file.Count)
	return file
}

// input turns a vector case into what a verifier is given: the legs, the
// wrapped octets held, and the case's key policy.
func (c vectorCase) input(t *testing.T) Input {
	t.Helper()
	in := Input{Objects: map[string][]byte{}, Policy: map[string][]string{}}
	for role, key := range c.KeyPolicy {
		in.Policy[role] = []string{key}
	}
	for _, r := range c.Records {
		l := Leg{Label: r.Label, Capsule: r.Capsule}
		if r.EnvelopeHex != "" {
			envelope, err := hex.DecodeString(r.EnvelopeHex)
			require.NoError(t, err)
			l.Envelope = envelope
		}
		in.Legs = append(in.Legs, l)
	}
	for _, w := range c.WrappedObjects {
		octets, err := base64.RawURLEncoding.DecodeString(w.OctetsB64u)
		require.NoError(t, err)
		in.Objects[w.Digest] = octets
	}
	return in
}

// comparable keeps what the vectors state: delivery_evidence, the legs' keys
// and the key policy flag are this implementation's additions.
func comparable(t *testing.T, result Result) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(encoded, &out))
	delete(out, "legs")
	delete(out, "key_policy")
	for _, s := range out["settlements"].([]any) {
		delete(s.(map[string]any), "delivery_evidence")
	}
	return out
}

func TestEverySettlementVectorDerivesItsStatedExpectation(t *testing.T) {
	for _, c := range loadVectors(t).Cases {
		t.Run(c.ID, func(t *testing.T) {
			var expect map[string]any
			require.NoError(t, json.Unmarshal(c.Expect, &expect))
			assert.Equal(t, expect, comparable(t, Verify(c.input(t))))
		})
	}
}

func TestPinnedVectorsMatchTheirChecksums(t *testing.T) {
	sums, err := os.Open(filepath.Join(vectorsDir, "SHA256SUMS"))
	require.NoError(t, err)
	defer sums.Close()
	listed := 0
	scanner := bufio.NewScanner(sums)
	for scanner.Scan() {
		digest, name, found := strings.Cut(scanner.Text(), "  ")
		require.True(t, found)
		data, err := os.ReadFile(filepath.Join(vectorsDir, name))
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		assert.Equal(t, digest, hex.EncodeToString(sum[:]), name)
		listed++
	}
	entries, err := os.ReadDir(vectorsDir)
	require.NoError(t, err)
	assert.Equal(t, len(entries)-1, listed, "SHA256SUMS lists every pinned file but itself")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRegistryMatchesTheVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(vectorsDir, "registry.json"))
	require.NoError(t, err)
	var registry struct {
		Legs               []string          `json:"legs"`
		SealerRoles        []string          `json:"sealer_roles"`
		Statuses           []string          `json:"statuses"`
		DeliveryDirections map[string]string `json:"delivery_directions"`
		PaymentStates      []string          `json:"payment_states"`
		DeliveryStates     []string          `json:"delivery_states"`
		FailureCodes       []string          `json:"failure_codes"`
		InformationalCodes []string          `json:"informational_codes"`
		BaseProfileCodes   []string          `json:"base_profile_codes"`
		PaymentRefTypes    map[string]struct {
			Qualifiers []string `json:"qualifiers"`
			ReceiveFee string   `json:"receive_fee"`
		} `json:"payment_ref_types"`
		WrappedTypes     map[string]string `json:"wrapped_types"`
		SettlementMember map[string]struct {
			Required []string `json:"required"`
			Optional []string `json:"optional"`
		} `json:"settlement_members"`
	}
	require.NoError(t, json.Unmarshal(raw, &registry))

	sorted := func(values []string) []string { out := slices.Clone(values); sort.Strings(out); return out }
	assert.Equal(t, sorted(registry.Legs), keys(legs))
	assert.Equal(t, sorted(registry.SealerRoles), keys(sealerRoles))
	assert.Equal(t, sorted(registry.Statuses), keys(statuses))
	assert.Equal(t, registry.DeliveryDirections, deliveryDirections)
	assert.Equal(t, registry.PaymentStates, []string{PaymentTermsOnly, PaymentPayerStated, PaymentPayeeStated, PaymentAgreed, PaymentMismatch, PaymentUnjoined})
	assert.Equal(t, registry.DeliveryStates, []string{DeliveryNone, DeliveryStated, DeliveryMatched, DeliveryMismatch})
	assert.Equal(t, registry.FailureCodes, []string{CodeSettlementMalformed, CodeAmountNotExact, CodeTermsRefUnresolved, CodeLegRoleMismatch, CodeSealerNotAuthorizedForRole, CodeSealerConflation, CodeWrappedDigestMismatch, CodeWrappedResigned})
	assert.Equal(t, registry.InformationalCodes, []string{CodePaymentRefTypeUnknown, CodeFeeUnstated, CodeFeeAssetDiffers})
	assert.Equal(t, registry.BaseProfileCodes, []string{CodeCapsuleInvalid, CodeEnvelopeInvalid})
	assert.Equal(t, keys(registry.PaymentRefTypes), keys(paymentRefTypes))
	for name, entry := range registry.PaymentRefTypes {
		assert.Equal(t, entry.Qualifiers, append([]string{}, paymentRefTypes[name].qualifiers...), name)
		assert.Equal(t, entry.ReceiveFee == "not_applicable", paymentRefTypes[name].receiveFeeNotApplicable, name)
	}
	assert.Equal(t, registry.WrappedTypes, wrappedTypes)
	assert.Equal(t, keys(registry.SettlementMember), keys(legMembers))
	for name, entry := range registry.SettlementMember {
		assert.Equal(t, entry.Required, legMembers[name].required, name)
		assert.Equal(t, entry.Optional, legMembers[name].optional, name)
	}
}
