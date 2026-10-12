package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyStore is five demo deals: two retail deals paid (one closed, one
// with a copy already sealed by `bundle --deal`), one only opened, one
// opened with a later note past its checkpoint, and one more retail deal.
func historyStore(t *testing.T) []string {
	t.Helper()
	dealFixture(t)
	closed := retailDeal(t)
	dealRun(t, "close", "--deal", closed, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	reported := retailDeal(t)
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", reported, "--out", filepath.Join(t.TempDir(), "own.json"))
	require.NoError(t, err, out)
	opened := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	noted := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", noted, "--input", filepath.Join(retailDemo, "check-pay.json"))
	other := retailDeal(t)
	return []string{closed, reported, opened, noted, other}
}

// writeHistory runs `deal history` over picked, checks it changed no byte
// of the store, and returns the container's path and its summary.
func writeHistory(t *testing.T, picked ...string) (string, map[string]any) {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	path := filepath.Join(t.TempDir(), "history.json")
	args := []string{"--profile", "deal", "deal", "history", "--out", path}
	for _, id := range picked {
		args = append(args, "--deal", id)
	}
	out, err := invoke(t, "", args...)
	require.NoError(t, err, out)
	assert.Equal(t, before, storeFiles(t, p), "the history copy changes no file under the store: no record sealed, no checkpoint cut")
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &summary), out)
	return path, summary
}

func readContainer(t *testing.T, path string) map[string]any {
	t.Helper()
	var c map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &c))
	return c
}

