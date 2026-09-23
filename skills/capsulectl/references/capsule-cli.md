# Capsule CLI contract

Compatible implementation: capsule-cli, installed on the execution host as
`capsulectl` on `PATH`. The CLI is a prerequisite, **not** copied into this
skill; invoke it as `capsulectl`. Requires a configured profile on the
execution host. No database credentials belong in prompts or skill output.

All successful stdout is a flat JSON object with `spec_version=capsule-cli-result/v1`.
There is no result wrapper. Check process exit status before parsing; a JSON
report does not imply success. Errors: 1 operational, 2 input, 3 partial
verification, 4 publication pending, 5 conflict. Never suppress errors with an
empty-list fallback.

This document extends the reference `evaluation-compiler` carried
(`references/capsule-cli.md` there) with the three verbs it never needed:
`contract validate`, `discover`, and `plugin ls`. Everything below reflects
the profile/get/verify/publish/cll contract as-is; nothing in those sections
was changed for this skill.

## Resolve the profile

```sh
capsulectl profile list
capsulectl profile show NAME
```

`profile list` returns `{"profiles":[NAME, …]}` — the configured profile
names, for discovery only (it carries no store/namespace details). Resolve a
specific one with `profile show NAME` (NAME is positional, not a `--profile`
flag). Read `StoreID`, `Namespace` and `LogID` to freeze the target;
`ReadOnly` describes write configuration. Do not save the entire profile or
its credentials in the run or skill output. The store backend (`Type`) may be
`mysql`, `sqlite` or `jsonl`; all three expose the identical CLL/get/verify/
publish contract below, so nothing downstream depends on which is configured.

## Enumerate

```sh
capsulectl cll list --profile NAME --after AFTER --through THROUGH --limit 1000
```

AFTER is exclusive, THROUGH inclusive. Omit through for discovery. Result
fields: entries (sequence, capsule_id, appended_at), next_after, log_id,
store_id. Default limit 100; maximum 1000. next_after is a cursor, not
has_more. Preserve through across pages. Stop at through or an empty page;
reject non-advancing cursors on nonempty pages. Account for every entry even
when excluded from downstream work.

## Retrieve and verify

```sh
capsulectl get --profile NAME --capsule-id ID
capsulectl get --profile NAME --capsule-id ID --raw --output RECORD.json
capsulectl verify --profile NAME --capsule RECORD.json
```

Readable `get` exposes capsule_id, capsule, producer_envelope, artifacts at
top level. JSON bytes become JSON values, text becomes strings, other bytes
use `{encoding:base64,data:...}`. Artifacts are unordered: select by name,
never index. Keep binding, state, content_sha256. An unbound artifact is not
authenticated simply because it was stored beside a Capsule.

Raw `--output` is an exact SDK record usable by verify; readable JSON must
not be used to reconstruct signed bytes. Verification reports identity/
signature/trust/content separately and does not establish business truth or
CLL inclusion. Missing original content is a limitation, not a match.

## Publish

```sh
capsulectl publish --profile NAME --request REQUEST.json
```

Requires a writable profile, signing configuration, trusted producer keys and
initialized artifact/CLL facilities (`capsulectl store init`). Do not
initialize production storage as a side effect of a skill run. Do not treat
changing `ReadOnly` as granting SQL privileges or adding a signing key.

REQUEST uses `capsule-seal-request/v1`, a `capsule` object with `ActionID`,
`ActionType`, `Operator`, `Developer`, `Timestamp`, and a `payload` carrying
whatever this action's record needs to hold. Resolve real operator/developer
identities from the profile and the skill's own name — never invent them.
Persist the exact request, timestamp and action ID before the first publish.
The CLI request field `payload` is committed at
`model_attestation.compute_attestation.agent_input_digest` in verified
records. Keep request field names and artifact binding paths distinct.

To link a Capsule to the one it derives from, add a `Chain` block to the
request `capsule` (same field spelling as `ActionID`/`ActionType`/
`Timestamp`): `"Chain": {"ParentCapsuleID": <parent capsule_id>, "Relation": <relation>}`.
Do not set `assurance` or `ledger_mode` in the request; the request capsule
rejects unknown fields, and supplying `Chain` derives `ledger_mode` to
`chained` automatically. In the sealed and verified Capsule this renders as
`assurance.ledger_mode: chained` and a `chain` block
`{parent_capsule_id, relation}`, committed into `capsule_id` under format 4,
so any later edit to the parent link fails verification. `Relation` states
how this Capsule relates to its parent and comes from an extensible registry
(seeded values `confirms`, `supersedes`, `epoch_opens`; namespaced extensions
such as `io.capsulectl.skill_step` are permitted and verify). `publish` runs a
store-level check that the parent already exists in the target CLL, so append
a Capsule only after its parent; a missing parent is a chain failure, not
success. The bare `seal` command (below) builds and signs the record to a
file without a database and does not run that parent-existence check — this
is exactly what lets a skill chain a run of read-only actions offline, before
any of them is durably published.

