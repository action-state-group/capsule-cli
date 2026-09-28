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
| `deal note --kind intent --input FILE` | Seals a change to what the user asked or allowed. |
| `deal export --output FILE` | Writes the sealed x-deal-v0 records (no raw values) as one JSON array. |
| `deal report [--html FILE]` | The three-part report: what you asked, what the agent did, anomalies on either side. `--html` writes it as one local page that checks itself. |

Each step is a Capsule in the profile's store. Each deal has its own
checkpointed log (`deal/<deal_id>`) in the same SQLite file, and each step
cites the one before it (`chain.parent_capsule_id`, relation `follows`). On
every read, each step's signature, payload binding and chain link are
verified again, so a dropped or edited step is reported as a conflict.

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

## Report

`deal report` reads the sealed steps into three parts:

1. **What you asked**: the user's exact words, from the opening step.
2. **What the agent did**: every check (and the user's answer to it), every
   action and the close, in order.
3. **Anomalies**, on either side:
   - agent side: `asked_vs_did` (tried something not asked, or did something
     other than what was checked), `skipped_check`, `unsealed_approval`
     (went ahead without a sealed proceed);
   - counterparty side: `changed_identifier`, `channel_hop`,
     `deadline_pressure`, `code_request`, `domain_recent`,
     `unverified_claim`, `delivered_differs`.

   Every cause a check paused on is an anomaly, in the card's own words
   (read from the check's sealed differences, the same source as the card):
   besides the kinds above, `terms_changed`, `recourse_changed` (payment
   method or refundability changed since agreed), `irreversible_rail`, and
   on the agent side `pay_before_seeing` and `credentials_requested`. When a
   check restates an item read from an earlier step (a changed payee, a
   code request), the two are one item that expands to both steps.

Every item lists the steps it was read from. `--html FILE` writes the report
as one local, self-contained page, built on this machine: nothing is hosted
and no link is minted. The page embeds an AAC Evidence Bundle of the deal's
own log (every step, its membership proof, the chain back to the opening
step, the signed checkpoint) and agent-action-capsule's own emitter and
browser verifier, vendored unmodified in `internal/cli/assets/` (rebuild and
compare with `scripts/build-evidence-graph-iife.sh`). Each item expands to
its steps, and a step's line is shown only if the verifier matched its record
against the step's seal; the words shown come from this device's local
store. If a byte was changed, the page says "This report did not verify"
instead. Message text appears only when an anomaly cites that message.

## Records: the x-deal-v0 profile

[`profile/`](profile/) is the deal record profile: `PROFILE.md` (normative),
the JSON Schema, fixtures and `check_profile.py`. It ships with the skill and
is not registered anywhere.

- Each step is sealed as one x-deal-v0 record: `{"x-deal-v0": {...}, "body":
  {...}}`, in JCS bytes, with `prev` and `baseline_ref` chaining the record
  digests. Record types: `baseline`, `intent`, `message`, `claim`,
  `evidence`, `detail_change`, `check`, `verdict`, `approval`, `action`,
  `outcome`, `close`.
- Counterparty identifiers are sealed only as per-deal HMAC fingerprints of
  their normalized form; text (the user's words, messages, the card, notes)
  only as salted commitments. The raw values, the per-deal keys, the store
  secret and the nonces stay in the local store (`deal_steps`, `deal_keys`,
  `deal_store` in the profile's SQLite file).
- Every action cites a sealed approval. A passing check is approved by the
  user's standing intent, sealed as its own step. An action without one is
  sealed as an `unchecked_action` outcome.
- Before sealing, each record is validated against the schema and scanned
  for raw phone numbers, emails, local values and grading words; a hit
  refuses the step. On every read, each local step is re-derived and must
  equal the sealed record bytes.
- `deal export --deal ID --output FILE` writes the sealed records as one JSON
  array; `python3 profile/check_profile.py FILE` checks them. The Go code
  that builds the records is `internal/cli/deal_profile.go`.

## Guarantee

**Tamper-evident, not non-repudiation.** The signing seed is a 0600 file on
the same machine as the agent. A later edit or deletion of a sealed step is
detectable; the trail does not prove who, the user or the machine, said
something.

## Demo

`demo/jet-ski/` is a scripted DEMO rental with a payee switch to Zelle. Run
`scripts/run-demo.sh [report.html]` (set `CAPSULECTL` to use a release
binary instead of building from source). The card it must produce is in
`demo/jet-ski/expected-card.txt`; the Go tests assert the same card.
