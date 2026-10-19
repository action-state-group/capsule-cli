package plugin

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockDNS makes every key lookup fail as an unreachable resolver does: a
// timeout, not an absent record.
func blockDNS(t *testing.T) {
	t.Helper()
	old := dkimLookup
	dkimLookup = func(name string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	t.Cleanup(func() { dkimLookup = old })
}

// When DNS cannot be reached, sealing a merchant's email is neither refused
// nor faked: nothing is sealed, the email waits on the device, and the result
// says only that the merchant's signature will be checked later. A tick that
// still cannot reach DNS leaves it waiting; the first tick that can reads the
// key records, seals the email as `deal note` would have, and it then verifies
// offline against the key sealed with it.
func TestAMerchantEmailIsDeferredToTheNextTickWhenDNSIsUnreachable(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	records := map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)}
	stubDNS(t, records)
	id := openMerchantDeal(t)

	before := dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"agent","text":"Order placed"}`))["step"].(float64)
	blockDNS(t)
	email := filepath.Join(merchantFixture, "confirmation.eml")
	deferred := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", email)
	assert.Equal(t, "deferred", deferred["state"])
	assert.Equal(t, false, deferred["sealed"])
	assert.Equal(t, "the merchant's signature will be checked later", deferred["say"])
	assert.NotContains(t, deferred, "step", "nothing is sealed while the key cannot be read")
	assert.NotContains(t, deferred, "dkim", "no verdict without a key")
	_, err := invoke(t, "", "--profile", "deal", "deal", "verify-email", "--deal", id)
	require.ErrorIs(t, err, cli.ErrInput, "no merchant email is sealed yet")

	// A tick that still cannot reach DNS keeps it waiting.
	still := dealRun(t, "tick")["merchant_emails"].(map[string]any)
	require.Len(t, still["waiting"], 1)
	assert.Equal(t, id, still["waiting"].([]any)[0].(map[string]any)["deal_id"])
	assert.Equal(t, "2026-10-03T21:00:00Z", still["waiting"].([]any)[0].(map[string]any)["queued_at"])
	assert.Empty(t, still["sealed"])
	assert.Empty(t, still["refused"])
	_, err = invoke(t, "", "--profile", "deal", "deal", "verify-email", "--deal", id)
	require.ErrorIs(t, err, cli.ErrInput, "a tick without DNS seals nothing")

	// DNS is back: the next tick seals it, read from DNS.
	offline := stubDNS(t, records)
	setDealClock(t, "2026-10-03T21:10:00Z")
	tick := dealRun(t, "tick")
	emails := tick["merchant_emails"].(map[string]any)
	assert.Empty(t, emails["waiting"])
	require.Len(t, emails["sealed"], 1)
	got := emails["sealed"].([]any)[0].(map[string]any)
	assert.Equal(t, id, got["deal_id"])
	assert.Equal(t, before+1, got["step"], "deferring sealed no step: the email is the next step after the one before it")
	assert.Equal(t, "reject", got["dmarc_policy"])
	assert.Equal(t, "SE-104233", got["parsed"].(map[string]any)["order_id"])
	assert.Equal(t, "pass", got["dkim"])
	assert.Equal(t, true, got["merchant_signed"])
	assert.Equal(t, "dns", got["key_source"])
	assert.Equal(t, "signed", got["checkpoint"].(map[string]any)["state"], "the key record is checkpointed at once")
	assert.Equal(t, "ticked", tick["state"])
	assert.Equal(t, true, sealedEvidenceBody(t, id, got["capsule_id"].(string))["verified"], "a passing, aligned signature is sealed verified")

	// Sealed once: the next tick has nothing queued.
	assert.NotContains(t, dealRun(t, "tick"), "merchant_emails")

	// From here on there is no network.
	offline()
	v := dealRun(t, "verify-email", "--deal", id)
	assert.Equal(t, true, v["verified"])
	require.Len(t, v["emails"], 1)
	row := v["emails"].([]any)[0].(map[string]any)
	assert.Equal(t, "dns", row["key_source"])
	assert.Equal(t, "2026-10-03T21:10:00Z", row["sealed_at"], "sealed when the key was read, not when the email was handed over")
}

// An email that is not a raw RFC 822 message is refused at once, before any
// lookup: only an unreachable resolver defers. (A malformed DKIM-Signature is
// not a refusal: the signature fails and the email is sealed as failing.)
func TestAnEmailThatIsNotRFC822IsRefusedNotDeferred(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	id := openMerchantDeal(t)
	blockDNS(t)
	bad := filepath.Join(t.TempDir(), "bad.eml")
	require.NoError(t, os.WriteFile(bad, []byte("not a header line\n"), 0o600))
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "evidence", "--email", bad)
	require.ErrorIs(t, err, cli.ErrInput)
	assert.NotContains(t, dealRun(t, "tick"), "merchant_emails", "a refused email is not queued")
}

// A record supplied with --key-record (a test, or a machine with no resolver)
// seals the email marked supplied, which the report and the emailed receipt
// both say: it was not read from the merchant's DNS by this tool, so it is
// weaker evidence than a resolved one.
func TestAMerchantEmailSealsWithASuppliedKeyRecord(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	id := openMerchantDeal(t)
	blockDNS(t)

	email := filepath.Join(merchantFixture, "confirmation.eml")
	sealed := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", email, "--key-record", filepath.Join(merchantFixture, "key-record.txt"))
	assert.Equal(t, "supplied", sealed["key_source"])

	dir := t.TempDir()
	receipt := filepath.Join(dir, "receipt.eml")
	report := dealRun(t, "report", "--deal", id, "--email", receipt)
	var row map[string]any
	for _, m := range report["merchant"].([]any) {
		row = m.(map[string]any)
	}
	require.NotNil(t, row, "the sealed email is in the report")
	assert.Equal(t, "supplied", row["key_source"])
	raw, err := os.ReadFile(receipt)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "supplied by hand, not read from the merchant's DNS")
}

// The deal skill never asks the user to do DNS: when the key cannot be read
// it says one plain thing, and the tick does the rest.
func TestTheDealSkillNeverAsksTheUserToDoDNS(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "deal", "SKILL.md"))
	require.NoError(t, err)
	skill := string(raw)
	for _, part := range []string{
		`"state": "deferred"`,
		`"the merchant's signature will be checked later"`,
		"Never ask\nthe user to look anything up",
	} {
		assert.Contains(t, skill, part)
	}
	for _, instruction := range []string{"TXT record", "_domainkey", "_dmarc", "dig ", "nslookup", "--key-record", "--dmarc-record", "look up the merchant", "host -t", "resolver"} {
		assert.False(t, strings.Contains(skill, instruction), "the skill names %q", instruction)
	}
}

// DMARC records count as key records here: one that cannot be read defers the
// capture, even when the key itself could be read.
func TestAnUnreachableDMARCRecordAloneDefers(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	key := merchantKeyTXT(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": key, dmarcName: merchantDMARC(t)})
	id := openMerchantDeal(t)
	old := dkimLookup
	dkimLookup = func(name string) ([]string, error) {
		if name == merchantSelector+"._domainkey.shop.example" {
			return []string{key}, nil
		}
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	t.Cleanup(func() { dkimLookup = old })
	got := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	assert.Equal(t, "deferred", got["state"])
}

// Two deals, two deferred emails. One deal closes while its email waits, and
// that email's note carried a cancel-by date a closed deal does not take: the
// tick refuses it, keeps it on the device and lists it at every tick, and
// still seals the other deal's email.
func TestADeferredEmailItsDealNoLongerTakesIsKeptAndListed(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	records := map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)}
	stubDNS(t, records)
	a, b := openMerchantDeal(t), openMerchantDeal(t)

	blockDNS(t)
	trial := dealRun(t, "note", "--deal", a, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "trial.eml"), "--input", writeJSON(t, `{
		"about": "Shop Example Plus trial", "source": "merchant_email",
		"obligation": {"kind": "trial_conversion", "cancel_by": "2026-10-16", "takes_effect": "2026-10-17", "amount_minor": 2400, "currency": "USD", "period": "month"}
	}`))
	assert.Equal(t, "deferred", trial["state"])
	assert.Equal(t, "deferred", dealRun(t, "note", "--deal", b, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))["state"])
	dealRun(t, "close", "--deal", a, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"sticker pack"}}`))

	stubDNS(t, records)
	emails := dealRun(t, "tick")["merchant_emails"].(map[string]any)
	require.Len(t, emails["sealed"], 1)
	assert.Equal(t, b, emails["sealed"].([]any)[0].(map[string]any)["deal_id"])
	require.Len(t, emails["refused"], 1)
	r := emails["refused"].([]any)[0].(map[string]any)
	assert.Equal(t, a, r["deal_id"])
	assert.Contains(t, r["reason"], "this deal is closed")
	assert.Contains(t, emails["refused_note"], "tell the user")

	// Kept and listed again, never retried, never sealed.
	again := dealRun(t, "tick")["merchant_emails"].(map[string]any)
	assert.Len(t, again["refused"], 1)
	assert.Empty(t, again["sealed"])
	_, err := invoke(t, "", "--profile", "deal", "deal", "verify-email", "--deal", a)
	require.ErrorIs(t, err, cli.ErrInput, "the refused email is not sealed on its deal")
}

