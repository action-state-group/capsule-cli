package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyDeals are the five deals historyStore makes:
//   - closed: a retail deal paid and closed;
//   - private: a deal whose baseline holds a home address, a card, a code and
//     the shop's contact details, with a counterparty copy and then the
//     user's own copy sealed (each seals its report and cuts a checkpoint);
//   - floor: a seller's deal with a price floor;
//   - opened: a deal only opened;
//   - other: one more retail deal.
type historyDeals struct{ closed, private, floor, opened, other string }

func historyStore(t *testing.T) historyDeals {
	t.Helper()
	dealFixture(t)
	var d historyDeals
	d.closed = retailDeal(t)
	dealRun(t, "close", "--deal", d.closed, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	d.private = openPrivateDeal(t)
	sharedCopy(t, d.private, dealAudienceCounterparty, "the shop")
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", d.private, "--out", filepath.Join(t.TempDir(), "own.json"))
	require.NoError(t, err, out)
	d.floor = dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerWithFloor))["deal_id"].(string)
	d.opened = dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	d.other = retailDeal(t)
	return d
}

func (d historyDeals) all() []string {
	return []string{d.closed, d.private, d.floor, d.opened, d.other}
}

// dealLogState is everything the store holds for a deal's own logs (its log
// and its disclosures log): every row of every table with a log_id column
// for those logs, and the deal's rows in every table with a deal_id column.
func dealLogState(t *testing.T, dealID string) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	db, err := sql.Open("sqlite", "file:"+p.Connection.Database+"?mode=ro")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	require.NoError(t, err)
	var names []string
	for tables.Next() {
		var n string
		require.NoError(t, tables.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, tables.Close())
	h := sha256.New()
	rows := 0
	dump := func(table, column string, values ...any) {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		r, err := db.Query(fmt.Sprintf(`SELECT * FROM %q WHERE %q IN (%s) ORDER BY rowid`, table, column, marks), values...)
		require.NoError(t, err)
		cols, err := r.Columns()
		require.NoError(t, err)
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, r.Scan(ptrs...))
			_, _ = fmt.Fprintf(h, "%s%v\n", table, vals)
			rows++
		}
		require.NoError(t, r.Close())
	}
	for _, table := range names {
		info, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
		require.NoError(t, err)
		var hasLog, hasDeal bool
		for info.Next() {
			var cid, notnull, pk int
			var name, typ string
			var dflt any
			require.NoError(t, info.Scan(&cid, &name, &typ, &notnull, &dflt, &pk))
			hasLog = hasLog || name == "log_id"
			hasDeal = hasDeal || name == "deal_id"
		}
		require.NoError(t, info.Close())
		if hasLog {
			dump(table, "log_id", dealLogID(dealID), dealDisclosureLogID(dealID))
		}
		if hasDeal {
			dump(table, "deal_id", dealID)
		}
	}
	require.NotZero(t, rows, "the deal's logs are in the store")
	return hex.EncodeToString(h.Sum(nil))
}

func logStates(t *testing.T, ids ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, id := range ids {
		out[id] = dealLogState(t, id)
	}
	return out
}

// writeHistory runs `deal history` over picked (with args, e.g. --audience),
// checks it changed nothing of any of the deals in all, and returns the
// file's path and the command's output.
func writeHistory(t *testing.T, all []string, args []string, picked ...string) (string, map[string]any) {
	t.Helper()
	before := logStates(t, all...)
	path := filepath.Join(t.TempDir(), "history.json")
	cmd := append([]string{"--profile", "deal", "deal", "history", "--out", path}, args...)
	for _, id := range picked {
		cmd = append(cmd, "--deal", id)
	}
	out, err := invoke(t, "", cmd...)
	require.NoError(t, err, out)
	assert.Equal(t, before, logStates(t, all...), "a History writes no deal's log: no record sealed, no checkpoint cut, no share recorded")
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &summary), out)
	return path, summary
}

func readHistory(t *testing.T, path string) map[string]any {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, path), &b))
	return b
}

