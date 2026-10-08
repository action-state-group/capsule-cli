# The external-check contract, v0

How a program that consumes records (here, `capsulectl deal check`) asks an external rules checker
for a verdict on one proposed action, and reads its answer. capsulectl parses only this contract,
never a checker's own format.

- [`input.schema.json`](input.schema.json), `external-check-input/v0`: what the checker reads on
  stdin, one JSON object holding:
  - `record`: the record of the proposed action;
  - `history`: earlier sealed acts in the same shape;
  - `history_scope`: the days covered, the maximum number of records, and whether the history is
    complete.
- [`result.schema.json`](result.schema.json), `external-check-result/v0`: what it prints on
  stdout, one JSON object holding:
  - `ruleset_id` and `definition_digest`;
  - `verdict`: `allow`, `deny`, `escalate` or `not_evaluable`;
  - `tier`: `recomputed`, `judged` or `human`. An absent tier reads as `judged`;
  - `judge`: the pin of the model, required when anything is judged;
  - `findings[]`: each with an id, its verdict and an optional tier. `limit` and `value` are
    numbers or short strings.

**Exit status.** 0 means a result was printed, whatever its verdict. Any other exit is a refusal of
the input: the cause goes on stderr, and no result is printed.

**Versioning.**
- **Additive changes are a new minor revision of v0,** recorded in the revisions list below: a new
  optional member, or a new enum value. A reader validating strictly accepts them once it vendors
  the revision.
- **A breaking change is v1,** at `../v1/`: removing or renaming a member, a member becoming
  required, a tightened constraint, or a changed meaning.
- **Released files are not edited in place.**

Revisions: v0.0, the first.

**Copies.** capsulectl embeds `result.schema.json` byte for byte (`internal/cli/assets/`), and a
test keeps the two identical. A checker that emits this contract should vendor these files byte for
byte and test its output against them.