// A queued email whose exact bytes its deal already holds (a tick that sealed
// it and stopped before removing it from the queue) is not sealed twice.
func TestADeferredEmailAlreadySealedIsNotSealedAgain(t *testing.T) {
	dir := dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	id := openMerchantDeal(t)
	email := filepath.Join(merchantFixture, "confirmation.eml")
	sealed := dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", email)

	raw, err := os.ReadFile(email)
	require.NoError(t, err)
	db, err := sql.Open("sqlite", filepath.Join(dir, "deal.db"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO deal_deferred_emails (deal_id, queued_at, evidence, raw) VALUES (?,?,?,?)`, id, "2026-10-03T21:00:00Z", `{"about":"merchant confirmation email","source":"merchant_email"}`, raw)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	emails := dealRun(t, "tick")["merchant_emails"].(map[string]any)
	require.Len(t, emails["sealed"], 1)
	got := emails["sealed"].([]any)[0].(map[string]any)
	assert.Equal(t, true, got["already_sealed"])
	assert.Equal(t, sealed["step"], got["step"])
	assert.Len(t, dealRun(t, "verify-email", "--deal", id)["emails"], 1, "one sealed copy, not two")
	assert.NotContains(t, dealRun(t, "tick"), "merchant_emails", "the queue row is gone")
}

// sealedEvidenceBody is the evidence body of one sealed step's x-deal record:
// the map that holds its merchant_email.
func sealedEvidenceBody(t *testing.T, dealID, capsuleID string) map[string]any {
	t.Helper()
	record, ok := dealRecords(t, dealID)[capsuleID]
	require.True(t, ok, "no sealed record for %s", capsuleID)
	var find func(v any) map[string]any
	find = func(v any) map[string]any {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["merchant_email"]; ok {
				return x
			}
			for _, c := range x {
				if m := find(c); m != nil {
					return m
				}
			}
		case []any:
			for _, c := range x {
				if m := find(c); m != nil {
					return m
				}
			}
		}
		return nil
	}
	body := find(record)
	require.NotNil(t, body, "the sealed record holds no merchant_email")
	return body
}

// A deferred email sealed at a tick is verified only when the merchant's own
// signature checks out, exactly as one sealed by `deal note`: a tampered
// body, a passing signature from a domain that is not the merchant's (a
// mailing service relaying it), and no signature at all are each sealed, and
// each is sealed NOT verified, in the record and in verify-email.
func TestADeferredEmailWhoseSignatureDoesNotCheckOutIsSealedNotVerified(t *testing.T) {
	confirmation, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	tampered := bytes.Replace(confirmation, []byte("Order total: $64.00"), []byte("Order total: $48.00"), 1)
	require.NotEqual(t, confirmation, tampered)

	// A real signature, from a mailing service's own domain and key.
	relayKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	var relayed bytes.Buffer
	require.NoError(t, dkim.Sign(&relayed, strings.NewReader(merchantMessage("SE-104300", "Sat, 03 Oct 2026 21:20:00 +0000", "64.00")), &dkim.SignOptions{
		Domain: "mailer.example", Selector: "m1", Signer: relayKey, Hash: crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"},
	}))
	der, err := x509.MarshalPKIXPublicKey(&relayKey.PublicKey)
	require.NoError(t, err)
	relayTXT := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)

	for _, tc := range []struct {
		name, dkim, says string
		raw              []byte
	}{
		{"tampered body", "fail", "not confirmed: ", tampered},
		{"signed by a relay, not the merchant", "pass", "signed by mailer.example, which is not the sender's own domain", relayed.Bytes()},
		{"unsigned", "none", "not confirmed: ", []byte(merchantMessage("SE-104301", "Sat, 03 Oct 2026 21:21:00 +0000", "64.00"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dealFixture(t)
			setDealClock(t, "2026-10-03T21:00:00Z")
			records := map[string]string{
				merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t),
				dmarcName:                      merchantDMARC(t),
				"m1._domainkey.mailer.example": relayTXT,
			}
			stubDNS(t, records)
			id := openMerchantDeal(t)
			email := filepath.Join(t.TempDir(), "email.eml")
			require.NoError(t, os.WriteFile(email, tc.raw, 0o600))

			blockDNS(t)
			require.Equal(t, "deferred", dealRun(t, "note", "--deal", id, "--kind", "evidence", "--email", email)["state"])

			offline := stubDNS(t, records)
			emails := dealRun(t, "tick")["merchant_emails"].(map[string]any)
			require.Len(t, emails["sealed"], 1, "sealed, whatever the signature says")
			got := emails["sealed"].([]any)[0].(map[string]any)
			assert.Equal(t, tc.dkim, got["dkim"])
			assert.Equal(t, false, got["merchant_signed"])
			assert.Contains(t, got["merchant_says"], tc.says)
			assert.NotContains(t, got["merchant_says"], "merchant-confirmed")

			body := sealedEvidenceBody(t, id, got["capsule_id"].(string))
			assert.Equal(t, false, body["verified"], "the sealed step is not verified")
			assert.Equal(t, false, body["merchant_email"].(map[string]any)["merchant_signed"])

			offline()
			out, err := invoke(t, "", "--profile", "deal", "deal", "verify-email", "--deal", id)
			require.ErrorIs(t, err, cli.ErrEmailUnverified)
			var v map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &v), out)
			assert.Equal(t, false, v["verified"])
			row := v["emails"].([]any)[0].(map[string]any)
			assert.Equal(t, tc.dkim, row["dkim_at_seal"])
			assert.Equal(t, false, row["merchant_signed"])
		})
	}
}
