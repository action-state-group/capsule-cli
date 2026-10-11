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
| source | PENDING RELEASE: agent-action-capsule branch `viewer-webcrypto-fallback` @ `38068f279a8e197a7d152af912d775cdcd947db1` with `@action-state-group/cll` `0.2.1` packed from cll-ts `viewer-webcrypto-fallback` @ `d0ce4461d08783816c42f404ca0a5872d203c086`; neither is merged or published, and `go.mod` still pins `ab4ee43b71d6e074c6a000b5b19a9caf9720235e`. Replace with the merged commit after the release (see `report.go`). |
| build | `cd ts && npm ci && npm run emitter:iife`, esbuild 0.28.2 |
| sha256 | `74b14d9728da076df2ccc6ce58d4c7aa6421343d05fe76bfb2aa5b2c080dd3ee` |

The digest is pinned again in `report.go` (`evidenceGraphIIFEDigest`) and
checked by `TestIIFEIsPinned`; `deal report` embeds the same file, with its digest in `evidence-graph.iife.js.sha256`. `scripts/build-evidence-graph-iife.sh [--write]` rebuilds it from the aac commit `go.mod` pins; `scripts/iife-sync.sh <aac checkout>` rebuilds
and replaces the file, and `make iife-check AAC=<aac checkout>` diffs the
vendored copy against a fresh build. Refresh the file, the digest here and
the constants in `report.go` together.

The file bundles agent-action-capsule's runtime (BSD-3-Clause) with cborg
and @action-state-group/cll (both Apache-2.0); their license texts are in
`THIRD_PARTY_NOTICES.md` beside it. CI runs `make iife-check` against the
pinned commit on every pull request.
