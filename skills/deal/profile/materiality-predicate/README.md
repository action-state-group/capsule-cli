# Materiality predicates

A deal check pauses for the user's nod on an attribute the agent picked on its own
(a size, a delivery option the user never named) when that attribute is
**material**. Which attributes are material is policy, so capsulectl does not
decide it: it evaluates a **materiality predicate**, a `materiality-predicate/v0`
JSON document, that the profile names and **pins by its digest**: `deal init
--materiality FILE`, or `profile update --materiality FILE` later (stored as
`materiality.predicate` and `materiality.digest`). A check takes no predicate of
its own: whoever runs it cannot choose another.

**Setting or changing the predicate is the user's, never the agent's: it is
policy.** A check refuses, and seals nothing, when the pinned file no longer has
the pinned digest ("materiality predicate changed since it was pinned"); the
user re-pins it with `profile update --materiality FILE`. Every check says
which predicate decided its pauses (name, version and digest, or digest `none`)
in its output. Its sealed verdict, and the deal's opening, carry the digest and
a commitment to the name and version: the user's own copy opens it, while a
copy shared with a counterparty discloses the digest only, since the name and
version describe the user's own policy.

**With no predicate configured, every attribute the agent picked pauses the check.**
That is the safe default: nothing the agent chose alone goes through unasked.

## The built-in predicate

capsulectl ships one predicate, `builtin:neutral`: `deal init --materiality
builtin:neutral` or `profile update --materiality builtin:neutral`. It is
pinned by that name and its digest, not by a path, so the pin survives an
upgrade or a move of capsulectl. Its name is "quantity, size, variant and
delivery picks", version 1.0.0, and its matchers are those of `neutral.json`
below: a quantity of 1 the agent filled in does not pause; any other quantity,
or a condition whose name mentions size, variant, shipping, delivery or
quantity, does. Its digest is
`05ec66b4a21025a66288cafc74e7fa1c540cc62695c5f536df84f4a7d7f66d4a`. A built-in
never changes under its name: a new version ships under a new name, and a
profile pinning the old one keeps its pin until the user re-pins.

A predicate sees the value the agent picked, not what else was offered: a size
picked when the offer had only one size still pauses.

`neutral.json` here is an **example, not policy**. It reproduces what capsulectl
paused on before predicates existed (a quantity other than 1; a condition whose
name mentions size, variant, shipping, delivery or quantity), so demos and tests
behave as before. A rule pack supplies its own predicate and supersedes it.

## The format

```json
{
  "type": "materiality-predicate/v0",
  "name": "a name for people",
  "version": "1.0.0",
  "material": [
    {"field": "quantity", "except_values": ["1"]},
    {"field_prefix": "conditions.", "name_contains_any": ["size"]}
  ]
}
```

An attribute the agent picked is material when it matches at least one matcher:

- `field`: the attribute's whole field (`item`, `quantity`, `when`, `place`,
  `conditions.<name>`); or `field_prefix`, with optional `name_contains_any`
  (case-insensitive words in the name after the prefix). Exactly one of the two.
- `except_values`: values of a matching attribute that are not material.

The document is read strictly: an unknown member, another `type`, a matcher with
both or neither of `field` and `field_prefix`, or an empty word refuses the
check, and nothing is sealed. `"material": []` is a valid, explicit choice that
nothing the agent picks is material. The predicate's identity is the SHA-256 of
its RFC 8785 (JCS) bytes.