// rewrite writes c back to a new file. With reselect, each selection digest
// is made to name the carried bundle again, as someone editing the file
// would, so only the nested bundles' own checks can catch the edit.
func rewrite(t *testing.T, c map[string]any, reselect bool) string {
	t.Helper()
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	if reselect {
		value, err := decodeBundleJSON(raw)
		require.NoError(t, err)
		deals := value["deals"].(map[string]interface{})
		for _, e := range value["selection"].([]interface{}) {
			entry := e.(map[string]interface{})
			if tb, ok := deals[entry["deal_id"].(string)]; ok {
				entry["bundle_digest"] = jcsDigest(tb)
			}
		}
		raw, err = json.Marshal(value)
		require.NoError(t, err)
	}
	path := filepath.Join(t.TempDir(), "edited.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func findings(result map[string]any) []string {
	var out []string
	for _, f := range result["findings"].([]any) {
		out = append(out, f.(string))
	}
	return out
}

// Five deals, three picked: one file whose every nested deal is checked as
// its own bundle, VALID, and the store untouched (writeHistory).
func TestDealHistoryVerifiesEachPickedDeal(t *testing.T) {
	deals := historyStore(t)
	picked := []string{deals[0], deals[1], deals[3]}
	path, summary := writeHistory(t, picked...)

	want := append([]string{}, picked...)
	sort.Strings(want)
	var got []string
	for _, d := range summary["deals"].([]any) {
		got = append(got, d.(map[string]any)["deal_id"].(string))
	}
	assert.Equal(t, want, got)

	result, err := verifyBundleOutput(t, path)
	require.NoError(t, err, "VALID exits 0")
	assert.Equal(t, "VALID", result["verdict"])
	assert.Empty(t, result["findings"])
	checked := result["deals"].([]any)
	require.Len(t, checked, 3)
	for i, raw := range checked {
		d := raw.(map[string]any)
		assert.Equal(t, want[i], d["deal_id"])
		assert.Equal(t, "VALID", d["verdict"], "each nested deal verified on its own")
		assert.Equal(t, "VALID", d["report"].(map[string]any)["verdict"], "the nested bundle's own report is carried")
	}

	c := readContainer(t, path)
	assert.Equal(t, dealHistoryKind, c["kind"])
	nested := c["deals"].(map[string]any)
	require.Len(t, nested, 3)
	for _, id := range picked {
		cert := nested[id].(map[string]any)["completeness_certificate"].(map[string]any)
		assert.Equal(t, dealLogID(id), cert["log_id"], "each nested bundle is its own deal's log")
	}
	// The deal with a copy already sealed points at that sealed report; the
	// others carry the section unsealed.
	reported := nested[deals[1]].(map[string]any)["extensions"].(map[string]any)[dealProfile].(map[string]any)
	assert.NotEmpty(t, reported[dealReportPointer])
	assert.NotContains(t, nested[deals[0]].(map[string]any)["extensions"].(map[string]any)[dealProfile], dealReportPointer)
}

// A step sealed after the deal's latest checkpoint is not in the copy (no
// checkpoint is cut for it) and the summary says how many were left out.
func TestDealHistoryStopsAtTheLatestCheckpoint(t *testing.T) {
	deals := historyStore(t)
	dealRun(t, "note", "--deal", deals[3], "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
	state := dealRun(t, "checkpoint", "status", "--deal", deals[3])
	path, summary := writeHistory(t, deals[3])
	line := summary["deals"].([]any)[0].(map[string]any)
	// The deal has no report sealed, so its log holds only its steps; its
	// checkpoint is the one its opening cut.
	assert.Equal(t, float64(1), state["checkpoint"].(map[string]any)["entries"])
	assert.Equal(t, float64(1), line["steps_shown"], "the steps the checkpoint covers")
	assert.Equal(t, state["log_entries"].(float64)-1, line["steps_after_checkpoint"], "every later step, not shown")
	nested := readContainer(t, path)["deals"].(map[string]any)[deals[3]].(map[string]any)
	assert.Len(t, nested["records"], 1)
	result, err := verifyBundleOutput(t, path)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
}

// One record edited inside one nested deal, selection digest updated to
// match: INVALID, naming that deal and no other.
func TestDealHistoryTamperedRecordNamesItsDeal(t *testing.T) {
	deals := historyStore(t)
	path, _ := writeHistory(t, deals[0], deals[1], deals[3])
	c := readContainer(t, path)
	target := deals[1]
	tb := c["deals"].(map[string]any)[target].(map[string]any)
	disclosures := tb["disclosures"].(map[string]any)
	ids := make([]string, 0, len(disclosures))
	for id := range disclosures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	input := disclosures[ids[0]].(map[string]any)["agent_input"].(map[string]any)
	input["deal_id"] = deals[2]

	result, err := verifyBundleOutput(t, rewrite(t, c, true))
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "INVALID", result["verdict"])
	assert.Equal(t, []string{"deal_invalid:" + target}, findings(result))
	for _, raw := range result["deals"].([]any) {
		d := raw.(map[string]any)
		if d["deal_id"] == target {
			assert.Equal(t, "INVALID", d["verdict"])
		} else {
			assert.Equal(t, "VALID", d["verdict"])
		}
	}
}

// Without its selection digest updated, the same edit is also caught as a
// bundle the selection did not name.
func TestDealHistoryEditedBundleIsNotTheSelectedOne(t *testing.T) {
	deals := historyStore(t)
	path, _ := writeHistory(t, deals[0])
	c := readContainer(t, path)
	tb := c["deals"].(map[string]any)[deals[0]].(map[string]any)
	tb["extensions"].(map[string]any)["x-extra"] = map[string]any{}
	result, err := verifyBundleOutput(t, rewrite(t, c, false))
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Contains(t, findings(result), "deal_bundle_not_the_selected_one:"+deals[0])
}

// A deal nobody picked cannot be added: not as a whole bundle beside the
// picked ones, not as a record inside a picked deal's bundle, and not under
// a picked deal's name.
func TestDealHistoryRefusesAnUnpickedDeal(t *testing.T) {
	deals := historyStore(t)
	path, _ := writeHistory(t, deals[0], deals[1], deals[3])
	unpickedPath, _ := writeHistory(t, deals[4])
	unpicked := readContainer(t, unpickedPath)["deals"].(map[string]any)[deals[4]].(map[string]any)

	t.Run("beside the picked deals", func(t *testing.T) {
		c := readContainer(t, path)
		c["deals"].(map[string]any)[deals[4]] = unpicked
		result, err := verifyBundleOutput(t, rewrite(t, c, true))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_in_selection:" + deals[4]}, findings(result))
	})
	t.Run("a record inside a picked deal", func(t *testing.T) {
		c := readContainer(t, path)
		tb := c["deals"].(map[string]any)[deals[0]].(map[string]any)
		record := unpicked["records"].([]any)[0].(map[string]any)
		tb["records"] = append(tb["records"].([]any), record)
		id := record["capsule_id"].(string)
		tb["disclosures"].(map[string]any)[id] = unpicked["disclosures"].(map[string]any)[id]
		result, err := verifyBundleOutput(t, rewrite(t, c, true))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_invalid:" + deals[0]}, findings(result))
	})
	t.Run("under a picked deal's name", func(t *testing.T) {
		c := readContainer(t, path)
		c["deals"].(map[string]any)[deals[0]] = unpicked
		result, err := verifyBundleOutput(t, rewrite(t, c, true))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_of_its_log:" + deals[0]}, findings(result))
	})
	t.Run("a picked deal dropped", func(t *testing.T) {
		c := readContainer(t, path)
		delete(c["deals"].(map[string]any), deals[3])
		result, err := verifyBundleOutput(t, rewrite(t, c, true))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_missing:" + deals[3]}, findings(result))
	})
}

// The builder takes only deals from `deal list`, each once; a sale has its
// own copy.
func TestDealHistoryRefusesWhatIsNotAPickedDeal(t *testing.T) {
	deals := historyStore(t)
	sale, _ := newSale(t)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"unknown":    {[]string{"--deal", "deal-0000000000000000"}, "unknown deal: deal-0000000000000000"},
		"twice":      {[]string{"--deal", deals[0], "--deal", deals[0]}, "is picked twice"},
		"a sale":     {[]string{"--deal", sale}, "a sale has its own copy"},
		"none":       {nil, "--deal (one per picked deal"},
		"not a deal": {[]string{"--deal", "not-a-deal"}, "must be a deal id"},
	} {
		out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "history", "--out", filepath.Join(t.TempDir(), "h.json")}, c.args...)...)
		require.Error(t, err, name)
		assert.Contains(t, out+err.Error(), c.want, name)
	}
}

