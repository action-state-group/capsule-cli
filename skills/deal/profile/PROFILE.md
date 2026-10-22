# x-deal-v0: the deal record profile

**Status: skill-local draft, v0. Not registered.** This profile ships inside the deal skill
(`capsule-cli/skills/deal/profile/`). It has no capsule-registry entry and no CPB artifact-type
entry, and none is proposed here. Registration is a separate item, taken up after the deal check
works end to end. Until then the name `x-deal-v0` and the artifact type name `deal-record` are
local to the skill.

Files in this directory:

| File | What it is |
|---|---|
| `PROFILE.md` | This document. The normative text for the skill. |
| `x-deal-v0.schema.json` | JSON Schema (Draft 2020-12) for one record: the block and each record type's body. |
| `records/` | JSON Schemas (Draft 2020-12) for the typed action records and the check request and response (section 9). |
| `fixtures/positive/` | One deal, 19 records, covering most record types (not `sale`, which `positive-sale/` carries; the fixture run of `check_profile.py` names the types no positive fixture carries yet), with the expected JCS digest of each. |
| `fixtures/positive-typed/` | One deal sealed in the typed action records beside x-deal-v0 evidence records (section 9). |
| `fixtures/positive-typed-seller/` | A seller's typed deal that gives a listing platform the public pickup area (`pickup_area`) before any offer. |
| `fixtures/negative/` | Records that MUST be rejected, each at one named stage. |
| `fixtures/fingerprint-vectors.json` | Test-only local store: a public test secret, the raw values, their normalized forms, fingerprints and commitments. |
| `fixtures/expected-digests.txt` | The positive digests as a manifest. |
| `check_profile.py` | The checker. Stdlib only; uses `jsonschema` when installed. |

The key words MUST, MUST NOT, SHOULD and MAY are used as in BCP 14 (RFC 2119, RFC 8174).

## 1. What a deal record is

A deal is a sequence of sealed steps. Each step is one **deal record**: a JSON object with
exactly two members.

```json
{ "x-deal-v0": { ...the extension block, section 2... },
  "body":      { ...shaped by record_type, section 3... } }
```

