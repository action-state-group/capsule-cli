# Expert verbs

The capsulectl skill ([SKILL.md](../SKILL.md); [AGENTS.md](../AGENTS.md) for
Codex) teaches five verbs: check, record, ask, verify and show. The verbs here
sit outside the five; their instructions are kept as they were taught before.
The skill's guardrails bind them, and its approval points still stop you
before the actions they name. This file is generated from `spec.yaml`
(`expert_verbs`); never edit it by hand.

| Command | Args | When | Emits | How |
|---|---|---|---|---|
| `contract validate` | `FILE --schema PATH_OR_URL`<br>`FILE --schema PATH_OR_URL --json` | Check a caller-supplied document against a JSON Schema before accepting, forwarding or acting on it. `contract validate` uses no built-in schema; --schema always names the caller's. | (no capsule — this is the local validation the evidence policy names explicitly) | not consequential — no capsule |
| `contract diff` | `A B`<br>`A B --schema PATH_OR_URL --json` | Before adopting a new version of an Evidence Contract, see whether the change is breaking (evidence that satisfied A may not satisfy B, or a claim against A no longer names a requirement of B) and why. Exit 1 means breaking; results evaluated against A must be re-evaluated. | (no capsule — comparing two local files is a check, not a consequential action) | not consequential — no capsule |
| `discover` | `--profile NAME --scope FILE --seal-output FILE`<br>`--profile NAME --scope FILE --effects --seal-output FILE`<br>`--profile NAME --scope FILE --format table\|json --seal-output FILE` | Build a read-only inventory of OTel/MCP/gateway/CI/repo config (or, with --effects, an effect-boundary map) strictly within the roots an operator named in --scope. Never invents a scope; refuses to run without one. | discover-scan/v1 (plain) or discover-effects-scan/v1 (--effects) | self-sealing (`--seal-output`) |
| `plugin ls` | _(none)_ | List trusted, handshake-valid capsulectl-* plugins discovered on the trusted plugin roots before invoking one by name. When a command says a plugin is not installed, stop and show the user the exact command it prints (such as `capsulectl plugin install ./<plugin file>`); never run `plugin install` or `plugin remove` yourself. | (no capsule — listing what is discovered is local inspection, not a consequential action) | not consequential — no capsule |
| `judge pin` | `FILE` | Compute a judge_pin_digest from a reproducible judge configuration (model id, model version, sampling params, prompt digest, axes digest) before citing it into an evaluation-report/v1 the skill is about to seal. | (no capsule — this is a pure computation over caller-supplied input) | not consequential — no capsule |
| `judge drift pin` | `FILE_A FILE_B` | Confirm two judge-pin inputs describe the same reproducible judge configuration before treating their outputs as comparable. | (no capsule — this is local comparison, not a consequential action) | not consequential — no capsule |
| `judge drift reports` | `FILE_A FILE_B` | Compare two evaluation-report/v1 sets by case_id and show drift as a real delta (pin mismatch, label mismatch, or both) — never as a silent disagreement the skill quietly drops. | (no capsule — this is local comparison, not a consequential action) | not consequential — no capsule |
| `calibration summarize` | `REPORTS_FILE RATINGS_FILE` | Fold an evaluation-report/v1 set and a human-rating/v1 set into k-of-n agreement per judge_pin_digest before citing the result into a calibration-summary/v1 the skill is about to seal. | (no capsule — this is a pure computation over caller-supplied input) | not consequential — no capsule |

[capsule-cli.md](capsule-cli.md) gives the full contract of the commands it
documents, with their output and exit codes; `capsulectl <command> --help`
gives every flag of each.
