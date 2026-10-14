package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// storeTemplate is a store built once per package run and copied into
// each test that uses it: under the race detector, making the deals and
// writing Histories of them was most of each History test's time. Each test
// gets its own copy, which it may write to or tamper with.
type storeTemplate struct {
	once  sync.Once
	dir   string // config/ (the XDG config home), deal/ (the profile's store) and the build's own files
	ready bool
}

// use builds the template on first use: build runs in the calling test's
// environment and may keep files under the template's directory, then the
// template keeps that environment's config home and deal store. It then
// points the test at its own copy and returns the copy's directory.
func (s *storeTemplate) use(t *testing.T, build func(t *testing.T, dir string)) string {
	t.Helper()
	s.once.Do(func() {
		var err error
		s.dir, err = os.MkdirTemp("", "capsule-history-store-")
		require.NoError(t, err)
		build(t, s.dir)
		p, err := loadProfile("deal")
		require.NoError(t, err)
		store := filepath.Dir(p.Connection.Database)
		copyTree(t, os.Getenv("XDG_CONFIG_HOME"), filepath.Join(s.dir, "config"))
		copyTree(t, store, filepath.Join(s.dir, "deal"))
		pointProfile(t, filepath.Join(s.dir, "config"), store, filepath.Join(s.dir, "deal"))
		s.ready = true
	})
	require.True(t, s.ready, "the History store failed to build in an earlier test")
	dir := t.TempDir()
	copyTree(t, s.dir, dir)
	pointProfile(t, filepath.Join(dir, "config"), filepath.Join(s.dir, "deal"), filepath.Join(dir, "deal"))
	dealTestEnv(t, filepath.Join(dir, "config"))
	return dir
}

var (
	dealsTemplate, historiesTemplate storeTemplate
	templateDeals                    historyDeals
	templatePickedSummary            map[string]any
)

func historyStore(t *testing.T) historyDeals {
	t.Helper()
	dealsTemplate.use(t, func(t *testing.T, _ string) { templateDeals = makeHistoryDeals(t) })
	return templateDeals
}

// sentTo is who a History sent in these tests is for (deal history --to).
var sentTo = []string{"--to", "buyer B"}

// historyFiles are Histories of historyStore's deals that tests read, edit
// copies of and verify, written in this order on one store, each on its own
// log:
//   - picked: closed, private and floor (pickedSummary is what the command
//     printed);
//   - other: other alone;
//   - closed: closed alone, and closedKeep, the user's own (keep) copy of it.
type historyFiles struct {
	picked, other, closed, closedKeep string
	pickedSummary                     map[string]any
}

// historyStoreWithFiles is historyStore after historyFiles were written to
// it: writeHistory checked, once, that none of them wrote a deal's log.
func historyStoreWithFiles(t *testing.T) (historyDeals, historyFiles) {
	t.Helper()
	dir := historiesTemplate.use(t, func(t *testing.T, dir string) {
		d := historyStore(t)
		for _, h := range []struct {
			name   string
			args   []string
			picked []string
		}{
			{"picked", sentTo, []string{d.closed, d.private, d.floor}},
			{"other", sentTo, []string{d.other}},
			{"closed", sentTo, []string{d.closed}},
			{"closed-keep", []string{"--audience", "keep"}, []string{d.closed}},
		} {
			path, summary := writeHistory(t, d.all(), h.args, h.picked...)
			if h.name == "picked" {
				templatePickedSummary = summary
			}
			copyTree(t, filepath.Dir(path), filepath.Join(dir, "histories", h.name))
		}
	})
	file := func(name string) string { return filepath.Join(dir, "histories", name, "history.json") }
	return templateDeals, historyFiles{
		picked: file("picked"), other: file("other"), closed: file("closed"), closedKeep: file("closed-keep"),
		pickedSummary: maps.Clone(templatePickedSummary),
	}
}

// copyTree copies the directory src to dst, keeping each file's mode.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(src, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), info.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, info.Mode().Perm())
	}))
}

