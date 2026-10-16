package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyStoreSecret is the deal store's secret, for tests that derive a
// History's handles themselves.
func historyStoreSecret(t *testing.T) []byte {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	return append([]byte{}, s.secret...)
}

// dealCounterparty is the counterparty `deal list` names for a deal.
func dealCounterparty(t *testing.T, dealID string) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	listed, err := s.dealListings(t.Context())
	require.NoError(t, err)
	for _, l := range listed {
		if l.DealID == dealID {
			return l.Summary.Counterparty
		}
	}
	t.Fatalf("deal %s not listed", dealID)
	return ""
}

// sealedHandles are the handles a History's sealed selection names, by deal.
func sealedHandles(t *testing.T, path string) map[string]string {
	t.Helper()
	b := readHistory(t, path)
	sealed := b["disclosures"].(map[string]any)[historyExt(b)["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
	out := map[string]string{}
	for _, raw := range sealed["selection"].([]any) {
		e := raw.(map[string]any)
		if h, ok := e["handle"].(string); ok {
			out[e["deal_id"].(string)] = h
		}
	}
	return out
}

// resolveHandle runs `deal history resolve` and returns its output.
func resolveHandle(t *testing.T, path, handle string) (map[string]any, string, error) {
	t.Helper()
	out, err := invoke(t, "", "--profile", "deal", "deal", "history", "resolve", "--history", path, "--handle", handle)
	if err != nil {
		return nil, out, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, out, nil
}

// nestedCapsuleIDs are the capsule ids of a History's nested deal's records.
func nestedCapsuleIDs(t *testing.T, path, dealID string) []string {
	t.Helper()
	tb := historyExt(readHistory(t, path))["deals"].(map[string]any)[dealID].(map[string]any)
	var ids []string
	for _, raw := range tb["records"].([]any) {
		ids = append(ids, raw.(map[string]any)["capsule_id"].(string))
	}
	return ids
}

// T3, the linkage check: two Histories of the same deal, for two
// recipients, share no handle, and neither file holds the other's; the
// holder maps each handle back to the deal, who the History was for and the
// counterparty; a handle quoted against the other History maps to nothing.
// What does link the two (the deal id in each copy's log and the records'
// capsule ids) is named by verify, and is so.
func TestDealHistoryHandlesArePerShare(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	toB := h.closed
	toC, _ := writeHistory(t, d.all(), []string{"--to", "buyer C"}, d.closed)
	hb, hc := sealedHandles(t, toB)[d.closed], sealedHandles(t, toC)[d.closed]
	require.Regexp(t, `^dh-[0-9a-f]{32}$`, hb)
	require.Regexp(t, `^dh-[0-9a-f]{32}$`, hc)
	assert.NotEqual(t, hb, hc, "one deal, two shares, two handles")
	assert.NotContains(t, string(mustRead(t, toB)), hc, "B's file holds nothing of C's handle")
	assert.NotContains(t, string(mustRead(t, toC)), hb, "C's file holds nothing of B's handle")
	assert.NotEqual(t, hb, sealedHandles(t, h.picked)[d.closed], "per share, not per recipient: two Histories for buyer B differ too")

	counterparty := dealCounterparty(t, d.closed)
	require.Equal(t, "Example Stickers", counterparty, "the demo deal's who.name, so the mapping is not \"not recorded\" to itself")
	for _, c := range []struct{ path, handle, to string }{{toB, hb, "buyer B"}, {toC, hc, "buyer C"}} {
		got, out, err := resolveHandle(t, c.path, c.handle)
		require.NoError(t, err, out)
		assert.Equal(t, c.handle, got["handle"])
		assert.Equal(t, d.closed, got["deal_id"], "the holder maps the handle back")
		assert.Equal(t, c.to, got["recipient"], "and knows which History it came from")
		assert.Equal(t, counterparty, got["counterparty"])
	}
	for _, c := range []struct{ path, handle string }{{toC, hb}, {toB, hc}} {
		_, out, err := resolveHandle(t, c.path, c.handle)
		require.Error(t, err)
		assert.Contains(t, out+err.Error(), "is not one of this History's deals", "a handle names a deal only in its own History")
	}

	for _, path := range []string{toB, toC} {
		verdict, entry, findings, err := verifyHistory(t, path)
		require.NoError(t, err, "%v", findings)
		assert.Equal(t, "VALID", verdict)
		assert.Equal(t, []any{"deal_id", "capsule_ids"}, entry["linkable_across_shares"])
	}
	assert.Equal(t, nestedCapsuleIDs(t, toB, d.closed), nestedCapsuleIDs(t, toC, d.closed), "the stated limit holds: the same capsule ids in both")
	for _, path := range []string{toB, toC} {
		tb := historyExt(readHistory(t, path))["deals"].(map[string]any)[d.closed].(map[string]any)
		assert.Equal(t, dealLogID(d.closed), tb["completeness_certificate"].(map[string]any)["log_id"], "and the same deal id in each copy's log")
	}
}

// A handle is optional: a History with none (as written before handles)
// verifies and says so. One that is there must be dh- and 32 lowercase hex,
// once in the file, in a history-audience entry; verify cannot recompute it,
// so a well-formed handle the holder sealed is not checked (holder_stated).
func TestDealHistoryChecksItsHandles(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	picked := readHistory(t, h.picked)
	members := historyExt(picked)["deals"].(map[string]any)
	entryFor := func(id string) map[string]any {
		return map[string]any{"deal_id": id, "bundle_digest": jcsDigest(members[id].(map[string]any)), "steps_after_checkpoint": 0}
	}
	closed := map[string]any{d.closed: members[d.closed]}

	t.Run("none, as before handles", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		verdict, entry, findings, err := verifyHistory(t, sealHistory(t, dealAudienceHistory, "buyer B", []any{entryFor(d.closed)}, closed))
		require.NoError(t, err, "%v", findings)
		assert.Equal(t, "VALID", verdict, "an absent handle is not a finding")
		assert.Equal(t, "none", entry["handles"])
		assert.NotContains(t, entry["deals"].([]any)[0], "handle")
		assert.Equal(t, []any{"deal_id", "capsule_ids"}, entry["linkable_across_shares"], "the limit is stated with or without handles")
	})
	t.Run("malformed", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		for name, handle := range map[string]any{
			"upper case": "dh-" + strings.Repeat("A", 32),
			"short":      "dh-" + strings.Repeat("a", 31),
			"no prefix":  strings.Repeat("a", 35),
			"a deal id":  d.closed,
			"empty":      "",
			"a number":   7,
		} {
			e := entryFor(d.closed)
			e["handle"] = handle
			_, entry, findings, err := verifyHistory(t, sealHistory(t, dealAudienceHistory, "buyer B", []any{e}, closed))
			require.ErrorIs(t, err, ErrBundleInvalid, name)
			assert.Equal(t, []string{"selection_entry_malformed:" + d.closed, "deal_not_in_selection:" + d.closed}, findings, name)
			assert.NotContains(t, entry, "handles", "%s: a malformed handle is not none", name)
		}
	})
	t.Run("in a keep entry", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		keep := historyExt(readHistory(t, h.closedKeep))["deals"].(map[string]any)[d.closed].(map[string]any)
		e := map[string]any{"deal_id": d.closed, "bundle_digest": jcsDigest(keep), "steps_after_checkpoint": 0, "handle": "dh-" + strings.Repeat("a", 32)}
		_, _, findings, err := verifyHistory(t, sealHistory(t, dealAudienceKeep, "", []any{e}, map[string]any{d.closed: keep}))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_entry_malformed:" + d.closed, "deal_not_in_selection:" + d.closed}, findings)
	})
	t.Run("one handle for two deals", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		first, second := entryFor(d.closed), entryFor(d.floor)
		first["handle"], second["handle"] = "dh-"+strings.Repeat("b", 32), "dh-"+strings.Repeat("b", 32)
		_, _, findings, err := verifyHistory(t, sealHistory(t, dealAudienceHistory, "buyer B", []any{first, second},
			map[string]any{d.closed: members[d.closed], d.floor: members[d.floor]}))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_entry_malformed:" + d.floor, "deal_not_in_selection:" + d.floor}, findings)
	})
	t.Run("on some deals and not others", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		first, second := entryFor(d.closed), entryFor(d.floor)
		first["handle"] = "dh-" + strings.Repeat("d", 32)
		_, entry, findings, err := verifyHistory(t, sealHistory(t, dealAudienceHistory, "buyer B", []any{first, second},
			map[string]any{d.closed: members[d.closed], d.floor: members[d.floor]}))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_entry_malformed:" + d.floor}, findings, "all or none: the deal without one is named")
		assert.NotContains(t, entry, "handles")
	})
	t.Run("well formed, not derived: holder stated", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		e := entryFor(d.closed)
		e["handle"] = "dh-" + strings.Repeat("c", 32)
		verdict, entry, findings, err := verifyHistory(t, sealHistory(t, dealAudienceHistory, "buyer B", []any{e}, closed))
		require.NoError(t, err, "%v", findings)
		assert.Equal(t, "VALID", verdict, "verify has no key to recompute a handle with")
		assert.Equal(t, e["handle"], entry["deals"].([]any)[0].(map[string]any)["handle"])
		assert.Contains(t, entry["holder_stated"], "handle")
		assert.NotContains(t, entry, "handles")
	})
}

