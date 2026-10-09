package cli

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/settlement"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type settlementVectorCase struct {
	ID        string            `json:"id"`
	KeyPolicy map[string]string `json:"key_policy"`
	Records   []struct {
		Label       string          `json:"label"`
		Capsule     json.RawMessage `json:"capsule"`
		EnvelopeHex string          `json:"envelope_hex"`
		EnvelopeKid string          `json:"envelope_kid"`
	} `json:"records"`
	WrappedObjects []struct {
		OctetsB64u string `json:"octets_b64u"`
	} `json:"wrapped_objects"`
}

// writeSettlementCase writes one conformance vector case as files: each leg a
// bare Capsule with its inline signature (or, for the labels in asRecord, an
// artifact.Record), each wrapped object as its octets. It returns the
// command's arguments.
func writeSettlementCase(t *testing.T, id string, skip map[string]bool, asRecord map[string]bool) []string {
	t.Helper()
	raw, err := os.ReadFile("../settlement/testdata/vectors/cases.json")
	require.NoError(t, err)
	var file struct {
		Cases []settlementVectorCase `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &file))
	dir := t.TempDir()
	for _, c := range file.Cases {
		if c.ID != id {
			continue
		}
		args := []string{"settlement", "status", "--payer-key", c.KeyPolicy["payer"], "--payee-key", c.KeyPolicy["payee"]}
		for _, r := range c.Records {
			if skip[r.Label] {
				continue
			}
			path := filepath.Join(dir, r.Label+".json")
			envelope, err := hex.DecodeString(r.EnvelopeHex)
			require.NoError(t, err)
			var data []byte
			if asRecord[r.Label] {
				var capsule map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(r.Capsule, &capsule))
				var capsuleID string
				require.NoError(t, json.Unmarshal(capsule["capsule_id"], &capsuleID))
				data, err = json.Marshal(artifact.Record{CapsuleID: capsuleID, Capsule: r.Capsule, ProducerEnvelope: envelope})
			} else {
				var capsule map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(r.Capsule, &capsule))
				capsule["signature"], _ = json.Marshal(r.EnvelopeHex)
				capsule["key_id"], _ = json.Marshal(r.EnvelopeKid)
				data, err = json.Marshal(capsule)
			}
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			args = append(args, "--leg", path)
		}
		for i, w := range c.WrappedObjects {
			octets, err := base64.RawURLEncoding.DecodeString(w.OctetsB64u)
			require.NoError(t, err)
			path := filepath.Join(dir, "object-"+string(rune('a'+i)))
			require.NoError(t, os.WriteFile(path, octets, 0o600))
			args = append(args, "--object", path)
		}
		return args
	}
	t.Fatalf("no vector case %s", id)
	return nil
}

type settlementStatusOutput struct {
	Settlement struct {
		Conforming  bool `json:"conforming"`
		Settlements []struct {
			PaymentState     string   `json:"payment_state"`
			AgreedStatus     string   `json:"agreed_status"`
			DeliveryState    string   `json:"delivery_state"`
			DeliveryEvidence []string `json:"delivery_evidence"`
		} `json:"settlements"`
		Legs []struct {
			AuthenticatedKey string `json:"authenticated_key"`
		} `json:"legs"`
		KeyPolicy string `json:"key_policy"`
	} `json:"settlement"`
	Readings []struct {
		Payment  string `json:"payment"`
		Delivery string `json:"delivery"`
	} `json:"readings"`
}

func runSettlementStatusCmd(t *testing.T, args []string) (settlementStatusOutput, error) {
	t.Helper()
	out, err := invoke(t, "", args...)
	var result settlementStatusOutput
	require.NoError(t, json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &result), out)
	return result, err
}

func TestSettlementStatusDerivesAgreedAndMatchedFromBothShapes(t *testing.T) {
	args := writeSettlementCase(t, "pos-x402-two-sided-agreed", nil, map[string]bool{"x402-payee-observed": true})
	result, err := runSettlementStatusCmd(t, args)
	require.NoError(t, err)
	assert.True(t, result.Settlement.Conforming)
	assert.Equal(t, "applied", result.Settlement.KeyPolicy)
	s := result.Settlement.Settlements[0]
	assert.Equal(t, "agreed", s.PaymentState)
	assert.Equal(t, "settled", s.AgreedStatus)
	assert.Equal(t, "matched", s.DeliveryState)
	assert.Equal(t, []string{"seller_handoff", "buyer_receipt"}, s.DeliveryEvidence)
	assert.Contains(t, result.Readings[0].Payment, "and the amount equals the terms")
	for _, l := range result.Settlement.Legs {
		assert.Len(t, l.AuthenticatedKey, 64)
	}
}

func TestSettlementStatusReadsOneSidedStatesAsStatedClaims(t *testing.T) {
	args := writeSettlementCase(t, "pos-x402-two-sided-agreed", map[string]bool{"x402-payer-observed": true, "x402-delivered-received": true}, nil)
	result, err := runSettlementStatusCmd(t, args)
	require.NoError(t, err)
	assert.Equal(t, "payee_stated", result.Settlement.Settlements[0].PaymentState)
	assert.Contains(t, result.Readings[0].Payment, "a stated claim by the payee alone, not an agreement")
	assert.Equal(t, "stated", result.Settlement.Settlements[0].DeliveryState)
	assert.Contains(t, result.Readings[0].Delivery, "not proof of receipt")
	assert.NotContains(t, result.Readings[0].Delivery, "received this content")
}

func TestSettlementStatusFailsOnAFailingLeg(t *testing.T) {
	args := writeSettlementCase(t, "neg-wrapped-content-digest-mismatch", nil, nil)
	result, err := runSettlementStatusCmd(t, args)
	require.Error(t, err)
	assert.False(t, result.Settlement.Conforming)
	assert.Equal(t, "payer_stated", result.Settlement.Settlements[0].PaymentState)
}

func TestSettlementStatusInputErrors(t *testing.T) {
	_, err := invoke(t, "", "settlement", "status")
	assert.ErrorContains(t, err, "at least one --leg")
	args := writeSettlementCase(t, "pos-x402-one-sided-payee", nil, nil)
	_, err = invoke(t, "", append(args, "--payer-key", "nothex")...)
	assert.ErrorContains(t, err, "--payer-key must be")
	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"capsule_id":"x","spec_version":"x","format_version":"4","signature":"zz","key_id":"x"}`), 0o600))
	_, err = invoke(t, "", "settlement", "status", "--leg", bad)
	assert.ErrorContains(t, err, "signature must be the hex Producer Envelope")
}

func TestAnAgreedPaymentThatDiffersFromTheTermsIsNeverReadAsClean(t *testing.T) {
	differs := paymentReading(settlement.Settlement{PaymentState: settlement.PaymentAgreed, AgreedStatus: "settled", TermsAmount: "differs"})
	assert.True(t, strings.HasPrefix(differs, "the amount paid DIFFERS FROM THE TERMS"), differs)
	assert.Contains(t, differs, "not the amount the terms state")
	assert.NotContains(t, differs, "equals the terms")

	equal := paymentReading(settlement.Settlement{PaymentState: settlement.PaymentAgreed, AgreedStatus: "settled", TermsAmount: "equal"})
	assert.Contains(t, equal, "and the amount equals the terms")
	assert.NotContains(t, equal, "DIFFERS")
}
