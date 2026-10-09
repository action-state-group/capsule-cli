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
| `fixtures/positive/` | One deal, 18 records, covering all 13 record types, with the expected JCS digest of each. |
| `fixtures/positive-typed/` | One deal sealed in the typed action records beside x-deal-v0 evidence records (section 9). |
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
  `chain.relation: "follows"`. A `follows` link is ordering only. The Capsule claims only what
  the record shows:
  - An `action` record (an act a sealed approval authorized) whose action has a registered AAC
    `effect.type` is `action_type: "decide"`. Today that is `pay`, as `send_payment`. Its
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
    `one_way_consequential`.
  - Every other step is `action_type: "fyi"`: the other actions (`commit`, `cancel`, `sign`)
    and disclosures have no registered `effect.type` yet, and an unchecked act is sealed as an
    `outcome`, never as an action. Steps sealed by an earlier capsulectl are all `fyi`.
- `record_type` is not AAC's `action_type`. AAC `action_type` stays `fyi` or `decide`.
  `record_type` is this profile's own closed vocabulary.
- The record carries **no raw personal identifiers** (section 4). Raw values stay in the
  device's local store.
- Records are closed. Unknown members fail the schema. A new member means a new profile
  version (`x-deal-v1`).

**Sealed reports are not steps.** Each copy of a deal report that is written to a file
(the user's own receipt, or a shared copy) seals its readable text as one more Capsule on the
deal's log:
- **The Capsule:** `action_id: "capsulectl-deal-report"`, `action_type: "fyi"`, not chained.
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

## 2. The extension block `x-deal-v0`

| Field | Type | Req | Meaning |
|---|---|---|---|
| `profile` | `"x-deal-v0"` | REQUIRED | Profile and version. |
| `canonicalization` | `"jcs"` | REQUIRED | Pinned: RFC 8785. Any other value fails closed (section 5). |
| `deal_id` | string `^deal-[0-9a-f]{16,64}$` | REQUIRED | Random, opaque, and the same on every record of the deal. Never derived from any identifier. |
| `record_type` | enum (section 3) | REQUIRED | One of the 13 record types. |
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

Thirteen record types. The set is closed: an unknown `record_type` fails the schema.

| record_type | What it records | Required body fields | Required refs / block fields |
|---|---|---|---|
| `baseline` | First contact: what the user asked, who the counterparty is, the terms and the way back. The deal's contract. | `deal_type` (`purchase`\|`rental`\|`booking`\|`service`), `intent` (as the intent body; its optional `party_role`, `buyer`\|`seller`, is which side of the deal the user is on, absent meaning `buyer`, as on every deal sealed before it was recorded), `terms`, `recourse.rail` + `recourse.refundable`; `materiality` (the materiality predicate pinned when the deal opened, as on a verdict; a verdict under another one carries a `materiality_changed` difference) | `seq` = 1; `channel`; `counterparty` (≥ 1 id). No `prev`, no `baseline_ref`. Optional `claims[]`, `counterparty_facts`, `demo`, `skill` (`{"skill_md_digest": <64 hex>, "other_copies": <int ≥ 0>}`: the SHA-256 of the SKILL.md the agent reported following, and how many other copies of that skill sat beside it; a boundary marker for accidents, not proof the instructions were followed). |
| `sale` | Where the user sells one item to one of several buyers: the first record of the sale's own log (deal_id `sale-…`), before any buyer. Followed by the sale's one `task-authority/v0` and nothing else. | `deal_type`, `intent` (the seller's request and limits, `party_role: seller`), `terms`, `recourse.rail` + `recourse.refundable`, `item_ref_commitment` (the sale's opaque item reference, committed) | `seq` = 1. No `prev`, no `baseline_ref`, no `counterparty`, no `refs`. Optional `demo`. |
| `intent` | The user restates or picks within the ask. Replaces `verbatim` / `asked` from here on. `allowed` and `max_total_minor` carry forward unchanged: an intent may lower the limit or drop actions, and a higher limit or a new action is only a proposal (section 6, rule 6). | `verbatim_commitment` | `baseline_ref`, `prev`. Optional `party_role`, equal to the baseline's (a deal's role is fixed when it opens). |
| `message` | One message in the thread. The text stays local. | `from` (`counterparty`\|`user`\|`agent`), `content_commitment` | `channel`. `counterparty` when the message shows identifiers (for example, a new phone number). |
| `claim` | Something the counterparty (or listing) asserts, recorded as a claim, not a fact. | `text_commitment` (the claim's words, ≤ 200 chars, committed), `source_kind` (whose it is: `merchant`\|`agent`\|`platform`\|`user`\|`external`), optional `source_ref_commitment` (the caller's note of where it was read, committed). A claim sealed before claims were committed carries `text` and `source` in the clear instead, and re-derives unchanged (see "Claims"). | none beyond the chain. |
| `evidence` | What was done to establish a claim, and whether it did. Optionally a merchant's own email (`merchant_email`, below). After the deal's final close, the only record type allowed: later evidence linked to that close. | `source`, `verified` | exactly one `about` → a `claim` or the `baseline`. At most one `confirms` → the deal's final `close` (required after it, not allowed before it). Optional `resolves_obligation` (a digest ref) → an earlier record holding a cancel-by date. |
| `detail_change` | The counterparty changed an identifier, a term or the rail after first contact. Recording it never accepts it. | `source`, `changed[]` (field names) | `counterparty.ids` kinds MUST equal the identifier kinds listed in `changed`. At most one `source` → a `message` or `evidence`. |
| `check` | The agent asks, before a point of no return, exactly what is about to happen (the snapshot). | `action` (`pay`\|`sign`\|`commit`\|`cancel`\|`share_contact`\|`share_credentials`\|`offer`; `offer` only where the user sells) | `baseline_ref` (always). `counterparty.ids.payee` when a payee is involved. Optional `amount_minor` (the expected charge), on a `pay` `authorized_max_minor` (the most it may take: what a limit binds, at least `amount_minor`), `currency`, `seen_item`, `terms`, `recourse`, the rule inputs (below), `pack_id`, `pack_digest`, `action_class` + `taxonomy_version` and `spend_minor` (see Action classes, below), with `spend_authorized_minor` beside them when `authorized_max_minor` is sealed, on a `cancel` `fee_minor`, `cancelled_amount_minor` and `direction` (`in`), on a refund's `cancel` `reverses_ref` (the payment it returns, see Action classes, below), on a check of a sale's thread `item_ref_commitment` (section 6, rule 10), and for a share `disclosing` (the classes about to be given, as for a `disclosure`) and `disclosing_to` (`counterparty`\|`other`), which every share check carries. |
| `verdict` | The answer to one check: pass, pause with the differences, or deny. | `result` (`pass`\|`pause`\|`deny`), `differences[]`, `options[]`; `rules` (what the profile's external rules checker said, §8 "The external rules checker"; absent on verdicts sealed before it was recorded); `materiality` (the predicate that decided which of the agent's own picks pause: its `digest`, and `label_commitment`, a commitment to its name and version, opened only in the user's own copy (`materiality_openings`); `digest: "none"` when none was configured and every pick paused; verdicts sealed by v0.1.0-rc8 carry `name` and `version` in the clear instead, and shared copies withhold them) | exactly one `checks` → a `check`; one verdict per check; optional `pack_id`, equal to the check's when either names one. `pause` ⇒ ≥ 1 difference and ≥ 1 option. `pass` ⇒ no options. `deny` ⇒ ≥ 1 difference, `rules`, and no option but `hold`. |
| `approval` | What authorizes, or declines, the next step; or the user's confirmation of the limits an intent proposed. | `choice` (`hold`\|`verify_contact`\|`proceed`\|`confirm_limits`), `proceed` (`true` on `proceed`, `false` on `hold` and `verify_contact`), `approver` (`user`\|`standing_intent`\|`agent_card`); on `confirm_limits` only, `limits` (`previous` and `new`, each `max_total_minor` and `allowed`) | `confirm_limits`: exactly one `approves` → the proposing `intent`, `approver: user`, and section 6, rule 6. Otherwise exactly one `approves` → a `verdict`. `user` ⇒ `said_commitment` (the user's own words), and on a pause the choice is one of the verdict's options. `agent_card` ⇒ no `said_commitment`: a card the agent composed was answered, and no words of the user's are on record; never on `confirm_limits`. Optional `card_commitment` (on an answer to a verdict, never on `confirm_limits` or `standing_intent`): the card text the answer was given on, committed under the verdict's own card nonce, so it MUST equal the verdict's `card_commitment`: equal means the card shown is the card checked, recomputable without the text. `standing_intent` ⇒ the verdict passed, the choice is `proceed`, and the checked action is in the current `allowed` (`allowed` absent = no restriction; `allowed` present and empty = nothing is allowed yet, as in "show me options, don't book"). |
| `action` | A point-of-no-return step actually taken. | `action`; optional `direction` (`out`: paid by the user; `in`: back to the user); optional `action_class` + `taxonomy_version` and `spend_minor` (see Action classes, below); on a `cancel`, optional `fee_minor` and `cancelled_amount_minor` (see Action classes, below) | exactly one `authorized_by` → an `approval` with `proceed: true` (section 6). An action that returns money (`direction: in`) carries exactly one `reverses` → the `pay` action it undoes (section 6, rule 8). |
| `outcome` | What was observed afterwards: delivered or not, or an action taken without approval. | `status`, `outcome`, `differences[]` | At most one `observes` → an `action`. |
| `close` | The deal ends (or pauses its record) with an outcome. | `outcome`, `unchecked_actions` | exactly one `outcome` → the latest `outcome` record, if any exists. Optional `carried_obligations`: the cancel-by dates still open at the close, each `{obligation: digest ref, cancel_by}`. |
| `disclosure` | Something the agent told someone about the user: what kind of thing, to whom, when, under what authority. | `to` (`counterparty`\|`other`), `fields[]` (each `class` + `value_commitment`), `authority` (`approval`\|`none`) | `authority: approval` ⇒ exactly one `authorized_by` → an `approval`, under the same rules as an `action` (section 6); `none` ⇒ no `authorized_by`, and `rule` says why. All `fields` share one covering action: contact classes `share_contact`, credential classes `share_credentials`. Optional `action_class` + `taxonomy_version` (see Action classes, below); optional `channel`; `counterparty` when the recipient is someone new. |
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
  or sign), `marketplace.offer` and `marketplace.sale` (no deal record carries either: an offer
  is `external_commitment.other`). A reader that keys a
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
| `offer` | `external_commitment.other` | `external_commitment.other` | `external_commitment.other` | `external_commitment.other` |

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
  `key_source` (`dns` | `supplied`), `dkim` (`pass` | `fail` | `none`), `merchant_signed` (a
  passing signature is DMARC-aligned with the From domain: the same organizational domain, or
  the exact domain under `adkim=s`), `dmarc_policy` (`reject` | `quarantine` | `none` |
  `absent` | `not_captured`; shown, never a reason to refuse sealing), optional
  `dmarc_record_digest` + `dmarc_source` (the DMARC records captured at seal time), optional
  `signer_matches_baseline`, and `parsed` (`method: heuristic`; optional `total_minor`,
  `currency`, `order_id_commitment`, `cancel_by`, `sent_at`, `item_count`, `kind`:
  `confirmation` | `cancellation`). `verified` is true
  only when `dkim` is `pass` and `merchant_signed` is true. The DKIM signature is the
  merchant's attestation; the record's own seal is the producer's. They are different claims.
