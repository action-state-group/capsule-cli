- **Added:** the `--rules-checker` pin file takes an optional `"kind"` (`admission`, `live`, `after_the_fact`,
  `deal` or `conduct`), the pack's `scope.kind`, stored on the profile. Only a `deal` pin is shared with a counterparty
  (`ProfileRulesChecker.SharesWithCounterparty`); a pin that states no kind, or any other, is private. A pin file replaces
  the whole pin, so re-pinning without a kind clears it. For evidencebook-deal#138.
