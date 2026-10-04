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
| `deal note --kind evidence --email FILE [--key-record FILE]` | Seals a merchant's own email (raw RFC 822, headers intact) with the DKIM key records read from DNS at that moment, checks the signature, and cuts a checkpoint. See [The merchant's own email](#the-merchants-own-email). |
| `deal note --kind evidence --input FILE` with `obligation` | Seals a cancel-by date (a trial that becomes paid, a renewal, the end of free cancellation, a payment on a date) with its source: the merchant's email (with `--email`) or a page snapshot. Cuts a checkpoint. |
| `deal deadlines [--deal ID] [--ics FILE] [--remind-days N] [--all]` | Lists open cancel-by dates, for one deal or every deal in the profile, as JSON; `--ics` also writes them as a calendar file with reminders for the host's scheduler. Records, never enforces. |
| `deal verify-email [--step N] [--email FILE]` | Re-checks a sealed merchant email offline, against the key records sealed with it. `--email` checks another copy, which must match the sealed bytes. Exit 1 when it does not verify. |
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
- A delivery whose request never reached the witness (refused, unresolved,
  timed out, or stopped by a proxy) is reported as "pending: network consent
  needed, or no network", in `deal tick`'s output and on the receipt: on an
  agent host that asks before network access, that is what a missing grant
  looks like. Ticks run unattended, so choose "Always allow this site" for
  the witness's site, not "allow once". That grant covers the site and all
  its subdomains: for the default witness, witness.agentactioncapsule.org,
  it covers agentactioncapsule.org and every subdomain of it. The message
  names the site for whichever witness the profile uses, and
  `capsulectl doctor --check-witness` says the same.

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

## The merchant's own email

What the user asked, what was proposed and what the user approved are rightly
recorded by the user's own agent: nobody else saw them. What was *done* is
different, and needs a source outside the agent. A merchant's
confirmation email is different: the merchant's mail server signs it with a
DKIM key published in the merchant's DNS, and that signature survives being
passed along by an agent nobody has to trust.

So a sealed merchant email carries two statements, and the report never
merges them:

| | Who stands behind it | What it shows |
|---|---|---|
| **The merchant's signature** | The merchant's domain (DKIM) | The merchant sent this email, and it has not changed since. Independent of the agent and of this device. |
| **Our seal** | This device's signing key | This device held these exact bytes at this time. Our own record. |

Only a passing signature that is DMARC-aligned with the sender's domain (the
same organizational domain, or the exact domain when the domain sets
`adkim=s`), verified by our own offline check, reads as **merchant-confirmed**.
Everything else is still sealed, never refused (a failing message is still
evidence of what the user received), and reads **not confirmed**, with the
reason and the domain's published policy, for example "not confirmed: fails
DMARC alignment (domain policy p=reject)". A signature from a mailing service
on another domain shows who relayed the email, not that the merchant sent
it. The sender's DMARC record (`_dmarc.<domain>`, then its organizational
domain's) is read from DNS at seal time and sealed with the keys, so the
policy shown later comes from the sealed record. SPF is not evaluated: it
depends on the connecting server, which a received message cannot prove.

**Keys rotate and are revoked.** A signature that checks out today can be
uncheckable next month, once the merchant has replaced its key. `deal note
--email` therefore reads the key record (`selector._domainkey.domain`) from
DNS when the email is sealed, seals it alongside the message, and cuts a
checkpoint at once. When a witness is configured, that checkpoint reaches it
at the next due tick (`deal tick`), like every deal checkpoint, normally
within one cadence interval plus its jitter; until the witness is reached the
receipt reads pending.
`deal verify-email` then checks against the sealed key only, never against
live DNS. **Sealing promptly is the whole protection**: an email sealed after
its key was withdrawn cannot be verified. The key is read from ordinary DNS,
which is not itself signed; the checkpoint fixes when it was read.
`--key-record` (and optionally `--dmarc-record`) supplies records by hand (for tests, or a machine with no
resolver); the record says `key_source: supplied`, and the report says the key
was not read from the merchant's DNS.

DKIM verification is [`github.com/emersion/go-msgauth/dkim`](https://github.com/emersion/go-msgauth)
(MIT), with the key lookup answered from the sealed records. capsulectl
implements no cryptography of its own here.

**What is sealed where.** The raw message stays in the local store. The
sealed record carries the message's SHA-256, the key records' SHA-256, where
the key came from, the DKIM result, whether the merchant's own domain signed
it, and the amounts and dates read from the email (`parsed.method:
heuristic`). The order number is committed, not written. Addresses, names
and the message text never enter a record.

A copy shared with someone else may carry the order number only when all of
these hold: it was found under the word "order", the email's signature checks
out against the sealed key, the From domain signed it, that domain is the
deal's own counterparty, and the copy is the counterparty's. Booking,
confirmation, reservation and PNR-style codes are never shared: with a
surname they work like a password.

**Scope, and what counts as independent.** Each email result says it covers
one email from the merchant about this deal, not everything the merchant
sent or charged. Only a merchant-confirmed email counts as an independent
source for what the agent did: the receipt's line on what the agent did
names it, and a sealed email that is not confirmed is not named.

**The mismatch.** The report sets each sealed merchant email beside what the
user approved (the amount at the check that went ahead, else the user's
limit, else the agreed price) and what the agent reported paying. It flags:

- a charge other than the one approved (over the limit, when only a limit is known);
- a possible duplicate: the same amount under two different orders, or paid twice;
- a charge dated before a cancel-by date when the email says nothing is charged before then (a trial);
- a quantity other than the one agreed.

The order number, total, cancel-by date and items are read by
merchant-agnostic heuristics and may be misread; the report labels them as
read from the email. The merchant's signature covers the bytes, not this
reading of them.

## Cancel-by dates and proving a cancellation

Some points of no return are a date passing, not an action: a free trial that
becomes $24.00/month on the 17th unless cancelled by the 16th. The deal seals
that obligation when it is created, with its source (the merchant's own
email, which `deal note --email` proposes it from when the email says nothing
is charged before a date, or a snapshot of the page). Every `deal check`
lists the deal's open dates, and `deal deadlines` emits them as JSON or an
iCalendar file with a reminder, for the host's own scheduler or the user's
calendar. No daemon runs here. **We record the deadline; we do not enforce
it, and nothing is cancelled for the user.** `deal close` refuses while a date
is open (close with status `pending` meanwhile), because closing would end the
record that holds it. A date that passed with no cancel sealed reads `passed`.

"I cancelled on the 4th" needs evidence. A sealed `cancel` action (checked and
approved like any other point of no return) plus the merchant's own
cancellation email, sealed on the same deal, is the strongest record this
produces. The report's "Your cancellation" section says exactly:

- **what is shown:** a cancel recorded at a time (this device's clock, fixed by
  the next witnessed checkpoint when a witness is configured); whether that
  time is on or before the cancel-by date; and, when the merchant's own
  cancellation email is merchant-confirmed, that the merchant says the
  cancellation went through, and when;
- **what is not:** that no later charge will come (only the merchant's records,
  or the user's statement, can show that); that the merchant acted beyond what
  its email says; and, with no confirmed email, that the merchant received the
  cancel at all.

An email that does not check out is named as such, with its DMARC policy, and
is never counted as the merchant's confirmation.

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
