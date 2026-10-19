#!/usr/bin/env python3
"""Regenerate the held-object vectors (capsule-cli#250) byte for byte.

Run from this directory: python3 -I generate.py

It writes the constructed fixtures, cases.json and SHA256SUMS. The copied
fixtures (acp-*, ucp-*) are taken as they are; see README.md for their
sources. Every expected digest is computed here, from settlement-records-00
section 8 rule 1, with no capsule-cli code: SHA-256 over the octets as
delivered (as-transmitted), or over their RFC 8785 form (jcs).

The RFC 8785 form below is json.dumps with sorted keys, no whitespace and
ensure_ascii off. That equals RFC 8785 for these fixtures: their member names
are ASCII (so code-unit order is byte order) and their numbers are integers.
The generator refuses any other input rather than computing a wrong form.
"""

import base64
import hashlib
import json
import os


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def _plain(value):
    if isinstance(value, dict):
        for k, v in value.items():
            if not k.isascii():
                raise ValueError("non-ASCII member name: RFC 8785 order needs UTF-16 sorting")
            _plain(v)
    elif isinstance(value, list):
        for v in value:
            _plain(v)
    elif isinstance(value, float):
        raise ValueError("a non-integer number: this generator does not write its RFC 8785 form")


def jcs(octets):
    def no_duplicates(pairs):
        keys = [k for k, _ in pairs]
        if len(keys) != len(set(keys)):
            raise ValueError("duplicate member")
        return dict(pairs)

    value = json.loads(octets.decode("utf-8"), object_pairs_hook=no_duplicates)
    _plain(value)
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def digest(octets, rule):
    return sha256(jcs(octets) if rule == "jcs" else octets)


def write(name, data):
    with open(name, "wb") as f:
        f.write(data)


def read(name):
    with open(name, "rb") as f:
        return f.read()


def one_byte_changed(data):
    # The last digit in the object becomes another digit: still the same
    # form, a different digest.
    i = max(data.rfind(c) for c in b"0123456789")
    return data[:i] + (b"1" if data[i:i + 1] != b"1" else b"2") + data[i + 1:]


# Constructed fixtures. The x402 JWS is the x402 signed offer of
# agent-action-capsule's settlement-records vectors (case
# pos-x402-two-sided-agreed); the EIP-712 envelope and the delivery proof are
# illustrative placeholders, not valid signatures.
JWS = read("x402-offer.jws") if os.path.exists("x402-offer.jws") else None
if JWS is None:
    raise SystemExit("x402-offer.jws is the settlement-records vector's offer; copy it first (README.md)")
protected, payload, signature = JWS.decode("ascii").split(".")
write("x402-offer-jws-newline.txt", JWS + b"\n")
write("x402-receipt-flattened-jws.json", json.dumps(
    {"protected": protected, "payload": payload, "signature": signature}, indent=2).encode() + b"\n")
write("x402-receipt-eip712.json", json.dumps({
    "format": "eip712",
    "payload": {"network": "eip155:8453", "resource": "https://api.example/weather", "transaction": "0x" + "ab" * 32, "issuedAt": 1790000000},
    "signature": "0x" + "cd" * 65,
}, indent=2).encode() + b"\n")
write("delivery-proof.txt", b"Proof of delivery (illustrative placeholder)\nParcel delivered 2026-10-19 to the front desk.\n")
write("duplicate-member.json", b'{"id": "a", "id": "b"}\n')
write("acp-checkout-session-with-order.changed.json", one_byte_changed(read("acp-checkout-session-with-order.json")))
headers = read("ucp-checkout-response.headers")
write("ucp-checkout-response.changed.headers", headers.replace(b"200 OK", b"201 OK", 1))

acp = read("acp-checkout-session-with-order.json")
ucp = read("ucp-checkout.json")
body = read("ucp-checkout-response.body")
eip = read("x402-receipt-eip712.json")
proof = read("delivery-proof.txt")


def case(name, what, seal, expect, object=None, object_digest=None, headers=None, verify=None):
    c = {"name": name, "what": what, "seal": dict(seal, object=object, object_digest=object_digest, headers=headers), "expect": expect}
    if verify:
        c["verify"] = verify
    return c


