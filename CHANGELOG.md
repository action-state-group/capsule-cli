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

### Changed

- `close --capsule-out`: a file already holding exactly the Close's bytes is
  now left alone, so a repeat with the same flags succeeds as a no-op; a file
  holding anything else is refused by name and nothing is overwritten.
  Before, any existing file at that path failed the command.

### Removed

- The `--html` flag on `bundle`, `disclose` and `permalink`. It was a stub
  that never rendered a page; `report build` replaces it.
