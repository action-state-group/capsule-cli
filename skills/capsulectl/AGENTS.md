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

## The five verbs

This skill teaches five verbs and nothing else: **check** before you act,
**record** what was done, **ask** for evidence or answer a request, **verify**
what you get, and **show** a record or report. Each row maps a verb to the
command that carries it out.

| Verb | Command | Args | When | Emits | How |
|---|---|---|---|---|---|
| record | `publish` | `--profile NAME --request FILE` | Seal a capsule-seal-request/v1, persist its artifacts, and append it to the CLL in one step — the primary way the skill records that it took an action with a real, durable effect. | (publish's own result IS the capsule; no wrapper) | primary action (running it *is* the record) |
| record | `cll append` | `--profile NAME --capsule FILE` | Add an already-sealed, already-verified Capsule (one with no missing original bindings) to the log as a new entry. | (the appended Capsule itself is the record; no wrapper capsule) | primary action (running it *is* the record) |
| record | `result build` | `--profile NAME --result FILE`<br>`--profile NAME --result FILE --contract REF --out FILE --capsule-out FILE` | Seal a caller-supplied, schema-valid Evidence Result v0 into the profile's log as the Capsule a Result page is rooted on, citing every record its claims name. Refuses a document whose headline values do not recompute from its claims, or that cites a record the profile's log and artifact store do not hold. A repeat of the same document seals nothing new. On a book profile it is the book plugin's `book result build`. | (the Result's Capsule itself; --capsule-out writes it for verify) | primary action (running it *is* the record) |
| verify | `verify` | `--profile NAME`<br>`--capsule FILE` | Confirm a Capsule's identity, signature, trust and bound artifacts before treating it as authentic — including a record another verb in this same run just fetched with `get --raw`, or a bare capsule another implementation sealed (its shape is read from the file and named in the output). | (no capsule — checking a bundle is local validation, not a consequential action) | not consequential — no capsule |
| show | `cll list` | `--profile NAME`<br>`--profile NAME --after N --through N --limit N` | Read the profile's checkpointed log in sequence order — to find an entry, to page through a range, or to confirm an append landed. | (no capsule — reading the log is local inspection, not a consequential action) | not consequential — no capsule |
| show | `get` | `--profile NAME --capsule-id ID`<br>`--profile NAME --capsule-id ID --raw --output FILE` | Read an existing artifact SDK record by capsule_id — plain for inspection, --raw when the exact bytes must be handed to verify. | (no capsule — reading a record is local inspection, not a consequential action) | not consequential — no capsule |

Check and ask have no command in this skill. Book and deal work, where an agent checks before it acts and asks a party for evidence, is taught by the agent skill that ships with the plugin adding those commands.

**Expert verbs.** `contract validate`, `contract diff`, `discover`, `plugin ls`, `judge pin`, `judge drift pin`, `judge drift reports`, `calibration summarize` are verbs of this file too, but are
not taught here: read [references/expert.md](references/expert.md), which
keeps each one's instructions, before you call one. The guardrails above bind
them as they bind the five.

## Capsule emission

**Capsules are for consequential actions only** (see Evidence policy
under Guardrails, above). A capsule is never manufactured for `verify`
or any other local validation — reading, checking, validating or opening
something is not a consequential action, and this skill produces no capsule
for it. The How column above says which mechanism applies to each verb:

- **primary action** — the verb's entire job already is creating or persisting
  a capsule (`publish`, `cll append`, `result build`). Running it
  satisfies the invariant by itself; nothing wraps it.
- **not consequential** — `verify`, `cll list`, `get`: each runs, and that is the end of it —
  **no wrapper seal, no synthetic evidence record.**

The expert verbs follow the same policy; [references/expert.md](references/expert.md)
says how each one does.

See [references/capsule-cli.md](references/capsule-cli.md) for the `publish`/
`cll append` request shape and the Chain/Relation fields, and for the bare
`capsulectl seal` command this skill does not use (it exists for a skill
whose verb surface actually needs it — this one does not).

## Exercise this skill

[scripts/run-scripted-demo.sh](scripts/run-scripted-demo.sh) runs every
verb above and in [references/expert.md](references/expert.md) against a
throwaway sqlite profile in a temp directory: the
consequential actions (`publish`, `cll append`, `result build`, `discover`) each seal or persist a capsule and every one of those is
verified; the not-consequential
actions (`verify`, `cll list`, `get`, `contract validate`, `contract diff`, `plugin ls`, `judge pin`, `judge drift pin`, `judge drift reports`, `calibration summarize`)
run and produce no capsule at all — the script asserts both halves, not just
that capsules exist. It never touches a real profile or a real CLL. It is a
contributor check, run after any change to the verb surface or the emission
mechanism: it builds `capsulectl` from a source checkout, so it needs Go
and this repository. Using the skill needs neither: install a release binary
and this skill from the same tag (the repository README, "Agent skill").

## Reference

- [Expert verbs](references/expert.md) — the verbs this skill may call
  but does not teach, each with its instructions.
- [Capsule CLI contract](references/capsule-cli.md) — the full command
  reference this skill is built against.
- [skills/SKILL-SPEC.md](https://github.com/action-state-group/capsule-cli/blob/main/skills/SKILL-SPEC.md) — the spec format this skill's
  `spec.yaml` is written against, and the regeneration contract that
  keeps this file and `AGENTS.md` byte-identical to what it renders.
