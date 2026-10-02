# Changelog

User-visible changes to `capsulectl`. The newest release is at the top.

## Unreleased

### Added

- `result build` seals a caller-supplied Evidence Result v0 into a jsonl
  profile's evidence book as one `evidence_result` record, after checking it
  against the vendored schema, recomputing its headline values, resolving
  every citation in the book, and walking each close claim's Close. Only
  UNILATERAL close claims can be sealed end to end today; see
  `capsulectl result build --help`.
- `report build` verifies a held, disclosed bundle rooted at a sealed Result
  v0 and renders an offline `report.html` (and, with `--permalink`, a viewer
  fragment over the same bytes).

- `disclose` / `permalink --attach-input-originals` (jsonl profiles): carry
  each published capsule's retained agent_input original in the bundle, in
  the `capsulectl/agent-input-originals/v1` extension, checked against the
  capsule's `agent_input_digest`. Opt-in at disclose time; the book never
  stores an original.

### Changed

- `close --capsule-out`: a file already holding exactly the Close's bytes is
  now left alone, so a repeat with the same flags succeeds as a no-op; a file
  holding anything else is refused by name and nothing is overwritten.
  Before, any existing file at that path failed the command.
- `disclose --suppress agent_input` also withholds an agent_input original a
  book record commits as a payload (records written by a pre-release build
  that stored originals in the book); under `--payloads all` it refuses
  instead, since that mode cannot withhold a payload.

### Removed

- The `--html` flag on `bundle`, `disclose` and `permalink`. It was a stub
  that never rendered a page; `report build` replaces it.
