- **Fixed:** `verify --bundle` on a bundle that is INCOMPLETE only because a `composed/v1`
  member is a refusal the observer signed no longer says "I couldn't check this file here."
  It says part of it was refused, the refusal is signed, and everything else in it checked out (#513, new
  `say.verify.refused` wording key; a pinned wording pack without it falls back to the
  built-in wording pack and `doctor` reports it).
