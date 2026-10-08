# Changelog

## Unreleased

### A check carries the inputs a rules checker reads

- **Added (wire, additive):** a check sealed from this version carries scalars a rules checker
  reads. They go on the `x-deal-v0` check and the typed `proposed-action/v0`, and each is absent
  when not known:
  - `recipient_role` (`fulfilling_merchant` or `third_party`, on a share);
  - `channel` and `first_contact_channel` (channel kinds);
  - `upfront_amount_minor` (the stated deposit, never inferred);
  - `material_fields_changed` and `offer_fields_changed` (counts over two fixed field lists, against
    what was agreed and against the user's own words), with `material_fields_basis` and
    `offer_fields_basis` (each list's digest);
  - on typed records, `task_authority_ref`.

  None carries a name, a contact detail or free text. Records sealed before re-derive unchanged.
- **Added:** a typed deal's `task-authority/v0` also carries its allowed actions as `allowed_actions`
  with `preconditions: []`. The external-check input gains an optional `task_authority` member: that
  record, whose digest is the check's `task_authority_ref`.
- **The deal skill** now says to always state `recourse` (the rail and whether it is refundable) on
  a pay, and to put a deposit the checkout asks for in `terms`.

### A rules checker's answer that cannot be sealed pauses the check instead of failing it

- **Fixed:** on the second purchase in a week, a checker's finding value such as "2000 authorised
  (capture 2000); 7d total 4000 (2000 earlier)" reads as a phone number to the personal-data scan.
  `deal check` refused to seal its verdict and exited 2, with no verdict and no prompt.
- **Now:** a checker answer that cannot be sealed never fails the check. The check pauses with
  "Your rules were not checked: the rules checker's answer could not be sealed (…)", and the verdict
  seals the rules as not evaluated, with cause `unreadable`. The personal-data scan itself is
  unchanged.
- **Numeric limits and values:** a checker that reports `limit` and `value` as numbers (as the
  contract allows) is never caught this way.

### A deal receipt's words for where the deal stands and the merchant's receipt

Consumer wording only, on the receipt page and in the emailed receipt. No record type, field or verb
changes. "receipt" is wording here, never a field, record type or key: a test keeps every deal and
external-check schema free of one.
- **An open deal** with no merchant email sealed yet reads "Open: waiting for the merchant's
  receipt." One with the merchant's email sealed reads "Open: the merchant's email is sealed; no
  close is sealed on this deal yet." Both are followed by the expected close date, as before.
- **A completed close** reads "Closed at …: matched, nothing left to match." Other outcomes are
  unchanged.
- **A merchant email sealed after the close** reads "The merchant's receipt arrived. It matches what
  you approved." or "The merchant's receipt arrived. It differs: you approved $48.00; their receipt
  says $64.00." This applies only when an approved amount is on record (not only a limit). A copy
  not confirmed by the merchant's signature says so. Shared copies keep their fixed words.
- **The user's choice on a check** reads as the option was shown: "you chose Pay anyway" over a
  finding, "you chose Hold". It used to read the option's id ("proceed", "hold").

### `deal check` runs the user's pinned rules checker and seals what it said

- **Added:** a deal profile may pin an external rules checker (`profile update --rules-checker
  FILE`): a command under a trusted plugin root, pinned by its executable's SHA-256 and,
  optionally, by the definition digest of the ruleset it must report. It is the user's policy,
  never the agent's.
  - **What it gets.** Every `deal check` runs it, from a private copy of the exact bytes it
    hashed, with one `external-check-input/v0` object on stdin
    (`skills/deal/profile/external-check-input-v0.schema.json`):
    - the capsule of the step being checked, with its disclosed record;
    - as history, the profile's sealed acts with an amount from every deal in the last 31
      days (at most 1,000), stating whether that is all of them.

    A rolling limit is evaluated over that history. A checker given too little reports it
    `not_evaluable`, and the check pauses.
  - **What it prints.** One `external-check-result/v0` object: the ruleset's id and definition
    digest, a verdict (`allow`, `deny`, `escalate` or `not_evaluable`), and its findings, each
    limit and value a number or a short line, and optionally how it reached the verdict (`tier`:
    `recomputed` or `judged`, as in Result v0; absent reads as judged, never recomputed) and
    the grade of its evidence (`grade`: `self-attested`, `witnessed` or `countersigned`, by
    reference to Result v0's Grade). The tier and grade are
    sealed as reported and named in the approval text; they change nothing. The schema ships at
    `skills/deal/profile/external-check-result-v0.schema.json`, and output outside it is
    refused.
- **One verdict, one prompt.** The checker's answer folds into the check's own differences.
  - **`deny`** is a new verdict: the card names the rule, its limit and the value, and offers
    only "Hold". An approval to proceed is refused.
  - **`escalate` and `not_evaluable`** pause.
  - **A checker that cannot answer pauses**, saying "Your rules were not checked:" and why. That
    covers a checker that changed since it was pinned, refused the record, timed out (10s by
    default, at most 60s), printed anything else, or reported another ruleset digest than the
    pinned one.
  - **No checker configured** does not pause, but the text says "Your rules were not checked:
    no rules checker configured."
- **Sealed (wire, additive).**
  - The `x-deal-v0` verdict gains `result: "deny"` and `rules`: the status, the ruleset id and
    definition digest, the checker's SHA-256, the verdict, every finding (id, verdict, limit,
    value), and `history` (how many earlier acts the checker was given, over how many days, and
    whether that is all of them). A checker that was not evaluated seals a
    `cause` token. No words the checker wrote are sealed in the clear: they reach the card,
    which is committed to.
  - Typed records: `action-evaluation/v0` gains disposition `DENY` and `rules_checks`, and
    `check-response/v0` gains `DENY`.
  - Shared copies carry none of the user's rules: the verdict record is withheld, and its
    findings read in fixed words.
- **Changed:** `approval_text` leads with the rules line and then "Before you go ahead:" in
  place of "Deal check:". The "Checked at … Stale after …" line has left the text; staleness
  stays in `checked_at`, `stale_after_minutes` and the sealed record.
- **Changed:** a card whose only finding is that the rules could not be checked offers "Pay" (or
  "Confirm", and so on), not "Pay anyway": there is nothing to override.

### A deal receipt's text is sealed with it

- **Fixed:** a deal receipt's readable text (summary lines, step lines, amounts, what was told)
  is now sealed. Writing a receipt (`deal report --html/--bundle/--email`, `bundle --deal`,
  `disclose --deal`) seals that copy's own text as a `deal_report` record on the deal's log,
  salted with a fresh nonce. The bundle's `x-deal-v0` extension only names it.
  - **What fails:** editing a line, removing the extension, or naming another record makes
    `verify --bundle` INVALID, and the page says the report did not verify.
  - **What the page says:** for a sealed copy, that it checked the text has not changed since
    it was sealed.
- **Changed (wire):**
  - The deal's log now holds a sealed report after its steps for each receipt written. A
    report that writes no file seals nothing.
  - Later copies carry every earlier report's record with its text withheld: a copy
    discloses its own text and no other copy's, and a shared copy seals the text already
    rewritten for its audience.
  - Witness coverage is counted in steps. A tick that holds the deal's steps, with only
    reports after them, still shows as witnessed, or as pending for its reason.
- Receipts written before this change verify as before, with their text reported
  `extension_unbound`.

### A deal receipt says its text is not verified; records declare their commitments' construction

- **Changed:** a deal receipt's readable text (summary lines, step lines, amounts, what was
  told) rides in the bundle's `x-deal-v0` extension, which no record seals: editing it left
  `verify --bundle` VALID and the page showing the edited text. `verify --bundle` now reports
  that extension with the finding `extension_unbound` and a note (the verdict is unchanged),
  and the receipt page says first that it does not check that text. Binding the text to a
  sealed record is planned.
- **Wire change (schema-visible, additive):** every deal record sealed from now on carries
  `commit_alg: "sha256-jcs-nonce256"` (in the `x-deal-v0` block; in a typed record's header),
  naming the construction of its `*_commitment` values: SHA-256 over the JCS of
  `{"nonce", "text"}`, a fresh 256-bit nonce per commitment, no key, recomputable only from the
  opening. Records sealed before carry none and re-derive unchanged. PROFILE.md section 4
  states the construction, its nonce and key scope, and where openings travel.

### A page is written only when it can stand behind what it shows

- **Behaviour change.** `bundle --html`, `disclose --html` and `deal report --html`
  write a page only for a bundle that `verify --bundle` calls VALID. For an INCOMPLETE
  or INVALID bundle no page is written: the command exits as `verify --bundle` would
  (3 or 1) and names the claims that did not pass. The page's viewer would otherwise
  have said "Bundle verification passed" for a bundle the CLI calls INCOMPLETE.
  `bundle` still writes the bundle (`--out`, or stdout), since `verify --bundle`
  states its verdict; `disclose` writes nothing and puts no disclosure on record when
  its page is refused.
- A page whose `report/v1` root would show fewer rows than it has (a row cites a
  record the bundle does not carry) is refused, naming how many rows it would show.
- A jsonl profile writes no page: its bundle carries its evidence book's records about
  what was sealed, not the records themselves, so a page cannot show them. Publish on
  a sqlite profile for a page; `bundle --out` still writes the bundle.
- Every page path measured on the previous release was VALID and is unchanged: deal
  receipts (own and shared copies) and the README's self-checking report.

### `profile retire NAME`: free a profile's name without touching its records

- Renames the profile to `NAME-retired-YYYYMMDD` (`-2`, `-3` … the same day), with
  its log id, keys and data directory unchanged: every record, checkpoint and witness
  receipt stays in its own log, and the retired profile still bundles and verifies
  them. Nothing is deleted, moved or re-logged. The freed name can be created anew,
  for example as a sqlite profile.
- The retired profile is not made read-only: a jsonl profile's bundle puts itself on
  record, which a read-only profile refuses.
- `--move-data NEW_DIR` also renames a jsonl profile's data directory (to a new path
  on the same filesystem), for an install that wants the old path. It refuses a sqlite
  profile, an existing path, and a key file inside the data directory.
- Anything that runs capsulectl with `--profile NAME` (a scheduled checkpoint cadence,
  a plugin) is not changed; the command says so.

### `verify --bundle` verifies composed/v1

- **Changed:** a bundle carrying a `composed/v1` extension (Evidence Bundle -01 §7.2) is now
  verified instead of being listed as uninterpreted: the composed digest, each member (with a full
  per-member assessment for carried member bundles), composition closure, joins and the
  redundancy flag. It counts toward the verdict: a failed block or an INVALID member bundle makes
  the bundle INVALID; a declared-missing member, a join that can't be derived or an unverified
  refusal signature makes it INCOMPLETE. Uses agent-action-capsule go/v0.7.0. Other extensions are
  still listed as carried but not checked.

### A spend limit binds the most a payment may take

- **Fixed: an authorization buffer evaded every limit.** A pay check carried only the expected
  charge, so a $4.54 order passed a $5 limit while the card approval let the merchant take up to
  $9.54.
- **New check field `authorized_max_minor`:** on a `pay`, the most the counterparty may take
  under the payment's authorization (a card hold, a pre-authorization with a buffer for tax
  settled later). It must be at least `amount_minor`, which stays the expected charge. The intent
  limit evaluates it, and the difference names it ("up to $9.54 may be taken (the authorized
  maximum; the expected charge is $4.54)", field `authorized_max_minor`).
