# Settlement records conformance vectors (draft-mih-agent-settlement-records-00)

Conformance vectors for the two-party settlement records defined in
`spec/draft-mih-agent-settlement-records-00.md`: leg records sealed by two
independent sealers, the typed payment reference join, wrapped objects carried
by digest, exact amounts, the delivered-content digest, and the states a
verifier derives from the legs present.

**Derived from the -00 text, not from any implementation.** Every expected
result is written by hand in the generator from the section it cites. The
generator builds the leg records, computes each Capsule ID (SHA-256 over
RFC 8785), signs each leg with a Producer Envelope from a fixed Ed25519 seed,
and builds the upstream objects the legs wrap. It runs no settlement verifier.
Every case is `"provenance": "spec-derived"`.

`python/scripts/generate_settlement_vectors.py` regenerates every file here
except this README, byte for byte. `python/tests/test_settlement_vectors.py`
checks that, verifies every leg with the base profile's Class 1 verifier and
every Producer Envelope, and derives the failures and states with an
independent reading of the draft.

## Files

| File | What it pins |
|---|---|
| `cases.json` | The cases. Each has the leg records (`capsule`, `capsule_id`, `envelope_hex`, `envelope_kid`), the wrapped objects' octets (`wrapped_objects`), the verifier's key policy (`key_policy`), and `expect`. Also carries the five fixed keys (seed and public key) and the envelope profile. |
| `registry.json` | The closed sets and registries of -00: legs, sealer roles, statuses, delivery directions, payment and delivery states, failure codes, the payment reference types with their qualifiers and whether a receive fee may apply, the wrapped object types with their issuer role, the members each leg carries, and the ISO 20022 status map. |
| `manifest.json` | SHA-256 and case count of every generated file. |
| `SHA256SUMS` | SHA-256 of every file in this directory except itself. |

## The cases

| Case | What it shows | Expected |
|---|---|---|
| `pos-x402-two-sided-agreed` | x402, 1.5 USDC on Base. Payee seals terms (wraps the signed offer by digest); payer and payee each seal their own observation; the payee cites the payer's leg as `counterparty_half`; each side seals its side of the delivery. | `agreed` (`settled`), delivery `matched` |
| `pos-x402-one-sided-payee` | Only the payee's observed leg. | `payee_stated`: a stated claim |
| `pos-x402-one-sided-payer` | Only the payer's observed leg and its received delivery. | `payer_stated`, delivery `stated` |
| `state-x402-mismatch-asset` | The payee reports a different asset. | `mismatch`, differs `amount` |
| `pos-x402-amount-equal-across-scales` | 1500000 at scale 6 against 150000000 at scale 8. | `agreed` |
| `pos-bolt12-two-sided-payer-proof` | Lightning, 21000000 msat (`assetScale` 11); payment hash fixed in the terms; the payer wraps its payer proof. | `agreed` |
| `pos-ap2-receipt-wrapped-by-digest` | The payer seals AP2 terms (Payment Mandate by digest); the payee wraps the processor-signed Payment Receipt by digest only. | `payee_stated` |
| `neg-wrapped-object-resigned` | The same receipt re-signed with the payee's own leg key. | failure `wrapped_resigned`; the leg is excluded: `terms_only` |
| `neg-wrapped-content-digest-mismatch` | Wrapped content does not hash to the stated digest. | failure `wrapped_digest_mismatch`: `payer_stated` |
| `neg-amount-json-float` | `value` is the JSON number 1.5. The leg cannot be canonicalized, so it has no Capsule ID and no envelope. | failures `capsule_invalid`, `amount_not_exact`: `payee_stated` |
| `neg-amount-decimal-fraction-string` | `value` is the string `"1.500000"`. | failure `amount_not_exact`: `payee_stated` |
| `neg-payment-ref-type-unknown` | Both sides use an unregistered `x-` type with identical values. | conforming (never-reject), finding `payment_ref_type_unknown`, state `unjoined` |
| `neg-payee-leg-sealed-by-payer-key` | The payee-observed leg is signed with the payer's key. | failures `sealer_not_authorized_for_role` (payee leg) and `sealer_conflation` (the pair): `payer_stated` |
| `state-delivery-mismatch` | The terms pin the content digest; the payer received different content. | `agreed`, delivery `mismatch` |
| `pos-ln-receive-fee-two-payments` | Two Lightning payments, from the numbers of a real run: the payer sent 1000 msat with `routing_fee` 0; the payee's wallet recorded `received` 995 with `receive_fee` 5. 1000 = 995 + 5. | both settlements `agreed` (naive equality would say `mismatch`) |
| `state-ln-receive-fee-mismatch` | The payee reports `received` 995 with `receive_fee` 0. | `mismatch`, differs `amount` |
| `neg-ln-receive-fee-absent` | The payee reports `received` 995 and no `receive_fee`. Lightning receive fees may apply, so absent is not zero. | finding `fee_unstated`, `unjoined` (never a false `agreed`) |
| `pos-amount-decimals-mixed-scales` | Non-trivial decimals at mixed scales: 0.000001 USDC as value 1 at scale 6, reported by the payee as 100 at scale 8; 1234.56 USD as value 123456 at scale 2, reported by the payee as 1234500 received plus 60 fee at scale 3. | both settlements `agreed`, compared exactly at the largest scale |
| `state-amount-decimals-off-by-one-unit` | The same, but the payee reports 1234501 at scale 3 (1234.561 with the fee). | the USD settlement `mismatch`, differs `amount`; the USDC one `agreed` |

