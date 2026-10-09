package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A buyer's deal is shared exactly as before the seller profile: the same
// records disclosed, with the same members, and the same report in each
// shared copy. testdata/share-parity/buyer-typed.json was written by this
// test on main before the seller profile (CAPSULECTL_UPDATE_SHARE_PARITY=1);
// widening what a merchant sees on a buyer's deal is a change of its own.

var parityVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`[0-9a-f]{64}`), "H"},
	{regexp.MustCompile(`deal-[0-9a-f]{16}`), "DEAL"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`), "T"},
}

func parityNormal(s string) string {
	for _, v := range parityVolatile {
		s = v.re.ReplaceAllString(s, v.with)
	}
	return s
}

// memberPaths are a record's member paths, sorted: its shape, not its values.
func memberPaths(v any, prefix string, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			p := prefix + "." + k
			*out = append(*out, p)
			memberPaths(c, p, out)
		}
	case []any:
		for _, c := range x {
			memberPaths(c, prefix+"[]", out)
		}
	}
}

func shareParitySummary(t *testing.T, id string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		b, _ := sharedCopy(t, id, audience, "the merchant")
		var disclosed []string
		for _, r := range disclosedRecords(b) {
			var paths []string
			memberPaths(r, "", &paths)
			sort.Strings(paths)
			paths = slicesCompact(paths)
			disclosed = append(disclosed, strings.Join(paths, " "))
		}
		sort.Strings(disclosed)
		report, err := json.Marshal(dealReportOf(b))
		require.NoError(t, err)
		out[audience] = map[string]any{
			"records":   len(b["records"].([]any)),
			"disclosed": disclosed,
			"report":    parityNormal(string(report)),
		}
	}
	return out
}

func slicesCompact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func TestABuyersSharedCopiesAreUnchanged(t *testing.T) {
	dealFixture(t)
	setDealClock(t, "2026-09-27T18:00:00Z")
	id := openTypedSticker(t)
	dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, `{"text": "Ships in 2 days", "source_kind": "merchant", "source": "listing_page"}`))
	checkPay(t, id, 600)
	payNow(t, id, 600)
	got := shareParitySummary(t, id)
	raw, err := json.MarshalIndent(got, "", "  ")
	require.NoError(t, err)
	path := filepath.Join("testdata", "share-parity", "buyer-typed.json")
	if os.Getenv("CAPSULECTL_UPDATE_SHARE_PARITY") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(raw))
}