func historyExt(b map[string]any) map[string]any {
	return b["extensions"].(map[string]any)[historySelectionExtension].(map[string]any)
}

// rewrite writes b back to a new file.
func rewrite(t *testing.T, b map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "edited.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// verifyHistory runs verify --bundle and returns the verdict, the History
// extension's entry and its findings.
func verifyHistory(t *testing.T, path string) (string, map[string]any, []string, error) {
	t.Helper()
	result, err := verifyBundleOutput(t, path)
	for _, raw := range result["extensions"].([]any) {
		e := raw.(map[string]any)
		if e["kind"] == historySelectionExtension {
			var findings []string
			for _, f := range e["findings"].([]any) {
				findings = append(findings, f.(string))
			}
			return result["verdict"].(string), e, findings, err
		}
	}
	t.Fatalf("no %s entry in %v", historySelectionExtension, result)
	return "", nil, nil, err
}

// Five deals, three picked: the History is the counterparty's copy of each,
// every nested deal checked as its own bundle, one signer key, no deal's log
// written; the pick is sealed on the history log.
func TestDealHistoryVerifiesEachPickedDeal(t *testing.T) {
	d := historyStore(t)
	picked := []string{d.closed, d.private, d.floor}
	path, summary := writeHistory(t, d.all(), nil, picked...)
	assert.Equal(t, dealAudienceCounterparty, summary["audience"], "the other party's copy unless asked otherwise")

	want := append([]string{}, picked...)
	sort.Strings(want)
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "VALID exits 0: %v", findings)
	assert.Equal(t, "VALID", verdict)
	assert.Empty(t, findings)
	assert.Equal(t, dealAudienceCounterparty, entry["audience"])
	holder := entry["holder_key"].(string)
	require.Len(t, holder, 64)
	assert.Equal(t, map[string]any{holder: []any{want[0], want[1], want[2]}}, entry["signer_keys"], "every nested record is signed by the selection's key")
	checked := entry["deals"].([]any)
	require.Len(t, checked, 3)
	for i, raw := range checked {
		m := raw.(map[string]any)
		assert.Equal(t, want[i], m["deal_id"])
		assert.Equal(t, "VALID", m["verdict"], "a counterparty copy verifies VALID on its own")
		assert.Equal(t, dealAudienceCounterparty, m["audience"])
	}

	b := readHistory(t, path)
	assert.Equal(t, "evidence-bundle/v2", b["bundle_kind"])
	assert.Equal(t, historyLogID, b["completeness_certificate"].(map[string]any)["log_id"])
	ext := historyExt(b)
	nested := ext["deals"].(map[string]any)
	require.Len(t, nested, 3)
	for _, id := range picked {
		cert := nested[id].(map[string]any)["completeness_certificate"].(map[string]any)
		assert.Equal(t, dealLogID(id), cert["log_id"], "each nested bundle is its own deal's log")
	}
	// The sealed selection record is disclosed and is what the file carries.
	sealed := b["disclosures"].(map[string]any)[ext["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
	assert.Equal(t, "history_selection", sealed["type"])
	assert.Equal(t, dealAudienceCounterparty, sealed["audience"])
	assert.Equal(t, ext["selection"], sealed["selection"])

	// A second History withholds the first one's selection.
	path2, _ := writeHistory(t, d.all(), nil, d.other)
	b2 := readHistory(t, path2)
	assert.Len(t, b2["records"], 2, "both selections are on the history log")
	earlier, _ := b2["disclosures"].(map[string]any)[ext["selection_record"].(string)].(map[string]any)
	assert.NotContains(t, earlier, "agent_input", "the earlier selection is withheld")
	verdict, _, findings, err = verifyHistory(t, path2)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
}

// The private values in the picked deals: none is anywhere in the file a
// person sends; all are in the user's own (keep) History.
func TestDealHistoryCarriesNoPrivateValue(t *testing.T) {
	d := historyStore(t)
	picked := []string{d.private, d.floor, d.closed}
	hidden := append([]string{homeAddress, "Larkspur", cardNumber, verifyCode, shopEmail, shopPhone,
		"170000", "min_total_minor", "bounds_openings"}, writtenAmounts(1700)...)

	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		var args []string
		if audience != dealAudienceCounterparty {
			args = []string{"--audience", audience}
		}
		if audience == dealAudienceAdjudicator {
			// The private deal's sealed reports are for the counterparty and
			// the user: an adjudicator's copy needs its own first.
			sharedCopy(t, d.private, dealAudienceAdjudicator, "the card issuer")
		}
		path, _ := writeHistory(t, d.all(), args, picked...)
		raw := string(mustRead(t, path))
		for _, v := range hidden {
			assert.NotContains(t, raw, v, audience+": the whole file")
		}
		verdict, _, findings, err := verifyHistory(t, path)
		require.NoError(t, err, "%v", findings)
		assert.Equal(t, "VALID", verdict, audience)
	}

	// A keep copy needs the user's own report to be the newest one.
	out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", d.private, "--out", filepath.Join(t.TempDir(), "own.json"))
	require.NoError(t, err, out)
	path, summary := writeHistory(t, d.all(), []string{"--audience", "keep"}, picked...)
	assert.Equal(t, dealAudienceKeep, summary["audience"])
	raw := string(mustRead(t, path))
	assert.Contains(t, raw, "Larkspur Lane", "your own copy carries the address")
	assert.Contains(t, raw, "min_total_minor", "and the floor")
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
	assert.Equal(t, dealAudienceKeep, entry["audience"], "verify says the file is the keep copy")
	for _, m := range entry["deals"].([]any) {
		assert.Equal(t, dealAudienceKeep, m.(map[string]any)["audience"])
	}
}

// A step sealed after the deal's latest checkpoint is not in the copy (no
// checkpoint is cut on the deal for it) and the output counts it.
func TestDealHistoryStopsAtTheLatestCheckpoint(t *testing.T) {
	d := historyStore(t)
	dealRun(t, "check", "--deal", d.opened, "--input", filepath.Join(retailDemo, "check-pay.json"))
	state := dealRun(t, "checkpoint", "status", "--deal", d.opened)
	path, summary := writeHistory(t, d.all(), nil, d.opened)
	line := summary["deals"].([]any)[0].(map[string]any)
	assert.Equal(t, float64(1), state["checkpoint"].(map[string]any)["entries"])
	assert.Equal(t, float64(1), line["steps_shown"], "the steps the checkpoint covers")
	assert.Equal(t, state["log_entries"].(float64)-1, line["steps_after_checkpoint"], "every later step, not shown")
	verdict, _, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
}

// One record edited inside one nested deal: INVALID, naming that deal and no
// other.
func TestDealHistoryTamperedRecordNamesItsDeal(t *testing.T) {
	d := historyStore(t)
	path, _ := writeHistory(t, d.all(), nil, d.closed, d.private, d.floor)
	b := readHistory(t, path)
	tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
	disclosures := tb["disclosures"].(map[string]any)
	ids := make([]string, 0, len(disclosures))
	for id, raw := range disclosures {
		if _, ok := raw.(map[string]any)["agent_input"]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	require.NotEmpty(t, ids)
	disclosures[ids[0]].(map[string]any)["agent_input"].(map[string]any)["deal_id"] = d.other

	verdict, entry, findings, err := verifyHistory(t, rewrite(t, b))
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "INVALID", verdict)
	assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_invalid:" + d.closed}, findings)
	for _, raw := range entry["deals"].([]any) {
		m := raw.(map[string]any)
		if m["deal_id"] == d.closed {
			assert.Equal(t, "INVALID", m["verdict"])
		} else {
			assert.Equal(t, "VALID", m["verdict"])
		}
	}
}

// A deal nobody picked cannot be added: not beside the picked ones, not as
// a record inside a picked deal's bundle, not under a picked deal's name; a
// picked deal cannot be dropped; and neither the carried nor the sealed
// selection can be edited to take one in.
func TestDealHistoryRefusesAnUnpickedDeal(t *testing.T) {
	d := historyStore(t)
	path, _ := writeHistory(t, d.all(), nil, d.closed, d.private, d.floor)
	otherPath, _ := writeHistory(t, d.all(), nil, d.other)
	unpicked := historyExt(readHistory(t, otherPath))["deals"].(map[string]any)[d.other].(map[string]any)

	t.Run("beside the picked deals", func(t *testing.T) {
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.other] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_in_selection:" + d.other}, findings)
	})
	t.Run("a record inside a picked deal", func(t *testing.T) {
		b := readHistory(t, path)
		tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
		tb["records"] = append(tb["records"].([]any), unpicked["records"].([]any)[0])
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_invalid:" + d.closed}, findings)
	})
	t.Run("under a picked deal's name", func(t *testing.T) {
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.closed] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_not_of_its_log:" + d.closed}, findings)
	})
	t.Run("a picked deal dropped", func(t *testing.T) {
		b := readHistory(t, path)
		delete(historyExt(b)["deals"].(map[string]any), d.floor)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_missing:" + d.floor}, findings)
	})
	t.Run("the carried selection edited", func(t *testing.T) {
		b := readHistory(t, path)
		ext := historyExt(b)
		ext["selection"] = append(ext["selection"].([]any), map[string]any{"deal_id": d.other, "bundle_digest": jcsDigest(unpicked)})
		ext["deals"].(map[string]any)[d.other] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_not_sealed", "deal_not_in_selection:" + d.other}, findings)
	})
	t.Run("the sealed selection edited", func(t *testing.T) {
		b := readHistory(t, path)
		ext := historyExt(b)
		sealed := b["disclosures"].(map[string]any)[ext["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
		sealed["selection"] = append(sealed["selection"].([]any), map[string]any{"deal_id": d.other, "bundle_digest": jcsDigest(unpicked)})
		ext["selection"] = sealed["selection"]
		ext["deals"].(map[string]any)[d.other] = unpicked
		verdict, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, "INVALID", verdict)
		assert.Equal(t, []string{"selection_record_unverified"}, findings)
	})
}

