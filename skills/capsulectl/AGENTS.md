# capsulectl agent skill

Call the capsulectl verb surface to inspect, validate and record Agent Action Capsules — never to reason about business meaning or invent a workflow beyond the verbs listed below.

This skill may call verbs, validate a contract, request evidence, verify a bundle, or open a result — nothing else. It never reasons about business meaning, and never invents a workflow beyond the verb surface below.

## Guardrails

**May:**

- call only the verbs listed in this file
- validate a contract
- run `discover` against a scope file supplied by the operator
- request evidence
- verify a bundle
- open a result

**Must never:**

- create or modify a scope file, Evidence Contract, or licence
- invent a relationship between a requirement and an evidence source
- modify, summarize, reinterpret, or restate the output of `verify`
- describe self-attested evidence as witnessed, countersigned, or independently verified
- transmit data off-machine without first showing exactly what will be transmitted

**Approval points:**

- Stop and ask a human before the first witness registration from a new key.
- Stop and ask a human before any publish operation.
- Stop and ask a human before `discover` traverses a root not already authorized by the scope file.
- Stop and ask a human whenever a command fails because of a licence, plugin, or authorization requirement.

**Evidence policy:**

Every consequential action taken by this skill must produce an evidence record. If evidence generation fails, report `evidence unavailable`, stop before the next consequential action, and do not infer that the underlying action failed to occur.

## Verb surface

| Verb | Args | When | Emits | How |
|---|---|---|---|---|
| `verify` | `--profile NAME`<br>`--capsule FILE` | Confirm a Capsule's identity, signature, trust and bound artifacts before treating it as authentic — including a record another verb in this same run just fetched with `get --raw`. | (no capsule — checking a bundle is local validation, not a consequential action) | not consequential — no capsule |
| `contract validate` | `FILE --schema PATH_OR_URL`<br>`FILE --schema PATH_OR_URL --json` | Check a caller-supplied document against a JSON Schema before accepting, forwarding or acting on it. capsulectl embeds no schema of its own; --schema always names the caller's. | (no capsule — this is the local validation the evidence policy names explicitly) | not consequential — no capsule |
| `discover` | `--profile NAME --scope FILE --seal-output FILE`<br>`--profile NAME --scope FILE --effects --seal-output FILE`<br>`--profile NAME --scope FILE --format table\|json --seal-output FILE` | Build a read-only inventory of OTel/MCP/gateway/CI/repo config (or, with --effects, an effect-boundary map) strictly within the roots an operator named in --scope. Never invents a scope; refuses to run without one. | discover-scan/v1 (plain) or discover-effects-scan/v1 (--effects) | self-sealing (`--seal-output`) |
| `plugin ls` | _(none)_ | List trusted, handshake-valid capsulectl-* plugins discovered on the trusted plugin roots before invoking one by name. | (no capsule — listing what is discovered is local inspection, not a consequential action) | not consequential — no capsule |
| `cll list` | `--profile NAME`<br>`--profile NAME --after N --through N --limit N` | Read the profile's checkpointed log in sequence order — to find an entry, to page through a range, or to confirm an append landed. | (no capsule — reading the log is local inspection, not a consequential action) | not consequential — no capsule |
| `cll append` | `--profile NAME --capsule FILE` | Add an already-sealed, already-verified Capsule (one with no missing original bindings) to the log as a new entry. | (the appended Capsule itself is the record; no wrapper capsule) | primary action (running it *is* the record) |
| `get` | `--profile NAME --capsule-id ID`<br>`--profile NAME --capsule-id ID --raw --output FILE` | Read an existing artifact SDK record by capsule_id — plain for inspection, --raw when the exact bytes must be handed to verify. | (no capsule — reading a record is local inspection, not a consequential action) | not consequential — no capsule |
| `publish` | `--profile NAME --request FILE` | Seal a capsule-seal-request/v1, persist its artifacts, and append it to the CLL in one step — the primary way the skill records that it took an action with a real, durable effect. | (publish's own result IS the capsule; no wrapper) | primary action (running it *is* the record) |
| `judge pin` | `FILE` | Compute a judge_pin_digest from a reproducible judge configuration (model id, model version, sampling params, prompt digest, axes digest) before citing it into an evaluation-report/v1 the skill is about to seal. | (no capsule — this is a pure computation over caller-supplied input) | not consequential — no capsule |
| `judge drift pin` | `FILE_A FILE_B` | Confirm two judge-pin inputs describe the same reproducible judge configuration before treating their outputs as comparable. | (no capsule — this is local comparison, not a consequential action) | not consequential — no capsule |
| `judge drift reports` | `FILE_A FILE_B` | Compare two evaluation-report/v1 sets by case_id and show drift as a real delta (pin mismatch, label mismatch, or both) — never as a silent disagreement the skill quietly drops. | (no capsule — this is local comparison, not a consequential action) | not consequential — no capsule |
| `calibration summarize` | `REPORTS_FILE RATINGS_FILE` | Fold an evaluation-report/v1 set and a human-rating/v1 set into k-of-n agreement per judge_pin_digest before citing the result into a calibration-summary/v1 the skill is about to seal. | (no capsule — this is a pure computation over caller-supplied input) | not consequential — no capsule |

## Capsule emission

**Capsules are for consequential actions only** (see Evidence policy
under Guardrails, above). A capsule is never manufactured for `result open`
or any other local validation — reading, checking, validating or opening
something is not a consequential action, and this skill produces no capsule
for it. The `emission_mode` column above says which of the two real
mechanisms applies to an actually-consequential verb:

- **self-sealing** — the verb has its own `--seal-output` flag (`discover`,
  mandated by the binary itself). Nothing further is needed.
- **primary action** — the verb's entire job already is creating or persisting
  a capsule (`publish`, `cll append`). Running it satisfies the
  invariant by itself; nothing wraps it.

Every other verb this skill calls (`verify`, `contract validate`,
`plugin ls`, `cll list`, `get`) is `not-consequential`: it runs, and
that is the end of it — **no wrapper seal, no synthetic evidence record.**

See [references/capsule-cli.md](references/capsule-cli.md) for the `publish`/
`cll append` request shape and the Chain/Relation fields, and for the bare
`capsulectl seal` command this skill does not use (it exists for a skill
whose verb surface actually needs it — this one does not).

## Exercise this skill

[scripts/run-scripted-demo.sh](scripts/run-scripted-demo.sh) runs every
verb above against a throwaway jsonl profile in a temp directory: the
consequential actions (`discover`, `publish`, `cll append`) each seal or
persist a capsule and every one of those is verified; the not-consequential
actions (`verify`, `contract validate`, `plugin ls`, `cll list`, `get`)
run and produce no capsule at all — the script asserts both halves, not just
that capsules exist. This is the "fresh-environment" test this skill ships
with. It never touches a real profile or a real CLL. Run it after any change
to the verb surface or the emission mechanism; it builds `capsulectl` from
source, so its own elapsed time is also this skill's install-through-first-
use timing.

## Reference

- [Capsule CLI contract](references/capsule-cli.md) — the full command
  reference this skill is built against.
- [skills/SKILL-SPEC.md](../SKILL-SPEC.md) — the spec format this skill's
  `spec.yaml` is written against, and the regeneration contract that
  keeps this file and `AGENTS.md` byte-identical to what it renders.
