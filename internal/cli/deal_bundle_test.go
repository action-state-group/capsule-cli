package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealLogEntries reads a deal's own log: the capsule ids of its steps.
func dealLogEntries(t *testing.T, dealID string) []string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	_, dsn, err := sqliteConnection(p)
	require.NoError(t, err)
	log, err := openLog(context.Background(), p, dsn, dealLogID(dealID))
	require.NoError(t, err)
	defer log.Close()
	entries, err := log.ScanEntries(context.Background(), 0, 100)
	require.NoError(t, err)
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = hex.EncodeToString(e.Value)
	}
	return ids
}

// sharedCopy writes a deal's shared bundle with disclose --deal and returns
// it decoded and as written.
func sharedCopy(t *testing.T, dealID, audience, recipient string) (map[string]any, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shared.json")
	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID, "--share", audience, "--to", recipient, "--out", path)
	require.NoError(t, err, out)
	raw := mustRead(t, path)
	var b map[string]any
	require.NoError(t, json.Unmarshal(raw, &b))
	return b, string(raw)
}

// bundle --deal is the user's own copy, built as `deal report` builds it,
// from the deal's own log and nothing else.
func TestDealBundleIsTheDealsOwnLog(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	path := filepath.Join(t.TempDir(), "b.json")
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID, "--out", path)
	require.NoError(t, err, out)
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])

	var bundle map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &bundle))
	cert := bundle["completeness_certificate"].(map[string]any)
	assert.Equal(t, dealLogID(dealID), cert["log_id"], "the bundle's log is the deal's own")
	steps := dealLogEntries(t, dealID)
	records := bundle["records"].([]any)
	require.Len(t, records, len(steps), "the whole deal")
	for _, r := range records {
		assert.Contains(t, steps, r.(map[string]any)["capsule_id"], "every record is a step of this deal")
	}
	// The cadence chain rides as its own extension (it anchors the deal's
	// checkpoint to the witness); nothing else names the cadence log.
	ext := bundle["extensions"].(map[string]any)
	require.Contains(t, ext, dealCadenceExtension)
	assert.Equal(t, dealAudienceKeep, ext["x-deal-v0"].(map[string]any)["audience"])
	rest := map[string]any{}
	for k, v := range bundle {
		rest[k] = v
	}
	rest["extensions"] = map[string]any{"x-deal-v0": ext["x-deal-v0"]}
	raw, err := json.Marshal(rest)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "deal-cadence/", "no record, proof or certificate comes from the cadence log")

	// Nothing went on record: the user's own copy is not a share.
	records2, onLog := sharedRecords(t, dealID)
	assert.Empty(t, records2)
	assert.Empty(t, onLog)
}

// A deal is not shared as a link: permalink --deal refuses with the report
// file to hand over instead, and puts nothing on record.
func TestDealPermalinkRefusesADeal(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	for _, args := range [][]string{
		{"permalink", "--deal", dealID},
		{"permalink", "--deal", dealID, "--share", "counterparty", "--to", "the shop"},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal"}, args...)...)
		require.ErrorIs(t, err, ErrInput, strings.Join(args, " "))
		assert.Contains(t, err.Error(), "a deal is not shared as a link")
		assert.Contains(t, err.Error(), "deal report --deal ID --html FILE")
	}
	records, onLog := sharedRecords(t, dealID)
	assert.Empty(t, records)
	assert.Empty(t, onLog)
}

// disclose --deal hands a copy to someone else: for both audiences a planted
// address, code or card never reaches it, it still verifies (withheld records
// show as WITHHELD), and each share is on the disclosure log before the file
// exists.
func TestDealDiscloseCarriesNoPrivateValues(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	steps := len(dealLogEntries(t, dealID))
	for _, args := range [][]string{
		{"disclose", "--deal", dealID, "--out", filepath.Join(t.TempDir(), "x.json")},
		{"disclose", "--deal", dealID, "--share", "keep", "--out", filepath.Join(t.TempDir(), "x.json")},
		{"disclose", "--deal", dealID, "--share", "counterparty", "--out", filepath.Join(t.TempDir(), "x.json")},
		{"disclose", "--deal", dealID, "--share", "counterparty", "--to", "x", "--payloads", "all", "--out", filepath.Join(t.TempDir(), "x.json")},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal"}, args...)...)
		require.ErrorIs(t, err, ErrInput, strings.Join(args, " "))
	}
	for i, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		b, raw := sharedCopy(t, dealID, audience, "the shop's support desk")
		assertCarriesNone(t, raw)
		assert.Equal(t, audience, b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["audience"])
		linked := filepath.Join(t.TempDir(), "copy.json")
		require.NoError(t, os.WriteFile(linked, []byte(raw), 0o600))
		verified, err := verifyWithDirectory(t, linked, "")
		require.NoError(t, err)
		assert.Equal(t, "VALID", verified["verdict"])
		records, onLog := sharedRecords(t, dealID)
		require.Len(t, records, i+1)
		assert.Equal(t, audience, records[i]["audience"])
		assert.NotEmpty(t, records[i]["withheld_records"])
		assert.Len(t, onLog, i+1)
	}
	assert.Len(t, dealLogEntries(t, dealID), steps, "the deal's own log takes steps only")
}