// The owner's copy of a deal swapped into a counterparty History is named.
func TestDealHistoryNamesACopyForAnotherAudience(t *testing.T) {
	d := historyStore(t)
	path, _ := writeHistory(t, d.all(), nil, d.closed)
	keepPath, _ := writeHistory(t, d.all(), []string{"--audience", "keep"}, d.closed)
	keep := historyExt(readHistory(t, keepPath))["deals"].(map[string]any)[d.closed]
	b := readHistory(t, path)
	historyExt(b)["deals"].(map[string]any)[d.closed] = keep
	_, entry, findings, err := verifyHistory(t, rewrite(t, b))
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_audience_not_selected:" + d.closed}, findings)
	assert.Equal(t, dealAudienceKeep, entry["deals"].([]any)[0].(map[string]any)["audience"])
}

// rotateSigningKey gives the deal profile a new producer key, trusting both.
func rotateSigningKey(t *testing.T) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	seed := make([]byte, ed25519.SeedSize)
	_, err = rand.Read(seed)
	require.NoError(t, err)
	public := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	p.Signing = Secret{Value: hex.EncodeToString(seed)}
	p.TrustedKeys = append(p.TrustedKeys, public)
	require.NoError(t, saveProfile(p, true))
	return public
}

// Deals signed by two keys in one History FAIL, naming each key's deals:
// one person whose key changed between deals included (a known limit).
func TestDealHistoryFailsDealsSignedByTwoKeys(t *testing.T) {
	d := historyStore(t)
	newKey := rotateSigningKey(t)
	after := retailDeal(t)
	path, _ := writeHistory(t, append(d.all(), after), nil, d.closed, after)
	verdict, entry, findings, err := verifyHistory(t, path)
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "INVALID", verdict)
	assert.Equal(t, newKey, entry["holder_key"], "the selection is signed by the key in use now")
	signers := entry["signer_keys"].(map[string]any)
	require.Len(t, signers, 2, "both keys named")
	assert.Equal(t, []any{after}, signers[newKey])
	for key, ids := range signers {
		if key != newKey {
			assert.Equal(t, []any{d.closed}, ids)
		}
	}
	assert.ElementsMatch(t, []string{"deal_signed_by_other_key:" + d.closed, "deal_signers_differ"}, findings)
}

