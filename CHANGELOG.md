# Changelog

## Unreleased

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

### `verify --capsule` reads a bare capsule

- `verify --capsule` also reads a bare Agent Action Capsule as capsule-emit seals it (the
  capsule's own fields with an inline `signature` and `key_id`), with the same checks as the
  artifact.Record wrapper. The file's shape is read from its members, never guessed, and named
  in the output (`shape`); a file with the members of both shapes, or of neither, is refused.

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
