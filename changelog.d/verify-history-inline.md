### `verify --history` checks any History instance inline (capsule-cli#523)

- **Added:** `verify --history` chooses the instance by the file's own `history` (or `profile`) id. It
  serves a `standard_history/1` answer `{history, request, reply}`, reports `anchor_witnessed` from
  `--receipt` files under the `--anchor-witness-policy` you hold (no witness list is built in), and
  names an instance it cannot read `not_evaluable`. With no receipt `anchor_witnessed` is `NOT_PRESENT`,
  never `PASS`.
- **Added:** a stapled `history_card/1`, in an `evidence-commitment/1` or a `{history, request, reply}`
  file, is read as that derivation, not as a History instance: the envelope, the card's form, its
  checkpoints (each signed, the last the anchor, and each extending the one before by its consistency proof) and `anchor_witnessed`, with no History property and no count. The sender is checked
  against `--counterparty-key`, which you hold and never read from the staple, and the log key against
  `--checkpoint-key`; a staple under another key reports `FAIL`. A commitment staples the reply octets
  only, so its `request_binding` is `NOT_PRESENT` and it is not `ACCEPTED`.