// A deal whose sealed reports include no current one for the History's
// audience is refused: its copy would have to name a report written for
// another reader, or before its later steps.
func TestDealHistoryRefusesAReportNotForThisCopy(t *testing.T) {
	d := historyStore(t)
	for name, args := range map[string][]string{
		"another audience": {"--audience", "adjudicator"},
		"keep, not newest": {"--audience", "keep"},
	} {
		if name == "keep, not newest" {
			// A step after the user's own report: the newest keep report no
			// longer covers every step (a counterparty one does).
			sharedCopy(t, d.private, dealAudienceCounterparty, "the shop")
			dealRun(t, "note", "--deal", d.private, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","channel":"web","text":"shipped"}`))
			sharedCopy(t, d.private, dealAudienceCounterparty, "the shop")
		}
		before := logStates(t, d.all()...)
		out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "history", "--out", filepath.Join(t.TempDir(), "h.json"), "--deal", d.private}, args...)...)
		require.Error(t, err, name)
		assert.Contains(t, out+err.Error(), "has a sealed report but none for", name)
		assert.Equal(t, before, logStates(t, d.all()...), name)
	}
}

// The builder takes only deals from `deal list`, each once, and an audience
// `deal report --share` knows; a sale has its own copy.
func TestDealHistoryRefusesWhatIsNotAPickedDeal(t *testing.T) {
	d := historyStore(t)
	sale, _ := newSale(t)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"unknown":    {[]string{"--deal", "deal-0000000000000000"}, "unknown deal: deal-0000000000000000"},
		"twice":      {[]string{"--deal", d.closed, "--deal", d.closed}, "is picked twice"},
		"a sale":     {[]string{"--deal", sale}, "a sale has its own copy"},
		"none":       {nil, "--deal (one per picked deal"},
		"not a deal": {[]string{"--deal", "not-a-deal"}, "must be a deal id"},
		"audience":   {[]string{"--deal", d.closed, "--audience", "everyone"}, "--audience must be counterparty"},
	} {
		out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "history", "--out", filepath.Join(t.TempDir(), "h.json")}, c.args...)...)
		require.Error(t, err, name)
		assert.Contains(t, out+err.Error(), c.want, name)
	}
}

