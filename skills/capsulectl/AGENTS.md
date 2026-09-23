# capsulectl agent skill

Call the capsulectl verb surface to inspect, validate and record Agent Action Capsules — never to reason about business meaning or invent a workflow beyond the verbs listed below.

This skill may call verbs, validate a contract, request evidence, verify a bundle, or inspect a result — nothing else. It never reasons about business meaning, and never invents a workflow beyond the verb surface below.

## Guardrails

**May:**

- TBD-STEVEN

**Must never:**

- TBD-STEVEN

**Approval points:**

- TBD-STEVEN

## Verb surface

| Verb | Args | When | Emits | How |
|---|---|---|---|---|
| `verify` | `--profile NAME`<br>`--capsule FILE` | Confirm a Capsule's identity, signature, trust and bound artifacts before treating it as authentic — including a record another verb in this same run just fetched with `get --raw`. | skill-verify-report/v1 | skill wraps with `capsulectl seal` |
| `contract validate` | `FILE --schema PATH_OR_URL`<br>`FILE --schema PATH_OR_URL --json` | Check a caller-supplied document against a JSON Schema before accepting, forwarding or acting on it. capsulectl embeds no schema of its own; --schema always names the caller's. | skill-contract-check/v1 | skill wraps with `capsulectl seal` |
| `discover` | `--profile NAME --scope FILE --seal-output FILE`<br>`--profile NAME --scope FILE --effects --seal-output FILE`<br>`--profile NAME --scope FILE --format table\|json --seal-output FILE` | Build a read-only inventory of OTel/MCP/gateway/CI/repo config (or, with --effects, an effect-boundary map) strictly within the roots an operator named in --scope. Never invents a scope; refuses to run without one. | discover-scan/v1 (plain) or discover-effects-scan/v1 (--effects) | self-sealing (`--seal-output`) |
| `plugin ls` | _(none)_ | List trusted, handshake-valid capsulectl-* plugins discovered on the trusted plugin roots before invoking one by name. | skill-plugin-inventory/v1 | skill wraps with `capsulectl seal` |
| `cll list` | `--profile NAME`<br>`--profile NAME --after N --through N --limit N` | Read the profile's checkpointed log in sequence order — to find an entry, to page through a range, or to confirm an append landed. | skill-cll-snapshot/v1 | skill wraps with `capsulectl seal` |
| `cll append` | `--profile NAME --capsule FILE` | Add an already-sealed, already-verified Capsule (one with no missing original bindings) to the log as a new entry. | (the appended Capsule itself is the record; no wrapper capsule) | primary action (running it *is* the record) |
| `get` | `--profile NAME --capsule-id ID`<br>`--profile NAME --capsule-id ID --raw --output FILE` | Read an existing artifact SDK record by capsule_id — plain for inspection, --raw when the exact bytes must be handed to verify. | skill-get-report/v1 | skill wraps with `capsulectl seal` |
| `publish` | `--profile NAME --request FILE` | Seal a capsule-seal-request/v1, persist its artifacts, and append it to the CLL in one step — the primary way the skill records that it took an action with a real, durable effect. | (publish's own result IS the capsule; no wrapper) | primary action (running it *is* the record) |

## Capsule emission

Every verb this skill calls leaves a capsule, by one of three mechanisms
(the `emission_mode` column above says which applies):

- **self-sealing** — the verb has its own `--seal-output` flag (`discover`).
  Nothing further is needed.
- **primary action** — the verb's entire job already is creating or chaining
  a capsule (`publish`, `cll append`). Running it satisfies the
  invariant by itself.
- **skill wraps** — the verb only reads or checks something and has no seal of
  its own. After running it, build a `capsule-seal-request/v1` whose
  `payload` is the verb's own JSON result, add a `Chain` block pointing
  at the previous action's `capsule_id` in this same run (when there is
  one), and call:

  ```sh
  capsulectl seal --profile NAME --request REQUEST.json --output CAPSULE.json
  ```

  This needs no store connection and no CLL append; it produces a file
  independently verifiable with `capsulectl verify`. Append it to a CLL
  later with `cll append` only when durable persistence is actually wanted.

See [references/capsule-cli.md](references/capsule-cli.md) for the full
request shape and the Chain/Relation fields.

## Exercise this skill

[scripts/run-scripted-demo.sh](scripts/run-scripted-demo.sh) runs every
verb above against a throwaway jsonl profile in a temp directory, seals or
publishes one capsule per action, verifies each one, and asserts the count
matches the verb surface — the "fresh-environment" test this skill ships
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