- Each record is the payload the skill seals into one Agent Action Capsule (AAC format 4,
  `canonicalization_id: "jcs"`). The Capsule commits to the record by its **record digest**
  (section 5). In capsulectl this is the payload binding: `agent_input_digest` = the AAC
  JSON-DIGEST of the payload, which is by construction the record digest. This profile changes nothing in the Capsule: `action_type`, `effect`,
  `disposition`, `chain` and `references` keep their AAC meaning. v0 seals each step with
  `action_id: "<deal_id>/<seq>"`, and chains each Capsule to the previous one with
  `chain.relation: "follows"`. Each step's Capsule also carries the deal id alone as its
  top-level `deal_id` member (a string, the same `deal_id` the records carry). The two parties
  to a deal (`deal open --deal-id`) seal different steps, but both seal this member, so a
  composition of their copies joins on it: a composed/v1 `pre_agreed_identifier` join with
  `pointer: "/deal_id"` over each copy's root step. Steps sealed by an earlier capsulectl
  carry no `deal_id`, and a join over them derives `unjoined`. A `follows` link is ordering only.
  A step of a deal opened since the deal store became an evidencebook shelf (capsule-cli#242)
  is also a record of the deal's own evidence book, so its Capsule carries one more top-level
  member, `evidence_record_header`: the record header the book assigned. It holds:
  - `record_type` `deal_step` for every step, whatever its kind;
  - `epistemic_type` `producer_claim`;
  - `committed_at`, the record's own `at`;
  - `subject_ref` `<deal_id>/<seq>`;
  - one `cites` link to the step's chain parent, when it has one (none for the baseline);
  - the record digest as its one payload commitment;
  - three salted field commitments (`sha256-jcs-nonce256`). `deal_local` is the step's local
    event, whose opening never leaves the device. `amount` and `currency` carry a received final
    close's money terms, and the empty text on every other step. Section 10 says where their
    openings go.

  The deal record itself is unchanged, so a verifier that ignores the member reads the deal as
  before. The Capsule claims only what the record shows:
  - An `action` record (an act a sealed approval authorized) whose action has an AAC
    `effect.type` is `action_type: "decide"`: `pay`, as `send_payment` (registered), and the
    other acts, with these **provisional** effect types (proposed for
    agent-action-capsule's REGISTRY section 3, "Provisional: commitment effect types": held for
    ratification and not yet registered; provisional here until they are registered):
    - `offer` as `make_offer` (provisional)
    - `commit` as `accept_agreement` (provisional)
    - `sign` as `sign_document` (provisional)
    - `cancel` as `cancel_commitment` (provisional)

    Until then each is an unregistered value, which a verifier treats as informational under
    the registry's never-reject invariant, never as a rejection. Its
    `disposition` is `accept` and `executed`, by `human` (with `human_disposed`) only when the
    user approved it in their own words (`approver: user`, with `said_commitment`), or by
    `policy` when a passing check approved it under their standing intent. An act approved by
    `agent_card` is not certified by anyone and stays `fyi`. In a deal sealed in typed records
    (section 9), an `action-record/v0`'s disposition is read from its own `authority_basis`,
    the canonical authority field, and `decide`/`fyi` is only its presentation: `human` (with
    `human_disposed`) when a `user_approval` is a layer, `policy` when the `task_authority`
    alone covered it (a `DO`). A `platform_approval` is that platform's own check and changes
    neither; any other basis stays `fyi`. Its `effect` is `dispatched` and `runtime_claimed` (the agent's
    report, with no response in hand), so `effect_mode` is `dispatched_unconfirmed`;
    `one_way_recoverable` when the agreed recourse is refundable, else
    `one_way_consequential`; an `offer` is `two_way` (it binds nothing until accepted, and a
    later offer supersedes it).
  - An `offer`, `commit`, `sign` or `cancel` record, which names no payee, seals the
    counterparty of the check it rests on, exactly as that check sealed it: the header `counterparty` in `x-deal-v0`, `body.counterparty` in a
    typed `action-record/v0`. A rule keying earlier acts on who they were with reads it from
    the sealed record. One sealed before this release names none, and re-derives unchanged.
  - Every other step is `action_type: "fyi"`: disclosures have no registered `effect.type`
    yet, and an unchecked act is sealed as an `outcome`, never as an action. Steps sealed by an
    earlier capsulectl are all `fyi`, and an `offer`, `commit`, `sign` or `cancel` sealed before
    this release stays `fyi`, and re-derives unchanged.
- `record_type` is not AAC's `action_type`. AAC `action_type` stays `fyi` or `decide`.
  `record_type` is this profile's own closed vocabulary.
- The record carries **no raw personal identifiers** (section 4). Raw values stay in the
  device's local store.
- Records are closed. Unknown members fail the schema. A new member means a new profile
  version (`x-deal-v1`).

**Sealed reports are not steps.** Each copy of a deal report that is written to a file
(the user's own receipt, or a shared copy) seals its readable text as one more Capsule on the
deal's log:
- **The Capsule:** `action_id: "capsulectl-deal-report"`, `action_type: "fyi"`, not chained. On a
  deal opened since the evidencebook shelf, it also carries `evidence_record_header` with
  `record_type` `deal_report`, and no `subject_ref`, link or field commitment.
- **Its payload:** `{"type": "deal_report", "profile": "x-deal-v0", "deal_id", "audience",
  "nonce", "report"}`.
  - `report` is that copy's deal section: summary lines, step lines, amounts, what was told,
    anomalies. A shared copy seals the section already rewritten for its audience.
  - `nonce` is a fresh 256-bit random value, so the Capsule's `agent_input_digest` confirms
    no guess at the text.
- **In the bundle:** the `x-deal-v0` extension only names the Capsule:
  `{"sealed_report": "<capsule id>"}`. The text is its disclosed payload, and any edit fails
  the disclosure check.
- **Later bundles:** they cover the deal's whole log, so they carry every earlier report's
  Capsule with its payload withheld. A copy discloses its own text and no other copy's.
- **Not steps:** a sealed report has no `seq` and no record in the step chain. A verifier
  checking a deal's steps (section 6) sets these Capsules aside.

**Standalone reports (capsule-cli#294).** A deal on the shelf whose log holds no sealed
report seals each copy's report off its log, so the log, its copies and a History never show
when or how often the deal was shared:
- **The Capsule** is the same, signed by the key that signed the deal's last step, and appended
  to no log. Its payload adds `deal_checkpoint`: `{log_id, mmr_size, root}` of the deal checkpoint
  the copy is made at. Its `report` is computed at that checkpoint's time, so its `lifecycle`'s
  `as_of` is that time.
- **Checkpoints:** such a deal cuts a checkpoint at every step, stamped with the step's time, so
  a copy never cuts one and a checkpoint's time is a step's.
- **In the bundle:** `x-deal-v0` is `{"sealed_report": "<capsule id>"}`, and `x-deal-report/v0`
  carries `{"record": <the Capsule with its signature and key_id>, "agent_input": <payload>}`.
  The copy carries this report only.
- **Verification** (`verify --bundle`, and the page): the record's signature verifies under
  the key of the bundle's last step; its disclosure matches; it is a `deal_report` of the
  bundle's deal for a copy audience; `deal_checkpoint` is the bundle's checkpoint; its
  lifecycle's `as_of` is that checkpoint's time; and the bundle holds no `deal_report` record.
  A copy of a deal on the shelf that holds no report record and carries deal text without naming a
  standalone report fails; one stripped of both sections is its steps only, with no report text.
- **Not covered:** a sale's own log still records each sale bundle's `sale_cut`, so a sale's
  bundle shows when and how often it was made.
- **A deal whose log already holds a report,** and every legacy deal, keeps sealing its reports
  in its log, where their positions still show when it was shared.

## 2. The extension block `x-deal-v0`

| Field | Type | Req | Meaning |
|---|---|---|---|
| `profile` | `"x-deal-v0"` | REQUIRED | Profile and version. |
| `canonicalization` | `"jcs"` | REQUIRED | Pinned: RFC 8785. Any other value fails closed (section 5). |
| `deal_id` | string `^deal-[0-9a-f]{16,64}$` | REQUIRED | Random, opaque, and the same on every record of the deal. Never derived from any identifier. |
| `record_type` | enum (section 3) | REQUIRED | One of the 19 record types. |
| `seq` | integer ≥ 1 | REQUIRED | Position in the deal: 1, 2, 3, … with no gaps (section 6). |
| `at` | string `YYYY-MM-DDThh:mm:ssZ` | REQUIRED | The seal time in UTC, whole seconds. |
| `prev` | digest ref | REQUIRED iff `seq` > 1 | The record digest of record `seq − 1`. |
| `baseline_ref` | digest ref | REQUIRED on every record except the baseline; absent on the baseline | The record digest of the baseline (`seq` 1). |
| `channel` | channel token | REQUIRED on `baseline` and `message`; OPTIONAL elsewhere | Where the exchange happened: `marketplace`, `web`, `sms`, `phone_call`, `email`, `app_chat`, `whatsapp`, `telegram`, `signal`, `in_person`, `other`. Any other value MUST be namespaced (`org.example.foo`). |
| `counterparty` | object | REQUIRED on `baseline`; per section 3 elsewhere | `{"fp_alg": "hmac-sha256-deal-key", "ids": {<kind>: <fingerprint>}}`. Kinds: `payee`, `name`, `domain`, `phone`, `email`, `relay_address`, `profile_id`. Each value is a 64-hex fingerprint (section 4). |
| `refs` | array of rel refs | per section 3 | Typed citations of earlier records of the same deal. |
| `producer` | object | OPTIONAL (absent on records sealed before it was recorded) | `{"name", "version", "commit"}`: the software build that sealed this record, for example `{"name": "capsulectl", "version": "v0.1.0-rc4", "commit": "<40 hex>"}`. A development build says so (`"0.1.0-dev"`, `"unknown"`). Fixed when the record is sealed: a later build re-derives the same record. |
| `commit_alg` | `"sha256-jcs-nonce256"` | OPTIONAL (absent on records sealed before it was declared) | The construction of every `*_commitment` value in this record (section 4, Commitment). Fixed when the record is sealed, like `producer`. |

A **digest ref** is `{"type": "deal-record", "digest_alg": "SHA-256", "digest": <64 lowercase
hex>}`: the typed digest reference shape AAC `references[]` uses. The digest alone is the
identity. A **rel ref** is a digest ref plus `rel` (one of `about`, `source`, `checks`,
`approves`, `authorized_by`, `observes`, `outcome`, `confirms`, `supersedes`).

`counterparty.ids` on a record states the identifiers observed or asserted *at that record*.
On the baseline they are first contact. Every later check compares against first contact,
never against a later change.

## 3. Record types (action-type conventions)

Nineteen record types. The set is closed: an unknown `record_type` fails the schema.

| record_type | What it records | Required body fields | Required refs / block fields |
|---|---|---|---|
| `baseline` | First contact: what the user asked, who the counterparty is, the terms and the way back. The deal's contract. | `deal_type` (`purchase`\|`rental`\|`booking`\|`service`), `intent` (as the intent body; its optional `party_role`, `buyer`\|`seller`, is which side of the deal the user is on, absent meaning `buyer`, as on every deal sealed before it was recorded), `terms`, `recourse.rail` + `recourse.refundable` (a deal opened at the user's first ask, before a merchant is in play, may have no `recourse` and no `counterparty`: its first leg starts later, section 6 rule 13); `materiality` (the materiality predicate pinned when the deal opened, as on a verdict; a verdict under another one carries a `materiality_changed` difference) | `seq` = 1; `channel`; `counterparty` (≥ 1 id) when a merchant is in play. No `prev`, no `baseline_ref`. Its intent has no `move`, `proposed` or `why_commitment`; it may carry `delegated` + `delegated_commitment`. Optional `claims[]`, `counterparty_facts`, `demo`, `skill` (`{"skill_md_digest": <64 hex>, "other_copies": <int ≥ 0>}`: the SHA-256 of the SKILL.md the agent reported following, and how many other copies of that skill sat beside it; a boundary marker for accidents, not proof the instructions were followed). |
| `sale` | Where the user sells one item to one of several buyers: the first record of the sale's own log (deal_id `sale-…`), before any buyer. Followed by the sale's one `task-authority/v0`, then `thread`, `thread_opened` and `sale_cut` records and at most one `sale_close`, after which only `sale_cut` records follow, and nothing else. | `deal_type`, `intent` (the seller's request and limits, `party_role: seller`), `terms`, `recourse.rail` + `recourse.refundable`, `item_ref_commitment` (the sale's opaque item reference, committed) | `seq` = 1. No `prev`, no `baseline_ref`, no `counterparty`, no `refs`. Optional `demo`. |
| `intent` | The user restates or picks within the ask. Replaces `verbatim` / `asked` from here on. `allowed` and `max_total_minor` carry forward unchanged: an intent may lower the limit or drop actions, and a higher limit or a new action is only a proposal (section 6, rule 6). | `verbatim_commitment` | `baseline_ref`, `prev`. Optional `party_role`, equal to the baseline's (a deal's role is fixed when it opens). Optional `ordered_representations` (where the user sells, section 6 rule 12): on the baseline, what the user orders; on a later intent, a proposal. Optional `delegated` (`["item"]`: the user's words hand the agent the choice of item) with `delegated_commitment` (those words); a later intent never widens it, and one that names `asked.item` takes the item back. On a later intent sealed since journey records: `move` (`narrowed`\|`switched`\|`raised`\|`picked`, derived by the producer, section 6 rule 13), `proposed: true` when it asks for more than the limits in force, `why_commitment` on a switch or raise, and a `counterparty` block when it starts a leg. |
| `agent_move` | One of the agent's moves on the errand, its label fixed by `move`: `searched`, `options` (Showed you N options), `recommended`, `chose` (Chose for you), `couldnt` (Couldn't). | `move`; `searched`: `where_commitment`, `results`; `options`: `options[]` (each `item_commitment`, optional `price_minor`), `found`; `recommended`: `option` (from 1), `reason_commitment`; `chose`: `item_commitment`, optional `price_minor`; `couldnt`: `reason` (`sign_in_required`\|`out_of_stock`\|`cannot_ship_here`\|`blocked`\|`payment_declined`\|`site_error`\|`other`), `detail_commitment` with `other` | `baseline_ref`, `prev`. A `chose` follows a `delegated` item in force; it carries a `counterparty` block only when it starts the first leg. Texts are commitments only. |
| `message` | One message in the thread. The text stays local. | `from` (`counterparty`\|`user`\|`agent`), `content_commitment` | `channel`. `counterparty` when the message shows identifiers (for example, a new phone number). |
| `claim` | Something the counterparty (or listing) asserts, recorded as a claim, not a fact. | `text_commitment` (the claim's words, ≤ 200 chars, committed), `source_kind` (whose it is: `merchant`\|`agent`\|`platform`\|`user`\|`external`), optional `source_ref_commitment` (the caller's note of where it was read, committed). A claim sealed before claims were committed carries `text` and `source` in the clear instead, and re-derives unchanged (see "Claims"). | none beyond the chain; where the user sells, under section 6 rule 12, an agent's claim also carries `authority_basis` + `evaluation_ref` + one `authorized_by` ref, or `unchecked: true` + `rule`. |
| `evidence` | What was done to establish a claim, and whether it did. Optionally a merchant's own email (`merchant_email`, below), or a held object (`wrapped`, below). After the deal's final close, the only record type allowed: later evidence linked to that close. | `source`, `verified` | exactly one `about` → a `claim` or the `baseline`. At most one `confirms` → the deal's final `close` (required after it, not allowed before it). Optional `resolves_obligation` (a digest ref) → an earlier record holding a cancel-by date. Optional `received_to` (`{"fp_alg": "hmac-sha256-deal-key", "payee": <64 hex>}`), on evidence of a payment the user received (a settlement leg's step, where the user sells): the account it was received to, fingerprinted per deal like every identifier. It is the user's own account, never a `counterparty` id, so it is not a change of the other party's details. The step's Capsule carries the leg as its top-level `settlement` member (draft-mih-agent-settlement-records-00); the record does not restate it. |
| `detail_change` | The counterparty changed an identifier, a term or the rail after first contact. Recording it never accepts it. | `source`, `changed[]` (field names) | `counterparty.ids` kinds MUST equal the identifier kinds listed in `changed`. At most one `source` → a `message` or `evidence`. |
| `check` | The agent asks, before a point of no return, exactly what is about to happen (the snapshot). | `action` (`pay`\|`sign`\|`commit`\|`cancel`\|`share_contact`\|`share_credentials`\|`offer`\|`represent`; `offer` and `represent` only where the user sells; `represent` carries `representing`, section 6 rule 12) | `baseline_ref` (always). `counterparty.ids.payee` when a payee is involved: on every `pay`, and, where the user buys, on a `commit`, `sign` or `cancel` that states an `amount_minor` (money to or from that merchant). A check sealed before this names a payee on a `pay` only, and re-derives unchanged. Optional `amount_minor` (the expected charge), on a `pay` `authorized_max_minor` (the most it may take: what a limit binds, at least `amount_minor`), `currency`, `seen_item`, `terms`, `recourse`, the rule inputs (below), `pack_id`, `pack_digest`, `action_class` + `taxonomy_version` and `spend_minor` (see Action classes, below), with `spend_authorized_minor` beside them when `authorized_max_minor` is sealed, on a `cancel` `fee_minor`, `cancelled_amount_minor` and `direction` (`in`), on a refund's `cancel` `reverses_ref` (the payment it returns, see Action classes, below), on a check of a sale's thread `item_ref_commitment` (section 6, rule 10), and for a share `disclosing` (the classes about to be given, as for a `disclosure`) and `disclosing_to` (`counterparty`\|`platform`\|`other`), which every share check carries. |
| `verdict` | The answer to one check: pass, pause with the differences, or deny. | `result` (`pass`\|`pause`\|`deny`), `differences[]`, `options[]`; `rules` (what the profile's external rules checker said, §8 "The external rules checker"; absent on verdicts sealed before it was recorded); `materiality` (the predicate that decided which of the agent's own picks pause: its `digest`, and `label_commitment`, a commitment to its name and version, opened only in the user's own copy (`materiality_openings`); `digest: "none"` when none was configured and every pick paused; verdicts sealed by v0.1.0-rc8 carry `name` and `version` in the clear instead, and shared copies withhold them) | exactly one `checks` → a `check`; one verdict per check; optional `pack_id`, equal to the check's when either names one. `pause` ⇒ ≥ 1 difference and ≥ 1 option. `pass` ⇒ no options. `deny` ⇒ ≥ 1 difference, `rules`, and no option but `hold`. |
| `approval` | What authorizes, or declines, the next step; or the user's confirmation of the limits an intent proposed. | `choice` (`hold`\|`verify_contact`\|`proceed`\|`confirm_limits`), `proceed` (`true` on `proceed`, `false` on `hold` and `verify_contact`), `approver` (`user`\|`standing_intent`\|`agent_card`); on `confirm_limits` only, `limits` (`previous` and `new`, each `max_total_minor` and `allowed`) | `confirm_limits`: exactly one `approves` → the proposing `intent`, `approver: user`, and section 6, rule 6. Otherwise exactly one `approves` → a `verdict`. `user` ⇒ `said_commitment` (the user's own words), and on a pause the choice is one of the verdict's options. `agent_card` ⇒ no `said_commitment`: a card the agent composed was answered, and no words of the user's are on record; never on `confirm_limits`. Optional `card_commitment` (on an answer to a verdict, never on `confirm_limits` or `standing_intent`): the card text the answer was given on, committed under the verdict's own card nonce, so it MUST equal the verdict's `card_commitment`: equal means the card shown is the card checked, recomputable without the text. `standing_intent` ⇒ the verdict passed, the choice is `proceed`, and the checked action is in the current `allowed` (`allowed` absent = no restriction; `allowed` present and empty = nothing is allowed yet, as in "show me options, don't book"). |
| `action` | A point-of-no-return step actually taken. | `action`; optional `direction` (`out`: paid by the user; `in`: back to the user); optional `action_class` + `taxonomy_version` and `spend_minor` (see Action classes, below); on a `cancel`, optional `fee_minor` and `cancelled_amount_minor` (see Action classes, below) | exactly one `authorized_by` → an `approval` with `proceed: true` (section 6). An action that returns money (`direction: in`) carries exactly one `reverses` → the `pay` action it undoes (section 6, rule 8). |
| `outcome` | What was observed afterwards: delivered or not, or an action taken without approval. | `status`, `outcome`, `differences[]` | At most one `observes` → an `action`. On the outcome a close seals: `actor` and `reason` (see the close). |
| `close` | The deal ends (or pauses its record) with an outcome. | `outcome`, `unchecked_actions` | exactly one `outcome` → the latest `outcome` record, if any exists. Optional `carried_obligations`: the cancel-by dates still open at the close, each `{obligation: digest ref, cancel_by}`. `actor` (`user`\|`agent`\|`platform`: whose decision ended the deal) and `reason` (why, from a closed set), together, equal to its outcome's. The reason follows from the status where it can: `completed` or `delivered_mismatch` (`received`), `awaiting_delivery` (`pending`), `not_selected`, `called_off` (actor `user`). With `not_received` it is one of `user_closed` (actor `user`), `agent_could_not_complete` (actor `agent`), `merchant_rejected`, `payment_failed`, `cancelled_in_window`, `not_delivered`, `other`. Absent on closes sealed before. |
| `disclosure` | Something the agent told someone about the user: what kind of thing, to whom, when, under what authority. | `to` (`counterparty`\|`platform`\|`other`), `fields[]` (each `class` + `value_commitment`), `authority` (`approval`\|`none`) | `authority: approval` ⇒ exactly one `authorized_by` → an `approval`, under the same rules as an `action` (section 6); `none` ⇒ no `authorized_by`, and `rule` says why. All `fields` share one covering action: contact classes `share_contact`, credential classes `share_credentials`. Optional `action_class` + `taxonomy_version` (see Action classes, below); optional `channel`; `counterparty` when the recipient is someone new. |
| `thread` | On a sale's own log: the registration of one buyer's thread, sealed by `deal open --sale` before the thread opens (section 6, rule 10). | `thread_ref_commitment`: the thread's deal id, committed under this step's own nonce | `deal_id` `sale-…`; follows the sale's previous record (`prev`, `baseline_ref` → the `sale` root). No `counterparty`, no `refs`. |
| `thread_opened` | On a sale's own log: a registered thread opened, sealed right after its task authority (or by the next sale bundle or `deal sale close`, if a crash came between) (section 6, rule 10). | `task_authority_commitment`: the thread's task-authority record digest, committed under this step's own nonce | `deal_id` `sale-…`; exactly one `registration` ref → an earlier `thread` record of this log, at most one `thread_opened` per registration. No `counterparty`. |
| `sale_cut` | On a sale's own log: the cut a sale bundle is made at, sealed after each thread's report and before the bundle's checkpoint (section 6, rule 10). | `thread_heads[]`: `{registration (digest ref), head_commitment}`, one per opened thread, each committing to that thread's last record at the cut | `deal_id` `sale-…`; each head names an opened thread, once. No `counterparty`, no `refs`. |
| `sale_close` | On a sale's own log: the end of the sale, sealed by `deal sale close` after every opened thread's `thread_opened` (section 6, rule 10). No buyer's thread opens on the sale after it; a thread already open goes on and ends at its own close. | nothing: `body` is empty | `deal_id` `sale-…`; follows the sale's previous record. No `counterparty`, no `refs`. At most one per sale, and only `sale_cut` records follow it, since every later sale bundle cuts the threads' heads again. |
| `counterparty_profile` | The payee of the `check` just before it, fingerprinted under this profile's own key, for the user's own history: one merchant has one value across the profile's deals. Sealed right after its check, before any rules checker runs; never in a shared copy. | `counterparty_profile` in the header: `{fp_alg: "hmac-sha256-profile-key", ids: {payee}}`; an empty body | exactly one `about` → the `check` it follows. No `counterparty` block. |