// pointProfile rewrites the deal profile under configHome to name store
// paths under to instead of from, and checks none under from is left.
func pointProfile(t *testing.T, configHome, from, to string) {
	t.Helper()
	profile := filepath.Join(configHome, "capsule", "profiles", "deal.yaml")
	raw, err := os.ReadFile(profile)
	require.NoError(t, err)
	sep := string(filepath.Separator)
	raw = bytes.ReplaceAll(raw, []byte(from+sep), []byte(to+sep))
	require.NotContains(t, string(raw), from+sep, "the copy's profile names only its own store")
	require.NoError(t, os.WriteFile(profile, raw, 0o600))
}

func makeHistoryDeals(t *testing.T) historyDeals {
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

// Five deals, three picked: the History is the history copy of each (every
// record's payload withheld), every nested deal checked as its own bundle,
// one signer key (that no deal's log was written is checked where the
// History is written, in historyStoreWithFiles); the pick and who it is for
// are sealed on the History's own log; outcome and action class are
// reported as not proven.
func TestDealHistoryVerifiesEachPickedDeal(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	picked := []string{d.closed, d.private, d.floor}
	path := h.picked
	assert.Equal(t, dealAudienceHistory, h.pickedSummary["audience"], "the history copy unless asked otherwise")

	want := append([]string{}, picked...)
	sort.Strings(want)
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "VALID exits 0: %v", findings)
	assert.Equal(t, "VALID", verdict, "withheld payloads are not a finding")
	assert.Empty(t, findings)
	assert.Equal(t, dealAudienceHistory, entry["audience"])
	assert.Equal(t, []any{"outcome", "action_class"}, entry["not_proven"])
	assert.Equal(t, "buyer B", entry["recipient"])
	assert.Equal(t, []any{"recipient", "steps_after_checkpoint"}, entry["holder_stated"])
	holder := entry["holder_key"].(string)
	require.Len(t, holder, 64)
	assert.Equal(t, map[string]any{holder: []any{want[0], want[1], want[2]}}, entry["signer_keys"], "every nested record is signed by the selection's key")
	checked := entry["deals"].([]any)
	require.Len(t, checked, 3)
	for i, raw := range checked {
		m := raw.(map[string]any)
		assert.Equal(t, want[i], m["deal_id"])
		assert.Equal(t, "VALID", m["verdict"], "a history copy verifies VALID on its own")
		assert.Equal(t, dealAudienceHistory, m["audience"])
	}

	b := readHistory(t, path)
	assert.Equal(t, "evidence-bundle/v2", b["bundle_kind"])
	ext := historyExt(b)
	nested := ext["deals"].(map[string]any)
	require.Len(t, nested, 3)
	effects := 0
	for _, id := range picked {
		tb := nested[id].(map[string]any)
		cert := tb["completeness_certificate"].(map[string]any)
		assert.Equal(t, dealLogID(id), cert["log_id"], "each nested bundle is its own deal's log")
		assert.False(t, payloadDisclosed(tb), "no payload of %s is disclosed", id)
		assert.Equal(t, map[string]any{"audience": dealAudienceHistory, "deal_id": id}, tb["extensions"].(map[string]any)[dealProfile], "no report text")
		for _, raw := range tb["records"].([]any) {
			r := raw.(map[string]any)
			assert.NotEmpty(t, r["timestamp"], "each record still shows its time")
			if e, ok := r["effect"].(map[string]any); ok {
				assert.NotEmpty(t, e["status"])
				effects++
			}
		}
	}
	assert.NotZero(t, effects, "an act's effect status shows")
	// The sealed selection record is disclosed and is what the file carries.
	sealed := b["disclosures"].(map[string]any)[ext["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
	assert.Equal(t, "history_selection", sealed["type"])
	assert.Equal(t, dealAudienceHistory, sealed["audience"])
	assert.Equal(t, "buyer B", sealed["recipient"])
	assert.Equal(t, ext["selection"], sealed["selection"])
	assert.Equal(t, historyLogPrefix+sealed["nonce"].(string), b["completeness_certificate"].(map[string]any)["log_id"], "the History's own log")
	assert.Len(t, b["records"], 1, "its selection is the only record on it")
}

// Two Histories share no log: a recipient learns nothing of any other
// History (how many came before, when, under which key).
func TestDealHistoriesShareNoLog(t *testing.T) {
	_, h := historyStoreWithFiles(t)
	logs := map[string]bool{}
	for _, path := range []string{h.picked, h.other, h.closed, h.closedKeep} {
		b := readHistory(t, path)
		log := b["completeness_certificate"].(map[string]any)["log_id"].(string)
		assert.True(t, strings.HasPrefix(log, historyLogPrefix), log)
		assert.False(t, logs[log], "log %s carries two Histories", log)
		logs[log] = true
		require.Len(t, b["records"], 1, "no other selection, not even withheld")
		assert.Equal(t, historyExt(b)["selection_record"], b["records"].([]any)[0].(map[string]any)["capsule_id"])
	}
	verdict, _, findings, err := verifyHistory(t, h.other)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
}

// sellerToBuyerA is a seller's deal with one buyer, A, whose name, contact,
// address, the item and its price are all in the deal's opening.
const sellerToBuyerA = `{"type": "purchase", "channel": "web",
 "intent": {"verbatim": "Sell my vintage road bike to Avery for 1450", "party_role": "seller",
            "asked": {"item": "vintage road bike", "quantity": 1}, "allowed": ["commit"]},
 "who": {"name": "Avery Quinlan", "domain": "quinlan.example", "email": "avery@quinlan.example", "phone": "+1 555 014 2233"},
 "terms": {"item": "vintage road bike", "quantity": 1, "price_minor": 145000, "currency": "USD", "place": "12 Juniper Court, Lakeview"},
 "recourse": {"rail": "card", "refundable": false}}`

// jsonLeaves decodes doc and returns every object key, string and number in
// it, each number as written.
func jsonLeaves(t *testing.T, doc []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	var leaves []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				leaves = append(leaves, k)
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		case string:
			leaves = append(leaves, v)
		case json.Number:
			leaves = append(leaves, v.String())
		}
	}
	walk(v)
	return leaves
}

