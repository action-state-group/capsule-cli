### `result build` accepts the `calibration` claim type (capsule-cli#531)

- **Added:** the vendored Evidence Result v0 schema carries aac #552's `calibration` claim type
  (agent-action-capsule at 09c8b86), so a Result whose claims cite weekly human-rating records
  passes `result build`. A calibration claim carries no `close` or `reconcile` body. That its
  evidence cites exactly one `calibration-summary/v1` record and otherwise only `human-rating/v1`
  records is aac's checker rule, not enforced by this schema.