### Action classes

A `check`, an `action` and a `disclosure` sealed by this release carry `action_class`: the
action's class in version `taxonomy_version` (`"6"`) of the action taxonomy published in
capsule-engine, `capsule_engine/guards/action_taxonomy.json`
(github.com/action-state-group/capsule-engine at `7ae6b95b37a3a1c4955af90f56ae4c1c0c404aa9`,
file sha256 `7563fcaeb6c192a27877fae3131bca1021526046c8976475c46eebf4fb374272`, JCS sha256
`fc12eb90bc2b3c85bb49c4d3d7be8f384c6e0b8e189bc3b68a753a4f1af5cd94`). The two members come
together or not at all. Steps sealed before carry neither, and re-derive without them. Steps
sealed by an earlier release under version `"2"` (at `2521ee6`), `"3"` (at `a5e9e30`), `"4"`
(at `5300e0b`) or `"5"` (at `e5ec2c1`) keep their own version. All five name the same classes:
- version 3 adds `approver_role` `account_holder` to `money.purchase`, `money.subscription`,
  `booking.create` and `booking.modify`;
- version 4 adds it to `booking.cancel`, `data.delete`, `communication.publish` and
  `disclosure.personal`;
- version 5 adds it to `communication.send`, a class no deal record carries;
- version 6 adds it to `agreement.accept` (a seller's commit, and a rental's or service's commit
  or sign), `marketplace.offer` (a seller's offer, from the step that marks it; an offer sealed
  before carries `external_commitment.other`) and `marketplace.sale` (no deal record carries it).
  A reader that keys a
limit on an action's class (a per-action or rolling spend cap, for example) selects on
`action_class`, and resolves it against the taxonomy version it names.

| Action | `purchase` | `rental` | `booking` | `service` |
|---|---|---|---|---|
| `pay` | `money.purchase` | `money.purchase` | `booking.create` | `money.purchase` |
| `commit` | `money.purchase` | `agreement.accept` | `booking.create` | `agreement.accept` |
| `sign` | | `agreement.accept` | | `agreement.accept` |
| `cancel` that returns a payment (`direction: in`) | `money.refund` | `money.refund` | `money.refund` | `money.refund` |
| any other `cancel` | `external_commitment.other` | `external_commitment.other` | `booking.cancel` | `external_commitment.other` |
| `share_contact` | `disclosure.personal` | `disclosure.personal` | `disclosure.personal` | `disclosure.personal` |
| `share_credentials` | `disclosure.secret` | `disclosure.secret` | `disclosure.secret` | `disclosure.secret` |
| `offer` | `marketplace.offer` | `marketplace.offer` | `marketplace.offer` | `marketplace.offer` |

- Committing to a purchase is the same class as paying for it, so a limit keyed on the class
  cannot be stepped around by committing first.
- An `offer` is a seller's proposal: its amount is what the buyer would pay, so its
  `spend_minor` is 0. A seller's `commit` is money coming in: `agreement.accept` in every deal
  type, with `spend_minor` 0, so a cap's rolling total never counts a sale.
- A cancel that returns a payment is money arriving, not leaving: `money.refund`. A `check`
  states no `direction`; its class is the one its `action` would carry, from the same sealed
  payments.
- **`spend_authorized_minor`** is sealed on a `check` beside `spend_minor` when the check
  states `authorized_max_minor`: the most the payment may take, which a per-action cap reads.
  `spend_minor` stays the expected charge on a check and what was taken on an `action`, which a
  rolling (weekly) cap sums.
- **`spend_minor`** is sealed with the class on a `check` and an `action`: the amount a spend
  cap evaluates, what the action pays out. It is `amount_minor` for an action that moves money
  out, absent when there is no amount, and **`0` on every `cancel`**: stopping a commitment is
  never spend, whether the cancel returns a payment, returns part of one, or costs a fee. A
  cancel's amount never reads as money paid out: a `cancel` with an amount carries
  `direction: "in"` on its `check` and its `action`. An amount that returns a sealed payment
  stays `amount_minor` (the refund; the `action` names the payment with `reverses`, rule 8).
  The refund's `check` names the same payment as `reverses_ref`, a typed reference
  (`{"type": "record", "digest_alg": "SHA-256", "digest": …}`) whose `digest` is the one the
  `action`'s `reverses` ref carries, so a reader can key a refund on what it returns before it
  is done. A check sealed before this release has none.
  Any other amount (part of a payment, or one that matches none) is `cancelled_amount_minor`:
  what the cancel was about, neither money moved nor spend. A fee the cancellation costs is its
  own `fee_minor` (only a `cancel` carries one). No cap evaluates any of them.
- A deal type or an action this table does not name is `external_commitment.other`, the
  taxonomy's explicit class for a consequential commitment it does not otherwise name; never
  no class, and never the non-consequential `info.query`.
- `action_class` sits beside the Capsule's AAC fields and changes none of them: a `pay` is
  still `decide` with `effect.type: send_payment`.

Field details:

- **Money** is integer minor units (`price_minor`, `deposit_minor`, `amount_minor`,
  `max_total_minor`) plus an ISO 4217 `currency`. There are no floats anywhere in a record, and
  integers stay within ±(2^53 − 1).
- **terms**: `item`, `quantity`, `price_minor`, `deposit_minor`, `currency`, `when`, `place`,
  `conditions` (token → string). `place` is a locality ("lakeside marina"), never a street
  address.
- **expect_close_by** (on `baseline`, optional): the day the deal is expected to close
  (YYYY-MM-DD). capsulectl seals one on every deal (by default 14 days for a purchase, 1 for a
  booking, 7 for a rental, 30 for a service) so an open deal can be listed against it.
- **Late records.** A record sealed after a final close is an `evidence` record whose Capsule
  chains to the close with the registered `chain.relation` `confirms` (non-terminal: it records
  an outcome of the close, whose state stands), never `follows` (ordering only), and whose
  `confirms` ref commits to the close record's digest, so it cannot be reattached to another
  deal. It shows that whoever sealed it held that deal; it does not show the deal expected it.
  `supersedes` (terminal) is not emitted: an expiry is computed when a receipt is made.
- **obligation** (on `evidence`, optional): a commitment tied to a date. Either a cancel-by
  kind, which takes effect when a date passes: `kind` (`trial_conversion` | `renewal` |
  `cancel_window` | `payment_due`) and `cancel_by` (the last day to cancel); or a due kind,
  something the user owes the other side by a date: `kind` (`deliver_by` | `perform_by`) and
  `due_by`. Never both dates. Optional `takes_effect`, `amount_minor` + `currency`, `period`
  (`week` | `month` | `year` | `once`) and `terms_commitment` (to the merchant's own wording, kept
  on the device). The evidence record's `source` says where it came from (`merchant_email`,
  `page_snapshot`). Recorded, never enforced. Each obligation records one date. A recurring
  renewal after that date is not tracked; seal a new obligation for each later date. A later
  `evidence` record resolves either kind (`resolves_obligation`); a sealed `cancel` ends a
  cancel-by obligation, never a due one.
- **merchant_email** (on `evidence`, optional): a merchant's DKIM-signed email, kept raw on the
  device. `message_digest` (SHA-256 of the exact RFC 822 bytes), `key_records_digest` (SHA-256
  of the JCS array of `{"name","txt"}` key records captured when the email was sealed),
  `key_source` (`dns` | `supplied`), optional `key_resolver` + `key_dnssec` on a `dns` key (how
  the key and DMARC records were read: `system`, this device's resolver, or `doh`,
  DNS-over-HTTPS; `resolver_validated` when the DoH resolver set the AD bit on every answer, else
  `not_validated`. This is the resolver's word that it checked DNSSEC; capsulectl does not check
  the chain itself), `dkim` (`pass` | `fail` | `none`), `merchant_signed` (a
  passing signature is DMARC-aligned with the From domain: the same organizational domain, or
  the exact domain under `adkim=s`), `dmarc_policy` (`reject` | `quarantine` | `none` |
  `absent` | `not_captured`; shown, never a reason to refuse sealing), optional
  `dmarc_record_digest` + `dmarc_source` (the DMARC records captured at seal time), optional
  `signer_matches_baseline`, and `parsed` (`method: heuristic`; optional `total_minor`,
  `currency`, `order_id_commitment`, `cancel_by`, `sent_at`, `item_count`, `kind`:
  `confirmation` | `cancellation`). `verified` is true
  only when `dkim` is `pass` and `merchant_signed` is true. The DKIM signature is the
  merchant's attestation; the record's own seal is the producer's. They are different claims.
