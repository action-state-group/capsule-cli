package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dealProfileDir = "../../skills/deal/profile"

// The Go port reproduces the profile's fingerprint and commitment vectors.
func TestDealFingerprintVectors(t *testing.T) {
	raw, err := os.ReadFile(dealProfileDir + "/fixtures/fingerprint-vectors.json")
	require.NoError(t, err)
	var v struct {
		StoreSecretHex string `json:"store_secret_hex"`
		DealID         string `json:"deal_id"`
		DealKeyHex     string `json:"deal_key_hex"`
		Vectors        []struct{ Kind, Raw, Normalized, FP string }
		Commitments    []struct{ Label, Nonce, Text, Commitment string }
	}
	require.NoError(t, json.Unmarshal(raw, &v))
	secret, err := hex.DecodeString(v.StoreSecretHex)
	require.NoError(t, err)
	key := dealKeyFor(secret, v.DealID)
	assert.Equal(t, v.DealKeyHex, hex.EncodeToString(key))
	for _, x := range v.Vectors {
		n, err := normalizeID(x.Kind, x.Raw)
		require.NoError(t, err, x.Raw)
		assert.Equal(t, x.Normalized, n, "%s %q", x.Kind, x.Raw)
		fp, err := fingerprintID(key, x.Kind, x.Raw)
		require.NoError(t, err)
		assert.Equal(t, x.FP, fp, "%s %q", x.Kind, x.Raw)
	}
	for _, x := range v.Commitments {
		c, err := commitText(x.Nonce, x.Text)
		require.NoError(t, err)
		assert.Equal(t, x.Commitment, c, x.Label)
	}
	for _, bad := range [][2]string{{"phone", "call me"}, {"phone", "555 0100"}, {"email", "nobody"}, {"name", "Inc."}, {"domain", "localhost"}} {
		_, err := normalizeID(bad[0], bad[1])
		assert.ErrorIs(t, err, errNotNormalizable, bad[1])
	}
	assert.True(t, sameID("phone", "+1 555 010 2000", "(555) 010-2000"))
	assert.True(t, sameID("domain", "book.coastaljetrentals.example", "https://CoastalJetRentals.example/"))
	assert.False(t, sameID("payee", "Coastal Jet Rentals LLC", "M. Torres"))
}

func TestDealScanRefusesRawIdentifiers(t *testing.T) {
	ok := map[string]interface{}{"body": map[string]interface{}{"text": "has 2 jet skis", "digest": "0123456789abcdef0123"}}
	require.NoError(t, scanRecord(ok, []string{"M. Torres"}))
	for _, text := range []string{"call (555) 010-2044", "mail bookings@coastal.example", "ask for m. torres", "seller rating 4.9"} {
		rec := map[string]interface{}{"body": map[string]interface{}{"text": text}}
		assert.ErrorIs(t, scanRecord(rec, []string{"M. Torres"}), ErrInput, text)
	}
}
