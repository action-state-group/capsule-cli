- **Fixed:** a checkpoint the witness refuses again after its retry, as not continuing the
  checkpoint it last accepted (HTTP 409), no longer stops every later checkpoint (capsule-cli#647).
  capsulectl re-anchors it once. It signs the same checkpoint again, with the witness's last
  accepted checkpoint (named in the 409) as its predecessor and a consistency proof from it, and
  sends that. Later checkpoints chain on it and get their own receipts. The refused delivery keeps
  its stored checkpoint and gets no receipt. Its status reads "not delivered: ... capsulectl
  re-anchored it ...; the receipt for the re-anchor is not kept.", `cll checkpoint publish` shows
  `state: re-anchored`, and the command ends without error. A re-anchor is signed only for the
  profile's own checkpoint, from a size and root of the log's own tree. capsulectl never re-proves
  to a witness whose last accepted checkpoint is not this log's (a fork). A re-anchor that gets no
  answer is tried again at a later run and holds later checkpoints meanwhile. A store that keeps
  no tree here (a plugin's) does not re-anchor, and its status says so.
