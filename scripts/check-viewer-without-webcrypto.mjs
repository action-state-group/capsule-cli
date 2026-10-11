#!/usr/bin/env node
// check-viewer-without-webcrypto.mjs -- run a page's own embedded verifier the
// way a page outside a secure context runs it.
//
//   node scripts/check-viewer-without-webcrypto.mjs PAGE.html [--crypto=none|webcrypto] [--fallback=on|off] [--expect=verified|unable]
//
// The page's inline scripts run in a fresh vm context. With --crypto=none
// (the default) that context has no `crypto` global at all, which is what a
// browser gives a page outside a secure context (an http: or data: page,
// some apps' attachment viewers): no SubtleCrypto, so no WebCrypto digest
// and no Ed25519.
// --crypto=webcrypto hands it Node's WebCrypto for comparison.
//
// --fallback=on (the default) prints the verification page's ten checks, the
// bundle digest, and the deal view's lists of checks this page ran and
// checks it left to `capsulectl verify`; --expect=verified exits 1 unless
// all ten pass. --fallback=off switches the runtime's plain SHA-256 off
// (its test hook), leaving the page no SHA-256 at all, then runs the page's
// own bootstrap against a minimal DOM and prints what it drew;
// --expect=unable exits 1 unless that is the "unable to verify in this
// viewer" state with a verify command and no failure banner.
//
// Node is needed to run this check, never to render a page.
import { readFileSync } from "node:fs";
import { webcrypto } from "node:crypto";
import vm from "node:vm";

const args = process.argv.slice(2);
const page = args.find((a) => !a.startsWith("--"));
const flag = (name, fallback) => (args.find((a) => a.startsWith(`--${name}=`)) || `=${fallback}`).split("=")[1];
const cryptoMode = flag("crypto", "none");
const fallback = flag("fallback", "on");
const expect = flag("expect", "");
if (!page || !["none", "webcrypto"].includes(cryptoMode) || !["on", "off"].includes(fallback)) {
  console.error("usage: check-viewer-without-webcrypto.mjs PAGE.html [--crypto=none|webcrypto] [--fallback=on|off] [--expect=verified|unable]");
  process.exit(2);
}

const html = readFileSync(page, "utf8");
const scripts = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
const bundleScript = scripts.find((s) => s.startsWith("window.__BUNDLE__ = "));
const runtime = scripts.find((s) => s.includes("var EvidenceGraph = "));
const bootstrap = scripts.find((s) => s.includes('document.getElementById("app")'));
if (!bundleScript || !runtime || !bootstrap) {
  console.error(`${page}: no embedded bundle, runtime or bootstrap found`);
  process.exit(2);
}
const modules = scripts.filter((s) => s !== bundleScript && s !== runtime && s !== bootstrap);

// Just enough DOM for the runtime's "unable to verify" renderer: elements
// with text, children and dataset. The full page needs a browser.
class Element {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.text = "";
  }
  set textContent(value) {
    this.children = [];
    this.text = String(value);
  }
  get textContent() {
    return this.text + this.children.map((c) => c.textContent).join("");
  }
  append(...nodes) {
    this.children.push(...nodes);
  }
  replaceChildren(...nodes) {
    this.text = "";
    this.children = [...nodes];
  }
  *walk() {
    yield this;
    for (const child of this.children) yield* child.walk();
  }
}
const app = new Element("main");

const sandbox = { console, TextEncoder, TextDecoder, atob, btoa, URL, setTimeout, clearTimeout, queueMicrotask };
if (cryptoMode === "webcrypto") sandbox.crypto = webcrypto;
sandbox.document = {
  createElement: (tag) => new Element(tag),
  getElementById: (id) => (id === "app" ? app : null),
};
const context = vm.createContext(sandbox);
vm.runInContext("globalThis.window = globalThis;", context);
if ((vm.runInContext("typeof crypto", context) === "undefined") !== (cryptoMode === "none")) {
  console.error("sandbox crypto global is not as requested");
  process.exit(2);
}
vm.runInContext(bundleScript, context, { filename: "bundle" });
vm.runInContext(runtime, context, { filename: "evidence-graph.iife.js" });
for (const [i, source] of modules.entries()) vm.runInContext(source, context, { filename: `module-${i}` });

const EG = vm.runInContext("EvidenceGraph", context);
const bundle = vm.runInContext("window.__BUNDLE__", context);
if (typeof EG.digestCapability !== "function") {
  console.error("this page's runtime predates digestCapability: it has no SHA-256 fallback");
  if (fallback === "off") process.exit(1);
}
if (fallback === "off") EG.setSha256FallbackForTesting(false);
const capability = typeof EG.digestCapability === "function" ? EG.digestCapability() : "webcrypto only";
console.log(`page: ${page}`);
console.log(`crypto global: ${cryptoMode === "none" ? "absent" : "node webcrypto"}; plain SHA-256: ${fallback}; runtime SHA-256: ${capability}`);

if (fallback === "off") {
  // The page's own bootstrap: it must not reach the verifier.
  await vm.runInContext(bootstrap.replace(/^\(async \(\) => \{/m, "globalThis.__run = (async () => {"), context, { filename: "bootstrap" });
  await vm.runInContext("globalThis.__run", context);
  const nodes = [...app.walk()];
  const state = nodes.find((n) => n.dataset.verify !== undefined)?.dataset.verify;
  const command = nodes.find((n) => n.dataset.verifyCommand !== undefined)?.textContent;
  const failed = nodes.some((n) => n.dataset.verify === "failed" || n.dataset.refusal === "unverified-bundle");
  for (const node of app.children) console.log(`  <${node.tagName.toLowerCase()}> ${node.textContent}`);
  console.log(`state: ${state}; command: ${command ?? "(none)"}; failure banner: ${failed ? "SHOWN" : "not shown"}`);
  if (expect === "unable") process.exit(state === "unable" && command && !failed ? 0 : 1);
  process.exit(expect === "" ? 0 : 1);
}

const ctx = await EG.buildVerifiedBundleContext(bundle);
const model = EG.buildVerificationPageModel(bundle, ctx.verification);
const pad = Math.max(...model.checks.map((c) => c.name.length));
for (const check of model.checks) console.log(`  ${check.name.padEnd(pad)}  ${check.status.toUpperCase().padEnd(11)}  ${check.result}`);
console.log(`bundle digest: ${model.bundleDigest ?? "uncomputable"}`);

let resolution;
try {
  resolution = await EG.resolvePresentation(ctx);
} catch (err) {
  resolution = { kind: "error", error: String(err) };
}
if (resolution.kind === "module") {
  const view = await resolution.module.buildModel(ctx);
  if (view && view.checks) {
    console.log(`checked by this page: ${view.checks.page.join(", ") || "(none)"}`);
    console.log(`not checked in this viewer (capsulectl verify): ${view.checks.cli.join(", ") || "(none)"}`);
  }
} else {
  console.log(`presentation: ${resolution.kind}${resolution.error ? ` (${resolution.error})` : ""}`);
}

const allPass = model.checks.length === 10 && model.checks.every((c) => c.status === "pass");
console.log(`verdict: ${allPass ? "all ten checks pass" : "NOT all checks pass"}`);
if (expect === "verified") process.exit(allPass ? 0 : 1);
process.exit(expect === "" ? 0 : 1);
