# Vendored browser runtime for `report build`

`evidence-graph.iife.js` is agent-action-capsule's evidence-graph browser
runtime, built with `npm run emitter:iife` (esbuild) in that repository's
`ts/` directory and embedded by `internal/cli/report.go` into every
`report.html` that `capsulectl report build` writes. It is vendored because
the pinned aac Go module ships the emitter shell (`go/emitter/shell.html`)
but not this file: upstream `ts/dist/` is gitignored and no aac release
carries a built IIFE. Node is therefore needed to *refresh* this file, never
to render a report.

| | |
|---|---|
| source | agent-action-capsule `go/v0.7.0` @ `df3af95221da3cd77792a7fd1c24c1db9ce88376`, the commit `go.mod` pins (after the outcome-report card, #160, the EU AI Act obligations card, #164, and the no-aggregate fix, #191, all merged); cll-ts `0ac72b3769d40a848abc4712a011c8a7da8190c1` |
| build | `cd ts && npm ci && npm run emitter:iife`, esbuild 0.28.2 |
| sha256 | `594d60b5dd0d85c0e752d2037430df42ab0f3db08214f1551d9f87fe9536e5d2` |

The digest is pinned again in `report.go` (`evidenceGraphIIFEDigest`) and
checked by `TestIIFEIsPinned`; `deal report` embeds the same file, with its digest in `evidence-graph.iife.js.sha256`. `scripts/build-evidence-graph-iife.sh [--write]` rebuilds it from the aac commit `go.mod` pins; `scripts/iife-sync.sh <aac checkout>` rebuilds
and replaces the file, and `make iife-check AAC=<aac checkout>` diffs the
vendored copy against a fresh build. Refresh the file, the digest here and
the constants in `report.go` together.

The file bundles agent-action-capsule's runtime (BSD-3-Clause) with cborg
and @action-state-group/cll (both Apache-2.0); their license texts are in
`THIRD_PARTY_NOTICES.md` beside it. CI runs `make iife-check` against the
pinned commit on every pull request.
