- **Fixed:** a witness's continuity refusal (HTTP 409) is no longer final on its first answer
  (capsule-cli#647). A later delivery call, a minute or more after the refusal, sends the refused
  checkpoint once more, and later checkpoints wait behind it instead of each being sent into a
  refusal of its own. A checkpoint refused again after that retry stays failed, as before. Until
  the retry its status reads "pending: the witness refused it as not continuing the checkpoint it
  last accepted (HTTP 409); it is sent once more at a later capsulectl run, a minute or more after
  the refusal.", it shows `state: pending`, and `cll checkpoint publish` ends pending rather than
  "permanently failed". The delivery record keeps the witness's own last-accepted size and root
  from the 409 body (nothing else from it). A store that cannot list its rows now re-arms the
  refused checkpoints its selection chains on, not only the selection. A 403 an older build
  stored as failed also shows `state: pending`, as it is re-armed at the next call.
