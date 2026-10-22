package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealPageJourneyDriver renders a deal page's deal view under node, with
// node's WebCrypto and a DOM just large enough for it, and prints the label
// of each "How it went" row, in order, and the section's heading if shown.
const dealPageJourneyDriver = `
const fs = require("fs");
const vm = require("vm");
const { webcrypto } = require("crypto");
const [pagePath, edit] = process.argv.slice(2);
const html = fs.readFileSync(pagePath, "utf8");
const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
const bundleScript = scripts.find((s) => s.startsWith("window.__BUNDLE__ = "));
const runtime = scripts.find((s) => s.includes("var EvidenceGraph = "));
const bootstrap = scripts.find((s) => s.includes('document.getElementById("app")'));
const modules = scripts.filter((s) => s !== bundleScript && s !== runtime && s !== bootstrap);
class Node {
  constructor(tag) { this.tag = tag; this.children = []; this.dataset = {}; this.className = ""; this._text = ""; }
  append(...nodes) { this.children.push(...nodes.map((n) => (typeof n === "string" ? Object.assign(new Node("#text"), { _text: n }) : n))); }
  set textContent(t) { this._text = t; this.children = []; }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(""); }
  all(pred, out = []) { if (pred(this)) out.push(this); this.children.forEach((c) => c.all(pred, out)); return out; }
}
const document = { head: new Node("head"), createElement: (tag) => new Node(tag), getElementById: () => null };
const sandbox = { console, TextEncoder, TextDecoder, atob, btoa, URL, setTimeout, clearTimeout, queueMicrotask, crypto: webcrypto, document };
const context = vm.createContext(sandbox);
vm.runInContext("globalThis.window = globalThis;", context);
vm.runInContext(bundleScript, context);
vm.runInContext(runtime, context);
for (const source of modules) vm.runInContext(source, context);
// The bootstrap's first statement sets the page's data; the rest renders.
vm.runInContext(bootstrap.slice(0, bootstrap.indexOf("\n(async")), context);
const EG = vm.runInContext("EvidenceGraph", context);
const bundle = vm.runInContext("window.__BUNDLE__", context);
const words = vm.runInContext("globalThis.capsulectlDealPage.words", context);
// "withhold": the switch step's disclosure is dropped from the file.
if (edit === "withhold") {
  const report = bundle.extensions["x-deal-report/v0"].agent_input.report;
  const step = report.journey.find((m) => m.move === "switched").step;
  delete bundle.disclosures[step];
}
(async () => {
  const ctx = await EG.buildVerifiedBundleContext(bundle);
  const resolution = await EG.resolvePresentation(ctx);
  const model = await resolution.module.buildModel(ctx);
  const L0 = new Node("div");
  resolution.module.render(model, { L0 });
  const headings = L0.all((n) => n.tag === "h2").map((n) => n.textContent);
  const rows = L0.all((n) => n.dataset.journey !== undefined);
  process.stdout.write(JSON.stringify({
    refusal: model.refusal || "",
    shown: headings.includes("How it went"),
    labels: rows.map((n) => n.dataset.journey),
    rows: rows.map((n) => n.children[0].textContent),
    words: Object.keys(words).filter((k) => k.startsWith("journey.") || k.startsWith("class.")),
  }));
})();
`

// renderJourney writes the deal's page (a shared copy when share is set) and
// renders it under node.
func renderJourney(t *testing.T, id string, share bool, edit ...string) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	args := []string{"report", "--deal", id, "--html", page}
	if share {
		args = append(args, "--share", cli.DealAudienceCounterparty, "--to", "the shop")
	}
	dealRun(t, args...)
	driver := filepath.Join(dir, "driver.js")
	require.NoError(t, os.WriteFile(driver, []byte(dealPageJourneyDriver), 0o600))
	out, err := exec.Command(node, append([]string{driver, page}, edit...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got), string(out))
	return got
}

// The deal page shows the errand's journey (capsule-cli#370): the page's
// rows are the report's journey, one model, worded alike, each saying who
// moved, so the switch to eBay reads as the user's. The sealed report keeps
// no pack words; a shared copy shows no journey.
func TestTheDealPageShowsTheJourney(t *testing.T) {
	dealFixture(t)
	id, _, _ := fawnDeal(t)
	var labels []any
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		labels = append(labels, s.(map[string]any)["label"])
	}
	got := renderJourney(t, id, false)
	require.Empty(t, got["refusal"])
	assert.Equal(t, true, got["shown"])
	assert.Equal(t, labels, got["labels"], "the page words the journey as the report does")
	rows := got["rows"].([]any)
	require.Len(t, rows, len(labels))
	assert.Regexp(t, `^\S.* · You · Asked$`, rows[0], "the first row says its time")
	assert.Equal(t, "Your agent · Couldn't (AliExpress sign-in)", rows[4], "the same time is said once")
	assert.Equal(t, "You · Switched to eBay", rows[5], "the switch is the user's")

	shared := renderJourney(t, id, true)
	require.Empty(t, shared["refusal"])
	assert.Equal(t, false, shared["shown"], "a shared copy carries no journey")
	assert.Empty(t, shared["words"], "nor the words to word one")
	assert.NotEmpty(t, got["words"])

	// A row whose step did not verify says so on the row itself.
	withheld := renderJourney(t, id, false, "withhold")
	require.Empty(t, withheld["refusal"])
	assert.Equal(t, "You · Switched to eBay · step not verified in this report", withheld["rows"].([]any)[5], "and gives no time")
}

