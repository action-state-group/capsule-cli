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
| source | agent-action-capsule `main` @ `eabc4adf271f6ed7b08ed4d276adff426c4196f0` (after #140 as `35de60c` and #141 as `dbadc74` merged; byte-identical to the earlier `desk/report-result-root` @ `173497d` build) |
| build | `cd ts && npm ci && npm run emitter:iife`, esbuild 0.28.2 |
| sha256 | `12cb62afd6c3bcdea2cfdec80190d6bd2600982e682fab561f2941642848c336` |

The digest is pinned again in `report.go` (`evidenceGraphIIFEDigest`) and
checked by `TestIIFEIsPinned`; `scripts/iife-sync.sh <aac checkout>` rebuilds
and replaces the file, and `make iife-check AAC=<aac checkout>` diffs the
vendored copy against a fresh build. Refresh the file, the digest here and
the constants in `report.go` together.
