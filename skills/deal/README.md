# deal skill

An agent skill ([SKILL.md](SKILL.md)) and the `capsulectl deal` command group
behind it. Before an agent pays, books, signs or shares on a user's behalf, it
checks the step against what the user asked and who they are really dealing
with, and shows the difference.

## Commands

| Command | What it does |
|---|---|
| `deal init --dir DIR` | Creates a SQLite deal profile: store plus signing and checkpoint seeds, each mode 0600. |
| `deal open --input FILE` | Seals the baseline: the user's verbatim words, who, terms, claims (each with its source) and recourse. Cuts a checkpoint. |
| `deal note --kind message\|claim\|evidence\|change --input FILE` | Seals what happened. |
| `deal check --input FILE` | Seals a snapshot of what is about to happen, asks the four questions, seals the result, returns the difference card. |
| `deal note --kind approval --check ID --choice OPT` | Seals the user's answer to a check. Cuts a checkpoint. |
| `deal note --kind act --input FILE` | Seals what the agent did and whether a passing check or approval covered it. |
| `deal close --input FILE` | Compares what was delivered with what was agreed: `completed`, `mismatch` or `open`. Cuts a checkpoint. |
| `deal report` | Prints the sealed trail in plain words. The offline HTML receipt is a later release. |

Each step is a Capsule in the profile's store, appended to its checkpointed
log, and carries the previous step's `capsule_id`. On every read, each step's
signature, payload binding and chain link are verified again, so a dropped
or edited step is reported as a conflict.

Concurrent writers (sub-agents) are serialized by a lock file beside the
store. `deal` commands run on Linux and macOS.

## Local rules

The four questions (asked, same who, same terms, claims and recourse) plus
these fixed safety rules:

| Rule | Pauses when |
|---|---|
| `payee_or_contact_changed` | a payee, name, website, phone, email, reply address or profile differs from first contact |
| `irreversible_rail` | paying by Zelle, wire, gift card, crypto, or another rail with no chargeback |
| `pay_before_seeing` | a purchase is paid before the item was seen |
| `verification_code_request` | the counterparty asked for a verification code |
| `off_platform_early` | the counterparty asked to move off the platform |
| `domain_recent` | the counterparty's website is under 90 days old |
| `credentials_requested` | the action is sharing a login or code |

Unverified claims and non-refundable terms are listed on a pause card but do
not pause on their own.

## What leaves the machine

Nothing, by default.

- **Witness (optional).** When the profile has a checkpoint endpoint, signed
  checkpoints are offered to it at three milestones only: baseline, approval
  and close. Checkpoints hold hashes, not content.
- **Remote checker (optional).** When `CAPSULE_DEAL_CHECK_URL` is set
  (HTTPS, or HTTP on loopback), `deal open` sends `POST /v1/warm` and
  `deal check` sends `POST /v1/check` with minimal fields: deal type, action,
  website domain, amount, currency, rail, refundable, website age, and the
  local rule ids that fired. Never message text, names, contact details or
  card numbers. `CAPSULE_DEAL_CHECK_TOKEN`, if set, is sent as a bearer
  token. The checker can only add differences. A remote pass never clears a
  local pause, and after 2 seconds, or on any error, the local rules decide
  alone.

## Guarantee

**Tamper-evident, not non-repudiation.** The signing seed is a 0600 file on
the same machine as the agent. A later edit or deletion of a sealed step is
detectable; the trail does not prove who, the user or the machine, said
something.

## Demo

`demo/jet-ski/` is a scripted DEMO rental with a payee switch to Zelle. Run
`scripts/run-demo.sh` (set `CAPSULECTL` to use a release binary instead of
building from source). The card it must produce is in
`demo/jet-ski/expected-card.txt`; the Go tests assert the same card.
