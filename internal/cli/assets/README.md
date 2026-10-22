# Vendored browser runtime for `report build`

`evidence-graph.iife.js` is the browser runtime every `report.html` that
`capsulectl report build` writes embeds (`deal report --html` and
`presentation build` embed the same file). It bundles two sources:

- **the page code** from capsule-viewer's `ts/src` (the views, the
  presentation mount and registry, the outcome and EU report cards), the
  repository the report pages moved to (capsule-viewer#33);
- **the verifier** from agent-action-capsule's `ts/src` (bundle and
  result-root verification, the producer and disclosure envelopes,
  countersignatures, fragments). The global keeps every name aac's browser
  entry exports, so the deal page's calls (`verifyProducerEnvelope`,
  `verifyDisclosureEnvelope`, `verifiedPayload`, `disclosureOf`,
  `ed25519Available`) still resolve; where both define a name,
  capsule-viewer's wins.

`scripts/build-report-runtime.mjs` does the bundling: capsule-viewer's own
build resolves the aac package to a released npm copy, and this one resolves
it to the pinned aac source instead, so the views and the deal page share one
verifier no older than the aac commit pinned here. The file is vendored
because the pinned capsule-viewer Go module ships the emitter shell
(`go/emitter/shell.html`) but not this file, and no release of either
repository carries a built IIFE. Node is therefore needed to *refresh* this
file, never to render a report.

| | |
|---|---|
| page code | capsule-viewer main @ `c48de50ea42c2b3fdec045daef6341c889fc22b7`, a merged commit (the same one `go.mod`'s `capsule-viewer/go v0.1.2-0.20261022205921-c48de50ea42c` names) |
| verifier | agent-action-capsule main @ `4593d42f300ab20df1a2c3c146f0d89aacf045a6`, a merged commit (the one `ts/v0.5.0` tags), pinned apart from `go.mod`; registry `@action-state-group/cll` `0.2.1` |
| build | `npm ci` in both `ts/` directories, then `node scripts/build-report-runtime.mjs <cv> <aac> <out>`, esbuild 0.28.2 (capsule-viewer's); the output opens with `"use strict";`, as both repositories build under `"strict": true` |
| sha256 | `a657b74ccf18fa7d927cdce009b388a8813f8aeef93133718dc82f541a6cba3c` |

The digest is pinned again in `evidence_graph_assets.go` (`evidenceGraphIIFEDigest`) and
checked by `TestIIFEIsPinned`; `deal report` embeds the same file, with its digest in `evidence-graph.iife.js.sha256`. `scripts/build-evidence-graph-iife.sh [--write]` rebuilds it from the two commits `evidenceGraphIIFESource` records; `scripts/iife-sync.sh <cv checkout> <aac checkout>` rebuilds
and replaces the file, and `make iife-check CV=<cv checkout> AAC=<aac checkout>` diffs the
vendored copy against a fresh build. Refresh the file, the digest here and
the constants in `evidence_graph_assets.go` together. The shell's CSP style
hashes come from the capsule-viewer Go module, so move `go.mod`'s
capsule-viewer version and the page-code commit together.

The file bundles capsule-viewer's page code (Apache-2.0 AND BSD-3-Clause) and
agent-action-capsule's verifier (BSD-3-Clause) with cborg and
@action-state-group/cll (both Apache-2.0); their license texts are in
`THIRD_PARTY_NOTICES.md` beside it. CI runs `make iife-check` against the
pinned commits on every pull request.
