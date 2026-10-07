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
| `fixtures/positive/` | One deal, 17 records, covering all 13 record types, with the expected JCS digest of each. |
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
    `agent_card` is not certified by anyone and stays `fyi`. Its `effect` is `dispatched` and `runtime_claimed` (the agent's
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

A **digest ref** is `{"type": "deal-record", "digest_alg": "SHA-256", "digest": <64 lowercase
hex>}`: the typed digest reference shape AAC `references[]` uses. The digest alone is the
identity. A **rel ref** is a digest ref plus `rel` (one of `about`, `source`, `checks`,
`approves`, `authorized_by`, `observes`, `outcome`, `confirms`).

`counterparty.ids` on a record states the identifiers observed or asserted *at that record*.
On the baseline they are first contact. Every later check compares against first contact,
never against a later change.

## 3. Record types (action-type conventions)

Thirteen record types. The set is closed: an unknown `record_type` fails the schema.

| record_type | What it records | Required body fields | Required refs / block fields |
|---|---|---|---|
| `baseline` | First contact: what the user asked, who the counterparty is, the terms and the way back. The deal's contract. | `deal_type` (`purchase`\|`rental`\|`booking`\|`service`), `intent` (as the intent body), `terms`, `recourse.rail` + `recourse.refundable` | `seq` = 1; `channel`; `counterparty` (≥ 1 id). No `prev`, no `baseline_ref`. Optional `claims[]`, `counterparty_facts`, `demo`, `skill` (`{"skill_md_digest": <64 hex>, "other_copies": <int ≥ 0>}`: the SHA-256 of the SKILL.md the agent reported following, and how many other copies of that skill sat beside it; a boundary marker for accidents, not proof the instructions were followed). |
| `intent` | The user restates or picks within the ask. Replaces `verbatim` / `asked` from here on. `allowed` and `max_total_minor` carry forward unchanged: an intent may lower the limit or drop actions, and a higher limit or a new action is only a proposal (section 6, rule 6). | `verbatim_commitment` | `baseline_ref`, `prev`. |
| `message` | One message in the thread. The text stays local. | `from` (`counterparty`\|`user`\|`agent`), `content_commitment` | `channel`. `counterparty` when the message shows identifiers (for example, a new phone number). |
| `claim` | Something the counterparty (or listing) asserts, recorded as a claim, not a fact. | `text` (≤ 200 chars, no identifiers), `source` | none beyond the chain. |
| `evidence` | What was done to establish a claim, and whether it did. Optionally a merchant's own email (`merchant_email`, below). After the deal's final close, the only record type allowed: later evidence linked to that close. | `source`, `verified` | exactly one `about` → a `claim` or the `baseline`. At most one `confirms` → the deal's final `close` (required after it, not allowed before it). Optional `resolves_obligation` (a digest ref) → an earlier record holding a cancel-by date. |
| `detail_change` | The counterparty changed an identifier, a term or the rail after first contact. Recording it never accepts it. | `source`, `changed[]` (field names) | `counterparty.ids` kinds MUST equal the identifier kinds listed in `changed`. At most one `source` → a `message` or `evidence`. |
| `check` | The agent asks, before a point of no return, exactly what is about to happen (the snapshot). | `action` (`pay`\|`sign`\|`commit`\|`cancel`\|`share_contact`\|`share_credentials`) | `baseline_ref` (always). `counterparty.ids.payee` when a payee is involved. Optional `amount_minor`, `currency`, `seen_item`, `terms`, `recourse`, `pack_id`, `pack_digest`, and for a share `disclosing` (the classes about to be given, as for a `disclosure`) and `disclosing_to` (`counterparty`\|`other`), which every share check carries. |
| `verdict` | The answer to one check: pass, or pause with the differences. | `result` (`pass`\|`pause`), `differences[]`, `options[]`; `materiality` (the predicate that decided which of the agent's own picks pause: `digest`, and `name` and `version`; `digest: "none"` when none was configured and every pick paused) | exactly one `checks` → a `check`; one verdict per check; optional `pack_id`, equal to the check's when either names one. `pause` ⇒ ≥ 1 difference and ≥ 1 option. `pass` ⇒ no options. |
| `approval` | What authorizes, or declines, the next step; or the user's confirmation of the limits an intent proposed. | `choice` (`hold`\|`verify_contact`\|`proceed`\|`confirm_limits`), `proceed` (`true` on `proceed`, `false` on `hold` and `verify_contact`), `approver` (`user`\|`standing_intent`\|`agent_card`); on `confirm_limits` only, `limits` (`previous` and `new`, each `max_total_minor` and `allowed`) | `confirm_limits`: exactly one `approves` → the proposing `intent`, `approver: user`, and section 6, rule 6. Otherwise exactly one `approves` → a `verdict`. `user` ⇒ `said_commitment` (the user's own words), and on a pause the choice is one of the verdict's options. `agent_card` ⇒ no `said_commitment`: a card the agent composed was answered, and no words of the user's are on record; never on `confirm_limits`. Optional `card_commitment` (on an answer to a verdict, never on `confirm_limits` or `standing_intent`): the card text the answer was given on, committed under the verdict's own card nonce, so it MUST equal the verdict's `card_commitment`: equal means the card shown is the card checked, recomputable without the text. `standing_intent` ⇒ the verdict passed, the choice is `proceed`, and the checked action is in the current `allowed` (`allowed` absent = no restriction; `allowed` present and empty = nothing is allowed yet, as in "show me options, don't book"). |
| `action` | A point-of-no-return step actually taken. | `action`; optional `direction` (`out`: paid by the user; `in`: back to the user) | exactly one `authorized_by` → an `approval` with `proceed: true` (section 6). An action that returns money (`direction: in`) carries exactly one `reverses` → the `pay` action it undoes (section 6, rule 8). |
| `outcome` | What was observed afterwards: delivered or not, or an action taken without approval. | `status`, `outcome`, `differences[]` | At most one `observes` → an `action`. |
| `close` | The deal ends (or pauses its record) with an outcome. | `outcome`, `unchecked_actions` | exactly one `outcome` → the latest `outcome` record, if any exists. Optional `carried_obligations`: the cancel-by dates still open at the close, each `{obligation: digest ref, cancel_by}`. |
| `disclosure` | Something the agent told someone about the user: what kind of thing, to whom, when, under what authority. | `to` (`counterparty`\|`other`), `fields[]` (each `class` + `value_commitment`), `authority` (`approval`\|`none`) | `authority: approval` ⇒ exactly one `authorized_by` → an `approval`, under the same rules as an `action` (section 6); `none` ⇒ no `authorized_by`, and `rule` says why. All `fields` share one covering action: contact classes `share_contact`, credential classes `share_credentials`. Optional `channel`; `counterparty` when the recipient is someone new. |

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
- **obligation** (on `evidence`, optional): a commitment that takes effect when a date passes.
  `kind` (`trial_conversion` | `renewal` | `cancel_window` | `payment_due`), `cancel_by` (the
  last day to cancel), optional `takes_effect`, `amount_minor` + `currency`, `period` (`week` |
  `month` | `year` | `once`) and `terms_commitment` (to the merchant's own wording, kept on the
  device). The evidence record's `source` says where it came from (`merchant_email`,
  `page_snapshot`). Recorded, never enforced. Each obligation records one cancel-by date. A recurring renewal after that date is not tracked; seal a new obligation for each later date.
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
  `agent_picked`, `terms_changed`, `recourse_changed`, `credentials_requested`, `not_delivered`
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
`said_commitment`, `note_commitment`.

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
   number of `unchecked_action` outcomes. A close with `completed` or `mismatch` is terminal:
   after it, only `evidence` records that `confirms` that close may follow. A close with `open`
   MAY be followed by later `outcome` and `close` records. Each `carried_obligations` entry names
   an earlier record holding that `cancel_by`.
8. **Reversals.** An `action` with `direction: "in"` returns money the user paid: it carries
   exactly one `reverses` ref to an earlier `action` with `action: "pay"` (direction `out`, or
   none, as records sealed before `direction` was recorded carry), with the same `amount_minor`
   and `currency`. A payment is reversed at most once. Summing amounts by direction gives what a
   deal moved: a pay and its reversal net to zero. Records sealed before this rule carry no
   `direction`; a `pay` among them moved money out, and no other action states a direction.

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
