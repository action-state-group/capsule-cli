# deal skill

An agent skill ([SKILL.md](SKILL.md)) and the `capsulectl deal` command group
behind it. Before an agent pays, books, signs or shares on a user's behalf, it
checks the step against what the user asked and who they are really dealing
with, and shows the difference.

Invocation is advisory, not enforced. The check seals what was asked,
proposed, approved and done, and the host elects to call it; its `pause`
verdict is advice to the host, not a lock. It holds an action only where the
host runs `deal check` from a pre-action hook. Without one:

- **`deal reconcile`** ([RECONCILE.md](RECONCILE.md)) reads the host's own
  execution records, the tool calls of the agent and of every sub-task it
  started, and lists each consequential action that has no deal record, with
  what it cannot see printed on its face. It does not depend on the agent
  remembering anything.
- **`deal check` writes the approval text.** Every check returns
  `approval_text`: what, who, the amount, the rail, what the check found, the
  check time and when it goes stale. An agent that asks the user with that
  text has run the check.
- **The skill is written as procedures** (purchase, booking, signature,
  disclosure) whose final-review step is `deal open` plus `deal check`. The
  trigger is the action, never the counterparty: a retail checkout from a
  known merchant at a fixed price is in scope. This helps once the skill is
  in use; it does not make the host invoke it.

## Commands

| Command | What it does |
|---|---|
| `deal init --dir DIR [--no-witness]` | Creates a SQLite deal profile: store plus signing and checkpoint seeds, each mode 0600, and the profile's cadence log. The public witness is configured by default. |
| `deal tick` | Run from a timer. When a tick is due (hourly with random jitter by default), cuts every deal's checkpoint locally, appends one entry to the cadence log and publishes its checkpoint to the witness; retries any delivery still pending. Deal steps never publish. |
| `deal open --input FILE` | Seals the baseline: the user's verbatim words, who, terms, claims (each with its source) and recourse. Cuts a checkpoint. |
| `deal note --kind message\|claim\|evidence\|change --input FILE` | Seals what happened. |
| `deal check --input FILE [--stale-after 15m]` | Seals a snapshot of what is about to happen, asks the four questions, seals the result, returns the difference card and the `approval_text` (with the check time and when it goes stale). |
| `deal note --kind approval --check ID --choice OPT` | Seals the user's answer to a check. Cuts a checkpoint. |
| `deal note --kind act --input FILE` | Seals what the agent did and whether a passing check or approval covered it. |
| `deal close --input FILE` | Compares what was delivered with what was agreed: `completed`, `mismatch` or `open`. Cuts a checkpoint. |
| `deal note --kind intent --input FILE` | Seals a change to what the user asked or allowed. |
| `deal export --output FILE` | Writes the sealed x-deal-v0 records (no raw values) as one JSON array. |
| `deal report [--html FILE]` | The three-part report: what you asked, what the agent did, anomalies on either side. `--html` writes it as one local page that checks itself. |
| `deal report --email FILE [--bundle FILE]` | Writes the receipt as a ready-to-send email (.eml, no sender or recipient) for the agent host's own email tool: a plain and a static HTML body that read on a phone, with `receipt.html` and `bundle.json` attached. `--bundle` writes the Evidence Bundle for `capsulectl verify --bundle`. Nothing is sent by capsulectl. |
| `deal reconcile --executions FILE [--approvals FILE] [--from T] [--to T]` | Reads the agent host's execution records (tool calls of the agent and its sub-tasks, in the format in [RECONCILE.md](RECONCILE.md)) and lists each consequential action that has no deal record, with what the pass cannot see. Seals nothing; exits 3 when anything is unrecorded. |

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

| Rule | The verdict is `pause` when |
|---|---|
| `payee_or_contact_changed` | a payee, name, website, phone, email, reply address or profile differs from first contact |
| `irreversible_rail` | paying by Zelle, wire, gift card, crypto, or another rail with no chargeback |
| `pay_before_seeing` | a purchase is paid before the item was seen |
| `verification_code_request` | the counterparty asked for a verification code |
| `off_platform_early` | the counterparty asked to move off the platform |
| `domain_recent` | the counterparty's website is under 90 days old |
| `credentials_requested` | the action is sharing a login or code |

Unverified claims and non-refundable terms are listed on the card but do not
make the verdict `pause` on their own.

## What leaves the machine

Content never leaves the machine. By default, one thing does: a checkpoint of
hashes, once a tick.

- **Witness (on by default; `deal init --no-witness` turns it off).** See
  "Witness cadence" below. The witness sees hashes and a checkpoint size
  that grows by the same amount every tick, at a time on the profile's
  cadence: never content, how many deals there are, or when they happen.