- **recourse**: `rail` (a token: `card`, `zelle`, `wire`, …; normalized as in **Normalization**
  below) and `refundable`.
- **differences[]**: `{question, rule, field?}`. `question` is one of `asked`, `who`, `terms`,
  `recourse`, `safety`, `delivered`. `rule` is the id of the rule that found the difference.
  `capsulectl`'s deal check writes its own built-in rules, among them `not_asked`, `over_limit`,
  `under_floor` (a price below the user's floor, rule 6), `agent_picked`, `terms_changed`, `recourse_changed`, `credentials_requested`, `not_delivered`
  and `delivered_differs`. Any change of
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
  `other_contact`, `credential`, `verification_code`, `payment_card`, `id_document`) and a
  `value_commitment` to the value given. The value itself stays in the local store; a ledger of
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
  page can label it: `condition`, `warranty`, `refund_terms`, `delivery_promise` or `other`. It
  is optional and in the clear; a claim without one re-derives unchanged.
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
   number of `unchecked_action` outcomes. A close with `completed`, `mismatch` or `not_selected`
   is terminal. `not_selected` (status and outcome alike, with no differences) says the other side
   was not chosen, one buyer of several for example, and nothing was done: no `action` precedes
   it. A terminal close:
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
   `pickup_location`), each need: an offer on record, an acceptance of the latest one, and no
   `detail_change`, and no `message` or `evidence` carrying `counterparty` identifiers or
   `counterparty_facts`, after that acceptance. A change leaves the acceptance behind: the
   offer is made and accepted again. Such a commit or share action cites the acceptance it rests
   on with exactly one `source` ref; no other action carries one. The acceptance is not
   authority: `authority_basis` is unchanged.