// A deal whose newest sealed report predates steps under its checkpoint
// (copied, then closed) is refused: its copy would have to name that report,
// whose text shows the deal before the close.
func TestDealHistoryRefusesAStaleSealedReport(t *testing.T) {
	deals := historyStore(t)
	dealRun(t, "close", "--deal", deals[1], "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)
	out, err := invoke(t, "", "--profile", "deal", "deal", "history", "--out", filepath.Join(t.TempDir(), "h.json"), "--deal", deals[1])
	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "newest sealed report predates its later steps")
	assert.Equal(t, before, storeFiles(t, p))
}

// A selection entry with no digest, or one that is not a sha256, is
// malformed, and a carried bundle that cannot be put in canonical form (a
// non-integer number) is never the selected one: neither is VALID.
func TestDealHistorySelectionDigestIsRequired(t *testing.T) {
	deals := historyStore(t)
	path, _ := writeHistory(t, deals[0])
	t.Run("missing", func(t *testing.T) {
		c := readContainer(t, path)
		delete(c["selection"].([]any)[0].(map[string]any), "bundle_digest")
		result, err := verifyBundleOutput(t, rewrite(t, c, false))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings(result), "selection_entry_malformed:"+deals[0])
	})
	t.Run("not canonical", func(t *testing.T) {
		c := readContainer(t, path)
		c["selection"].([]any)[0].(map[string]any)["bundle_digest"] = ""
		c["deals"].(map[string]any)[deals[0]].(map[string]any)["extensions"].(map[string]any)["x-pad"] = 1.5
		result, err := verifyBundleOutput(t, rewrite(t, c, false))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.NotEqual(t, "VALID", result["verdict"])
	})
	t.Run("no selection", func(t *testing.T) {
		c := readContainer(t, path)
		c["selection"] = []any{}
		c["deals"] = map[string]any{}
		result, err := verifyBundleOutput(t, rewrite(t, c, false))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_missing"}, findings(result))
	})
}