- **Remote checker (optional).** When `CAPSULE_DEAL_CHECK_URL` is set
  (HTTPS, or HTTP on loopback), `deal open` sends `POST /v1/warm` and
  `deal check` sends `POST /v1/check` with minimal fields: deal type, action,
  website domain, amount, currency, rail, refundable, website age, and the
  local rule ids that fired. Never message text, names, contact details or
  card numbers. `CAPSULE_DEAL_CHECK_TOKEN`, if set, is sent as a bearer
  token. The checker can only add differences. A remote pass never clears a
  local `pause`, and after 2 seconds, or on any error, the local rules decide
  alone.

## Witness cadence

Checkpoints are checkpoints of a log at a size, not a registration of each
record: the witness never receives a record, a record id or a deal id. And
no deal's own log is ever published. If it were, the witness would learn how
many deals there are (one log each), when each starts, and how many steps
each has. Instead the profile has one cadence log (its `log_id`), and
`deal tick`, run from a timer, publishes on time alone:

- A tick is due at the previous tick plus `cadence.interval` (default `1h`)
  moved by a random amount within `cadence.jitter` (default `10m`). Deal
  activity never brings a tick forward, and an explicit
  `cll checkpoint publish` is the only other way anything reaches the
  witness.
- At a tick, every deal's checkpoint is cut locally and becomes a leaf of a
  fixed-depth (16) Merkle tree, with a fresh random salt at a fresh random
  position, plus one random filler leaf. The tree's root is appended as
  exactly one entry of the cadence log, and the cadence log's checkpoint is
  published. Ticks happen whether or not anything happened.
- So the witness sees one log that grows by one entry per tick. This reduces
  the volume signal to the tick count, and the timing signal to the
  cadence; it does not remove what the cadence itself shows (that the
  device was on to run a tick). With `cadence.pad_bucket` above 1, each tick
  also appends Evidence Layer padding records (`record_type: "padding"`,
  only a fresh random value) until the leaf count is a multiple of it. The
  padding is never an action and never counted, and nothing that reads a
  deal reads it.
- A witness that is slow or down never stops a deal. The delivery stays
  pending and is retried at every `deal tick`, or by hand with
  `capsulectl --profile deal cll checkpoint publish --checkpoint SIZE`.

A deal's receipt states its witness state as it is: **scheduled** (not yet
in a tick), **pending** (in a tick, no receipt back yet) or **witnessed**.
Witnessed means the bundle carries the whole chain: the deal checkpoint's
leaf, its salt and position, the 16-hash path (the same length for every
deal, so it says nothing about the others), the cadence entry's inclusion
proof, the cadence checkpoint (signed by the same key as the deal
checkpoint) and the witness receipt. `capsulectl verify --bundle FILE
--witness-directory DIRECTORY.json` checks every link.

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

   Every cause a check flagged is an anomaly, in the card's own words
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

When a witness receipt covers the checkpoint the report carries (through the
cadence chain above, re-checked against the pinned witness key when the
report is made), the page and the email say "Witnessed" instead of "Sealed
by my agent", and otherwise say whether it is scheduled or pending. The page does not
check the receipt itself and says so; `capsulectl verify --bundle FILE
--witness-directory DIRECTORY.json` does, against a witness directory the
reader chooses. Making a report never contacts the witness.

The page and the email both say, on their face, "This receipt covers this
one deal. It is not a record of everything the agent did." They also say
that what was asked, proposed and approved is sealed where it happened, while
what the agent did is the agent's own report until an independent source is
attached.

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

**Tamper-evident against ourselves and the agent, not non-repudiation.** The
signing seed is a 0600 file on the same machine as the agent. A later edit or
deletion of a sealed step is detectable, and once a tick has been witnessed,
not even this device can rewrite what it had sealed by then. The trail does
not prove who, the user or the machine, said something. It covers this
skill's own records only: the agent host's own store is not covered, and a
change there is not detected.

## Demo

`demo/jet-ski/` is a scripted DEMO rental with a payee switch to Zelle. Run
`scripts/run-demo.sh [report.html]` (set `CAPSULECTL` to use a release
binary instead of building from source). The card it must produce is in
`demo/jet-ski/expected-card.txt`; the Go tests assert the same card.

`demo/retail-checkout/` is a DEMO retail order from a known merchant at a
fixed price, where nothing differs. The user's request does not name the
skill. The Go tests walk the Purchase procedure in SKILL.md step by step
against it: its final-review step seals the deal, the check passes quietly,
and a `deal reconcile` over the host's execution records for the same period
lists no unrecorded action.
