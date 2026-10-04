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
| `deal tick [--wait-up-to D]` | Run every minute from a timer (or every D with `--wait-up-to D`). When a tick is due (hourly with random jitter by default), cuts every deal's checkpoint locally, appends one entry to the cadence log and publishes its checkpoint to the witness; retries any delivery still pending. Deal steps never publish. |
| `deal open --input FILE` | Seals the baseline: the user's verbatim words, who, terms, claims (each with its source) and recourse. Cuts a checkpoint. |
| `deal note --kind message\|claim\|evidence\|change --input FILE` | Seals what happened. |
| `deal check --input FILE [--stale-after 15m]` | Seals a snapshot of what is about to happen, asks the four questions, seals the result, returns the difference card and the `approval_text` (with the check time and when it goes stale). |
| `deal note --kind approval --check ID --choice OPT` | Seals the user's answer to a check. Cuts a checkpoint. |
| `deal note --kind act --input FILE` | Seals what the agent did and whether a passing check or approval covered it. |
| `deal close --input FILE` | Compares what was delivered with what was agreed: `completed`, `mismatch` or `open`. Cuts a checkpoint. |
| `deal note --kind intent --input FILE` | Seals a change to what the user asked or allowed. |
| `deal note --kind evidence --email FILE [--key-record FILE]` | Seals a merchant's own email (raw RFC 822, headers intact) with the DKIM key records read from DNS at that moment, checks the signature, and cuts a checkpoint. See [The merchant's own email](#the-merchants-own-email). |
| `deal note --kind evidence --input FILE` with `obligation` | Seals a cancel-by date (a trial that becomes paid, a renewal, the end of free cancellation, a payment on a date) with its source: the merchant's email (with `--email`) or a page snapshot. Cuts a checkpoint. |
| `deal deadlines [--deal ID] [--ics FILE] [--remind-days N] [--all]` | Lists open cancel-by dates (on an open deal, or CARRIED AT CLOSE on a closed one) and open deals against their expected close date, for one deal or every deal in the profile, as JSON; `--all` adds resolved, cancel-sealed and passed dates; `--ics` also writes the open ones as a calendar file with reminders for the host's scheduler. Records, never enforces. |
| `deal close --carry-open-obligations` | Closes a deal whose cancel-by date is still open: the close seals the open dates, and they stay open after it. |
| `deal verify-email [--step N] [--email FILE]` | Re-checks a sealed merchant email offline, against the key records sealed with it. `--email` checks another copy, which must match the sealed bytes. Exit 1 when it does not verify. |
| `bundle --deal ID [--out FILE]` | The deal's Evidence Bundle, your own copy: the same file as `deal report --bundle` (the whole deal from its own log, `deal/<deal_id>`, with its cadence chain and any witness receipt held). Nothing withheld, nothing on record. |
| `disclose --deal ID --share counterparty\|adjudicator --to WHO --out FILE` | A bundle file for someone else: a share, exactly as `deal report --share` (see [Sharing a copy](#sharing-a-copy)). The copy withholds what that reader may not see, the final bytes pass the share gate, and the share is on record on `deal/<deal_id>/disclosures` before the file is written. A deal is not shared as a link: `permalink --deal` refuses and names the report file to hand over instead. |
| `deal export --output FILE` | Writes the sealed x-deal-v0 records (no raw values) as one JSON array. |
| `deal report [--html FILE]` | The three-part report: what you asked, what the agent did, anomalies on either side. `--html` writes it as one local page that checks itself. |
| `deal report --email FILE [--bundle FILE]` | Writes the receipt as a ready-to-send email (.eml, no sender or recipient) for the agent host's own email tool: a plain and a static HTML body that read on a phone, with `receipt.html` and `bundle.json` attached. `--bundle` writes the Evidence Bundle for `capsulectl verify --bundle`. Nothing is sent by capsulectl. |
| `deal countersign --bundle FILE --directory DIR [--entry FILE]` | Verifies (and with `--entry`, attaches) a countersignature you already hold over this deal's report bundle, and names its rung; a self-countersignature is shown as NOT INDEPENDENT. Sends nothing. See [COUNTERSIGN.md](COUNTERSIGN.md). |
| `deal report --from-bundle FILE --directory DIR` | Renders the receipt from that same bundle file, so a countersignature covers what it shows. Every receipt names its countersign rung; without one it says "Not countersigned". |
| `deal reconcile --executions FILE [--approvals FILE] [--from T] [--to T]` | Reads the agent host's execution records (tool calls of the agent and its sub-tasks, in the format in [RECONCILE.md](RECONCILE.md)) and lists each consequential action that has no deal record, with what the pass cannot see. Seals nothing; exits 3 when anything is unrecorded. |
| `deal report --html FILE --share counterparty\|adjudicator --to WHO` | A copy for someone else. It leaves out what that reader must not get, and seals a disclosure record of the share before the file is written. |

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
| `first_disclosure` | a share names a class of the user's data (`disclosing`) never given to this counterparty before |

Unverified claims and non-refundable terms are listed on the card but do not
make the verdict `pause` on their own.

**Pause on the recipient, not the field.** A share check that names what it
gives (`"disclosing": ["name","email","address"]`) is judged by who receives
it: the first time a class goes to a counterparty, the check pauses, naming
them and the class; a class given to them before is a note and no pause. It
does not ask whether the user named the share in their words: an address
typed into a merchant's delivery form for the tenth time never pauses, and
an address sent to a stranger always does. A `share_contact` check must name
what it gives. The disclosure is noted when the form is filled, before it is
sent: a class going to this counterparty for the first time that no check
the user approved named is held there (nothing is sealed, `"proceed":
false`, exit code 7), so leaving a class out of the check is no way around
its first telling. Every share check names who receives it,
`"disclosing_to": "counterparty"` or `"other"`; nothing defaults. An approval
covers a telling only to the party its check was about: a check names
someone other than the counterparty with `"disclosing_to": "other"` and a
`recipient` carrying a mechanical identity,
and an address approved for the seller is held when it is noted for a
courier. A repeat class the check did not name is sealed and flagged.

**Counterparty memory.** Who the user has dealt with, and what each was
told, is read from this device's own store across all deals: every deal's
opening and every sealed disclosure. A counterparty is known by mechanical
identities only (the website's registrable domain, an email address, a
phone number, a marketplace profile id, a reply address), never by name.
When either side has a per-party identity (an email, a phone, a profile id,
a reply address), only those decide: every seller on a marketplace shares
its domain, so a second seller there is a stranger. On a marketplace (the
deal's channel, or a known marketplace host such as facebook.com, ebay.com
or craigslist.org) the domain is never an identity. Elsewhere the
registrable domain decides only when it is all both sides have, as for a
merchant known by its website. The memory lives in this profile on this
device: a new or reinstalled profile has none, so every counterparty is
first-time. Its first pause says so once ("This profile has no record of
earlier counterparties yet, so everyone is first-time; this settles after
your first deals") and keeps the pause. A
counterparty with no earlier deal gets "First time dealing with …" on the
card. A check seals only rule tokens from the memory (`first_disclosure`,
`first_time_counterparty`, `repeat_disclosure`), never a name or a value.

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
- A tick is published when `deal tick` runs at or after its due time, so
  run `deal tick` **every minute**: a run that is not due exits at once, and
  publication then lands within a minute of the jittered due time. A
  scheduler that runs it less often publishes on its own grid, which hides
  the jitter and can push a tick to a later run. Where the host can only
  schedule every N minutes, use `deal tick --wait-up-to Nm`: each run waits,
  with the store unlocked, for every tick due inside its window, and
  publishes it on time. Such a run stays alive up to N (an hour for
  `--wait-up-to 60m`): on a host that caps how long a scheduled job may run,
  use a smaller N or schedule every minute instead.
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

A deal is **not witnessed at the moment it happens**. Its receipt says
"Sealed, witness pending" until the next cadence tick has carried its
checkpoint to the witness and a receipt has come back; only then does it say
"Witnessed". How long that is depends on the profile's cadence
(`cadence.interval`, give or take `cadence.jitter`; 1h and 10m by default),
and the receipt names the profile's own, for example "every 5m, give or take
2m". The states, as
the receipt and `deal report` name them: **scheduled** (not yet in a tick),
**pending** (in a tick, no receipt back yet; both read "Sealed, witness
pending") and **witnessed**.
Witnessed means the bundle carries the whole chain: the deal checkpoint's
leaf, its salt and position, the 16-hash path (the same length for every
deal, so it says nothing about the others), the cadence entry's inclusion
proof, the cadence checkpoint (signed by the same key as the deal
checkpoint) and the witness receipt. `capsulectl verify --bundle FILE
--witness-directory DIRECTORY.json` checks every link.

## Report

`deal report` reads the sealed steps into four parts:

1. **What you asked**: the user's exact words, from the opening step.
2. **What the agent did**: every check (and the user's answer to it), every
   action and the close, in order.
3. **What your agent told whom**: every sealed disclosure, in order: what
   kind of thing (phone, address, pickup location, a login, ...), to whom,
   when, and the sealed approval that covered it, or that none did. The
   user's own copy shows what was given; a shared copy names only the kind
   of thing and the fact, never the value, and names the recipient by role.
   It lists what the agent sealed as told; a telling it never sealed is not
   in it.
4. **Anomalies**, on either side:
   - agent side: `asked_vs_did` (tried something not asked, or did something
     other than what was checked), `skipped_check`, `unsealed_approval`
     (went ahead without a sealed proceed), `unapproved_disclosure` (told
     someone about the user with no covering approval);
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

The page also has a **What this does not claim** block: it is
tamper-evident, not non-repudiation; it records what the agent reported; it
does not prove the merchant shipped. It names its countersign rung: "Not
countersigned" unless the file carries a countersignature that capsulectl
verified, and NOT INDEPENDENT for one by the producer's own key (see
[COUNTERSIGN.md](COUNTERSIGN.md)). And it gives one command anyone can run on
the file, offline: `capsulectl verify --bundle receipt.html` (`verify
--bundle` reads the page's embedded bundle). `deal report` prints the scope
line as `scope`.

### Sharing a copy

`--html` alone writes the user's own copy (audience `keep`: nothing withheld). To
hand a copy to someone else, name the reader:

```sh
capsulectl --profile deal deal report --deal ID --html receipt.html \
  --share counterparty --to "the shop's support desk"
```

`--share` writes the page alone. `--email` and `--bundle` are the user's own
copy, with nothing withheld, so they cannot be combined with `--share`.

| Audience | What the copy carries |
|---|---|
| `keep` (default) | Nothing withheld. No disclosure record: it is the user's own copy. |
| `counterparty` | Amounts, rails, timestamps and digests only. No home address, no names, payees or contact details, no card or payment identifiers, no verification codes, no message text, no claim text or sources, none of the user's own words, and not the user's spending limit (a record that carries it is withheld; the merchant section leaves out an "approved" amount that is only that limit; the gate refuses it as the field or as money unless it equals the asked price or an amount paid). |
| `adjudicator` | The counterparty copy plus message text and claim text with sources, and the spending limit. Codes, card numbers, phones, emails and street addresses are replaced with `[withheld]`. |

A sealed record is disclosed whole or not at all, so a shared copy withholds
every record holding a string it may not carry. That record still verifies:
it shows as WITHHELD, and its place in the log is still proven. The deal
section is rewritten for the reader from fixed words, amounts and rails.
In the adjudicator copy, codes are withheld wherever they sit: inside a word
(`G739142`, `code739142`) or split by a space or dash (`739 142`). Any piece
of the sealed place or address (`Larkspur`, `Springfield`, `41`) is withheld
wherever it appears in message text, in any order.

Both the scrubber and the gate read text in the same plain form:

- Escapes are decoded, nested ones too: percent-escapes, HTML character
  references and `\uXXXX` escapes (surrogate pairs included).
- Characters that draw nothing are removed. These are the format characters
  (Cf: zero-width characters, the soft hyphen, the bidirectional controls)
  and the other default-ignorable ones, such as the Hangul fillers U+115F,
  U+1160, U+3164 and U+FFA0.
- NFKD is applied and every nonspacing mark (Mn) is dropped: `Làrkspur`,
  `Laŗkspur` and `73̧9142` read `Larkspur` and `739142`.
- Every decimal digit of any script becomes the ASCII digit of the same
  value: `٧٣٩١٤٢` and `७३९१४२` read `739142`.
- Letters are matched through the TR39 confusables skeleton, from Unicode's
  official `confusables.txt` (version 18.0.0). The file is vendored
  unmodified in `third_party/unicode/`, with only its mappings compiled in
  (`internal/cli/confusables_table.go`, generated by
  `scripts/genconfusables`). A test checks the file's digest and
  regenerates the table. Refresh both with `scripts/update-confusables.sh
  VERSION`. TR39 skeletons are
  case-sensitive, so each character is read as written, lower-cased and
  upper-cased, and a match takes any one reading at each position:
  `Larkspսr`, `ᏞᎪᎡkspur` and `SPRINGFIELD` all read as the place.
- A non-ASCII prototype takes one step through case to the ASCII letter
  that a case variant of it, or of a character mapped to it, has as
  prototype, so `Larᴋspur` reads `Larkspur`. The step never repeats: a
  transitive closure under case merges `r`, `e`, `t`, `u` and `y`.

A value is read by its letters and digits alone, forwards and backwards, so
whatever stands between the characters does not hide it (`L-a-r-k-s-p-u-r`,
`7/3/9/1/4/2`, `rupskraL`). Runs of four or more number words are read as
digits (`seven three nine one four two`). A base64 or hex run that decodes to
text is read for what it holds. A number of six digits or more counts
wherever it stands; a shorter one only where no digit adjoins it, so `1200`
is not found in the amount `120000`. Dates, times and money amounts
(`09/27/2026`, `October 3, 2026`, `18:00`, `$1,200.00`) are kept. In the
adjudicator copy, message text appears in the plain form.

The tests:

- 28 fixed transforms of the planted address, code and card number, each in
  its own deal end to end; every one is withheld.
- The listed lookalike and digit leaks, end to end.
- A seeded randomized property test that combines confusables, marks,
  digits of other scripts, separators, case, reversal and encodings. The gate
  alone must refuse every rewriting, and the scrubber's output of it must
  pass the gate. `DEAL_LEAK_SEED` and `DEAL_LEAK_ROUNDS` widen it.
- A Go fuzz target, `FuzzDealSharePageGate`, whose found inputs are kept in
  `internal/cli/testdata/fuzz`.

Before the file is written, a last gate reads the **final page bytes** with
its own detectors. It reads the deal's local steps itself and shares no
detector with the scrubber. It looks for every place and
address fragment, identifier, payment reference, code, card number and email
in the deal's local store. It checks each one as written, in any case, digits
only, with separators removed, base64 (all four alphabets), hex, and URL-,
JSON- and HTML-escaped. It also looks for any Luhn-valid card number and any
code word (code, OTP, PIN, passcode) followed by a number. A short value is
skipped only inside a digest or signature (a run of 64 or more hex or base64
characters). In an order reference or a URL path it counts. A hit refuses the
copy: nothing is written and nothing goes on record.

**A merchant's own email** (sealed with `deal note --kind evidence --email`)
is private material too. Its order number, the names and addresses in its
headers, its text and its items are all withheld. Each shared copy shows the
signature's verdict in fixed words, without the signing domain (that is the
counterparty's), and the amounts and dates. The counterparty's copy also
carries the order number, but only where `shareableOrderID` allows it: an
order number (never a booking, confirmation or reservation code) from an
email whose signature checks out and was signed by the deal's own
counterparty. The gate takes exactly that value out before it checks. The
adjudicator's copy carries no order number, and also lists the items.

**Sharing is on record.** Before the file exists, a disclosure record is
sealed. It names the root, the payloads mode, the records withheld, the
audience, the recipient (`--to`), what the copy leaves out, and the SHA-256
of the exact page. It is kept in the local store (`deal_disclosures`), and
its digest is appended to the deal's own disclosure log
(`deal/<deal_id>/disclosures`) under a fresh signed checkpoint. That log sits
beside the deal's log, which holds only steps. Nothing is hosted and there
are no accounts or links. The user hands over the file.

**A second opinion without trusting the page.** The report page checks
itself, and says so. A reader who would rather not take its word opens
verify.agentactioncapsule.org and drops the file in: the report page (its
embedded bundle is read out of it) or a bundle `.json`. That verifier checks
it in the reader's own browser; the file is never uploaded.

## Sizes

Measured on a synthetic retail deal (`demo/retail-checkout/`; the live
numbers depend on each deal's steps, and vary a little from run to run with
each record's nonces and times):

| Deal | Own bundle (`bundle --deal`) |
|---|---|
| 4 steps (open, then a passing check: snapshot, check, approval) | about 14,570 B |
| 5 steps (the same, then the act) | about 17,870 B |
| Merchant deal, 5 steps (open, a passing check, the merchant's own email) | about 19,460 B |
| Merchant deal, 6 steps (the same with the act before the email) | about 22,420 B |

A shared copy carries the merchant's email only as its digests
(`message_digest`, `key_records_digest`) and the verification result (DKIM,
DMARC, whether the signer is the deal's counterparty) with the amounts and
dates read from it: never a header, the body or a name. The raw `.eml` stays
in the local deal store, where `deal verify-email` re-checks it; no bundle
carries it.

Most of the self-contained `deal report --html` page is the embedded
verifier, not evidence: of a 221,847-byte page for the 5-step deal,
194,730 B is the vendored `evidence-graph.iife.js` and 7,488 B is
`deal-view.js` (91%).

## Which build made a record, and what is never collected

Every sealed step names the build that sealed it, in the record itself
(`x-deal-v0.producer`: name, version, commit). A development build says so.
The report's `produced_by`, the assurance line ("Produced by capsulectl
<version> (<commit>)") and the receipt page show it. The page compares it
with the version of the capsulectl that made the page, from data already in
the file. If the record is older it says so ("Produced by an older version
... What changed:") and links the release notes. It fetches nothing to
decide.

Nothing phones home. capsulectl collects nothing and sends no version, usage
or error data anywhere. The only things that leave the device are what the
user turns on or chooses to share: the witness checkpoints (hashes), the
optional remote checker, and receipts the user sends or publishes.

What the public witness can and cannot show about versions: a witness holds
cadence checkpoints, which carry a log id, a size, a root, a time and the
signing key id. They carry no record content, so the version that produced a
deal is NOT visible from the witness. It is visible only in records someone
chooses to disclose: a receipt or bundle the user shares or publishes.

## Which instructions were present

`deal open --skill FILE` (or `CAPSULE_DEAL_SKILL`) takes the SKILL.md the
agent says it is following. It seals the file's SHA-256 in the deal's
baseline, with how many other copies of the same skill (the same `name:`)
sit in the folders beside it. The paths stay on this device. The report, the
page and the assurance line say "Instructions present when the deal opened:
deal SKILL.md sha256:...", and two cases are anomalies:

- `skill_copies_visible`: another copy of the skill was visible, for example
  an old backup left inside the skills folder, which the host may load
  instead of the current one;
- `skill_changed`: the digest differs from the previous deal's.

It is a boundary marker, not a detector. The agent computes it, so it catches
accidents (a stale backup the host loaded, a partial upgrade, skill files that
drifted from the binary), not a dishonest agent. It records which
instructions were present, never that the agent followed them. A deal opened
without `--skill` says that this was not recorded.

## Records: the x-deal-v0 profile

[`profile/`](profile/) is the deal record profile: `PROFILE.md` (normative),
the JSON Schema, fixtures and `check_profile.py`. It ships with the skill and
is not registered anywhere.

- Each step is sealed as one x-deal-v0 record: `{"x-deal-v0": {...}, "body":
  {...}}`, in JCS bytes, with `prev` and `baseline_ref` chaining the record
  digests. Record types: `baseline`, `intent`, `message`, `claim`,
  `evidence`, `detail_change`, `check`, `verdict`, `approval`, `action`,
  `outcome`, `close`, `disclosure`.
- A `disclosure` records what the agent told someone about the user: the
  class of each field and a salted commitment to its value, the recipient
  (`counterparty` or `other`, with fingerprints when new), and the approval
  that covered it, or `authority: "none"` with the rule that failed. The
  value stays in the local store. A ledger of disclosures must never itself
  be a disclosure.
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

## Closing a deal, and records that arrive later

A deal is closed when it reaches its point of resolution (a parcel at
delivery, a flight at ticketing, a stay after the stay, a rental when it is
returned, a service when the work is done), and evidence that arrives after
that is linked to the closed deal instead of holding it open.

- **Linked, not appended.** A record sealed after a deal's final close is a
  new evidence record whose Capsule chains to the close with the registered
  `chain.relation` `confirms` (agent-action-capsule REGISTRY.md §6:
  non-terminal; it records an outcome of the parent, whose state stands).
  Ordinary steps keep `follows`, which is ordering only: verifiers must not
  read it as confirming anything, so a late record never uses it. The record
  also commits to the close record's digest, not only its id, so it cannot be
  reattached to another deal. It shows that whoever sealed it held that deal;
  it does not show the deal expected it. A closed deal takes nothing else.
- **Closing is proposed, not remembered.** Every deal carries an expected
  close date (`expect_close_by`, sealed in the baseline; by default 14 days
  for a purchase, 1 for a booking, 7 for a rental, 30 for a service).
  `deal deadlines` lists open deals against it under `open_deals`, and
  `--ics` writes a "Close deal …?" event into its file, so the host's own
  tracking can surface a deal nobody closed.
- **The receipt shows the chain, or says it may be incomplete.** Every
  receipt (page, email, JSON, and shared copies, in fixed words) says whether
  the deal is open or closed, lists the records linked after the close, and
  says that more can be linked after it was made and how to see them (make a
  new report). A deal never closed reads "Open: no close is sealed on this
  deal", with its expected date and, once that has passed, "which has passed
  (as of …)".
- **Carried cancel-by dates.** `deal close` still refuses while a cancel-by
  date is open. `--carry-open-obligations` closes anyway: the close record
  seals the open dates (`carried_obligations`), and they stay open after the
  close, listed as CARRIED AT CLOSE (in `deal deadlines`, and in the title of
  their calendar event, with the deal's id). A later record that confirms the
  close resolves one (`resolves_step`); otherwise, once the date passes, it
  reads "The date passed (as of …). No cancellation confirmation is sealed on
  this deal." "Passed" is computed when the listing or receipt is made, never
  sealed, and every receipt states what the deal holds, never what anyone did
  or did not do.

## Cancel-by dates and proving a cancellation

Some points of no return are a date passing, not an action: a free trial that
becomes $24.00/month on the 17th unless cancelled by the 16th. The deal seals
that obligation when it is created, with its source (the merchant's own
email, which `deal note --email` proposes it from when the email says nothing
is charged before a date, or a snapshot of the page). Every `deal check`
lists the deal's open dates, and `deal deadlines` emits them as JSON (the
interface: hand them to the host's own goals, tasks or scheduler) or, as a
convenience, an iCalendar file with an alarm. No daemon runs here.
**We record the deadline; we do not enforce it, and nothing is cancelled for
the user.**

**We emit; we never place.** `--ics` writes a file and nothing else:
capsulectl never writes to a calendar, never asks for calendar access, and
holds no calendar credential. A tool about agents committing users to things
without their approval must not quietly change the user's calendar; the user
or the host imports the file if they choose. The sealed date is ours and
durable; a reminder is the host's and best-effort. So a receipt may say the
date was recorded, and never that the user was reminded. `deal close` refuses while a date
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

**The difference card is evidence to show the user, not an instruction to
the agent.** The agent is told to show it verbatim because paraphrase
destroys its value as a record, not because the card has authority over the
agent.

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