cases = [
    case("acp-checkout-as-transmitted", "An ACP completed checkout with its order, held as delivered. Its type is the name acp-v0 open point A-3 proposes, not registered.",
         {"type": "acp.checkout-session", "octets": "as-transmitted"},
         {"sealed": True, "digest": digest(acp, "as-transmitted"), "type_registered": False, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "NOT_PRESENT"},
         object="acp-checkout-session-with-order.json",
         verify=[{"object": "acp-checkout-session-with-order.changed.json", "expect": {"object_digest": "FAIL", "code": "wrapped_digest_mismatch"}}]),
    case("ucp-checkout-jcs", "A UCP checkout with ap2.merchant_authorization (agent-action-capsule's UCP adapter vector 'positive'). ucp-v0 section 6 proposes ucp.checkout over the RFC 8785 form.",
         {"type": "ucp.checkout", "octets": "jcs"},
         {"sealed": True, "digest": digest(ucp, "jcs"), "type_registered": False, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "NOT_PRESENT"},
         object="ucp-checkout.json"),
    case("ucp-checkout-response-with-headers", "A signed UCP checkout response held whole: the raw body as the object, its status line and fields as the carrier headers. The raw body is stored, so its Content-Digest and signature can be checked again.",
         {"type": "ucp.checkout", "octets": "jcs"},
         {"sealed": True, "digest": digest(body, "jcs"), "type_registered": False, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "PASS"},
         object="ucp-checkout-response.body", headers="ucp-checkout-response.headers",
         verify=[{"headers": "ucp-checkout-response.changed.headers", "expect": {"carrier_headers": "FAIL"}}]),
    case("registered-as-transmitted", "A registered type whose octets are as delivered.",
         {"type": "delivery.proof", "octets": "as-transmitted"},
         {"sealed": True, "digest": digest(proof, "as-transmitted"), "type_registered": True, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "NOT_PRESENT"},
         object="delivery-proof.txt"),
    case("registered-wrong-rule", "A registered type sealed under a rule its definition does not give.",
         {"type": "delivery.proof", "octets": "jcs"}, {"sealed": False, "error": "as delivered"}, object="delivery-proof.txt"),
    case("x402-eip712-jcs", "An x402 receipt in EIP-712 form: the RFC 8785 form of the envelope.",
         {"type": "x402.receipt", "octets": "jcs"},
         {"sealed": True, "digest": digest(eip, "jcs"), "type_registered": True, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "NOT_PRESENT"},
         object="x402-receipt-eip712.json"),
    case("x402-eip712-as-transmitted", "The same envelope sealed as delivered.",
         {"type": "x402.receipt", "octets": "as-transmitted"}, {"sealed": False, "error": "its form takes jcs"}, object="x402-receipt-eip712.json"),
    case("x402-jws-as-transmitted", "An x402 offer as a JWS compact serialization: its ASCII octets.",
         {"type": "x402.offer", "octets": "as-transmitted"},
         {"sealed": True, "digest": digest(JWS, "as-transmitted"), "type_registered": True, "wrapped_held": "octets", "object_digest": "PASS", "carrier_headers": "NOT_PRESENT"},
         object="x402-offer.jws"),
    case("x402-jws-jcs", "The same JWS sealed under jcs.",
         {"type": "x402.offer", "octets": "jcs"}, {"sealed": False, "error": "its form takes as-transmitted"}, object="x402-offer.jws"),
    case("x402-jws-trailing-newline", "The JWS saved with a trailing newline: not the octets as received.",
         {"type": "x402.offer", "octets": "as-transmitted"}, {"sealed": False, "error": "trailing whitespace"}, object="x402-offer-jws-newline.txt"),
    case("x402-flattened-jws", "A JWS in flattened JSON serialization: a JSON object, but not a form the type defines.",
         {"type": "x402.receipt", "octets": "jcs"}, {"sealed": False, "error": "neither a JWS compact serialization nor an EIP-712 envelope"}, object="x402-receipt-flattened-jws.json"),
    case("digest-only", "Only the digest of the ACP checkout: no octets held, no possession claimed.",
         {"type": "acp.checkout-session", "octets": "as-transmitted"},
         {"sealed": True, "digest": digest(acp, "as-transmitted"), "type_registered": False, "wrapped_held": "digest", "object_digest": "NOT_PRESENT", "carrier_headers": "NOT_PRESENT"},
         object_digest=digest(acp, "as-transmitted"),
         verify=[{"object": "acp-checkout-session-with-order.json", "expect": {"object_digest": "PASS"}}]),
    case("jcs-duplicate-member", "jcs over an object with a duplicate member: not I-JSON.",
         {"type": "x-example.object", "octets": "jcs"}, {"sealed": False, "error": "not I-JSON"}, object="duplicate-member.json"),
]

recipient = [
    {"name": "recipient-pass", "what": "A recipient checks the object against the digest and wrapped_octets of the record in its copy.",
     "object": "ucp-checkout-response.body", "digest": digest(body, "jcs"), "octets": "jcs", "expect": {"object_digest": "PASS"}},
    {"name": "recipient-changed-byte", "what": "The same check with one byte changed.",
     "object": "acp-checkout-session-with-order.changed.json", "digest": digest(acp, "as-transmitted"), "octets": "as-transmitted",
     "expect": {"object_digest": "FAIL", "code": "wrapped_digest_mismatch"}},
]

write("cases.json", json.dumps({"vectors": "capsule-cli held object (#250)", "cases": cases, "recipient": recipient}, indent=2).encode() + b"\n")
names = sorted(n for n in os.listdir(".") if n not in ("SHA256SUMS",) and os.path.isfile(n))
write("SHA256SUMS", "".join(f"{sha256(read(n))}  {n}\n" for n in names).encode())