// The deal view never presents a History: it forbids the extension.
func TestDealViewForbidsAHistory(t *testing.T) {
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(dealViewManifestJSON, &m))
	assert.True(t, manifestSet(manifestMap(m, "forbids")["extensions"])[historySelectionExtension])
}

// sealHistory writes a History the holder seals over any selection and deals
// (as a holder misusing their own key could), through the builder's own
// sealing.
func sealHistory(t *testing.T, audience string, selection []any, deals map[string]any) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	sel := make([]interface{}, len(selection))
	copy(sel, selection)
	b, id, err := s.historyBundle(t.Context(), audience, sel)
	require.NoError(t, err)
	b["extensions"] = map[string]interface{}{historySelectionExtension: map[string]interface{}{"selection_record": id, "selection": sel, "deals": deals}}
	return rewrite(t, b)
}

// A deal with no sealed report is carried with its section rewritten for the
// audience, as `deal report --share` writes it: none of the private values
// in its baseline, nor the user's own words, is in the file.
func TestDealHistoryInlineSharedCopyCarriesNoPrivateValue(t *testing.T) {
	d := historyStore(t)
	fresh := openPrivateDeal(t)
	path, _ := writeHistory(t, append(d.all(), fresh), nil, fresh)
	b := readHistory(t, path)
	section := historyExt(b)["deals"].(map[string]any)[fresh].(map[string]any)["extensions"].(map[string]any)[dealProfile].(map[string]any)
	assert.NotContains(t, section, dealReportPointer, "no report sealed: the section rides rewritten")
	assert.Equal(t, dealAudienceCounterparty, section["audience"])
	raw := string(mustRead(t, path))
	for _, v := range []string{homeAddress, "Larkspur", cardNumber, verifyCode, shopEmail, shopPhone, "Buy the sticker", "texted me"} {
		assert.NotContains(t, raw, v)
	}
	verdict, _, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
}