// leavesCarrying returns the leaves that carry v: equal to it, or holding it
// with no letter or digit directly on either side. A digest or encoded blob
// is all letters and digits, so a run of digits inside one never counts,
// while a name or an amount inside a sentence does.
func leavesCarrying(leaves []string, v string) []string {
	isWord := func(s string, at int) bool {
		if at < 0 || at >= len(s) {
			return false
		}
		c := s[at]
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	var out []string
	for _, leaf := range leaves {
		for from := 0; ; {
			i := strings.Index(leaf[from:], v)
			if i < 0 {
				break
			}
			i += from
			if !isWord(leaf, i-1) && !isWord(leaf, i+len(v)) {
				out = append(out, leaf)
				break
			}
			from = i + 1
		}
	}
	return out
}

// A value carried in a JSON leaf counts, alone or inside text; the same
// digits inside a digest or an encoded blob do not (the flake this replaced:
// "190000" inside a hex digest).
func TestLeavesCarryingMatchesWholeValuesNotDigests(t *testing.T) {
	for _, leaf := range []string{"190000", "sold for 190000.", "$190000", "price 190000 USD"} {
		assert.Equal(t, []string{leaf}, leavesCarrying([]string{leaf}, "190000"), leaf)
	}
	for _, leaf := range []string{
		"65db213ef58a8e8067dc190000d5476b5226dd156bf1e75d030e26ccd9",
		"190000a876f1275de6e08c1e32921a7e89a51fcd3377b18d94bf71fb3f495f",
		"0oRY7KQBJwN4H2Fw190000Y2F0aW9uL2NsbC1jaGVja3BvaW50K2Nib3IEWCCkkEYra96",
		"1190000", "1900001",
	} {
		assert.Empty(t, leavesCarrying([]string{leaf}, "190000"), leaf)
	}
}

// Two unrelated buyers: a History the seller sends to buyer B carries none
// of buyer A's name, contact, address, item, price or message text, nor any
// private value of the user's other picked deals; the user's own (keep)
// History of the same deals does (the control).
func TestDealHistoryForAnotherBuyerCarriesNothingOfTheFirst(t *testing.T) {
	raceSkip(t)
	d := historyStore(t)
	sale := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerToBuyerA))["deal_id"].(string)
	dealRun(t, "note", "--deal", sale, "--kind", "message", "--input", writeJSON(t,
		`{"from":"counterparty","channel":"web","text":"Avery here: please hold the vintage road bike, I will collect it at 12 Juniper Court"}`))
	dealRun(t, "check", "--deal", sale, "--input", writeJSON(t, `{"action":"commit","terms":{"item":"vintage road bike","price_minor":145000}}`))
	for _, id := range []string{sale, d.floor, d.closed} {
		// The user's own copy of each, so every step is under a checkpoint
		// and the keep control can name a current report.
		out, err := invoke(t, "", "--profile", "deal", "bundle", "--deal", id, "--out", filepath.Join(t.TempDir(), "own.json"))
		require.NoError(t, err, out)
	}
	all := append(d.all(), sale)
	picked := []string{sale, d.private, d.floor, d.closed}
	buyerA := append([]string{"Avery", "Quinlan", "quinlan.example", "555 014 2233", "Juniper", "vintage road bike", "145000", "please hold"}, writtenAmounts(1450)...)
	private := append([]string{homeAddress, "Larkspur", cardNumber, verifyCode, shopEmail, shopPhone, "Buy the sticker",
		"example bicycle", "190000", "170000", "min_total_minor", "bounds_openings", "cat sticker"}, writtenAmounts(1700)...)

	path, _ := writeHistory(t, all, sentTo, picked...)
	leaves := jsonLeaves(t, mustRead(t, path))
	for _, v := range append(buyerA, private...) {
		assert.Empty(t, leavesCarrying(leaves, v), "the History sent to buyer B carries %q", v)
	}
	verdict, _, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)

	keepPath, summary := writeHistory(t, all, []string{"--audience", "keep"}, picked...)
	assert.Equal(t, dealAudienceKeep, summary["audience"])
	keep := jsonLeaves(t, mustRead(t, keepPath))
	for _, v := range []string{"Avery", "vintage road bike", "145000", "Juniper", "Larkspur Lane", "min_total_minor"} {
		assert.NotEmpty(t, leavesCarrying(keep, v), "the keep control carries %q", v)
	}
	for _, v := range buyerA {
		t.Logf("keep control: %q in %d values", v, len(leavesCarrying(keep, v)))
	}
	verdict, entry, findings, err := verifyHistory(t, keepPath)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
	assert.Equal(t, dealAudienceKeep, entry["audience"], "verify says the file is the keep copy")
	assert.NotContains(t, entry, "not_proven", "a keep copy discloses every payload")
}

