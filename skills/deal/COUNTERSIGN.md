# The countersign rung on a deal receipt

Every deal receipt names how far it can be trusted, in rungs. The receipt,
the report JSON and the page each carry a `countersign` field. With no
countersignature it says:

> Not countersigned: no other party has signed this record. It stands on the
> agent's own seal and any witness receipt above.

That is the honest value for every receipt today.

## What a countersignature is here

A countersignature is a signature by another party over the digest of one
exact Evidence Bundle file, in the countersign/v1 shape that `capsulectl
countersign request` and `capsulectl countersign verify` already handle. The
bundle digest leaves out only the `countersignatures` member itself, so the
signature covers everything else in that file.

capsulectl sends nothing to get one. How a countersigner came to sign a
bundle is outside capsulectl. The deal commands only check a countersignature
you already hold:

```sh
# 1. Write the deal's report bundle.
capsulectl --profile deal deal report --deal DEAL_ID --bundle bundle.json

# 2. Attach a countersignature you hold. It is verified before it is written;
#    one that does not verify is refused, and the file is left unchanged.
capsulectl --profile deal deal countersign --deal DEAL_ID --bundle bundle.json \
  --entry countersignature.json --directory DIRECTORY

# 3. Render the receipt from that same file, so the signature covers what it shows.
capsulectl --profile deal deal report --deal DEAL_ID --from-bundle bundle.json \
  --directory DIRECTORY --html receipt.html --email receipt.eml
```

`--directory` is a countersigner directory (an HTTPS URL or a file, as for
`countersign verify`) that you choose. It resolves which party a signing key
belongs to. No list is built in.

`deal countersign` refuses a bundle that is not this deal's own. The bundle
must verify, its log must be the deal's, its checkpoint must be signed by
this profile's checkpoint key, its root must be one of the deal's steps, and
its deal extension must name this deal. Without `--entry`,
`deal countersign` only verifies the countersignatures the file already
carries.

## What each value says

The evidence grade ladder is self-attested → witnessed → countersigned.
`countersigned` is its top rung. `self_countersigned` and
`unresolved_signer` are annotations, not rungs: they add nothing, and the
receipt's grade stays where witnessing put it (`assurance.rung`:
`self_attested`, `witnessed` or `witnessed_in_part`), so a self-countersigned
receipt with no witness receipt is self-attested. `not_countersigned` and
`unverified` name no rung.

| Value | When | What the receipt says |
|---|---|---|
| `not_countersigned` | no countersignature | Not countersigned (above) |
| `countersigned` | the signature verifies and the directory lists the key | Countersigned by NAME, a signer listed in the directory you chose, over this exact bundle, with its own check results. It vouches only as far as you trust that directory. |
| `self_countersigned` | the key is the producer's own (a key in the profile's `trusted_keys`, or the one the bundle's `producer-key/v1` extension declares) | **NOT INDEPENDENT**: countersigned by the producer's own key. A self-countersignature is not a check by anyone else. A directory listing that key does not change this. |
| `unresolved_signer` | the signature verifies, but the directory does not list the key | Countersigned by a key the directory does not list: its signer is unknown. |
| `unverified` | a countersignature type capsulectl does not check | Attached, not counted. |

The page shows what capsulectl found when it wrote the page: a page cannot
resolve a signer against a directory itself. Anyone can check the same file
with `capsulectl countersign verify bundle.json --directory DIRECTORY`.

## The optional second reader

`CAPSULE_DEAL_CHECK_URL` is a different, separate hook. It names an optional
second reader of a `deal check`:

- It is off unless set.
- It receives only the minimal fields (no message text, names, contact
  details or card numbers).
- It can only add differences: its "pass" never clears a local pause.
- It gets 2 seconds. When it is slow or unreachable, the local rules decide
  alone.