// disclose --deal is a share too: it writes the shared bundle file, and its
// disclosure record goes on the deal's disclosure log, never the deal's own.
func TestDealDiscloseIsAShareOnTheDisclosureLog(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	steps := len(dealLogEntries(t, dealID))
	path := filepath.Join(t.TempDir(), "shared.json")
	_, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID, "--share", "counterparty", "--to", "x")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "disclose --deal needs --out FILE")

	out, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID, "--share", "adjudicator", "--to", "the card issuer", "--out", path)
	require.NoError(t, err, out)
	assertCarriesNone(t, string(mustRead(t, path)))
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	records, onLog := sharedRecords(t, dealID)
	require.Len(t, records, 1)
	assert.Equal(t, "adjudicator", records[0]["audience"])
	assert.Len(t, onLog, 1, "the disclosure record is on deal/<id>/disclosures")
	assert.Len(t, dealLogEntries(t, dealID), steps, "the deal's own log takes steps only")
	dealRun(t, "report", "--deal", dealID)
}

func TestDealBundleNamesWhatItNeeds(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bundle", "--root", "x"}, "name the deal with --deal DEAL_ID"},
		{[]string{"bundle", "--deal", dealID, "--log-id", "x"}, "do not apply"},
		{[]string{"bundle", "--deal", dealID, "--root", "x"}, "do not apply"},
		{[]string{"bundle", "--log-id", dealLogID(dealID), "--root", "x"}, "read with --deal DEAL_ID"},
		{[]string{"disclose", "--log-id", dealDisclosureLogID(dealID), "--root", "x"}, "read with --deal DEAL_ID"},
		{[]string{"bundle", "--deal", dealID, "--share", "counterparty", "--to", "x"}, "bundle --deal writes your own copy"},
		{[]string{"bundle", "--log-id", "other", "--root", "x", "--share", "counterparty"}, "--share and --to go with --deal"},
		{[]string{"bundle", "--deal", "not-a-deal"}, "deal id"},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal"}, tc.args...)...)
		require.ErrorIs(t, err, ErrInput, strings.Join(tc.args, " "))
		assert.Contains(t, err.Error(), tc.want, strings.Join(tc.args, " "))
	}
	// The deal still works afterwards: nothing was appended to its log.
	dealRun(t, "report", "--deal", dealID)
}

// A sqlite deal profile has no evidence book: the deal path reads the deal's
// own log through the deal session.
func TestDealProfileHasNoBook(t *testing.T) {
	dealFixture(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	require.Equal(t, "sqlite", p.Type)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	defer func() { require.NoError(t, target.close()) }()
	assert.Nil(t, target.book)
}

// A deal with a merchant's own email (addressed To a display name) shares as
// a file: the counterparty's copy carries the shareable order id and none of
// the customer's name, email, code or card; the adjudicator's no order id.
func TestDealDiscloseCarriesTheShareableOrderID(t *testing.T) {
	dealFixture(t)
	stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))

	b, raw := sharedCopy(t, dealID, "counterparty", "the shop")
	content := strings.ToLower(raw)
	assert.Contains(t, raw, "SE-104233", "the counterparty's copy carries the merchant-confirmed order id")
	for _, v := range []string{"Sam Customer", "sam.customer@mail.example", "orders@shop.example", "card ending 4242", "99812", "Customer"} {
		assert.NotContains(t, content, strings.ToLower(v))
	}
	rows := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["merchant"].([]any)
	require.Len(t, rows, 1)
	assert.Equal(t, "SE-104233", rows[0].(map[string]any)["order_id"])

	_, raw = sharedCopy(t, dealID, "adjudicator", "the card issuer")
	for _, v := range []string{"SE-104233", "104233", "Sam Customer", "sam.customer@mail.example"} {
		assert.NotContains(t, strings.ToLower(raw), strings.ToLower(v))
	}
}