// A step sealed after the deal's latest checkpoint is not in the copy (no
// checkpoint is cut on the deal for it); the output counts it, and so does
// the deal's sealed selection entry, which verify reports (the holder's own
// word: holder_stated).
func TestDealHistoryStopsAtTheLatestCheckpoint(t *testing.T) {
	d := historyStore(t)
	dealRun(t, "check", "--deal", d.opened, "--input", filepath.Join(retailDemo, "check-pay.json"))
	state := dealRun(t, "checkpoint", "status", "--deal", d.opened)
	path, summary := writeHistory(t, d.all(), sentTo, d.opened)
	line := summary["deals"].([]any)[0].(map[string]any)
	later := state["log_entries"].(float64) - 1
	require.Positive(t, later)
	assert.Equal(t, float64(1), state["checkpoint"].(map[string]any)["entries"])
	assert.Equal(t, float64(1), line["steps_shown"], "the steps the checkpoint covers")
	assert.Equal(t, later, line["steps_after_checkpoint"], "every later step, not shown")
	b := readHistory(t, path)
	sealed := b["disclosures"].(map[string]any)[historyExt(b)["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
	assert.Equal(t, later, sealed["selection"].([]any)[0].(map[string]any)["steps_after_checkpoint"], "sealed in the selection, not only printed")
	verdict, entry, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
	assert.Equal(t, later, entry["deals"].([]any)[0].(map[string]any)["steps_after_checkpoint"])
	assert.Contains(t, entry["holder_stated"], "steps_after_checkpoint")
}

// One record edited inside one nested deal: INVALID, naming that deal and no
// other.
func TestDealHistoryTamperedRecordNamesItsDeal(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	b := readHistory(t, h.picked)
	tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
	tb["records"].([]any)[0].(map[string]any)["timestamp"] = "2020-01-01T00:00:00Z"

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
	d, h := historyStoreWithFiles(t)
	path := h.picked
	unpicked := historyExt(readHistory(t, h.other))["deals"].(map[string]any)[d.other].(map[string]any)

	t.Run("beside the picked deals", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.other] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_in_selection:" + d.other}, findings)
	})
	t.Run("a record inside a picked deal", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
		tb["records"] = append(tb["records"].([]any), unpicked["records"].([]any)[0])
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_invalid:" + d.closed}, findings)
	})
	t.Run("under a picked deal's name", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.closed] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_not_of_its_log:" + d.closed}, findings)
	})
	t.Run("a picked deal dropped", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		delete(historyExt(b)["deals"].(map[string]any), d.floor)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_missing:" + d.floor}, findings)
	})
	t.Run("the carried selection edited", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		ext := historyExt(b)
		ext["selection"] = append(ext["selection"].([]any), map[string]any{"deal_id": d.other, "bundle_digest": jcsDigest(unpicked), "steps_after_checkpoint": 0})
		ext["deals"].(map[string]any)[d.other] = unpicked
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_not_sealed", "deal_not_in_selection:" + d.other}, findings)
	})
	t.Run("the sealed selection edited", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		ext := historyExt(b)
		sealed := b["disclosures"].(map[string]any)[ext["selection_record"].(string)].(map[string]any)["agent_input"].(map[string]any)
		sealed["selection"] = append(sealed["selection"].([]any), map[string]any{"deal_id": d.other, "bundle_digest": jcsDigest(unpicked), "steps_after_checkpoint": 0})
		ext["selection"] = sealed["selection"]
		ext["deals"].(map[string]any)[d.other] = unpicked
		verdict, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, "INVALID", verdict)
		assert.Equal(t, []string{"selection_record_unverified"}, findings)
	})
}

