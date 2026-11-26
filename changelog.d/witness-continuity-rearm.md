- **Fixed:** a witness's continuity refusal (HTTP 409) is no longer final on its first answer
  (capsule-cli#647). The next delivery call sends the refused checkpoint once more, and later
  checkpoints wait behind it instead of each being sent into a refusal of its own. A checkpoint
  refused again after that retry stays failed, as before. Its status reads "pending: the witness
  refused it as not continuing the checkpoint it last accepted (HTTP 409); it is sent once more at
  the next capsulectl run." until then. The delivery record keeps the witness's own last-accepted
  size and root from the 409 body (nothing else from it). A store that cannot list its rows now
  re-arms the refused checkpoints its selection chains on, not only the selection. A
  delivery due again at the next call (this one, or a 403 an older build stored as failed) shows
  `state: pending`, not `failed`.
