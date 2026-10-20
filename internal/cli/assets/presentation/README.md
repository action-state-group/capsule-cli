# Vendored presentation manifest schema

`presentation-manifest-v0.json` is agent-action-capsule's
`schemas/presentation-manifest-v0.json` (the presentation contract,
`spec/presentation-contract-v0.md`), copied unmodified. capsulectl checks a
plugin's presentation manifests and wording packs against it
(`internal/cli/plugin_presentations.go`). It is vendored because the pinned
aac Go module does not carry the repository's `schemas/` directory.

| | |
|---|---|
| source | agent-action-capsule main @ `6f5b48f3fe0eb7eefd2114344f2a5c03e1ce4436`; `go.mod` has moved on and this copy has not been refreshed since (agent-action-capsule#215 changed the schema) |
| sha256 | `077a69fd0dc670bdff6f9fe9793540ffb241873fb5687000cc519241e69ef5a1` |

The digest is pinned again in `plugin_presentations_test.go`
(`TestThePresentationSchemaIsPinned`). Refresh the file and both digests
together, from the commit `go.mod` pins, as a change of its own: a new schema
changes which plugin manifests are accepted.

## Built-in page manifests

`builtin/builtin-*.json` are agent-action-capsule's
`schemas/examples/presentation-manifest-v0/builtin-*.json` at the same commit,
copied unmodified: the six page modules the vendored browser runtime
registers by default. `capsulectl presentation build` checks the plugin
modules it puts in a page against them (presentation contract section 4.5),
after leaving out every module whose audiences exclude the page's.
Their digests are pinned in `presentation_build_test.go`
(`TestThePresentationRuntimeDeclarationIsTheVendoredOne`), which also checks
that the vendored runtime carries each id and declares the runtime
(`aac.presentation-api/v0`, version `0.1.0`) capsulectl checks modules
against. Refresh them with the runtime, from the commit `go.mod` pins.

`internal/cli/testdata/presentation-manifests/` holds the same directory's
example and negative manifests, as test vectors only.