// What the History's own records and members must be: the selection signed
// and the bundle's root on the history log; each member a bundle whose
// every record is signed; each selection entry well formed; each member the
// copy for the sealed audience, even when the holder sealed its digest.
func TestDealHistoryChecksItsOwnParts(t *testing.T) {
	d := historyStore(t)
	path, _ := writeHistory(t, d.all(), nil, d.closed)
	keepPath, _ := writeHistory(t, d.all(), []string{"--audience", "keep"}, d.closed)
	keep := historyExt(readHistory(t, keepPath))["deals"].(map[string]any)[d.closed].(map[string]any)

	t.Run("selection unsigned", func(t *testing.T) {
		b := readHistory(t, path)
		id := historyExt(b)["selection_record"].(string)
		for _, raw := range b["records"].([]any) {
			if r := raw.(map[string]any); r["capsule_id"] == id {
				delete(r, "signature")
				delete(r, "key_id")
			}
		}
		_, _, findings, _ := verifyHistory(t, rewrite(t, b))
		assert.Contains(t, findings, "selection_record_unsigned")
	})
	t.Run("selection not the root", func(t *testing.T) {
		b := readHistory(t, path)
		b["root"] = strings.Repeat("0", 64)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "selection_record_not_root")
	})
	t.Run("not on the history log", func(t *testing.T) {
		b := readHistory(t, path)
		b["completeness_certificate"].(map[string]any)["log_id"] = dealLogID(d.closed)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "not_a_history_log")
	})
	t.Run("a member's inline report edited", func(t *testing.T) {
		// The CHANGELOG's known limit: the nested deal stays VALID, and only
		// the History's bundle_digest catches the edit.
		b := readHistory(t, path)
		tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
		section := tb["extensions"].(map[string]any)[dealProfile].(map[string]any)
		require.NotContains(t, section, dealReportPointer, "an inline report")
		section["scope"] = "edited"
		_, entry, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_bundle_not_the_selected_one:" + d.closed}, findings)
		assert.Equal(t, "VALID", entry["deals"].([]any)[0].(map[string]any)["verdict"])
	})
	t.Run("a member record unsigned", func(t *testing.T) {
		b := readHistory(t, path)
		r := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)["records"].([]any)[0].(map[string]any)
		delete(r, "signature")
		delete(r, "key_id")
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "deal_record_unsigned:"+d.closed)
	})
	t.Run("a member not a bundle", func(t *testing.T) {
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.closed] = map[string]any{"bundle_kind": "something-else"}
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_a_bundle:" + d.closed}, findings)
	})
	t.Run("a selection entry malformed, sealed by the holder", func(t *testing.T) {
		member := historyExt(readHistory(t, path))["deals"].(map[string]any)[d.closed]
		edited := sealHistory(t, dealAudienceCounterparty, []any{map[string]any{"deal_id": d.closed, "bundle_digest": "not-a-digest"}}, map[string]any{d.closed: member})
		_, _, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_entry_malformed:" + d.closed, "deal_not_in_selection:" + d.closed}, findings)
	})
	t.Run("the keep copy, its digest sealed under counterparty", func(t *testing.T) {
		edited := sealHistory(t, dealAudienceCounterparty, []any{map[string]any{"deal_id": d.closed, "bundle_digest": jcsDigest(keep)}}, map[string]any{d.closed: keep})
		_, _, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_audience_not_selected:" + d.closed}, findings)
	})
}
