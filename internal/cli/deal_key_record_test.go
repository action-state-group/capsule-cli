package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When DNS cannot be reached, sealing a merchant's email does not just stop:
// the error names the key record it needed and the way on, --key-record with
// that record obtained some other way. A record supplied that way seals the
// email, marked supplied, which the report and the emailed receipt both say:
// it was not read from the merchant's DNS by this tool, so it is weaker
// evidence than a resolved one.
func TestAMerchantEmailSealsWithASuppliedKeyRecordWhenDNSIsUnreachable(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-10-03T21:00:00Z")
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	id := openMerchantDeal(t)

	// DNS unreachable: a timeout, not an absent record.
	old := dkimLookup
	dkimLookup = func(name string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	t.Cleanup(func() { dkimLookup = old })
	email := filepath.Join(merchantFixture, "confirmation.eml")
	_, err := invoke(t, "", "--profile", "deal", "deal", "note", "--deal", id, "--kind", "evidence", "--email", email)
	require.ErrorIs(t, err, ErrInput)
	for _, part := range []string{
		"could not reach DNS",
		merchantSelector + "._domainkey.shop.example",
		"--key-record FILE",
		"sealed as supplied",
		"weaker evidence",
	} {
		assert.Contains(t, err.Error(), part)
	}

	// The user supplies the record, obtained some other way.
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

// The deal skill tells the agent what to do when the seal is refused for want
// of DNS: offer the supplied-record way on, never stop, and say what a
// supplied record is worth.
func TestTheDealSkillOffersTheKeyRecordWhenDNSIsUnreachable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "deal", "SKILL.md"))
	require.NoError(t, err)
	skill := string(raw)
	for _, part := range []string{
		"**If DNS cannot be reached.**",
		"do not stop",
		"If you\ncan look up the merchant's key record, I can still seal the email with it.",
		"SELECTOR._domainkey.DOMAIN",
		"--key-record key-record.txt",
		"marked **supplied**",
		"**weaker evidence**",
	} {
		assert.Contains(t, skill, part)
	}
}