// The sealed report's journey is facts only: no word from a wording pack,
// so the same bundle words alike under any pack.
func TestTheSealedJourneyHoldsNoWords(t *testing.T) {
	dealFixture(t)
	id, _, _ := fawnDeal(t)
	path := filepath.Join(t.TempDir(), "b.json")
	dealRun(t, "report", "--deal", id, "--bundle", path)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var b map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &b))
	report, _, _ := cli.DealSealedReport(b)
	journey := report["journey"].([]interface{})
	require.Len(t, journey, 11)
	for _, m := range journey {
		assert.NotContains(t, m, "label")
	}
	couldnt := journey[4].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"who": "AliExpress", "reason": "sign_in_required"}, couldnt["facts"])
	assert.Equal(t, "journey.agent.couldnt.who", couldnt["key"])
	told := journey[8].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"who": "eBay", "classes": []interface{}{"name", "address"}}, told["facts"])
}

// pinJourneyPack pins a copy of the built-in pack, edited, as the deal
// profile's wording.
func pinJourneyPack(t *testing.T, edit func(entries map[string]any)) {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(cli.BuiltinWordingBytes(), &doc))
	doc["id"] = "org.example.product.wording/v0"
	edit(doc["entries"].(map[string]any))
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "pack.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	p.Wording = cli.ProfileWording{Path: path, SHA256: hex.EncodeToString(sum[:])}
	require.NoError(t, cli.SaveProfile(p, true))
}

// A pinned pack words the page's journey as it words the report's; one
// without the journey keys is not used, and both fall back to the built-in
// words, as every other line does.
func TestThePageWordsTheJourneyFromThePinnedPack(t *testing.T) {
	dealFixture(t)
	id, _, _ := fawnDeal(t)
	labels := func() []any {
		var out []any
		for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
			out = append(out, s.(map[string]any)["label"])
		}
		return out
	}
	pinJourneyPack(t, func(e map[string]any) {
		// A class or reason line that says nothing falls back as the
		// report's does: the class's own word, an empty reason.
		e["class.name"] = "{nothing}"
		e["journey.reason.sign_in_required"] = "{nothing}"
	})
	want := labels()
	assert.Contains(t, want, "Told eBay your name and address")
	assert.Contains(t, want, "Couldn't (AliExpress )")
	assert.Equal(t, want, renderJourney(t, id, false)["labels"])

	pinJourneyPack(t, func(e map[string]any) {
		e["journey.user.switched"] = "You moved the errand to {who}"
		e["journey.reason.sign_in_required"] = "needed a sign-in"
		e["class.address"] = "street address"
	})
	want = labels()
	assert.Contains(t, want, "You moved the errand to eBay")
	assert.Contains(t, want, "Couldn't (AliExpress needed a sign-in)")
	assert.Contains(t, want, "Told eBay your name and street address")
	assert.Equal(t, want, renderJourney(t, id, false)["labels"])

	pinJourneyPack(t, func(e map[string]any) {
		for k := range e {
			if strings.HasPrefix(k, "journey.") {
				delete(e, k)
			}
		}
	})
	want = labels()
	assert.Contains(t, want, "Switched to eBay", "the built-in words")
	assert.Equal(t, want, renderJourney(t, id, false)["labels"])
}

// An "other" reason is worded by its own detail, on the page as in the
// report.
func TestTheJourneyWordsAnOtherReasonByItsDetail(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, fawnErrand))["deal_id"].(string)
	note(t, id, "couldnt", `{"reason":"other","detail":"the checkout page kept reloading"}`)
	var labels []any
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		labels = append(labels, s.(map[string]any)["label"])
	}
	assert.Contains(t, labels, "Couldn't (the checkout page kept reloading)")
	assert.Equal(t, labels, renderJourney(t, id, false)["labels"])

	// A detail is a reason's value, not a placeholder of its own.
	pinJourneyPack(t, func(e map[string]any) { e["journey.agent.couldnt"] = "Couldn't ({detail})" })
	labels = nil
	for _, s := range dealRun(t, "report", "--deal", id)["journey"].([]any) {
		labels = append(labels, s.(map[string]any)["label"])
	}
	assert.Contains(t, labels, "")
	assert.Equal(t, labels, renderJourney(t, id, false)["labels"])
}