// The owner's copy of a deal swapped into a History sent to someone is named.
func TestDealHistoryNamesACopyForAnotherAudience(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	keep := historyExt(readHistory(t, h.closedKeep))["deals"].(map[string]any)[d.closed]
	b := readHistory(t, h.closed)
	historyExt(b)["deals"].(map[string]any)[d.closed] = keep
	_, entry, findings, err := verifyHistory(t, rewrite(t, b))
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.ElementsMatch(t, []string{"deal_bundle_not_the_selected_one:" + d.closed, "deal_audience_not_selected:" + d.closed, "deal_payload_disclosed:" + d.closed}, findings)
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
	path, _ := writeHistory(t, append(d.all(), after), sentTo, d.closed, after)
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

// A keep History of a deal whose newest report of the user's own does not
// cover every step is refused: its copy would have to name a report written
// before its later steps. A history copy carries no report, so it is not.
func TestDealHistoryRefusesAStaleOwnReport(t *testing.T) {
	raceSkip(t)
	d := historyStore(t)
	dealRun(t, "note", "--deal", d.private, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","channel":"web","text":"shipped"}`))
	sharedCopy(t, d.private, dealAudienceCounterparty, "the shop")
	before := logStates(t, d.all()...)
	out, err := invoke(t, "", "--profile", "deal", "deal", "history", "--out", filepath.Join(t.TempDir(), "h.json"), "--deal", d.private, "--audience", "keep")
	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "has a sealed report but none of your own that covers its later steps")
	assert.Equal(t, before, logStates(t, d.all()...))
	path, _ := writeHistory(t, d.all(), sentTo, d.private)
	verdict, _, findings, err := verifyHistory(t, path)
	require.NoError(t, err, "%v", findings)
	assert.Equal(t, "VALID", verdict)
}

// The builder takes only deals from `deal list`, each once, and only the
// history copy, for whom --to names (or keep, the user's own, for no one):
// a copy for someone in the deal is refused; a sale has its own copy.
func TestDealHistoryRefusesWhatIsNotAPickedDeal(t *testing.T) {
	d := historyStore(t)
	sale, _ := newSale(t)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"unknown":        {[]string{"--to", "B", "--deal", "deal-0000000000000000"}, "unknown deal: deal-0000000000000000"},
		"twice":          {[]string{"--to", "B", "--deal", d.closed, "--deal", d.closed}, "is picked twice"},
		"a sale":         {[]string{"--to", "B", "--deal", sale}, "a sale has its own copy"},
		"none":           {[]string{"--to", "B"}, "--deal (one per picked deal"},
		"not a deal":     {[]string{"--to", "B", "--deal", "not-a-deal"}, "must be a deal id"},
		"counterparty":   {[]string{"--to", "B", "--deal", d.closed, "--audience", "counterparty"}, "a History may be sent to anyone"},
		"adjudicator":    {[]string{"--to", "B", "--deal", d.closed, "--audience", "adjudicator"}, "a History may be sent to anyone"},
		"audience":       {[]string{"--to", "B", "--deal", d.closed, "--audience", "everyone"}, "--audience must be history"},
		"no recipient":   {[]string{"--deal", d.closed}, "deal history needs --to"},
		"blank":          {[]string{"--to", " ", "--deal", d.closed}, "deal history needs --to"},
		"keep, for whom": {[]string{"--to", "B", "--deal", d.closed, "--audience", "keep"}, "--audience keep is your own copy"},
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

// sealHistory writes a History the holder seals for recipient over any
// selection and deals (as a holder misusing their own key could), through
// the builder's own sealing.
func sealHistory(t *testing.T, audience, recipient string, selection []any, deals map[string]any) string {
	t.Helper()
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	sel := make([]interface{}, len(selection))
	copy(sel, selection)
	b, id, err := s.historyBundle(t.Context(), audience, recipient, sel)
	require.NoError(t, err)
	b["extensions"] = map[string]interface{}{historySelectionExtension: map[string]interface{}{"selection_record": id, "selection": sel, "deals": deals}}
	return rewrite(t, b)
}

// What the History's own records and members must be: the selection signed
// and the bundle's root on the history log; each member a bundle whose
// every record is signed; each selection entry well formed; each member the
// copy for the sealed audience, even when the holder sealed its digest.
func TestDealHistoryChecksItsOwnParts(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	path := h.closed
	keep := historyExt(readHistory(t, h.closedKeep))["deals"].(map[string]any)[d.closed].(map[string]any)

	t.Run("selection unsigned", func(t *testing.T) {
		raceSampleKey(t, t.Name())
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
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		b["root"] = strings.Repeat("0", 64)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "selection_record_not_root")
	})
	t.Run("not on the history log", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		b["completeness_certificate"].(map[string]any)["log_id"] = dealLogID(d.closed)
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "not_a_history_log")
	})
	t.Run("a member's deal section edited", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		// The CHANGELOG's known limit: a member's deal section is sealed by
		// no record; the nested deal stays VALID, and only the History's
		// bundle_digest catches the edit.
		b := readHistory(t, path)
		tb := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)
		section := tb["extensions"].(map[string]any)[dealProfile].(map[string]any)
		require.NotContains(t, section, dealReportPointer, "an unsealed section")
		section["deal_id"] = "edited"
		_, entry, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_bundle_not_the_selected_one:" + d.closed}, findings)
		assert.Equal(t, "VALID", entry["deals"].([]any)[0].(map[string]any)["verdict"])
	})
	t.Run("a member record unsigned", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		r := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)["records"].([]any)[0].(map[string]any)
		delete(r, "signature")
		delete(r, "key_id")
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, findings, "deal_record_unsigned:"+d.closed)
	})
	t.Run("a member not a bundle", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		historyExt(b)["deals"].(map[string]any)[d.closed] = map[string]any{"bundle_kind": "something-else"}
		_, _, findings, err := verifyHistory(t, rewrite(t, b))
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_not_a_bundle:" + d.closed}, findings)
	})
	t.Run("a selection entry malformed, sealed by the holder", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		member := historyExt(readHistory(t, path))["deals"].(map[string]any)[d.closed]
		digest := jcsDigest(member.(map[string]any))
		for name, e := range map[string]map[string]any{
			"digest":                    {"deal_id": d.closed, "bundle_digest": "not-a-digest", "steps_after_checkpoint": 0},
			"no steps_after_checkpoint": {"deal_id": d.closed, "bundle_digest": digest},
			"negative steps":            {"deal_id": d.closed, "bundle_digest": digest, "steps_after_checkpoint": -1},
			"steps as text":             {"deal_id": d.closed, "bundle_digest": digest, "steps_after_checkpoint": "1"},
		} {
			edited := sealHistory(t, dealAudienceHistory, "buyer B", []any{e}, map[string]any{d.closed: member})
			_, _, findings, err := verifyHistory(t, edited)
			require.ErrorIs(t, err, ErrBundleInvalid, name)
			assert.Equal(t, []string{"selection_entry_malformed:" + d.closed, "deal_not_in_selection:" + d.closed}, findings, name)
		}
	})
	t.Run("no recipient, sealed by the holder", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		ext := historyExt(b)
		edited := sealHistory(t, dealAudienceHistory, "", ext["selection"].([]any), ext["deals"].(map[string]any))
		_, entry, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_recipient_missing"}, findings)
		assert.NotContains(t, entry, "recipient")
	})
	t.Run("on another History's log", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		b := readHistory(t, path)
		other := readHistory(t, h.other)["completeness_certificate"].(map[string]any)["log_id"]
		b["completeness_certificate"].(map[string]any)["log_id"] = other
		_, _, findings, _ := verifyHistory(t, rewrite(t, b))
		assert.Contains(t, findings, "not_a_history_log")
	})
	t.Run("the keep copy, its digest sealed under history", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		edited := sealHistory(t, dealAudienceHistory, "buyer B", []any{map[string]any{"deal_id": d.closed, "bundle_digest": jcsDigest(keep), "steps_after_checkpoint": 0}}, map[string]any{d.closed: keep})
		_, _, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_audience_not_selected:" + d.closed, "deal_payload_disclosed:" + d.closed}, findings)
	})
	t.Run("one payload disclosed, sealed by the holder", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		// A history copy with one record's payload put back (from the keep
		// copy, so it still matches its seal and the member verifies).
		member := historyExt(readHistory(t, path))["deals"].(map[string]any)[d.closed].(map[string]any)
		first := member["records"].([]any)[0].(map[string]any)["capsule_id"].(string)
		member["disclosures"].(map[string]any)[first] = keep["disclosures"].(map[string]any)[first]
		edited := sealHistory(t, dealAudienceHistory, "buyer B", []any{map[string]any{"deal_id": d.closed, "bundle_digest": jcsDigest(member), "steps_after_checkpoint": 0}}, map[string]any{d.closed: member})
		_, entry, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"deal_payload_disclosed:" + d.closed}, findings)
		assert.Equal(t, "VALID", entry["deals"].([]any)[0].(map[string]any)["verdict"], "the member itself verifies: only the History's rule catches it")
	})
	t.Run("a selection sealed for the other party", func(t *testing.T) {
		raceSampleKey(t, t.Name())
		member := historyExt(readHistory(t, path))["deals"].(map[string]any)[d.closed].(map[string]any)
		edited := sealHistory(t, dealAudienceCounterparty, "buyer B", []any{map[string]any{"deal_id": d.closed, "bundle_digest": jcsDigest(member), "steps_after_checkpoint": 0}}, map[string]any{d.closed: member})
		_, entry, findings, err := verifyHistory(t, edited)
		require.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, []string{"selection_audience_not_history", "deal_audience_not_selected:" + d.closed}, findings)
		assert.NotContains(t, entry, "not_proven")
	})
}