Every observed leg in the other cases carries explicit fee members:
`routing_fee` 0 on the payer side and `receive_fee` 0 on the payee side.

`neg-payment-ref-type-unknown` is negative for the join, not for the records:
the legs stay valid Capsules (the base profile's never-reject invariant) and a
verifier reports `unjoined` instead of `agreed`.

## How to consume them

For each case, a verifier:

1. Checks each leg with the base profile's Class 1 checks. These vectors report
   a failure there as `capsule_invalid`, and a Producer Envelope that does not
   verify as `envelope_invalid`; these two codes are local to the vectors, the
   draft refers to the base profile's own checks (§9.1).
2. Authenticates each envelope and compares its key with `key_policy` for the
   leg's `sealer_role` (§3).
3. Checks the `settlement` member (§4.2), the amounts (§6), and the wrapped
   entries (§8). For `wrapped_resigned`, the wrapped object's octets come from
   the entry's `content`, or else from `wrapped_objects` by digest; an object in
   JWS compact form whose signature verifies under the leg's own key, where the
   type's issuer role in `registry.json` is not the leg's `sealer_role`, is
   re-signed.
4. Reports `sealer_conflation` for a payer-observed and payee-observed pair for
   one terms leg under one key. This is a failure of the pair: it blocks
   `agreed` but does not by itself exclude either leg.
5. Excludes every leg with a failure of its own, groups the remaining legs by
   `terms_ref`, and for each terms leg derives the payment state (§9.2) and the
   delivery state (§9.3). The payment state uses the amount rule:
   `payer.amount = payee.received + payee.receive_fee`, exactly, at the largest
   scale, with one asset. An absent `receive_fee` counts as zero only where
   `registry.json` marks the payment reference type `"receive_fee":
   "not_applicable"` (only `x402.transaction`, and only when the x402 scheme
   is established as `exact` from a wrapped `x402.offer` or
   `x402.payment-payload` whose octets the verifier holds, §7.2); otherwise
   the pair is `unjoined` with finding `fee_unstated` (§6.1). Every x402 leg
   in these vectors carries an explicit `receive_fee`, so no case depends on
   that reading.

`expect.settlements` has one entry per terms leg, in record order, each
naming the terms leg's label. `expect.failures` and `expect.findings` list `{records, code}` in the order a
verifier following the steps above, record by record, reports them.
`expect.conforming` is false when any failure is present.

## Conventions

- Digests are SHA-256, 64 lowercase hex characters.
- Capsule IDs are SHA-256 over RFC 8785 of the Capsule without `capsule_id`.
- Producer Envelopes follow the base profile: tagged COSE_Sign1, protected
  `{1: -8, 3: "application/agent-action-capsule-id", 4: <raw Ed25519 public key>}`,
  empty unprotected map, attached payload the raw 32-byte Capsule ID.
- `wrapped[].content` and `wrapped_objects[].octets_b64u` are base64url without
  padding.
- The upstream objects are illustrative. They use the field names of their
  specifications (x402 v2 and its offer and receipt extension 0.6, AP2 v0.2) but
  are not conformance vectors for those specifications. Each JWS uses EdDSA so
  the signature is deterministic. The BOLT 12 octets are fixed placeholder bytes,
  not a valid TLV stream.
- Keys: `payer` and `payee` seal legs; `payee x402 offer signing` signs the
  x402 offer; `payer AP2 mandate signing` signs the AP2 mandate; `AP2 payment
  processor` signs the AP2 receipt. Each seed is SHA-256 of
  `"draft-mih-agent-settlement-records-00 conformance vectors: <name> key"`.