Publication returns capsule_id, sequence, state and target identifiers.
Pending/conflict is not success. Retry only the identical request, signer and
profile target and reconcile existing publication; never regenerate a
timestamp or signer after an uncertain append.

## Seal without a database

```sh
capsulectl seal --profile NAME --request REQUEST.json --output CAPSULE.json
```

Builds and signs a Capsule to a file — no store connection, no CLL append,
no parent-existence check. A `Chain` block in the request can point at a
prior `capsule_id` the same way `publish` accepts one (see above); the
output file is independently verifiable with `capsulectl verify` with no
store required, and can be appended to a CLL later with `cll append` if
durable persistence turns out to be wanted after all.

**capsulectl's own skill (`skills/capsulectl/`) does not call this
command.** Its evidence policy scopes capsule emission to consequential
actions only — a durable write, not a read or a check — and every verb in
its surface that only reads or checks (`verify`, `contract validate`,
`plugin ls`, `cll list`, `get`) is `not-consequential`: it runs and nothing
more, no wrapper capsule. `seal` exists here as a capability of the CLI
itself, in case a different skill's evidence policy someday needs it for a
verb that neither self-seals nor is a primary action; capsulectl's own
skill has no such verb today.

## Validate a contract

```sh
capsulectl contract validate FILE --schema PATH_OR_URL
capsulectl contract validate FILE --schema PATH_OR_URL --json
```

Validates FILE (JSON) against `--schema` (a JSON Schema, given as a path or an
`http(s)` URL) — capsulectl embeds no schema of its own; `--schema` always
names the caller's. Default output is human-readable (`FILE: valid` or
`FILE: INVALID` plus one line per issue); `--json` instead emits a
`capsule-cli-result/v1` report with `file`, `schema`, `valid` and a
structured `issues[]` (each `{path, message}`, `path` a JSON Pointer,
`<root>` for the document itself). Exit code 1 on an invalid document
(`ErrSchemaInvalid`), not a crash — treat exit 1 with a populated `issues[]`
as the validator working correctly, not as an operational failure.

## Discover (read-only inventory)

```sh
capsulectl discover --profile NAME --scope SCOPE.yaml --seal-output SCAN.json
capsulectl discover --profile NAME --scope SCOPE.yaml --effects --seal-output EFFECTS.json
capsulectl discover --profile NAME --scope SCOPE.yaml --format json --seal-output SCAN.json
```

Read-only inventory of OTel/MCP/gateway/CI/repo config. `--scope` is
**required** and operator-authored (a YAML file with a `roots: []` list, plus
an optional `deny: []` of extra exclusions on top of the built-in credential
deny-list); discover refuses to guess which directories it may read and
never invents a default. `--seal-output` is likewise **required** — a scan
always seals a capsule of exactly what it scanned (see `emits` below) —
because this verb is `self-sealing` and needs no wrapping.

Every scanned path is classified into a disposition: `scanned` (an
allow-listed config file, opened and digested — never its raw content),
`excluded_secret` (matched the built-in or scope-supplied deny pattern,
never opened), `excluded_symlink` (its resolved target is unresolvable or
escapes the scope root), or `present_not_read` (path and size recorded, bytes
never touched). Plain mode seals the full file list
(`{root, path, kind, disposition, digest_alg, digest, size_bytes, reason}`
per row) as `discover-scan/v1`. `--effects` instead extracts small, named
structural facts from already-scanned, already-allow-listed config content —
an MCP tool name, a `METHOD path` pair, a bus topic name, each tagged
`effect` or `observation` — and seals that list as `discover-effects-scan/v1`;
this is narrower than "digests of config": named structural facts about
config, never a secret's bytes, never a whole file's contents.

## Inspect plugins

```sh
capsulectl plugin ls
```

Read-only. Lists the trusted, handshake-valid `capsulectl-*` plugins
discovered on the trusted plugin roots (`{roots: [...], plugins: [{name,
vendor, version, plugin_api, subcommands, path}, ...]}`). There is
deliberately no install/enable/disable here — the core never mutates plugin
state; a plugin is installed onto a trusted root as a separate operator step,
outside this skill's authority.
