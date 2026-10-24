package deal

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealPageStandaloneDriver runs a deal page's own runtime and deal view
// under node, with node's WebCrypto, after an optional edit to its bundle,
// and prints whether the view refused the report or showed it.
const dealPageStandaloneDriver = `
const fs = require("fs");
const vm = require("vm");
const { webcrypto } = require("crypto");
const [page, edit] = process.argv.slice(2);
const html = fs.readFileSync(page, "utf8");
const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
const bundleScript = scripts.find((s) => s.startsWith("window.__BUNDLE__ = "));
const runtime = scripts.find((s) => s.includes("var EvidenceGraph = "));
const bootstrap = scripts.find((s) => s.includes('document.getElementById("app")'));
const modules = scripts.filter((s) => s !== bundleScript && s !== runtime && s !== bootstrap);
const sandbox = { console, TextEncoder, TextDecoder, atob, btoa, URL, setTimeout, clearTimeout, queueMicrotask, crypto: webcrypto };
const context = vm.createContext(sandbox);
vm.runInContext("globalThis.window = globalThis;", context);
vm.runInContext(bundleScript, context);
vm.runInContext(runtime, context);
for (const source of modules) vm.runInContext(source, context);
const EG = vm.runInContext("EvidenceGraph", context);
const bundle = vm.runInContext("window.__BUNDLE__", context);
const x = (bundle.extensions || {})["x-deal-report/v0"] || {};
const flip = (h) => h.slice(0, -2) + (h.endsWith("00") ? "01" : "00");
if (edit === "step-key") {
  const step = bundle.records.find((r) => r.evidence_record_header && r.evidence_record_header.record_type === "deal_step");
  step.key_id = x.record.key_id === step.key_id ? flip(step.key_id) : x.record.key_id;
}
if (edit === "report-signature") x.record.signature = flip(x.record.signature);
if (edit === "report-text") x.agent_input.report.scope = "edited";
if (edit === "downgraded-in-log") {
  const id = bundle.extensions["x-deal-v0"].sealed_report;
  bundle.extensions["x-deal-v0"] = Object.assign({}, bundle.disclosures[id].agent_input.report, { did_line: "A line nobody sealed." });
  delete bundle.disclosures[id];
}
if (edit === "stripped") {
  bundle.extensions["x-deal-v0"] = x.agent_input.report;
  delete bundle.extensions["x-deal-report/v0"];
}
(async () => {
  const ctx = await EG.buildVerifiedBundleContext(bundle);
  const resolution = await EG.resolvePresentation(ctx);
  if (resolution.kind !== "module") {
    process.stdout.write(JSON.stringify({ presentation: resolution.kind }));
    return;
  }
  const view = await resolution.module.buildModel(ctx);
  process.stdout.write(JSON.stringify({ refusal: view.refusal || "", scope: view.report ? view.report.scope : "" }));
})();
`

// The deal page checks a standalone report itself (capsule-cli#294): the
// page as written shows it; a step whose key_id is not the key its envelope
// verified under, a report signature that does not verify, an edited
// report, and a copy stripped to inline text are each refused.
func TestTheDealPageChecksAStandaloneReport(t *testing.T) {
	run := dealPageUnderNode(t)
	shown := run("")
	assert.Empty(t, shown["refusal"], "the page as written shows its report")
	assert.NotEmpty(t, shown["scope"])
	for _, edit := range []string{"step-key", "report-signature", "report-text", "stripped"} {
		got := run(edit)
		assert.NotEmpty(t, got["refusal"], "%s: the deal view refuses it, not shown", edit)
	}
}

// A receipt sealed on the deal's log, as capsulectl wrote before
// capsule-cli#294, with its report's disclosure dropped and edited text put
// inline: the page refuses it rather than show it under the "not checked"
// label. capsulectl no longer writes such a copy, so scripts/check-html-render.sh
// cannot; this keeps the page's in-log path covered.
func TestTheDealPageRefusesADowngradedInLogReceipt(t *testing.T) {
	inLogReports(t)
	run := dealPageUnderNode(t)
	assert.Empty(t, run("")["refusal"], "the page as written shows its report")
	assert.Equal(t, "⚠️ The deal's text could not be checked against its sealed record. Do not rely on it.", run("downgraded-in-log")["refusal"])
}

// dealPageUnderNode writes a retail deal's counterparty page and returns a
// runner that loads it under node after the named edit.
func dealPageUnderNode(t *testing.T) func(edit string) map[string]string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dealFixture(t)
	id := retailDeal(t)
	dir := t.TempDir()
	page := filepath.Join(dir, "shared.html")
	dealRun(t, "report", "--deal", id, "--share", cli.DealAudienceCounterparty, "--to", "the shop", "--html", page)
	driver := filepath.Join(dir, "driver.js")
	require.NoError(t, os.WriteFile(driver, []byte(dealPageStandaloneDriver), 0o600))
	return func(edit string) map[string]string {
		out, err := exec.Command(node, driver, page, edit).CombinedOutput()
		require.NoError(t, err, string(out))
		var got map[string]string
		require.NoError(t, json.Unmarshal(out, &got), string(out))
		return got
	}
}