// A History is a bundle a presentation module can match: `presentation
// list` says it verified and names its extension, and `presentation build`
// writes its page. One whose nested deal fails is neither listed as verified
// nor given a page.
func TestDealHistoryHasAPage(t *testing.T) {
	d, h := historyStoreWithFiles(t)
	path := h.picked

	out, err := invoke(t, "", "presentation", "list", "--bundle", path)
	require.NoError(t, err, out)
	var list map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &list), out)
	descriptor := list["descriptor"].(map[string]any)
	assert.Equal(t, true, descriptor["verified"])
	assert.Equal(t, "evidence-bundle/v2", descriptor["bundle_kind"])
	assert.Equal(t, []any{historySelectionExtension}, descriptor["extensions"])
	page := filepath.Join(t.TempDir(), "page.html")
	out, err = invoke(t, "", "presentation", "build", "--bundle", path, "--out", page)
	require.NoError(t, err, out)
	assert.FileExists(t, page)

	b := readHistory(t, path)
	r := historyExt(b)["deals"].(map[string]any)[d.closed].(map[string]any)["records"].([]any)[0].(map[string]any)
	delete(r, "signature")
	delete(r, "key_id")
	bad := rewrite(t, b)
	out, _ = invoke(t, "", "presentation", "list", "--bundle", bad)
	require.NoError(t, json.Unmarshal([]byte(out), &list), out)
	assert.Equal(t, false, list["descriptor"].(map[string]any)["verified"], "a History verify --bundle fails is not listed as verified")
	refused := filepath.Join(t.TempDir(), "refused.html")
	out, err = invoke(t, "", "presentation", "build", "--bundle", bad, "--out", refused)
	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "no page written")
	assert.NoFileExists(t, refused)
}

// A History counts deals, not records: its sealed selection names each
// picked deal once (one deal's many steps are one entry), while the
// record-level reads a viewer buckets (each record's time, each act's
// effect status) are per record, in the nested bundles.
func TestDealHistoryCountsDealsNotRecords(t *testing.T) {
	_, h := historyStoreWithFiles(t)
	b := readHistory(t, h.picked)
	ext := historyExt(b)
	selection := ext["selection"].([]any)
	deals := map[string]bool{}
	for _, e := range selection {
		deals[e.(map[string]any)["deal_id"].(string)] = true
	}
	assert.Len(t, selection, 3)
	assert.Len(t, deals, 3, "each deal once")
	records, effects := 0, map[string]int{}
	for _, tb := range ext["deals"].(map[string]any) {
		for _, raw := range tb.(map[string]any)["records"].([]any) {
			records++
			if e, ok := raw.(map[string]any)["effect"].(map[string]any); ok {
				effects[e["status"].(string)]++
			}
		}
	}
	assert.Greater(t, records, len(deals), "records are not deals")
	assert.Equal(t, map[string]int{"dispatched": 1}, effects, "one act, one effect status, in one deal")
}
