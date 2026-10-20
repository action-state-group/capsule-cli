# Vendored browser runtime for `report build`

`evidence-graph.iife.js` is agent-action-capsule's evidence-graph browser
runtime, built with `npm run emitter:iife` (esbuild) in that repository's
`ts/` directory and embedded by `internal/cli/evidence_graph_assets.go` into every
`report.html` that `capsulectl report build` writes. It is vendored because
the pinned capsule-viewer Go module ships the emitter shell
(`go/emitter/shell.html`) but not this file, which is built from aac's `ts/`:
that repository's `ts/dist/` is gitignored and no aac release carries a built IIFE. Node is therefore needed to *refresh* this file, never
to render a report.

| | |
|---|---|
| source | agent-action-capsule main @ `6ffe7c1139d3c18adeb26645bc73a4b279f24fe0`, the merged commit `go.mod` pins; registry `@action-state-group/cll` `0.2.1` |
| build | `cd ts && npm ci && npm run emitter:iife`, esbuild 0.28.2 |
| sha256 | `25679a22b837562fa5179f2540307514a87488b4c6c088b60206efe5b48626da` |

The digest is pinned again in `evidence_graph_assets.go` (`evidenceGraphIIFEDigest`) and
checked by `TestIIFEIsPinned`; `deal report` embeds the same file, with its digest in `evidence-graph.iife.js.sha256`. `scripts/build-evidence-graph-iife.sh [--write]` rebuilds it from the aac commit `go.mod` pins; `scripts/iife-sync.sh <aac checkout>` rebuilds
and replaces the file, and `make iife-check AAC=<aac checkout>` diffs the
vendored copy against a fresh build. Refresh the file, the digest here and
the constants in `evidence_graph_assets.go` together.

The file bundles agent-action-capsule's runtime (BSD-3-Clause) with cborg
and @action-state-group/cll (both Apache-2.0); their license texts are in
`THIRD_PARTY_NOTICES.md` beside it. CI runs `make iife-check` against the
pinned commit on every pull request.