- **wrapped** (on `evidence`, optional; `source` is then `wrapped_object`): an object the user
  holds, sealed by digest only (`deal note --object`). `wrapped` is an array of exactly one entry
  in the shape of draft-mih-agent-settlement-records-00 §8, `{"type", "digest_alg": "SHA-256",
  "digest"}`, never with `content`. `type` is a registered Settlement Wrapped Object Type, or any
  other name (`^[a-z0-9][a-z0-9.-]{0,63}$`), which is then the sealer's label for an opaque
  digest, not a registered type. `wrapped_octets` says what the digest is over: `as-transmitted`
  (the octets as delivered) or `jcs` (their RFC 8785 form); a registered type is held to the
  rule its registry entry gives. `wrapped_held` is `octets` when the sealer held the object, or
  `digest` when it was given only the digest, which claims no possession. Optional
  `carrier_headers_commitment`: a salted commitment (as `order_id_commitment`) to the base64url
  form, without padding, of the header block the object arrived with. The octets and the headers
  stay in the step's local event, on the device; nothing is read from the object into the
  record. The record's seal says the producer held octets with that digest, not that it issued
  them, and the object's own signature, if it has one, is its own protocol's to check (§8 rule 4).
- **verifier_statement** (not a record; `$defs/verifier_statement`): a verifier's signed
  statement that one sealed `merchant_email`'s DKIM signature checked out, which can travel
  without the email. `statement_type` (`dkim-verification`), `dkim_domain` (the d= of the passing
  signature), optional `message_date` (the Date header, UTC, present only when the passing
  signature's h= covers Date; a covered Date that is repeated or unreadable gets no statement), `email_digest` (`{"alg": "sha-256", "value"}`: the record's own
  `message_digest`, no new value), `key_records_digest` (the record's own), `key_source` (`dns`
  only), `checked_at`, `verifier` (`{key_id, public_key}`, both the 64-hex Ed25519 public key)
  and `signature` (Ed25519, 128 lowercase hex, over UTF8(`x-deal-v0/verifier-statement/v1`) ||
  0x00 || JCS(the statement without `signature`)). It is issued only after a re-check of the full
  sealed raw message against the sealed key records passes; never on `fail` or `none`, never from
  a header set, and never over a `supplied` key. Selector, h= list and all header values are left
  out. A statement is shown as V's statement and never as a verified receipt. `capsulectl` issues
  it under the holder's own key, so it says "checked by the holder's own tool" and never
  independent or third-party.
- **recourse**: `rail` (a token: `card`, `zelle`, `wire`, …; normalized as in **Normalization**
  below) and `refundable`.
- **differences[]**: `{question, rule, field?}`. `question` is one of `asked`, `who`, `terms`,
  `recourse`, `safety`, `delivered`. `rule` is the id of the rule that found the difference.
  `capsulectl`'s deal check writes its own built-in rules, among them `not_asked`, `over_limit`,
  `under_floor` (a price below the user's floor, rule 6), `agent_picked`, `terms_changed`, `recourse_changed`, `credentials_requested`, `not_delivered`
  and `delivered_differs`. On a sale's thread, a share made after the seller cancels the
  buyer's acceptance is an `acceptance_called_off` difference (`safety`): the acceptance no
  longer releases anything in that thread until the buyer accepts again (a later acceptance
  ends it). Any change of
  `recourse.rail` or `recourse.refundable` from what was agreed is a
  `recourse_changed` difference, for every action. The human card text contains raw values, so it is
  **not** in the record. The verdict carries `card_commitment` instead, and the text stays
  local.
- **pack_id** (OPTIONAL): `publisher/name/semver`, naming a rule pack that actually ran.
  `capsulectl`'s deal check loads no pack (it runs its own built-in rules), so from the release
  after v0.1.0-rc6 it writes no `pack_id`. Records sealed by v0.1.0-rc6 and earlier carry
  `capsule/marketplace-rentals-safety/0.1.0`, and still validate. A producer that does run a pack
  may name it, and then `pack_digest` (OPTIONAL) is SHA-256 over the JCS bytes of the exact
  `pack.json` that ran: an identity reference (exact bytes). A pack version range is a policy
  reference and is never written here.
- **verdict.judge**: `{kind: "rules"|"remote", status?, response_digest?}`. A remote judge's
  signed response is committed by digest, and the response stays local.
- **Actions without approval.** An `action` record exists only for an authorized step. If the
  agent took a point-of-no-return step with no sealed approval, the skill still seals it, as an
  `outcome` with `status: "unchecked_action"`, `outcome: "mismatch"` and the step in `unchecked`.
  It is counted in `close.unchecked_actions`. The trail stays honest, and the approval rule stays
  absolute.
- **Disclosures.** A `disclosure` records what the agent told someone about the user. Each
  field is a `class` (`name`, `phone`, `email`, `home_address`, `address`, `pickup_location`,
  `pickup_area`, `other_contact`, `credential`, `verification_code`, `payment_card`,
  `id_document`) and a `value_commitment` to the value given. `pickup_location` is the exact
  spot; `pickup_area` is the public area a listing shows (a neighborhood, city, region or ZIP,
  never a street address) and is not an address class. The value itself stays in the local store; a ledger of
  disclosures must never itself be a disclosure. Unlike an unauthorized `action`, a disclosure
  with no covering approval is still a `disclosure` record, with `authority: "none"` and the
  `rule` that failed (for example `no_check` or `answer_was_not_proceed`), so every telling
  is in one list. A verifier and a report treat `authority: "none"` as an agent-side anomaly.
- **Authority.** An approval's authority is the recorded answer (the user's, or the user's own
  standing intent on a pass). The store's signing key only makes the log tamper-evident. A
  record signed by the key is never, by that fact, an approval, and no verifier may read key
  control as authority.

## 4. Counterparty identifiers: fingerprints, never raw values

