- **Fixed:** `verify --bundle` on a bundle that is INCOMPLETE only because a `composed/v1`
  member is a refusal the observer signed no longer says "I couldn't check this file here."
  It says the other side said no to part of it and that what was sent checked out (#513, new
  `say.verify.refused` wording key; a pinned wording pack without it falls back to the
  built-in line and `doctor` reports it).
