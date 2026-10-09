# Vendored presentation manifest schema

`presentation-manifest-v0.json` is agent-action-capsule's
`schemas/presentation-manifest-v0.json` (the presentation contract,
`spec/presentation-contract-v0.md`), copied unmodified. capsulectl checks a
plugin's presentation manifests and wording packs against it
(`internal/cli/plugin_presentations.go`). It is vendored because the pinned
aac Go module does not carry the repository's `schemas/` directory.

| | |
|---|---|
| source | agent-action-capsule main @ `ab4ee43b71d6e074c6a000b5b19a9caf9720235e`, the commit `go.mod` pins |
| sha256 | `077a69fd0dc670bdff6f9fe9793540ffb241873fb5687000cc519241e69ef5a1` |

The digest is pinned again in `plugin_presentations_test.go`
(`TestThePresentationSchemaIsPinned`). Refresh the file and both digests
together, from the commit `go.mod` pins.