Raw phone numbers, emails, names, payees, domains, reply addresses and profile ids stay in the
local store. They are shown on the user's own screen or disclosed when the user chooses. A
record carries only fingerprints. Free text that may contain identifiers (message text, the
user's own words, card text, notes) is carried as a salted **commitment**.

**Store secret.** On first use the skill generates `store_secret`: 32 bytes from a CSPRNG. It
lives only in the local store. It is never written into a record, never sent, and never
logged.

**Per-deal key.**
`deal_key = HMAC-SHA256(key = store_secret, msg = "x-deal-v0/deal-key" ‖ 0x00 ‖ UTF8(deal_id))`.
The skill stores `deal_key` with the deal at open, so a later rotation of `store_secret` cannot
break a deal in progress.

**Fingerprint.**
`fp = lowercase_hex(HMAC-SHA256(key = deal_key, msg = "x-deal-v0/fp" ‖ 0x00 ‖ kind ‖ 0x00 ‖ UTF8(normalize(kind, raw))))`.
Including the kind separates the domains, so the same string as `name` and as `payee` gives
different fingerprints.

**Normalization**:

| kind | normalize(kind, raw) |
|---|---|
| `phone` | NFKC, trim. Drop a trailing extension (`ext`, `x`, `#` + digits). Keep the digits. With a leading `+`, the result is `+digits`. Without one, apply the default region (v0: NANP): 10 digits → `+1` + digits; 11 digits starting with 1 → `+` + digits; anything else is not normalizable. 8–15 digits, E.164 form: `+15550102000`. |
| `email` | Trim, NFC, lowercase the whole address. Dots and `+tags` are kept (they can be different mailboxes). |
| `domain` | Trim, NFC, lowercase. Strip the scheme, path, query, fragment, userinfo, port and trailing dot. IDNA to Unicode (U-label). Reduce to the registrable domain (eTLD+1) using the Public Suffix List. `https://Book.CoastalJetRentals.example:443/q` → `coastaljetrentals.example`. |
| `name` | NFC, casefold. Replace every Unicode punctuation character (category P*) with a space. Collapse whitespace. Drop a leading `the`. Repeatedly drop trailing legal-form tokens (`llc`, `inc`, `ltd`, `corp`, `co`, `company`, `gmbh`, `llp`, `plc`). `The Coastal Jet Rentals, Inc.` → `coastal jet rentals`. |
| `payee` | If the value contains `@`, email rule. If it is only phone characters with ≥ 7 digits, phone rule. Otherwise, name rule. The kind stays `payee`. |
| `relay_address` | Trim, NFC, lowercase. |
| `profile_id` | Trim, NFC. Case is preserved (exact match). |

If a value cannot be normalized, it is not fingerprinted and not recorded. It stays local. The
baseline MUST carry `payee` explicitly: when first contact names no separate payee, the
producer fingerprints the first-contact name under kind `payee`.

**Commitment** (for text):
`commitment = lowercase_hex(SHA-256(JCS({"nonce": <64-hex, 32 random bytes>, "text": <text>})))`.
The nonce and text stay in the local store. To disclose, the user reveals the `{nonce, text}`
pair for that one commitment. Fields: `verbatim_commitment`, `content_commitment`,
`detail_commitment`, `description_commitment`, `reference_commitment`, `card_commitment`,
`said_commitment`, `note_commitment`, `value_commitment`, `label_commitment`, and in typed
records `rendering_commitment`.

A record declares this construction as `commit_alg: "sha256-jcs-nonce256"` (section 2; in a
typed record, in its header), next to the fingerprints' `fp_alg`. What it states:

- **Construction:** SHA-256 over the JCS bytes of `{"nonce": N, "text": T}`, as above.
- **Nonce scope:** one fresh 256-bit random nonce per committed text, drawn when its step is
  sealed: never per deal or per store. The one reuse is deliberate: what a person was shown
  is committed under the nonce of the card it answers (section 9, One rendering commitment),
  so equal values mean the same text. A guess of `T` cannot be tested without `N`.
- **Key scope:** none. Unlike a fingerprint (`fp_alg: "hmac-sha256-deal-key"`), a commitment
  uses no store secret or deal key.
- **Recomputable by:** whoever holds the opening `{N, T}`, and nobody else. A record alone
  never lets anyone recompute or test a commitment.
- **Where openings travel:** nowhere by default. The user's own report carries three:
  - `asked_opening` (the baseline's `verbatim_commitment`);
  - `materiality_openings` (each verdict's `label_commitment`);
  - `claim_openings` (each claim's `text_commitment` and `source_ref_commitment`).

  An adjudicator's copy carries the `claim_openings` of claims whose words and source note hold
  none of the user's private details. A counterparty's copy carries none, except where the user
  sells: there it opens the agent's own claims (its representations to the buyer) whose words
  and source note hold none of the user's private details, and no other claim.

**Claims.** A claim's words and its source note are sealed as commitments; only whose it is,
`source_kind`, is in the clear.
- **On a claim step and on each of the baseline's `claims[]`:** `{text_commitment, source_kind,
  source_ref_commitment?, class?}`. `class` says what kind of representation the claim is, so a
  page can label it, from a seller's representation classes: `price`, `condition`, `features`,
  `authenticity`, `availability`, `delivery_date`, `service_scope`, `warranty`, `refund_terms`,
  `payment_methods`, `pickup`, `deadline`, `address` or `other`. It is optional and in the clear;
  a claim without one re-derives unchanged. `delivery_promise` is valid only on a claim sealed
  before `delivery_date` replaced it; an input that says `delivery_promise` is sealed as
  `delivery_date`.
- **`source_kind`** is stated by the caller. A source that can only mean the counterparty
  (`counterparty`, `seller_message`, `merchant_email` and the like) is taken as `merchant`. Any
  other source (a page, a photo, a snapshot: the merchant's own or a marketplace's) must state it,
  and a claim that leaves it open is refused.
- **`claim_openings`** in a copy are `{record_digest, index (a baseline claim), text: {nonce,
  text}, source: {nonce, text}}`. `check_profile.py --openings=FILE` recomputes each against the
  sealed record.
- **`representations`**, where the user sells: the agent's own claims (`source_kind: agent`), as
  `{step, index (a baseline claim), class?, nonce, text}`, in the user's copy and, for those whose
  words hold none of the user's private details, in both shared copies. The page checks each
  against its sealed `text_commitment` and labels it by the sealed `class`. A buyer's deal has
  none: there, the agent's claims are its own notes.
- **A verdict** names the claims it found unverified by reference, as `unverified_claims`
  (`{claim: <digest ref>, index?}`), not by their words.
- **Older records:** a step sealed before this carries `text` and `source` (and a verdict
  `unverified`) in the clear, and re-derives unchanged.

A record without `commit_alg` was sealed before it was declared. Its commitments use the same
construction.

**What a fingerprint allows:**
- Within one deal, anyone holding the records can see that two identifiers are equal or
  different: "the payee now differs from first contact". This is checkable offline, with no raw
  value.
- The holder of the local store can link a counterparty across the holder's own deals by
  recomputing from the raw values.
- The user can prove one identifier in one deal by disclosing `deal_key` and the raw value. That
  exposes that deal's fingerprints to guessing, and no other deal's.

**What it does not allow:**
- Recovering a raw value without `deal_key`. (With `deal_key`, short spaces such as phone numbers
  can be guessed. That is why the key is per deal and never leaves the store.)
- Linking the same counterparty across two deals from records alone. Each deal has its own key.
- Correlating across two users' stores. Their secrets differ, and no shared or global
  identifier exists.
- Establishing who a counterparty *is*. A fingerprint says "same as before" or "changed", and nothing
  about the counterparty's conduct.

## 5. Canonicalization, pinned from day one

- The record digest is `lowercase_hex(SHA-256(JCS(record)))`, where JCS is RFC 8785 applied to
  the whole record (block and body). The canonicalization id is **`jcs`**, as registered in
  CPB's Canonicalization Algorithm registry and required by AAC format 4. There is no
  normalization pass: `null`, empty arrays and empty objects participate when present. No key
  filtering happens at any depth.
- The block declares `"canonicalization": "jcs"`. Any other value, absent or not, fails closed
  before any digest is computed. That includes the withdrawn `jcs-n`.
- No floats. Integers stay within the IEEE-754 safe range. Strings SHOULD be NFC before
  sealing; the digest covers the bytes as given and is never renormalized.
- A serializer's default output is not JCS. Go's `encoding/json` emits struct fields in
  declaration order, escapes `<`, `>` and `&` as `\u003c` and so on, and sorts map keys by UTF-8
  bytes, not UTF-16 code units. Python's
  `json.dumps(sort_keys=True)` escapes non-ASCII by default. A producer MUST run its bytes
  through a JCS implementation. `fixtures/negative/neg-non-jcs-digest.json` fails precisely
  because of `ensure_ascii` escaping.
- Digests of cited records (`prev`, `baseline_ref`, `refs`) use the same construction.

## 6. Order, sequence and authorization rules

A verifier holding one deal's records in `seq` order checks:

1. **Sequence.** `seq` starts at 1 and rises by exactly 1. A gap or a repeat (regression) is
   invalid.
2. **Baseline first.** Record 1 is the `baseline`, and there is exactly one. Every later record
   carries `baseline_ref` = the baseline's digest.
3. **Hash chain.** Every record after the first carries `prev` = the digest of the record
   before it. `deal_id` is constant, and `at` never decreases.
4. **References point back.** Every `refs` entry names an earlier record of the same deal, with
   a `rel` allowed for that record type, pointing at the record type section 3 names.
5. **Never act on an unsealed approval.** An `action`, and a `disclosure` with
   `authority: "approval"`, MUST carry exactly one `authorized_by` →
   a sealed `approval` with `proceed: true`. That approval `approves` a `verdict`, which `checks`
   a `check` whose `action` equals the action's. Also:
   - No `detail_change`, and no `message` or `evidence` carrying `counterparty` identifiers or
     `counterparty_facts`, is sealed between that check and the action. Such a step after the
     check requires a new check (a new payee in a message is still a new payee).
   - `amount_minor`, `currency`, `rail` and the payee fingerprint, where both sides carry them,
     equal the checked ones.
   - One approval authorizes at most one action or disclosure.
   - It is the verdict's first `approval`. A later answer to the same verdict never authorizes:
     changing the answer ("Hold", then "Pay anyway") requires a new check. The later answer is
     still sealed as the user gave it (`proceed` records the choice, as for every approval); no
     `action` may cite it.
6. **The user's limits, and standing intent.** The limits in force start as the baseline
   intent's `max_total_minor` and `allowed`. An `intent` record may lower the limit or drop
   actions; a higher limit or an action not in force is a proposal, recorded and not applied.
   A floor (the lowest total the user will take, a seller's `min_total_minor`) runs the other
   way: an `intent` may raise it, and a lower floor is a proposal. The floor is never sealed: a
   record carries `bounds_commitment`, the salted commitment (`commit_alg`) to the
   `commercial-bounds/v0` document holding it (`records/commercial-bounds-v0.schema.json`; the
   checker's vector, `fixtures/commercial-bounds-vectors.json`), on the baseline or intent that
   states it, on a typed `task-authority/v0`, and in a `confirm_limits` approval's `limits`
   (the commitment of the intent that stated that floor). Only the user's own copy carries its
   opening (`bounds_openings` in its sealed report), and the profile's own rules checker is
   given the opening in force (`commercial_bounds_opening`). A floor cannot be compared from its
   commitment, so a checker treats an intent that states another floor as possibly asking for
   more.
   Only the user's `approval` with `choice: "confirm_limits"` that `approves` the proposing
   `intent` puts it in force, as a new version: its `limits.previous` equals the limits in force,
   its `limits.new` is what the intent proposed (a field the intent leaves out keeps its value),
   it carries `proceed: true`, and the intent asks for more. A proposal replaced by a later
   `intent`, or already confirmed, may still be answered, with `proceed: false`; that changes
   nothing. A `confirm_limits` approval authorizes no `action` or `disclosure`.
   `approver: "standing_intent"` is valid only on a passing verdict, and only for an action in
   the `allowed` in force. An absent `allowed` places no restriction; a present, empty `allowed`
   allows nothing. A pause always needs an answer: the user's own (`user`, with their words), or
   a card's (`agent_card`), which certifies only that the card was answered, not that the user
   consented to the action.
7. **Close.** `close` references the latest `outcome` (if any), and its `outcome` equals that
   outcome's. Without an outcome record, the close is `open`. `unchecked_actions` equals the
   number of `unchecked_action` outcomes. A close with `completed`, `mismatch`, `not_selected` or
   `called_off` is terminal. `not_selected` (status and outcome alike, with no differences) says the
   other side was not chosen, one buyer of several for example, and nothing was done: no `action`
   precedes it. `called_off` (status, outcome and reason alike, actor `user`) says the user dropped
   the errand, with nothing done, and never reads as nothing delivered. A terminal close:
   after it, only `evidence` records that `confirms` that close may follow. A close with `open`
   MAY be followed by later `outcome` and `close` records. Each `carried_obligations` entry names
   an earlier record holding that `cancel_by`, or that `due_by`.
8. **Reversals.** An `action` with `direction: "in"` and an `amount_minor` returns money the user paid: it carries
   exactly one `reverses` ref to an earlier `action` with `action: "pay"` (direction `out`, or
   none, as records sealed before `direction` was recorded carry), with the same `amount_minor`
   and `currency`. A payment is reversed at most once. Summing amounts by direction gives what a
   deal moved: a pay and its reversal net to zero. Records sealed before this rule carry no
   `direction`; a `pay` among them moved money out, and no other action states a direction.
9. **Offers and acceptance (where the user sells).** A deal whose `party_role` is `seller` is
   sealed in the typed action records (section 9). An `offer` is a `check` (typed: a
   `proposed-action/v0`) with `action: "offer"`, on such a deal; its
   record is the exact proposal, every material term under this deal's id. Each later offer
   carries exactly one `supersedes` ref, to the latest earlier offer, and the first carries
   none; no other `check` carries one. Only the latest offer can be accepted. A counterparty
   acceptance observation (section 9) names, by `proposed_action_ref`, the latest offer, after
   an `offer` action that rests on it (the offer was made). On a typed seller deal, a `commit`
   action, and a share action that discloses an address class (`home_address`, `address`,
   `pickup_location`; not `pickup_area`), each need: an offer on record, an acceptance of the
   latest one, and no `detail_change`, and no `message` or `evidence` carrying `counterparty` identifiers or
   `counterparty_facts`, after that acceptance. A change leaves the acceptance behind: the
   offer is made and accepted again. Such a commit or share action cites the acceptance it rests
   on with exactly one `source` ref; no other action carries one. The acceptance is not
   authority: `authority_basis` is unchanged.
10. **A sale to one of several buyers.** A sale's own log is a `sale` record, then its one
    `task-authority/v0` (`source` → the sale record, the floor's `bounds_commitment` when the
    sale states one, no `sale_authority_commitment`), then its `thread` registrations, the
    `thread_opened` of each that opened, the `sale_cut` of each sale bundle, and at most one
    `sale_close`, after which only `sale_cut` records follow; nothing else. A closed sale has
    ended for the cadence, as a deal has at its final close; no thread opens on it after the
    close. Each buyer's negotiation is a deal of its own, a thread, whose task
    authority carries `sale_authority_commitment`: the digest of the sale's task authority,
    committed under that step's own nonce. A thread is registered on the sale's log before it
    opens (a `thread` record committing to its deal id), and its task authority names that
    registration with exactly one `registration` ref, a record on the sale's log, not the
    thread's; a thread opened before threads were registered has none. The user's own sale
    bundle (`deal sale bundle`) is the sale log's Evidence Bundle, whose completeness
    certificate covers every registration through its checkpoint. It carries each registered
    thread's own copy in the `x-deal-sale/v0` extension (`{"threads": {"<deal id>": <bundle>}}`),
    and its x-deal-v0 section lists `sale_threads`, one entry per registration in seal order:
    `{registration, nonce, thread_id, member}`, with `member` `present` (carried), `missing`
    (withheld; no `nonce` or `thread_id`) or `never_opened`; a `present` entry also carries
    `opened: {nonce}` (opening its `thread_opened`) and `head: {nonce, record_digest}` (opening its
    head in the bundle's `sale_cut`). The bundle is made in this order: each opened thread's
    report is sealed (in its own log, or, for a thread whose reports are off its log, as a
    standalone report bound to its checkpoint), then the `sale_cut` naming each thread's last
    record (that report, or the thread's last step), then the sale log's checkpoint, which is
    therefore new and certifies the cut: the interval covers every thread's acts up to the
    bundle. `capsulectl verify` opens each
    registration, holds the carried threads to exactly the `present` ones, verifies each as its
    own Evidence Bundle, and checks it names its registration and opens its
    `sale_authority_commitment` to this sale's task authority, that its `thread_opened` opens to
    its task authority, and that its last record on its log is its head in the latest
    `sale_cut`; a `never_opened` registration has no `thread_opened` and no head in the latest
    cut, and is shown only when every certified record of the sale's log is disclosed (one
    withheld could be its opening). A `missing` thread, a sale
    whose threads predate registration (`threads_predate_registration`), a copy without
    `thread_opened` or `sale_cut` (made before they were sealed), or a present thread the latest
    cut does not name, is INCOMPLETE; any mismatch is INVALID. No buyer's copy carries the sale bundle, a `thread` record or an
    opening. A thread's
    request and limits are the sale's and do not change per buyer (no `intent` steps). Each
    `check` of a thread carries `item_ref_commitment`: the sale's item reference committed under
    that check's own nonce. A thread's `check` that names no one carries the thread's buyer as its
    `counterparty`: exactly the keyed fingerprints the thread's `baseline` sealed (per deal, as on a
    purchase's check), so a rule keyed on the counterparty has a target. A check sealed before this
    carries none. So no value in a thread's records is equal across the sale's threads,
    and a buyer's copy carries the thread's task authority and its offers in full. The user's own
    copy and an adjudicator's carry `sale_authority_opening` (`{record_digest, nonce, text}`, the
    text being the sale's task-authority digest), which ties the thread to the sale; a buyer's
    copy does not. A thread verifies alone, and the producer checks, before each check, that the
    sale's task authority is still the one the thread opened under. The profile's own rules
    checker is given the plain reference (`item_ref`), the same on every thread, which is how it
    holds a sale to one accepted commitment; no record carries it.
11. **A seller commits on the accepted terms.** A seller's `commit` rests on its check, which
    seals exactly the `amount_minor`, `currency`, `terms` and `recourse` of the offer the other
    party accepted; a commit on other terms is refused (`changed_after_acceptance`).
12. **Representations (where the user sells).** A seller's deal, or a sale and its threads,
    opened since this rule carries `ordered_representations` in its opening intent and task
    authority. It lists the representation classes the user's words order the agent to state
    ("tell them about the scratch": `condition`), and is empty when nothing is ordered. A deal or
    sale opened before has none and keeps the earlier rules; a sale's thread follows its sale.
    Under this rule:
    - Every `claim` with `source_kind: agent` names a `class` and carries exactly one of these:
      - `authority_basis` with `evaluation_ref` and one `authorized_by` ref;
      - `unchecked: true` with a `rule` and no ref.

      The claim rests on a `check` with `action: "represent"`. Typed, that check is a
      `proposed-action/v0` whose `representing` seals the class in the clear, and the exact words
      (and where they will be said) as commitments.
    - A `represent` check of an ordered class is DO. The claim is authorized by its evaluation
      (`authority_basis: [task_authority]`), and no approval is sealed.
    - A `represent` check of any other class is ASK. Only the user's own approval, in their words,
      authorizes the claim (`[task_authority, user_approval]`, then any platform approvals, as on
      an action).
    - Authorization holds only when all of these are true:
      - the claim's class equals the checked one, and its words are the checked words. The claim
        is sealed under the check's own nonces, so its `text_commitment` (and
        `source_ref_commitment`) equals the check's `representing` ones;
      - no `detail_change`, and no `message` or `evidence` carrying `counterparty` identifiers or
        `counterparty_facts`, is sealed between the check and the claim;
      - the check is unused (one check or approval covers one claim, action or disclosure);
      - the check is still within its `valid_until`;
      - the task authority in force is the one the evaluation names.
    - A claim nothing authorized is still sealed, since the words may already have been said,
      with `unchecked: true` and its rule:
      - `no_check`, `not_approved`, `denied` or `approval_used`;
      - `differs_from_check`, `changed_after_check`, `authority_changed` or `check_expired`.
    - `represent` is never an `action` and never in `allowed`.
    - The list changes only by the user's `confirm_limits` of an `intent` that proposes another
      one. Adding a class and dropping one are both proposals.
    - While an ordered class has no authorized claim in this deal (on a sale, in this thread), a
      check of `offer`, `commit`, `sign`, `pay`, `share_contact` or `share_credentials` asks, with
      finding `required_representation_missing` and no `proceed` option. No transaction approval
      waives it. That holds whenever the step is noted too: such an action sealed since the user
      ordered more is unchecked. A `cancel` is never held.
    - Approving a `represent` check approves those words only. It never adopts a change of
      terms or recourse as agreed.
    - **Known limit.** `source_kind` is the agent's own label. On these deals a `merchant`,
      `platform` or `external` claim must name its `source`, but a statement the agent labels
      `user` is not held to this rule.
13. **One deal per errand: legs and moves.** A deal records an errand from the user's first ask.
    It may open with no counterparty (no `counterparty` block, no `recourse`). A *leg* is the
    stretch of the errand with one counterparty: it starts at the baseline's counterparty, at a
    later `intent` that carries a `counterparty` block (the user picked or switched to that
    merchant), or at an `agent_move` `chose` that carries one while no leg exists. A leg start
    is a change of details: no approval sealed before it covers a later `action` or
    `disclosure`. Within a leg, `payee_or_contact_changed` compares with that leg's first
    contact, so the user's own switch never reads as the counterparty changing its details,
    while a `detail_change` within the leg still does. The ask and the limits carry over a
    switch: a later intent's `asked` fields overlay the ask in force, field by field.
    An intent's `move` is derived by the producer from the intent in force and the latest
    `options`, never stated by the agent, first match winning: `switched` (another merchant once
    a leg exists, another merchant after a `couldnt` or off the shortlist after a
    `recommended`, or another item than a non-empty `asked.item`), `picked` (an item or merchant
    of the latest `options`), `raised` (only more than the limits in force), `narrowed` (any
    other change). An intent that changes nothing and proposes nothing is refused. `proposed`
    marks an intent that asks for more, whatever its move; it applies only on `confirm_limits`.
    An approval agrees only to the terms its card named: on a check that states
    `rule_inputs_version` 3 (section 8), the card names each term the check changes from what was
    agreed, and an approved proceed adopts a changed term, with the value the check proposed (a
    dropped term included), only when the card it was given on named it, so a `standing_intent`
    approval adopts none and the next check carrying that term still counts it. An approval of a
    check without it adopts the checked terms whole, as before.

## 7. Outcome conventions: `completed | mismatch | open`

| outcome | Meaning | Set when |
|---|---|---|
| `completed` | What was delivered matches what was agreed (the baseline, as updated by approved checks). | `status: "received"` and no differences. |
| `mismatch` | What happened differs from what was agreed. The differences say how. | `status: "received"` with differences; `not_received`; `unchecked_action`. `reverted` is either, depending on its differences. |
| `open` | No final result was observed when the record was sealed. | `status: "pending"`, or a close with no outcome record. |
| `not_selected` | The other side was not chosen; nothing was done. | `status: "not_selected"`. |
| `called_off` | The user dropped the errand; nothing was done. Never "nothing delivered". | `status: "called_off"`, actor `user`. |

These are record facts about delivered-versus-agreed. They are not judgments of the
counterparty. Records never label or grade a counterparty, a person or a business, and the
checker rejects such vocabulary in any field.

## 8. Checking

`python3 check_profile.py` runs the RFC 8785 self-test and then, for every fixture, these
stages: canonicalization → personal_data → wording → schema → chain → digest. The
personal_data stage flags phone and email shapes, and any raw value from the local store
(`fingerprint-vectors.json` stands in for it). A producer SHOULD run the same scan against its
own store before sealing. `python3 check_profile.py deal.json` checks a JSON array of one deal's
records. `--regen` rewrites the fixtures deterministically.

The story in the fixtures (all fictional; 555-01xx numbers, `.example` domains):
1. Baseline: 2 jet skis Saturday from "Coastal Jet Rentals LLC", card, refundable.
2. Quote.
3. Claim: skis available.
4. Evidence: a listing photo doesn't establish it.
5. Intent: the user allows `share_contact`.
6–9. Check, pass, standing-intent approval, action: the user's own number is shared.
10. SMS from a new number.
11. detail_change: payee → "M. Torres", Zelle, non-refundable.
12. Check `pay` $200.
13. Verdict pause: payee and phone changed since first contact, Zelle to a business, site
    registered 3 weeks ago.
14. The user taps **Hold**.
15. Outcome: nothing delivered, `mismatch`.
16. Close `mismatch`.

### Rule inputs on a check

A check sealed from this version carries, beside what is about to happen, the scalars a rules
checker reads. Each is a number, a member of a small closed set, or an opaque reference, never a
name, a contact detail, a diff or free text, and each is absent when it is not known. A check
sealed before carries none and re-derives unchanged.

| Field | Value |
| --- | --- |
| `recipient_role` | A share's recipient, by role: `fulfilling_merchant` (`disclosing_to: counterparty`, on a deal where the user buys or states no `party_role`), `buyer` (`disclosing_to: counterparty`, where the user sells), `platform` (`disclosing_to: platform`: a marketplace, booking site or mail relay between the user and the counterparty) or `third_party` (`disclosing_to: other`). `self` is in the set and never derived. Absent on other actions. |
| `channel`, `first_contact_channel` | Channel kinds (the `channel` set above): the one in use at this check (the latest sealed message's, else first contact's) and the baseline's. |
| `upfront_amount_minor` | The deposit the check's own terms state (`terms.deposit_minor`), in minor units. Never inferred. |
| `material_fields_changed`, `material_fields_basis` | How many of `terms.item`, `terms.quantity`, `terms.price_minor`, `terms.deposit_minor`, `terms.currency`, `terms.when`, `terms.place`, `terms.conditions`, `recourse.rail`, `recourse.refundable`, `who.payee` the proposal (the deal as it stands with the check on top) changes from what was agreed (payee: the first contact's payee, else its name), and the hex SHA-256 of that list's JCS bytes. On a check sealed since rule inputs version 2, "what was agreed" is the leg's (section 6 rule 13): a quantity either side leaves out is 1, a place counts only when both state it and a refinement of the same place (a postal code or region added) is no change, and the way to pay counts only once something was agreed about it with that leg. What was agreed is updated by each approval by the rule of the check it answers (`rule_inputs_version`). |
| `rule_inputs_version` | `3` on a check sealed under the rule that an approval of it adopts only the terms its card named (section 6 rule 13); it then carries `material_fields_changed` and `material_fields_basis`. Absent on a check sealed before, which re-derives and verifies as before. In a deal that mixes versions, each approval adopts by its own check's rule. |
| `offer_fields_changed`, `offer_fields_basis` | How many of the offer fields (`terms.item`, `terms.quantity`, `terms.price_minor`, `terms.deposit_minor`, `terms.conditions`, `recourse.refundable`) the user's own words state (`intent.asked`) that the proposal does not match, and the list's digest. Only the keys the user's words state are compared: a key they never state is not a change. Both are absent when the user's words state none of them. |
| `task_authority_ref` | Typed records only: the task authority in force, as on the evaluation. |

**Counting.** For the material count, a key counts as changed when it is present on one side and
absent on the other, or present on both with unequal JCS values. For the offer count, only a key the
user's words state counts, when the proposal lacks it or differs. `terms.conditions` counts as one key, so a count is never
more than its list is long. The record also keeps the separate `recourse {rail, refundable}` block,
which a rules checker reads as the rail and refundability; a pay check always states it.

### The external rules checker

A deal profile may pin an external rules checker: the user's policy (`profile update
--rules-checker FILE`), never the agent's. `FILE` is `{"command": ["/absolute/path", "arg", …],
"timeout": "10s", "definition_digest": "<64 hex>"}`. `timeout` (default 10s, at most 60s) and
`definition_digest` (the ruleset the checker must report) are optional. To pin more than one
ruleset, for a checker whose ruleset depends on the deal (on its `party_role`, say), give
`"definition_digests": ["<64 hex>", …]`, with or without `definition_digest`: the checker must
report one of them all. A profile
stores one digest as `definition_digest` and two or more as `definition_digests`; `profile show`
and `doctor` print every one. The executable must sit under a trusted plugin root, and the
profile pins its SHA-256.

At every check, after the `check` record is sealed:
- **What the checker gets.** The command runs exactly as pinned, from a private copy of the
  executable bytes that were hashed, with only `PATH` and `HOME` in its environment. On stdin it
  gets one `external-check-input/v0` object (`external-check-input-v0.schema.json`, beside this
  file):
  - `record`: that `check` capsule, with its disclosed `agent_input` (the deal record, whose
    body is what is about to happen), and `counterparty_profile` when the check names a payee:
    the value its `counterparty_profile` record seals, `{fp_alg: "hmac-sha256-profile-key",
    ids: {payee: <64 lowercase hex>}}`, supplied beside the capsule and never part of it; and,
    on the check of a seller's `commit` that rests on an accepted offer (the latest offer, with a
    recorded acceptance and no change of details since; section 6, rules 9 and 11), `proposal_at`: that
    offer's proposed-action record's sealed `at`, byte for byte, so a rule can hold the offer to its
    expiry. It is absent on a buyer's deal, on any other action, with no acceptance in force, and on
    history entries;
    and, where the user sells, `deal_claims`: the deal's claims with `source_kind` `agent` and a
    `class`, sealed before the checked step, in seal order, each as its sealed claim record states
    it (`capsule_id`, `record_digest`, `class`, `source_kind`, `at`, `text_commitment`; never the
    words), so a rule on a required disclosure finds a statement the agent made earlier in the deal.
    Under the representation rules (section 6, rule 12) each also carries the claim's own
    `authority_basis`, exactly as sealed, or `unchecked: true`, so a rule tells a statement the
    user ordered or approved from one they did not.
    It is absent on a buyer's deal, when there are none, and on history entries;
    and, on a `represent` check, `representing_opening`: `{nonce, text}`, the opening of the
    check's `representing.text_commitment`, the exact words the agent is about to say, so a rule
    judges the words (a promise it never allows). A checker verifies it against the commitment.
    It is absent on any other check and on history entries;
  - `history`: the profile's earlier sealed acts with an amount, from every deal on this
    profile's own store, sealed in the last 31 days, newest first, at most 1,000 of them, in the
    same shape. Each act carries the `counterparty_profile` of the check it rests on (act →
    approval → verdict → check → that check's `counterparty_profile` record; in typed records a
    verdict that needed no approval authorizes the act itself: act → verdict → check → record),
    and none when it has none (an act done without a check, or one checked before checks had a
    companion). A rule keyed on who was paid before reads one merchant as one payee across the
    profile's deals from there on; acts checked before keep only their per-deal fingerprints.
    Each act also carries `counterparty`, the counterparty that check sealed, exactly as sealed (an
    x-deal-v0 check's header `counterparty`, `hmac-sha256-deal-key`; a typed check's
    `body.counterparty`, `hmac-sha256-chain-key`), beside the record and never in it: an act record
    names no counterparty itself. It is keyed per deal, so it matches the deal's own checks (a
    repeated booking in one deal) and never another deal's; none when the act rests on no check or
    the check names none.
    An act on a sale's thread also carries `item_ref`, the sale's item reference (the same plain
    value as the top-level `item_ref` below, beside the record and never in it), so a rule sees an
    earlier thread's acceptance of the same item; an act of any other deal carries none;
  - `history_scope`: `{days, max_records, complete}`.
  - `party_role`: `buyer` or `seller`, the side of the deal the user is on, as the deal's opening
    intent sealed it (`buyer` when it names none). Sent on every check, always explicit.
  - `task_authority_record` (typed deals): the whole sealed task-authority record the check's
    `task_authority_ref` names, exactly as sealed: SHA-256 over its JCS bytes is the ref's digest,
    and its plan (`outcome_id`, `allowed_actions`, `preconditions`) is at `body`. It is the opening
    task authority, or the later version the user confirmed (`confirm_limits`, sealed as a
    task-authority record). Absent in a deal with no task-authority record.
  - `item_ref` (a sale's thread): the sale's item reference, 64 lowercase hex, the same on every
    thread of the sale (section 6, rule 10). Only this device's checker is given it; no record
    carries it. A rule that needs it and is given none reports `not_evaluable`, never a pass.

  A rolling window is evaluated over the history the deal check supplies. A checker given no
  history, or too little (`complete` false, or a window longer than `days`), reports that rule
  `not_evaluable`.
- **What it prints.** Exit 0 means it printed one `external-check-result/v0` object
  (`external-check-result-v0.schema.json`, beside this file): `ruleset_id`, `definition_digest`,
  `verdict` (`allow`\|`deny`\|`escalate`\|`not_evaluable`), an optional `tier`
  (`recomputed`\|`judged`, as in Result v0; an absent tier reads as judged, never recomputed), an
  optional `grade` (`self-attested`\|`witnessed`\|`countersigned`, by reference to Result v0's
  Grade) and
  `findings[]` (`{id, check,
  verdict: pass|fail|not_applicable|not_evaluable, disposition, reason, limit, value}`, passes
  included; `disposition`, optional, is what a failing rule does on its own: `ask` or `deny`;
  `limit` and `value` are each a number, or a string of at most 64 characters with no line
  break).
  Members outside the schema are refused. Any other exit is a refusal of the input, with the
  cause on stderr.
- **What the check does with it.** It folds the answer into its one verdict:
  - `allow` adds nothing;
  - `escalate` and `not_evaluable` add a `rules_escalate` or `rules_not_evaluable` difference
    per failing finding (with its limit and value), and the check pauses. A finding the
    checker reports as `not_evaluable` with the reason `declared, not measured` (a rule its
    ruleset declares and it does not measure) is never a difference: it never moves the verdict,
    and the approval text and the user's own receipt name it quietly beside it;
  - `deny` adds `rules_deny` differences, and the verdict is `deny` with no way to proceed.
  - Each difference's words follow its own finding's `disposition`: under a `deny`, a rule
    that only asks reads "Your rules ask for approval", and one with no disposition reads
    "Flagged by your rules", unless it is the only failing rule under the deny, which reads
    "Not allowed". Under an `escalate` nothing reads "Not allowed".
  - A checker that is configured but changed since it was pinned, refused the input, timed
    out, printed anything else, or reported a `definition_digest` that is not pinned
    adds a `rules_not_checked` difference naming why, and the check pauses.

The verdict's `rules` seals the outcome as data:
- `status: evaluated`, with `ruleset_id`, `definition_digest`, `checker_sha256`, `verdict`,
  `tier` (as reported, or `not_stated`), `grade` (as reported, or absent; both recorded and
  shown in the approval text, changing nothing), `findings` (each `{id, check, verdict, disposition, limit, value}`), and `history` (`{days, acts, complete}`:
  what the checker was given beside the record);
- `status: not_evaluated`, with `cause` (`checker_unavailable`, `checker_changed`, `refused`,
  `timeout`, `unreadable` or `ruleset_changed`) and `checker_sha256`;

A finding's `limit` and `value` never carry counterparty data: they are the limit and the value from
the record the rule compared with it (an amount, a count, a window total), never a name, a contact
detail or an identifier. A checker should report them as numbers. A string there is scanned like
every other string in a record, and an answer that cannot be sealed (one that would carry a phone
number, an email address, the counterparty's details or a word a record may not carry, including a
run of amounts that reads like a phone number) never fails the check. The check pauses with the
rules not checked, cause `unreadable`, and its reason says it could not be sealed.
- `status: not_configured`, when no checker is pinned. That check does not pause for it, but its
  record and its approval text say the rules were not checked.

No words the checker wrote are sealed in the clear: its reasons and its stderr reach the card and
the approval text, and the card is committed to.

## 9. Typed action records and the check contract

A deal opened with `--records typed` seals its authority steps as typed action records, one
schema per type under `records/`, and its evidence (baseline, messages, claims, evidence,
detail changes, intents, disclosures with no authority, close) as x-deal-v0 records. Both kinds
sit in ONE chain: one `seq`, one `prev` chain, one root. No typed record type carries "deal".

Every typed record has the common header (`records/record-common-v0.schema.json`): `type`,
`canonicalization` (`"jcs"`), `chain_id` (the deal's id), `seq`, `at`, `prev`, `chain_root`
(the baseline), optional `refs` (`{rel, type: "record", digest_alg, digest}`), optional
`producer` and `commit_alg` (as in section 2) and `body`.
Counterparty fingerprints in a typed body name `fp_alg: "hmac-sha256-chain-key"` (the same keyed
fingerprint as section 4). Digests are over the record's JCS bytes, as for x-deal-v0.

| Type | Sealed for | Replaces (x-deal-v0) |
|---|---|---|
| `task-authority/v0` | The user's task authority (on a sale's thread, also its `sale_authority_commitment` and its `registration` ref, section 6 rule 10): their words by commitment, `max_total_minor`, `allowed` (and, from this version, the plan shape a rules checker reads: `outcome_id`, a fixed id per deal type such as `capsulectl.deal.purchase/1.0.0`, the same actions as `allowed_actions`, and `preconditions: []`; on a seller's deal, `ordered_representations`, section 6 rule 12). Sealed after the baseline (`source` ref), and again when the user confirms new limits (`approves` the intent, `previous_ref`, `said_commitment`). | the baseline intent; `approval` with `confirm_limits` |
| `proposed-action/v0` | The action about to be taken, exactly as checked; on a seller's deal also a `represent` check, with `representing` (section 6, rule 12). | `check` |
| `action-evaluation/v0` | The deal check's disposition (`DO`, `ASK` or `DENY`) and findings, with the contract fields below. | `verdict` |
| `action-approval/v0` | An approval artifact of one stated `authority`: `user_approval` (the user's own answer, with their words and the `rendering_commitment` of what they were shown), `card_answer` (a card answered with no words), `platform_approval` (a platform approval observation, below), `policy_change` (the user confirming a policy change: `rendering_commitment`, `effective_policy_digest`, `semantic_diff_digest`), `one_shot_override` (reserved). | `approval` |
| `action-record/v0` | What the agent did, with `evaluation_ref` and `authority_basis`; a disclosure that needed approval is an `action-record/v0` with `disclosed`. | `action`; `disclosure` with authority `approval` |
| `action-outcome/v0` | What was observed (`attempted` names an unchecked action). On the outcome a close seals, `actor` and `reason`, as on the close. | `outcome` |
| `action-report/v0` | Reserved for the report a chain is summarized into. | — |

**The evaluation's contract fields.** `proposed_action_digest` is the digest of the
`proposed-action/v0` it evaluates; `task_authority_ref` names the task authority in force;
`ruleset_digest` is the digest of the rule table the evaluator ran (`{evaluator, rules,
materiality_digest}`), so it also covers the materiality predicate; `materiality` and
`materiality_digest` state the predicate the check evaluated, always, never by absence: mode
`predicate` with the digest (SHA-256 of the JCS bytes) of the `materiality-predicate/v0`
document the profile pins (`deal init` / `profile update --materiality`; a check cannot
choose another); or, with none configured, mode
`none_fail_safe` (every pick the agent made alone pauses) with `materiality_digest: null`, which
`ruleset_digest` then covers; `valid_until` is when the evaluation
stops covering a step; `authority_basis` is `[{type: "task_authority", ref}]`;
`rendering_commitment` commits to the card the evaluation rendered. `rules_checks`, when the
profile's external rules checker ran, names the ruleset it reported (`ruleset_id`,
`definition_digest`) and its verdict; `ruleset_digest` stays the built-in rule table's. `DENY`
(the rules did not allow the action) has ≥ 1 finding, `rules_checks`, and no option but `hold`.

**Typed authorization.** In a chain whose second record is a `task-authority/v0`:
- every check, verdict, approval, action and outcome step is a typed record, and an approved
  disclosure is an `action-record/v0`;
- an `action-evaluation/v0` names the proposed action it follows (`checks`) by
  `proposed_action_digest`, and the task authority in force by `task_authority_ref`;
- a `DO` evaluation authorizes one step on the task authority alone (`authorized_by` names
  the evaluation); no approval is sealed;
- an `ASK` is answered only by a `user_approval`, the user's first answer to that
  evaluation. Its `rendering_commitment`, when present, equals the evaluation's. A
  `card_answer` and a `platform_approval` never answer it;
- a step after the evaluation's `valid_until` is not covered;
- an approval covers only the proposed action its evaluation names. A changed proposed action
  is a new evaluation with another `proposed_action_digest`; an earlier approval never extends
  to it, and the materiality predicate never extends one;
- the step's `evaluation_ref` names the evaluation it relied on, and its `authority_basis`
  lists, in order: the evaluation's task authority; on an `ASK`, the user's answer it cites;
  every platform approval observation whose `proposed_action_ref` is that evaluation's proposed
  action, with `scope: "mismatch"` exactly when the displayed text stated another amount.
  `one_shot_override` is refused.

**Platform approval observation.** An `action-approval/v0` with `authority: "platform_approval"`
and `kind: "platform-approval-observation"` records that another platform's own approval
interaction for a proposed action happened with these bytes: `platform` and `mechanism` (names
the caller gives), `displayed_text_digest` (what the platform displayed),
`returned_user_text_digest` (the user's text it returned, when there is one),
`proposed_action_ref` and `observed_at`, and the amount the displayed text stated. It proves
only that this interaction was recorded with these bytes. It does not say that the platform
authorized anything or that any rule is satisfied, carries no choice, and never answers a check.

**Counterparty acceptance observation.** An `action-approval/v0` with
`authority: "counterparty_acceptance"` and `kind: "counterparty-acceptance-observation"` records
that the counterparty accepted one exact offer: `proposed_action_ref` (the offer's proposed
action), `observed_at`, the channel kind they accepted on (`channel`, from x-deal-v0's channel
set; optional), and their words by `content_commitment` (the same `{nonce, text}` commitment).
It is recorded as observed, authorizes nothing by itself and never answers a check; a seller's
`commit` rests on it (section 6, rule 9).

**One rendering commitment.** What a person was shown is committed one way: the text under the
nonce of its rendering (`{nonce, text}`, JCS, SHA-256). An evaluation's `rendering_commitment`
and the user's answer to it (equal exactly when what was shown is what was checked), a
policy-change confirmation's `rendering_commitment`, and an observation's
`displayed_text_digest` are that one commitment.

**The check request and response.** `records/check-request-v0.schema.json` and
`records/check-response-v0.schema.json` are the shapes an evaluator takes and returns, sealed or
not. A request carries the `phase`, the ruleset and task authority it is checked against, the
proposed action, history refs and any platform approvals; a response carries the
`disposition`, `valid_until`, `proposed_action_digest`, findings, `authority_basis`,
`evaluation_ref`, `ruleset_digest`, `task_authority_ref`, `materiality` and `materiality_digest`.
`capsulectl deal check` returns the response for the evaluation it sealed (`check_response`).

## 10. The two halves of a deal (`ec:deal-reconcile@1`)

Each party to a deal (`deal open --deal-id`) seals its own close. `deal terms-result` reconciles
the two closes under agent-action-capsule's Deal Reconcile Contract, `ec:deal-reconcile@1`
(`spec/deal-reconcile-contract-v0.md`, cited here as DR), with evidencebook's recompute. This
section is the profile's declaration that DR-4 asks for.

**The half.** A party's half is its final close: the last `close` step whose outcome is not
`open`. It is taken as its bundle record: the Capsule with its producer `signature` and `key_id`,
the object every deal copy carries in `records`. The half's octets are the JCS of that object,
and its digest is their SHA-256. That digest is not the `capsule_id`. Both parties compute the same
digest from the same record. Only a deal whose steps are book records (since capsule-cli#242) has
halves.

**The half mapping (DR §4.2):**

```json
{"profile": "x-deal-v0",
 "members": {"deal_id": "/deal_id", "book_id": "/key_id",
             "recorded_at": "/evidence_record_header/committed_at",
             "commit_alg": "/evidence_record_header/commit_alg",
             "amount_commitment": "/evidence_record_header/field_commitments/amount",
             "currency_commitment": "/evidence_record_header/field_commitments/currency"}}
```

`book_id` is the hex public key the record's producer signature verifies under. Both parties'
evidence books are named `deal/<deal_id>`, so the signer is the one per-party identity a half
carries, and this profile defines it as the book identity for this contract. `recorded_at` is the
step's own time, to the second, with `Z`.

**The commitments** are built as DR §4.1 requires: `sha256-jcs-nonce256`, with a fresh 32-byte
nonce for each record and each field. `amount` is the price in minor units as a base-10 integer
string, and `currency` is the code as the close states it. Both are written the same way by both
parties' capsulectl (DR-1). Only a final close whose status is `received` commits non-empty terms:
the delivered price and currency, or the agreed ones where the close names none. Every other step,
and a received close with no known price or currency, commits the empty text. So the two halves
match only where both parties closed as received, and every other pair is `INSUFFICIENT` (DR §6
rule 5). A finally closed deal takes later evidence only, chained `confirms` to the close, so the
final close is the deal's last step chained `follows`.

**Admission (DR §4.1).** A half is admitted only after these checks; any failure refuses the
reconcile:
- its producer signature verifies, under this profile's key for the own half, and under the
  responder key pinned for the recorded copy for the counterparty's;
- its inline header is a `deal_step` of `deal/<deal_id>`;
- a verified checkpoint covers it: the book's own, or the copy's;
- it is the final close. The own half is found by its steps. The counterparty's is the record its
  `field_openings` names, which must chain `follows`, with every later `deal_step` in the copy
  chaining `confirms` to it (sealed reports are passed over), and no record missing between it and the copy's checkpoint. Each record's `seq` is read only where it equals the log position the copy's verified membership gives it (`completeness_certificate.memberships[*].log_coordinates.seq`), since a bundle verifier checks those coordinates and not the header's `seq`. Where the copy discloses its payload,
  it must be a close whose outcome is not `open`, and a verified opening must not contradict the
  outcome record it refs.

**Openings (DR §5).** A `disclose --deal ID --share counterparty --to WHO` file, and a
`disclose --deal ID --share adjudicator --to WHO` file for an adjudicator the user names, carries
`field_openings` for the party's own final close only, keyed by its half digest. It does so when
the close commits non-empty terms. The close's amount is a structured field, so it is carried
even when it equals the user's spending limit or floor: the bounds are kept by field (the bound
fields and their openings are in no counterparty copy, and the floor is in no shared copy), and by
value only in free text, which can say that a number is a bound. Each release
is on record on the deal's disclosures log, and each party releases only its own openings. No
other copy carries openings: not a `deal respond` answer (its requester is not checked), a report
page, the user's own bundle, or a History. `deal_local` is never opened.

**The claim (DR §7).** The Result has one claim per requirement, `tier` `recomputed`, citing no
Close, with the counterparty's signing key as `peer`. That key is long-lived, so Results about
different deals with one counterparty can be linked by anyone who holds them. DR §7.5's per-deal
state is not shown. `book result build` refuses a claim under exactly `ec:deal-reconcile@1`, because its
evidence is not in any book.

**Sealing.** `deal terms-result` seals each Result it writes as an `evidence_result` record on the
deal's results book, `deal/<deal_id>/results`: a book of the profile's shelf, never the deal's own,
so no copy of the deal carries it. The record is the profile's own Capsule (action id
`capsulectl-deal-terms-result`), with the header's subject `ec:deal-reconcile@1`. Its payload is
the JCS of `{"type": "deal_terms_result", "profile": "x-deal-v0", "deal_id", "nonce", "result"}`:
the Result under a fresh 256-bit nonce, as a sealed report's text is. A share withholds the other
Results' payloads, but keeps their Capsules, so the nonce stops anyone from confirming a guessed
earlier Result against a payload digest. The Result is never in the record's header, which every
bundle of the book carries. It is sealed through the same path as `book result build`, and its only resolver
is the run's own halves. A run equal to the newest sealed Result in every member but
`generated_at` returns that record. Any other run seals a new one, and the book keeps every Result.
A sealed Result proves what the producer reported, and when by its own clock; a witnessed tick
then covers its book's checkpoint, as it covers a deal's. The results book ends with its deal. A
sealed Result goes to the counterparty as a bundle of the results book that discloses that Result
only (`deal terms-result --share counterparty`).

**Checking a Result.** `deal terms-result --check` recomputes a Result from the inputs given.
The Result names its halves, since each claim's `evidence` is the sorted digests of every half in
the reconcile (DR §7.3), so the check requires the halves it reads to be exactly those: the
producer's halves are provable. Two inputs are not recorded in the Result:
- which openings were carried, since it carries no opening (DR §7.3). A missing opening is
  `INSUFFICIENT` where a carried one could match, so a check uses exactly the openings named: the
  counterparty's own come only from the disclose file it sent (`--sent`), whose digest must be a
  share it recorded;
- the coverage declaration, which no claim cites (DR-3). A different coverage input can turn an
  `A_ONLY` into `INSUFFICIENT`, and a check cannot tell which the producer used. The verb gives
  none, so its own Results never hold `A_ONLY` or `B_ONLY`.

**An adjudicator's check** (capsule-cli#278) needs no deal store. It takes each party's own
adjudicator file and the signing and checkpoint keys it holds for each party, never read from the
files: `deal terms-result --check RESULT --copy FILE --copy FILE --party-key SIGNER:CHECKPOINT
--party-key SIGNER:CHECKPOINT`.
- **Admission.** Each file must verify as a bundle whose checkpoint is signed by one party's pinned
  checkpoint key. Its half is the one record its own openings name, admitted as above under that
  party's signing key.
- **The two files** must be of one deal and from two parties. The Result's peer must be one of
  them.
- **Recompute.** Each party's openings come from its own file, and the outcomes are those of any
  `--check`.
- **One file** (a party that declines to disclose) is "cannot reproduce": one side cannot be
  checked alone.
- **A Result made without the other party's openings** names one half where the files give two,
  and "differs" on its evidence. That is never a finding that a claim is false.
