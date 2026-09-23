# Skill spec format v0

A skill spec is data, not prose: a small YAML document validated against
[`schemas/skill-spec-v0.json`](../schemas/skill-spec-v0.json) that a generator
renders into whichever shape a given host agent reads (a Claude Code
`SKILL.md`, a Codex `AGENTS.md`, ...). The spec is the single source of truth;
the rendered files are build output, never edited by hand.

This exists because skill vocabulary — what a skill may do, must never do,
where it must stop for a human, and which commands it is allowed to call —
previously lived only as prose inside one skill file. Prose drifts from the
tool it describes and cannot be validated. A schema-checked spec can be.

## Why a schema, and not a schema-plus-free-text-body

A generated skill file may (and usually does) carry the standard prose a
Claude Code `SKILL.md` or Codex `AGENTS.md` needs — a title, a reference link,
worked examples. What this schema owns is narrower and load-bearing: the
closed verb surface and the three guardrail lists. If those five fields are
right, a skill cannot reason its way past its own boundary or call a command
that does not exist; everything else is presentation.

## The boundary this format exists to enforce

A **thin, tool-calling skill** — the shape this format is for — may call
verbs, validate a contract, request evidence, verify a bundle, or inspect a
result. It never reasons about business meaning, invents a workflow beyond
that verb list, or authors its own guardrail wording. Contrast a
**reasoning-heavy** skill (a compiler that decomposes a value proposition
into axes and writes new instructions) — that shape does not fit this
schema and should not be forced into it. See `evaluation-compiler`'s
`SKILL.md` for what the opposite end of that spectrum looks like; it is
correct for what it does, and wrong for a thin skill to imitate.

## Fields

`spec_version` — always the literal string `skill-spec/v0`.

`skill.name` / `skill.description` — become the rendered `SKILL.md`
frontmatter. `name` is lowercase-hyphenated; `description` is one line.

`may[]`, `must_never[]`, `approval_points[]` — the guardrail block, in the
skill's own words. **Human-authored, not generator-authored.** A skill whose
gate says guardrails are `HUMANS-WRITE-FIRST` ships these arrays holding the
literal placeholder string `"TBD-STEVEN"` until the real wording arrives
through the PM; the generator renders that placeholder verbatim rather than
inventing text to fill the gap. `approval_points` may be an empty array (a
skill can legitimately have none) but the key itself is always present.

`verbs[]` — the skill's entire action surface, one entry per command the
underlying tool actually implements **today**. Each entry:

- `name` — the command as typed, e.g. `contract validate`.
- `args[]` — one string per accepted invocation shape, in the tool's own flag
  syntax.
- `when` — the single condition under which the skill calls it. A directive
  ("confirm X before treating it as authentic"), not a rationale for why X
  matters.
- `emits` — which capsule kind (or kinds) the invocation results in.
- `emission_mode` — one of three values, because "every skill-driven action
  emits a capsule" is satisfied three different ways depending on what the
  verb already does:
  - `self-sealing` — the verb has its own `--seal-output` flag; nothing more
    is needed.
  - `primary-action` — the verb's entire job already is creating or chaining
    a capsule (`publish`, `cll append`); running it satisfies the invariant
    by itself.
  - `skill-wraps` — the verb only reads or checks something and has no seal
    of its own, so the skill's own steps must call the base `seal` command
    over the verb's JSON result, chained (`Chain.ParentCapsuleID`) to the
    previous action in the same run. This is what turns a run of read-only
    commands into one verifiable chain with no database required.

## The regeneration contract

Given one spec, the generator (`cmd/skillgen`) must produce byte-identical
output on every run: same spec in, same `SKILL.md` and `AGENTS.md` out, no
timestamps or nondeterministic ordering baked into the rendered files. This
is what lets CI enforce the only two moves that are ever legitimate on a
generated skill:

1. **Edit the spec, regenerate.** Both rendered files change together,
   because both come from the same data.
2. **Edit a rendered file by hand.** `cmd/skillgen --check` fails, because
   the checked-in file no longer matches what the spec renders. Fix the spec
   instead, or the generator's template if the shape itself is wrong.

There is no third move. A rendered `SKILL.md` or `AGENTS.md` is build output;
treat a hand-edit to one exactly like a hand-edit to a compiled binary.

## A minimal instance

```yaml
spec_version: skill-spec/v0
skill:
  name: example
  description: One line describing what this skill does and when to use it.
may: ["TBD-STEVEN"]
must_never: ["TBD-STEVEN"]
approval_points: []
verbs:
  - name: verify
    args: ["--profile NAME", "--capsule FILE"]
    when: Confirm a Capsule's identity, signature and bindings before trusting it.
    emits: skill-verify-report/v1
    emission_mode: skill-wraps
```
