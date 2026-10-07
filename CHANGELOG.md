# Changelog

## Unreleased

### Deal checks: which agent picks pause is a pinned materiality predicate

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
- Every check records which predicate decided (name, version and digest, or digest
  `none`) in its output and its sealed verdict (`materiality`, an additive x-deal-v0
  field).
- `deal open` seals the predicate pinned when the deal opened into the baseline
  (`materiality`, additive), from the profile and never from the input. A later check
  under another predicate (a re-pin mid-deal) carries a `materiality_changed`
  difference: it shows on the card and pauses the check.
- A pinned predicate whose file is gone refuses with its own message ("materiality
  predicate file missing; re-pin it with profile update").
- Shared copies keep the opening and check records again: since the verdict began
  recording `materiality`, a counterparty's copy withheld every check step.
