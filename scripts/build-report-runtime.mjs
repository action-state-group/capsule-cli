// build-report-runtime.mjs -- bundle the browser runtime `report build`,
// `deal report` and `presentation build` embed (internal/cli/assets/
// evidence-graph.iife.js) from two checkouts:
//
//   node scripts/build-report-runtime.mjs <capsule-viewer checkout> <agent-action-capsule checkout> <out file>
//
// The page code (the views, the presentation mount and registry, the outcome
// and EU report cards) comes from capsule-viewer's ts/src, the repository the
// report pages moved to (capsule-viewer#33). The verifier comes from
// agent-action-capsule's ts/src at the commit evidenceGraphIIFESource pins:
// bundle and result-root verification, the producer and disclosure envelopes,
// countersignatures, fragments. capsule-viewer's own build resolves that
// package to a released npm copy (its scripts/aac-browser.mjs); here the
// package resolves to the pinned aac source instead, so the views and the
// deal page (assets/deal-view.js, which calls EvidenceGraph.verifyProducerEnvelope,
// verifyDisclosureEnvelope, verifiedPayload, disclosureOf and ed25519Available)
// share one verifier, and capsulectl's pages never verify with an older one
// than the aac commit pinned here.
//
// The global keeps every name aac's own browser entry exported, so nothing a
// page or module calls goes missing; where capsule-viewer and aac both define
// a name, capsule-viewer's wins (aac still carries its copy of the views).
// Both checkouts need `npm ci` in ts/ first; esbuild is capsule-viewer's
// pinned devDependency. Node is a build-time tool here, never a render-time one.
import { mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [cvArg, aacArg, outArg] = process.argv.slice(2);
if (!cvArg || !aacArg || !outArg) {
  console.error("usage: node scripts/build-report-runtime.mjs <capsule-viewer checkout> <agent-action-capsule checkout> <out file>");
  process.exit(2);
}
// esbuild writes each bundled file's path, relative to its working
// directory, into the output. The build runs from a scratch directory that
// links the two checkouts under fixed names, so the bytes do not depend on
// where they were checked out.
const outFile = resolve(outArg);
const work = realpathSync(mkdtempSync(join(tmpdir(), "report-runtime-")));
process.on("exit", () => rmSync(work, { recursive: true, force: true }));
symlinkSync(resolve(cvArg), join(work, "capsule-viewer"));
symlinkSync(resolve(aacArg), join(work, "agent-action-capsule"));
process.chdir(work);
const cvTs = join(work, "capsule-viewer", "ts");
const aacTs = join(work, "agent-action-capsule", "ts");
const common = { absWorkingDir: work, preserveSymlinks: true, bundle: true };
const { build } = createRequire(join(cvTs, "package.json"))("esbuild");

// aac's browser entry, read rather than restated, minus the views
// capsule-viewer now owns. A module aac adds to its entry comes along.
const viewModules = new Set(["evidence-graph-view", "outcome-report-view", "compliance-view", "presentation-mount"]);
const aacModules = [...readFileSync(join(aacTs, "src", "browser.ts"), "utf8").matchAll(/^export \* from "\.\/([a-z0-9-]+)\.js";$/gm)]
  .map((m) => m[1])
  .filter((m) => !viewModules.has(m));
if (aacModules.length === 0) throw new Error("no modules read from aac's ts/src/browser.ts");
const star = (mods) => mods.map((m) => `export * from ${JSON.stringify(join(aacTs, "src", `${m}.ts`))};`).join("\n");
const aacStar = star(aacModules);
// What capsule-viewer's views import from the package: the modules its own
// bundling plugin (ts/scripts/aac-browser.mjs) names, read from it, plus
// aac's browser entry. A view importing from a module neither lists fails
// the build with "No matching export".
const cvModules = [...(readFileSync(join(cvTs, "scripts", "aac-browser.mjs"), "utf8").match(/const modules = \[([^\]]*)\]/)?.[1] ?? "").matchAll(/"([a-z0-9-]+)\.js"/g)].map((m) => m[1]);
if (cvModules.length === 0) throw new Error("no module list read from capsule-viewer's ts/scripts/aac-browser.mjs");
const pkgStar = star([...new Set([...aacModules, ...cvModules])]);

const pkg = "@action-state-group/agent-action-capsule";
const aacSource = {
  name: "aac-source",
  setup(b) {
    b.onResolve({ filter: /^@action-state-group\/agent-action-capsule$/ }, () => ({ path: pkg, namespace: "aac-source" }));
    b.onLoad({ filter: /.*/, namespace: "aac-source" }, () => ({ contents: pkgStar, loader: "ts", resolveDir: aacTs }));
  },
};

// The names capsule-viewer's browser entry exports, listed by esbuild, so the
// entry can name them: an explicit export shadows the `export *` below.
const cvEntry = join(cvTs, "src", "browser.ts");
const probe = await build({
  ...common,
  entryPoints: [cvEntry],
  format: "esm",
  write: false,
  metafile: true,
  logLevel: "silent",
  outdir: "probe",
  plugins: [aacSource],
});
const cvNames = Object.values(probe.metafile.outputs)[0].exports.sort();
if (!cvNames.includes("renderEvidenceGraph")) throw new Error("capsule-viewer's browser entry does not export renderEvidenceGraph");

const out = await build({
  stdin: {
    contents: `export { ${cvNames.join(", ")} } from ${JSON.stringify(cvEntry)};\n${aacStar}\n`,
    resolveDir: cvTs,
    sourcefile: "capsulectl-report-runtime.ts",
    loader: "ts",
  },
  ...common,
  format: "iife",
  globalName: "EvidenceGraph",
  footer: { js: "globalThis.renderEvidenceGraph = EvidenceGraph.renderEvidenceGraph;" },
  // Paths in the output are written relative to the outfile's directory:
  // it sits in the scratch directory too, and the bytes are copied out.
  outfile: join(work, "evidence-graph.iife.js"),
  write: false,
  logLevel: "warning",
  plugins: [aacSource],
});
writeFileSync(outFile, out.outputFiles[0].contents);