10. **A sale to one of several buyers.** A sale's own log is a `sale` record and then its one
    `task-authority/v0` (`source` → the sale record, the floor's `bounds_commitment` when the
    sale states one, no `sale_authority_commitment`), and nothing else. Each buyer's negotiation
    is a deal of its own, a thread, whose task authority carries `sale_authority_commitment`: the
    digest of the sale's task authority, committed under that step's own nonce. A thread's
    request and limits are the sale's and do not change per buyer (no `intent` steps). Each
    `check` of a thread carries `item_ref_commitment`: the sale's item reference committed under
    that check's own nonce. So no value in a thread's records is equal across the sale's threads,
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

## 7. Outcome conventions: `completed | mismatch | open`

| outcome | Meaning | Set when |
|---|---|---|
| `completed` | What was delivered matches what was agreed (the baseline, as updated by approved checks). | `status: "received"` and no differences. |
| `mismatch` | What happened differs from what was agreed. The differences say how. | `status: "received"` with differences; `not_received`; `unchecked_action`. `reverted` is either, depending on its differences. |
| `open` | No final result was observed when the record was sealed. | `status: "pending"`, or a close with no outcome record. |

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
| `recipient_role` | A share's recipient, by role: `fulfilling_merchant` (`disclosing_to: counterparty`, on a deal where the user buys or states no `party_role`), `buyer` (`disclosing_to: counterparty`, where the user sells) or `third_party` (`disclosing_to: other`). `self` is in the set and never derived. Absent on other actions. |
| `channel`, `first_contact_channel` | Channel kinds (the `channel` set above): the one in use at this check (the latest sealed message's, else first contact's) and the baseline's. |
| `upfront_amount_minor` | The deposit the check's own terms state (`terms.deposit_minor`), in minor units. Never inferred. |
| `material_fields_changed`, `material_fields_basis` | How many of `terms.item`, `terms.quantity`, `terms.price_minor`, `terms.deposit_minor`, `terms.currency`, `terms.when`, `terms.place`, `terms.conditions`, `recourse.rail`, `recourse.refundable`, `who.payee` the proposal (the deal as it stands with the check on top) changes from what was agreed (payee: the first contact's payee, else its name), and the hex SHA-256 of that list's JCS bytes. |
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
`definition_digest` (the ruleset the checker must report) are optional. The executable must sit
under a trusted plugin root, and the profile pins its SHA-256.

At every check, after the `check` record is sealed:
- **What the checker gets.** The command runs exactly as pinned, from a private copy of the
  executable bytes that were hashed, with only `PATH` and `HOME` in its environment. On stdin it
  gets one `external-check-input/v0` object (`external-check-input-v0.schema.json`, beside this
  file):
  - `record`: that `check` capsule, with its disclosed `agent_input` (the deal record, whose
    body is what is about to happen), and `counterparty_profile` when the check names a payee:
    the value its `counterparty_profile` record seals, `{fp_alg: "hmac-sha256-profile-key",
    ids: {payee: <64 lowercase hex>}}`, supplied beside the capsule and never part of it;
  - `history`: the profile's earlier sealed acts with an amount, from every deal on this
    profile's own store, sealed in the last 31 days, newest first, at most 1,000 of them, in the
    same shape. Each act carries the `counterparty_profile` of the check it rests on (act →
    approval → verdict → check → that check's `counterparty_profile` record; in typed records a
    verdict that needed no approval authorizes the act itself: act → verdict → check → record),
    and none when it has none (an act done without a check, or one checked before checks had a
    companion). A rule keyed on who was paid before reads one merchant as one payee across the
    profile's deals from there on; acts checked before keep only their per-deal fingerprints;
  - `history_scope`: `{days, max_records, complete}`.
  - `party_role`: `buyer` or `seller`, the side of the deal the user is on, as the deal's opening
    intent sealed it (`buyer` when it names none). Sent on every check, always explicit.
  - `task_authority_record` (typed deals): the whole sealed task-authority record the check's
    `task_authority_ref` names, exactly as sealed: SHA-256 over its JCS bytes is the ref's digest,
    and its plan (`outcome_id`, `allowed_actions`, `preconditions`) is at `body`. Absent when the limits in force were confirmed
    later in an approval, and in a deal with no task-authority record.
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
  verdict: pass|fail|not_applicable|not_evaluable, reason, limit, value}`, passes included;
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
  - A checker that is configured but changed since it was pinned, refused the input, timed
    out, printed anything else, or reported another `definition_digest` than the pinned one
    adds a `rules_not_checked` difference naming why, and the check pauses.

The verdict's `rules` seals the outcome as data:
- `status: evaluated`, with `ruleset_id`, `definition_digest`, `checker_sha256`, `verdict`,
  `tier` (as reported, or `not_stated`), `grade` (as reported, or absent; both recorded and
  shown in the approval text, changing nothing), `findings` (each `{id, check, verdict, limit, value}`), and `history` (`{days, acts, complete}`:
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
| `task-authority/v0` | The user's task authority: their words by commitment, `max_total_minor`, `allowed` (and, from this version, the plan shape a rules checker reads: `outcome_id`, a fixed id per deal type such as `capsulectl.deal.purchase/1.0.0`, the same actions as `allowed_actions`, and `preconditions: []`). Sealed after the baseline (`source` ref), and again when the user confirms new limits (`approves` the intent, `previous_ref`, `said_commitment`). | the baseline intent; `approval` with `confirm_limits` |
| `proposed-action/v0` | The action about to be taken, exactly as checked. | `check` |
| `action-evaluation/v0` | The deal check's disposition (`DO`, `ASK` or `DENY`) and findings, with the contract fields below. | `verdict` |
| `action-approval/v0` | An approval artifact of one stated `authority`: `user_approval` (the user's own answer, with their words and the `rendering_commitment` of what they were shown), `card_answer` (a card answered with no words), `platform_approval` (a platform approval observation, below), `policy_change` (the user confirming a policy change: `rendering_commitment`, `effective_policy_digest`, `semantic_diff_digest`), `one_shot_override` (reserved). | `approval` |
| `action-record/v0` | What the agent did, with `evaluation_ref` and `authority_basis`; a disclosure that needed approval is an `action-record/v0` with `disclosed`. | `action`; `disclosure` with authority `approval` |
| `action-outcome/v0` | What was observed (`attempted` names an unchecked action). | `outcome` |
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