- **Behaviour change, fail safe:** a pay by card, wallet, PayPal, or any rail not known to be
  holdless is refused (exit 2) unless it states `authorized_max_minor` (the same as
  `amount_minor` when no larger maximum is shown). A pay on a holdless rail (bank transfer,
  Zelle, wire, cash and the like) defaults it to the amount.
- **Sealed:** the check carries `authorized_max_minor`, and `spend_authorized_minor` beside
  `spend_minor` (which a per-action cap reads; a rolling cap sums the actions' `spend_minor`). A
  typed `proposed-action/v0` carries it, so an approval never extends to a larger maximum.
  Optional members, kept per step: records already sealed re-derive unchanged.
- **The capture:** an action that takes more than the authorized maximum differs from its
  check; one below it, though not the estimate, is what a hold is for.
- The deal skill tells the agent to read the maximum from the approval or card hold and pass it.

### A second report on a profile works after a checkpoint covers a disclosure

- **Fixed:** once a checkpoint covered a disclosure, every later `bundle` and `disclose` on that
  profile failed ("operation failed … sensitive details suppressed"). A re-run of an install, or
  any later report, hit it.
- **Wire change: what `disclose` puts on the log.** It used to append the bare digest of a
  disclosure record, kept nowhere else, which no later bundle could supply. It now seals the
  disclosure record as an ordinary `fyi` capsule (`action_id: capsulectl-disclosure`) with the
  profile's signing key, stores it, and appends its capsule id. Its input holds only ids, digests
  and labels, never disclosed payload bytes, plus a random 256-bit nonce, so the digest a later
  bundle shows cannot confirm a guess at what was disclosed.
- **A later bundle** proves an earlier disclosure capsule's place in the log but withholds its
  input, so a copy made for one party never shows what was disclosed to another. Such a bundle
  states `payloads_mode: selected`.
- **Logs written by an earlier release** that disclosed and then checkpointed cannot be bundled
  over: the record behind a bare digest is gone. The error now says what it can know, exit 2:
  "log entry N is checkpointed but not in this profile's store: either a disclosure an earlier
  release appended as a bare digest, or a lost record; no bundle over this log can include it;
  start a new profile (or log id) for new reports".

### A merchant email whose key record DNS could not reach: offer a supplied record

- **Fixed:** when DNS could not be reached, sealing a merchant email (`deal note --kind evidence
  --email`) stopped with an error that only said to retry or pass `--key-record`, and the deal
  skill never mentioned `--key-record`, so an agent stopped there.
- **The error now names the record it needed and the way on:** "could not reach DNS for the DKIM
  key record SELECTOR._domainkey.DOMAIN (…): retry, or get that TXT record another way (another
  resolver, or a lookup the user runs) and pass it with --key-record FILE; it is sealed as
  supplied, which is weaker evidence than a record read from DNS". A DMARC record it could not
  read names `--dmarc-record FILE` beside `--key-record FILE` (it used to name `--key-record`
  alone).
- **The deal skill** tells the agent to offer that way on instead of stopping. It says:
  - which record is needed, and where it can come from;
  - that the email is sealed as supplied (`key_source: supplied`), and the report and the emailed
    receipt say so;
  - that a supplied record is weaker evidence than one read from DNS.

### `verify --capsule` reads a bare capsule

- `verify --capsule` also reads a bare Agent Action Capsule as capsule-emit seals it (the
  capsule's own fields with an inline `signature` and `key_id`), with the same checks as the
  artifact.Record wrapper. The file's shape is read from its members, never guessed, and named
  in the output (`shape`); a file with the members of both shapes, or of neither, is refused.

### `result build` and `report build`

- `result build` seals a caller-supplied Evidence Result v0 into a jsonl profile's evidence book
  as one `evidence_result` record, after checking it against the vendored schema, recomputing its
  headline values, resolving every citation in the book, and walking each close claim's Close.
  Only UNILATERAL close claims can be sealed end to end today; see `capsulectl result build
  --help`.
- `report build` verifies a held, disclosed bundle rooted at a sealed Result v0 and renders an
  offline `report.html` (and, with `--permalink`, a viewer fragment over the same bytes), through
  the same vendored evidence-graph viewer `deal report` embeds.
- `disclose` / `permalink --attach-input-originals` (jsonl profiles): carry each published
  capsule's retained agent_input original in the bundle, in the
  `capsulectl/agent-input-originals/v1` extension, checked against the capsule's
  `agent_input_digest`. Opt-in at disclose time; the book never stores an original.
- **Changed:** `close --capsule-out`: a file already holding exactly the Close's bytes is now left
  alone, so a repeat with the same flags succeeds as a no-op; a file holding anything else is
  refused by name and nothing is overwritten. Before, any existing file at that path failed the
  command.
- **Changed:** `disclose --suppress agent_input` also withholds an agent_input original a book
  record commits as a payload (records written by a pre-release build that stored originals in
  the book); under `--payloads all` it refuses instead, since that mode cannot withhold a payload.
- **Removed:** the `--html` flag on `bundle`, `disclose` and `permalink`. It was a stub that never
  rendered a page; `report build` replaces it.

## v0.1.0-rc9

### The checkpoint cadence: every 5m by default

- **Behaviour change for existing deal profiles.** A deal profile's witness cadence
  (its checkpoint cadence) now defaults to a tick every 5m, give or take 1m, instead
  of every 1h, give or take 10m. A profile that sets no cadence moves at upgrade; one
  that sets both `cadence.interval` and `cadence.jitter` keeps them.
- **A profile that sets `cadence.jitter` but not `cadence.interval` may now be
  refused.** Its jitter must be under half the 5m default, so under 2m30s. The error
  names both values; set `cadence.interval` (for example `1h`), or lower the jitter.
- The poll is `cll checkpoint cadence` (`deal tick` stays callable). Schedule it every
  5 minutes with `--wait-up-to 5m`. A run with no tick due publishes nothing.

### Deal checks: which agent picks pause is a pinned materiality predicate

The pinned predicate itself first shipped in v0.1.0-rc8, whose release notes did not describe
it. v0.1.0-rc9 added sealing it at a deal's opening, flagging a re-pin mid-deal, the
missing-file message, and the commitment to its name and version.

- **Behaviour change for existing deal profiles.** Which attributes the agent picked
  on its own (a size, a delivery option the user never named) pause a deal check is
  no longer compiled into capsulectl: it is a materiality predicate
  (`materiality-predicate/v0`, see `skills/deal/profile/materiality-predicate/`).
  **A profile with no predicate now pauses on every attribute the agent picked.**
  To keep the earlier behaviour, pin the example predicate, which reproduces it:
  `capsulectl --profile NAME profile update --materiality skills/deal/profile/materiality-predicate/neutral.json`.
  `capsulectl doctor --profile NAME` says which applies.
- The predicate is pinned in the profile by its digest (`materiality.digest`). A check
  refuses, sealing nothing, when the pinned file has changed; re-pin it with
  `profile update --materiality FILE`. Setting the predicate is the user's policy: a
  check takes no predicate of its own.
- A profile that names a predicate without a pinned digest (written by a build between
  the two changes) refuses checks until it is pinned the same way.
- Every check says which predicate decided (name, version and digest, or digest
  `none`) in its output, and seals it in its verdict (`materiality`, an additive
  x-deal-v0 field).
- `deal open` seals the predicate pinned when the deal opened into the baseline
  (`materiality`, additive), from the profile and never from the input. A later check
  under another predicate (a re-pin mid-deal) carries a `materiality_changed`
  difference: it shows on the card and pauses the check.
- A pinned predicate whose file is gone refuses with its own message ("materiality
  predicate file missing; re-pin it with profile update").
- Sealed records carry the predicate's digest and a commitment to its name and
  version (`label_commitment`); the user's own copy opens it (`materiality_openings`).
  A copy shared with a counterparty discloses the digest only: the name and version
  describe the user's own policy. This also fixes shared copies, which withheld every
  check step once the verdict began recording `materiality` in v0.1.0-rc8. Verdicts
  rc8 sealed with the name and version in the clear stay withheld from shared copies.
