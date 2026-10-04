package cli

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
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

// A deal is its own log: bundle and permalink read it with --deal.
func TestDealBundleAndPermalinkReadTheDealsOwnLog(t *testing.T) {
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
	assert.Len(t, bundle["records"], 5, "the whole deal by default")

	// Uncompressed, a 5-step deal is too large for a link: the bundle file
	// is the way to share it, and nothing is hosted.
	_, err = invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--payloads", "selected")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "too large for a link")
	assert.Contains(t, err.Error(), "share the bundle file")
	link, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--payloads", "selected", "--max-fragment", "0")
	require.NoError(t, err, link)
	assert.True(t, strings.HasPrefix(link, defaultBundleURL+"#"), "a link that is made goes to the neutral verifier")
	fragment := strings.TrimSpace(link[strings.Index(link, "#")+1:])
	decoded, err := aacbundle.DecodeFragment(fragment)
	require.NoError(t, err)
	raw, err := json.Marshal(decoded)
	require.NoError(t, err)
	linked := filepath.Join(t.TempDir(), "linked.json")
	require.NoError(t, os.WriteFile(linked, raw, 0o600))
	result, err = verifyWithDirectory(t, linked, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"], "the permalink's bundle verifies")

	// The same deal through --log-id.
	out, err = invoke(t, "", "--profile", "deal", "bundle", "--log-id", dealLogID(dealID), "--root", bundle["root"].(string), "--closure-depth", "4")
	require.NoError(t, err, out)
}

func TestDealBundleNamesTheLogItNeeds(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	_, err := invoke(t, "", "--profile", "deal", "bundle", "--root", "x")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "name the deal with --deal DEAL_ID")
	_, err = invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "a deal's log takes deal steps only")
	_, err = invoke(t, "", "--profile", "deal", "bundle", "--deal", "not-a-deal")
	require.ErrorIs(t, err, ErrInput)
	_, err = invoke(t, "", "--profile", "deal", "bundle", "--deal", dealID, "--log-id", "x")
	require.ErrorIs(t, err, ErrInput)
	// The deal still works afterwards: nothing was appended to its log.
	dealRun(t, "report", "--deal", dealID)
}

func TestFragmentCodecZ1(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	link, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--payloads", "selected", "--max-fragment", "0")
	require.NoError(t, err)
	plain := strings.TrimSpace(link[strings.Index(link, "#")+1:])
	value, err := aacbundle.DecodeFragment(plain)
	require.NoError(t, err)

	z1, err := encodeFragmentZ1(value)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(z1, "z1."))
	back, err := decodeFragmentAny(z1)
	require.NoError(t, err)
	assert.Equal(t, value, back, "z1 round-trips")
	old, err := decodeFragmentAny(plain)
	require.NoError(t, err)
	assert.Equal(t, value, old, "a plain fragment still decodes")
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
		for _, payloads := range []string{"selected", "all"} {
			link, err := invoke(t, "", "--profile", "deal", "permalink", "--deal", dealID, "--payloads", payloads, "--max-fragment", "0")
			if err != nil {
				fmt.Printf("steps=%d payloads=%s: %v\n", steps, payloads, err)
				continue
			}
			plain := strings.TrimSpace(link[strings.Index(link, "#")+1:])
			value, err := aacbundle.DecodeFragment(plain)
			require.NoError(t, err)
			z1, err := encodeFragmentZ1(value)
			require.NoError(t, err)
			jcs, _ := base64.RawURLEncoding.DecodeString(plain)
			fmt.Printf("steps=%d payloads=%s bundle(no disclosure)=%dB disclosed-JCS=%dB plain-fragment=%d z1-fragment=%d ratio=%.2fx\n",
				steps, payloads, len(mustRead(t, bundlePath)), len(jcs), len(plain), len(z1), float64(len(plain))/float64(len(z1)))
		}
	}
}

// A sqlite deal profile has no evidence book: bundle takes the log path, so
// it must be told which log (a deal's own) to read.
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
