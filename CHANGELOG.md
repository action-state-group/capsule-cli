# Changelog

## Unreleased

### `verify --bundle` verifies composed/v1

- **Changed:** a bundle carrying a `composed/v1` extension (Evidence Bundle -01 §7.2) is now
  verified instead of being listed as uninterpreted: the composed digest, each member (with a full
  per-member assessment for carried member bundles), composition closure, joins and the
  redundancy flag. It counts toward the verdict: a failed block or an INVALID member bundle makes
  the bundle INVALID; a declared-missing member, a join that can't be derived or an unverified
  refusal signature makes it INCOMPLETE. Uses agent-action-capsule go/v0.7.0. Other extensions are
  still listed as carried but not checked.

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
