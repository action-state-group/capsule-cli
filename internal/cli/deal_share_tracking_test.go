package cli

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shippingSelector = "ship2026"

// cardShapedTracking is a 22-digit USPS-style tracking number, grouped as
// carriers print it, whose digits fit one of the anchored card windows: on its
// own the gate would read it as a card number.
func cardShapedTracking(t *testing.T) string {
	t.Helper()
	for i := int64(0); i < 1000; i++ {
		d := fmt.Sprintf("9400%018d", 111_222_333_444_555_666+i*7919)
		if gateCardWindows(d) {
			var groups []string
			for j := 0; j < len(d); j += 4 {
				groups = append(groups, d[j:min(j+4, len(d))])
			}
			return strings.Join(groups, " ")
		}
	}
	t.Fatal("no card-shaped tracking number in range")
	return ""
}

// shippingEmail is the shop's shipping notice carrying tracking, signed for
// shop.example with a throwaway key; it returns the signed bytes and the key's
// DNS record.
func shippingEmail(t *testing.T, tracking string) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	msg := simpleMessage("orders@shop.example", "Mon, 05 Oct 2026 09:30:00 +0000", "<ship-104233@shop.example>",
		"Your Shop Example order SE-104233 has shipped",
		"Good news: your order has shipped.\r\n\r\nOrder number: SE-104233\r\nTracking number: "+tracking+"\r\n\r\nShop Example\r\n")
	var signed bytes.Buffer
	require.NoError(t, dkim.Sign(&signed, strings.NewReader(msg), &dkim.SignOptions{
		Domain: "shop.example", Selector: shippingSelector, Signer: key, Hash: crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"},
	}))
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return signed.Bytes(), "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
}

// A tracking number the deal recorded, from the shop's own signed shipping
// email, is carried in the counterparty's copy although its digits fit a card
// window; the adjudicator's copy withholds it, as it does the order id.
func TestDealDiscloseCarriesARecordedTrackingNumber(t *testing.T) {
	tracking := cardShapedTracking(t)
	raw, txt := shippingEmail(t, tracking)
	dealFixture(t)
	stubDNS(t, map[string]string{
		merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t),
		shippingSelector + "._domainkey.shop.example": txt,
		dmarcName: merchantDMARC(t),
	})
	dealID := openMerchantDeal(t)
	email := filepath.Join(t.TempDir(), "shipped.eml")
	require.NoError(t, os.WriteFile(email, raw, 0o600))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", email)

	b, shared := sharedCopy(t, dealID, "counterparty", "the shop")
	rows := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["merchant"].([]any)
	require.Len(t, rows, 1)
	assert.Equal(t, tracking, rows[0].(map[string]any)["tracking"])
	assert.Contains(t, shared, tracking)

	_, shared = sharedCopy(t, dealID, "adjudicator", "the card issuer")
	assert.NotContains(t, shared, tracking)
	assert.NotContains(t, shared, strings.ReplaceAll(tracking, " ", ""))
}

// At the gate: the same card-shaped digits pass in the counterparty's copy
// only when the deal recorded them from a signed shipping email that checks
// out, signed by the deal's own counterparty. Not recorded, recorded from an
// email that does not check out, or in the adjudicator's copy, they are
// refused.
func TestDealShareGateTrackingNumberOnlyWhenRecorded(t *testing.T) {
	tracking := cardShapedTracking(t)
	raw, txt := shippingEmail(t, tracking)
	keys, err := parseKeyRecord([]byte(shippingSelector+`._domainkey.shop.example. 3600 IN TXT "`+txt+`"`+"\n"), raw)
	require.NoError(t, err)
	m, err := captureEmail(raw, keys)
	require.NoError(t, err)
	require.Equal(t, tracking, m.Parsed.Tracking)
	altered, err := captureEmail(bytes.Replace(raw, []byte("Good news"), []byte("Great news"), 1), keys)
	require.NoError(t, err)

	open := sealedEvent{Event: dealEvent{Kind: "open", Open: &dealOpen{Who: dealWho{Name: "Shop Example", Domain: "shop.example"}}}}
	recorded := func(m *merchantEmail) []sealedEvent {
		return []sealedEvent{open, {Event: dealEvent{Kind: "evidence", Evidence: &dealEvidence{About: "shipped", Source: "email", Email: m}}}}
	}
	for _, page := range [][]byte{
		gatePage(t, "Your parcel: "+tracking),
		gatePage(t, "Your parcel: "+strings.ReplaceAll(tracking, " ", "")),
	} {
		assert.ErrorContains(t, dealShareGate(page, []sealedEvent{open}, dealAudienceCounterparty), "card number", "not recorded")
		assert.Error(t, dealShareGate(page, recorded(altered), dealAudienceCounterparty), "recorded from an email that does not check out")
		assert.Error(t, dealShareGate(page, recorded(m), dealAudienceAdjudicator), "the adjudicator's copy")
	}
	assert.NoError(t, dealShareGate(gatePage(t, "Your parcel: "+tracking), recorded(m), dealAudienceCounterparty), "recorded, as written")
}