// resolve maps only a handle this store derived for the History it is
// quoted against, from a History with handles that this store wrote; what it
// reports is the store's own record, not the file's; and it changes no byte
// of the store, on a read_only profile too.
func TestDealHistoryResolveRefuses(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	shareKey := historyShareKey(historyStoreSecret(t), hex.EncodeToString(nonce))
	derived := historyHandle(shareKey, d.closed)
	other := "dh-" + strings.Repeat("c", 32)
	// The holder's file says d.closed is "other": edited, or another store's.
	edited := sealHistoryWithNonce(t, nonce, dealAudienceHistory, "buyer B",
		[]any{map[string]any{"deal_id": d.closed, "bundle_digest": strings.Repeat("0", 64), "steps_after_checkpoint": 0, "handle": other}}, map[string]any{})
	unknown := "deal-0000000000000000"
	notOurs := sealHistoryWithNonce(t, nonce, dealAudienceHistory, "buyer B",
		[]any{map[string]any{"deal_id": unknown, "bundle_digest": strings.Repeat("0", 64), "steps_after_checkpoint": 0, "handle": historyHandle(shareKey, unknown)}}, map[string]any{})
	deal := filepath.Join(t.TempDir(), "deal.json")
	raw, err := json.Marshal(historyExt(readHistory(t, h.closed))["deals"].(map[string]any)[d.closed])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(deal, raw, 0o600))
	elsewhere := readHistory(t, h.closed)
	elsewhere["completeness_certificate"].(map[string]any)["log_id"] = historyLogPrefix + strings.Repeat("e", 64)
	notWritten := rewrite(t, elsewhere)
	otherRecord := readHistory(t, h.closed)
	historyExt(otherRecord)["selection_record"] = strings.Repeat("a", 64)
	recordNotOnLog := rewrite(t, otherRecord)
	beforeHandles := sealHistory(t, dealAudienceHistory, "buyer B",
		[]any{map[string]any{"deal_id": d.closed, "bundle_digest": strings.Repeat("0", 64), "steps_after_checkpoint": 0}}, map[string]any{})
	p, err := loadProfile("deal")
	require.NoError(t, err)
	before := storeFiles(t, p)

	for name, c := range map[string]struct{ path, handle, want string }{
		"another handle in the file":  {edited, derived, "but the History's sealed selection names that deal under another handle"},
		"the file's own, not derived": {edited, other, "is not one of this History's deals"},
		"a deal not in this store":    {notOurs, historyHandle(shareKey, unknown), "which is not in this store"},
		"a keep copy":                 {h.closedKeep, derived, "this History has no handles"},
		"from before handles":         {beforeHandles, derived, "this History has no handles"},
		"not a History":               {deal, derived, "--history is not a History"},
		"not written by this store":   {notWritten, sealedHandles(t, h.closed)[d.closed], "is not a History this store wrote"},
		"a record not on its log":     {recordNotOnLog, sealedHandles(t, h.closed)[d.closed], "its selection record is not on its log here"},
		"not a handle":                {h.closed, d.closed, "--handle must be a handle"},
		"no handle":                   {h.closed, " ", "are required"},
	} {
		_, out, err := resolveHandle(t, c.path, c.handle)
		require.Error(t, err, name)
		assert.Contains(t, out+err.Error(), c.want, name)
	}
	assert.Equal(t, before, storeFiles(t, p), "a refusal changes no file under the store")

	// The file's recipient edited: resolve still reports the store's.
	b := readHistory(t, h.closed)
	sealed := b["disclosures"].(map[string]any)[historyExt(b)["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
	sealed["recipient"] = "buyer Z"
	p.ReadOnly = true
	require.NoError(t, saveProfile(p, true))
	got, out, err := resolveHandle(t, rewrite(t, b), sealedHandles(t, h.closed)[d.closed])
	require.NoError(t, err, "a read_only profile resolves: %s", out)
	assert.Equal(t, "buyer B", got["recipient"], "from the store's record, not the file's")
	assert.Equal(t, d.closed, got["deal_id"])
	assert.Equal(t, before, storeFiles(t, p), "resolve changes no file under the store")
}
