package cli

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
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

// linkBundle decodes the bundle a permalink carries.
func linkBundle(t *testing.T, link string) (map[string]any, string) {
	t.Helper()
	fragment := strings.TrimSpace(link[strings.Index(link, "#")+1:])
	decoded, err := aacbundle.DecodeFragment(fragment)
	require.NoError(t, err)
	b, ok := decoded.(map[string]any)
	require.True(t, ok)
	return b, fragment
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

// permalink --deal hands a copy to someone else: it is a share, with the
// audience's withholding, the gate and a disclosure record, and a planted
// address, code or card never reaches the fragment.
func TestDealPermalinkIsAShare(t *testing.T) {
	dealFixture(t)
	dealID := openPrivateDeal(t)
	steps := len(dealLogEntries(t, dealID))

	for _, args := range [][]string{
		{"permalink", "--deal", dealID},
		{"permalink", "--deal", dealID, "--share", "keep"},
		{"permalink", "--deal", dealID, "--share", "counterparty"},
		{"permalink", "--deal", dealID, "--share", "counterparty", "--to", "x", "--payloads", "all"},
	} {
		_, err := invoke(t, "", append([]string{"--profile", "deal"}, args...)...)
		require.ErrorIs(t, err, ErrInput, strings.Join(args, " "))
	}
	_, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID)
	assert.Contains(t, err.Error(), "permalink --deal hands a copy to someone else: it needs --share counterparty or adjudicator")

	for i, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		out, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--share", audience, "--to", "the shop's support desk", "--max-fragment", "0")
		require.NoError(t, err, out)
		var result map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &result))
		link := result["permalink"].(string)
		assert.True(t, strings.HasPrefix(link, defaultBundleURL+"#"), "a link goes to the neutral verifier and carries the bundle in its fragment")
		b, fragment := linkBundle(t, link)
		decoded, err := json.Marshal(b)
		require.NoError(t, err)
		assertCarriesNone(t, link)
		assertCarriesNone(t, string(decoded))
		assert.NotContains(t, fragment, base64.RawURLEncoding.EncodeToString([]byte(homeAddress)))
		ext := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)
		assert.Equal(t, audience, ext["audience"])

		linked := filepath.Join(t.TempDir(), "linked.json")
		require.NoError(t, os.WriteFile(linked, decoded, 0o600))
		verified, err := verifyWithDirectory(t, linked, "")
		require.NoError(t, err)
		assert.Equal(t, "VALID", verified["verdict"], "the shared link's bundle still verifies: withheld records show as WITHHELD")

		records, onLog := sharedRecords(t, dealID)
		require.Len(t, records, i+1, "every link is on record before it is printed")
		assert.Equal(t, audience, records[i]["audience"])
		assert.Equal(t, "the shop's support desk", records[i]["recipient"])
		assert.NotEmpty(t, records[i]["withheld_records"], "the private records are withheld")
		assert.Len(t, onLog, i+1)
		assert.Equal(t, dealDisclosureLogID(dealID), result["share"].(map[string]any)["log_id"])
	}
	assert.Len(t, dealLogEntries(t, dealID), steps, "the deal's own log takes steps only")

	// Too large for a link: refused before anything is on record.
	_, err = invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--share", "counterparty", "--to", "x", "--max-fragment", "100")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "too large for a link")
	assert.Contains(t, err.Error(), "share the bundle file instead")
	records, _ := sharedRecords(t, dealID)
	assert.Len(t, records, 2, "a refused link is not on record")
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

func TestFragmentCodecZ1(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	out, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--share", "counterparty", "--to", "x", "--max-fragment", "0")
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	value, plain := linkBundle(t, result["permalink"].(string))

	z1, err := encodeFragmentZ1(value)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(z1, "z1."))
	back, err := decodeFragmentAny(z1)
	require.NoError(t, err)
	assert.Equal(t, any(value), back, "z1 round-trips")
	old, err := decodeFragmentAny(plain)
	require.NoError(t, err)
	assert.Equal(t, any(value), old, "a plain fragment still decodes")
	assert.Less(t, len(z1)*2, len(plain), "z1 at least halves the fragment")

	_, err = decodeFragmentAny("z2." + z1[3:])
	assert.ErrorContains(t, err, "unsupported fragment codec")
	// A compression bomb is refused at the cap.
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	_, _ = w.Write(bytes.Repeat([]byte{' '}, fragmentMaxInflated+10))
	_ = w.Close()
	_, err = decodeFragmentAny("z1." + base64.RawURLEncoding.EncodeToString(buf.Bytes()))
	assert.ErrorContains(t, err, "size cap")
}

// Measures the fragments for the size profile; run with
// CAPSULE_MEASURE_FRAGMENTS=1 to print them.
func TestMeasureDealFragments(t *testing.T) {
	if os.Getenv("CAPSULE_MEASURE_FRAGMENTS") == "" {
		t.Skip("set CAPSULE_MEASURE_FRAGMENTS=1 to measure")
	}
	for _, withAct := range []bool{false, true} {
		dealFixture(t)
		dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
		dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
		steps := 4
		if withAct {
			dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
			steps = 5
		}
		bundlePath := filepath.Join(t.TempDir(), "b.json")
		_, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID, "--out", bundlePath)
		require.NoError(t, err)
		for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
			out, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--share", audience, "--to", "x", "--max-fragment", "0")
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			value, plain := linkBundle(t, result["permalink"].(string))
			z1, err := encodeFragmentZ1(value)
			require.NoError(t, err)
			fmt.Printf("steps=%d audience=%s own-bundle=%dB plain-fragment=%d z1-fragment=%d ratio=%.2fx\n",
				steps, audience, len(mustRead(t, bundlePath)), len(plain), len(z1), float64(len(plain))/float64(len(z1)))
		}
	}
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
