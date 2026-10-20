# Held-object vectors (capsule-cli#250)

Vectors for `deal note --object` and `deal verify-object`: an object the user
holds, sealed on a buyer's deal as one entry in the wrapped-object shape of
`draft-mih-agent-settlement-records-00` §8 (`{type, digest_alg, digest}`),
with `wrapped_octets`, `wrapped_held` and an optional carrier headers
commitment beside it.

**Derived from the spec text, not from capsule-cli.** `generate.py` writes
every expected digest from §8 rule 1, with no capsule-cli code: SHA-256 over
the octets as delivered (`as-transmitted`), or over their RFC 8785 form
(`jcs`). It regenerates every file here except the copied fixtures, this
README and `generate.py` itself, byte for byte:

```sh
python3 -I generate.py
```

`deal_wrapped_test.go` seals each case on a buyer's book deal, checks the
record, and runs `deal verify-object` in a new process on the reopened store.
`TestWrappedVectorFilesMatchTheirSums` holds the files to `SHA256SUMS`.

## Files

| File | Source |
|---|---|
| `acp-checkout-session-with-order.json` | evidencebook v0.6.0 `adapter/acp/testdata/checkout-session-with-order.json`: constructed from ACP's published examples at commit `7fdd78df677a94dce04c770644b0fbbb1401272b` (member `complete_checkout_session_response`), not recorded traffic. |
| `ucp-checkout.json` | agent-action-capsule `vectors/adapters/ucp/positive.checkout.json` (as copied into evidencebook v0.6.0 `adapter/ucp/testdata/aac-vectors/`): a UCP checkout carrying `ap2.merchant_authorization`, signed with the RFC 7515 Appendix A.3 example key. |
| `ucp-checkout-response.headers`, `ucp-checkout-response.body` | evidencebook v0.6.0 `adapter/ucp/testdata/checkout-response-signed.http`, split at the empty line (CRLF): the status line and fields, and the raw body. |
| `ucp-trust-policy.json` | evidencebook v0.6.0 `adapter/ucp/testdata/trust-policy.json`: the example key set the response signature checks against (used only by the adapter cross-check test). |
| `x402-offer.jws` | agent-action-capsule settlement-records vectors, case `pos-x402-two-sided-agreed`, the wrapped `x402.offer` (a JWS compact serialization). |
| `x402-offer-jws-newline.txt`, `x402-receipt-flattened-jws.json`, `x402-receipt-eip712.json`, `delivery-proof.txt`, `duplicate-member.json`, `decimal-numbers.json`, `*.changed.*` | Written by `generate.py`. The EIP-712 envelope and the proof of delivery are illustrative placeholders: their signatures are not valid. |
| `cases.json` | The cases, below. |
| `SHA256SUMS` | SHA-256 of every file here but itself. |

## The cases

Each `cases[]` entry gives the seal's inputs (`type`, `octets`, and an
`object` file, an `object_digest`, or both with `headers`) and `expect`: either
`sealed: false` with a phrase of the refusal, or the sealed `digest`,
`type_registered`, `wrapped_held` and the `verify-object` results from the
held bytes. `verify[]` repeats the check with another copy (`--object`,
`--headers`) and its expected results. `recipient[]` checks an object against
a digest with no deal.

| Case | Expect |
|---|---|
| `acp-checkout-as-transmitted` | sealed, `type_registered` false; a copy with one byte changed is `FAIL` `wrapped_digest_mismatch` |
| `ucp-checkout-jcs` | sealed over the RFC 8785 form |
| `ucp-checkout-response-with-headers` | the raw body held, digest over its RFC 8785 form, headers `PASS`; changed headers `FAIL` |
| `registered-as-transmitted` | `delivery.proof`, `type_registered` true |
| `registered-wrong-rule` | `delivery.proof` under `jcs`: refused |
| `x402-eip712-jcs` / `x402-eip712-as-transmitted` | the EIP-712 envelope (exactly `format`, `payload`, `signature`) takes `jcs`; `as-transmitted` is refused |
| `x402-jws-as-transmitted` / `x402-jws-jcs` | a JWS compact serialization takes `as-transmitted`; `jcs` is refused |
| `x402-jws-trailing-newline` | refused, naming the trailing whitespace |
| `x402-flattened-jws` | a JSON-serialized JWS is neither form the type defines: refused |
| `digest-only` | `wrapped_held` digest, `object_digest` `NOT_PRESENT`; given the object, `PASS` |
| `jcs-decimal-numbers` | non-integer and exponent numbers under `jcs`: the expected RFC 8785 form is written out by hand in `generate.py` (ECMAScript number output), not computed |
| `jcs-duplicate-member` | not I-JSON: refused |

## Conventions

- Digests are SHA-256, 64 lowercase hex characters.
- The ACP and UCP type names (`acp.checkout-session`, `ucp.checkout`) are the
  ones the agent-action-capsule adapter declarations propose (acp-v0 open
  point A-3, ucp-v0 §6). They are not registered, so these vectors seal them
  as labels, `type_registered: false`.
- `generate.py`'s RFC 8785 form is `json.dumps` with sorted keys, no
  whitespace and `ensure_ascii` off, which equals RFC 8785 for these fixtures
  (ASCII member names, integer numbers); it refuses any other input. The one
  case with other numbers, `jcs-decimal-numbers`, has its expected form
  written out by hand.
