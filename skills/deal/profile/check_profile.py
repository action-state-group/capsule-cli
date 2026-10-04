#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Checker for the x-deal-v0 deal record profile (skill-local, not registered).

  python3 check_profile.py            validate every fixture, recompute every JCS digest
  python3 check_profile.py --selftest RFC 8785 (JCS) self-test only
  python3 check_profile.py --regen    rewrite fixtures/ deterministically (maintainers only)
  python3 check_profile.py FILE...    check deal records (one JSON record per file, or a
                                      JSON array = one deal in seq order)

Stdlib only. Uses `jsonschema` for the schema stage when it is installed; without it a
reduced structural check runs and the output says so.

Stages, in order (a negative fixture names the stage it must fail at):
  canonicalization -> personal_data -> wording -> schema -> chain -> digest
(the privacy and wording scans run before the schema, so a leak is caught even in a record
that is malformed in other ways)
"""
from __future__ import annotations

import hashlib
import hmac
import json
import math
import re
import struct
import sys
import unicodedata
from decimal import Decimal
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCHEMA_PATH = HERE / "x-deal-v0.schema.json"
FIX = HERE / "fixtures"

RECORD_TYPES = ["intent", "baseline", "message", "claim", "evidence", "detail_change",
                "check", "verdict", "approval", "action", "outcome", "close", "disclosure"]
# The point of no return whose check covers a disclosed field of each class.
DISCLOSURE_ACTION = {c: "share_contact" for c in ("name", "phone", "email", "home_address", "address",
                                                  "pickup_location", "other_contact")}
DISCLOSURE_ACTION.update({c: "share_credentials" for c in ("credential", "verification_code",
                                                           "payment_card", "id_document")})
IDENTIFIER_KINDS = ["payee", "name", "domain", "phone", "email", "relay_address", "profile_id"]
SAFE_INT = 2**53 - 1


# ---------------------------------------------------------------------------
# RFC 8785 JSON Canonicalization Scheme
# ---------------------------------------------------------------------------

class JCSError(ValueError):
    pass


def _jcs_number(x) -> str:
    if isinstance(x, bool):
        raise JCSError("bool is not a number")
    if isinstance(x, int):
        if abs(x) > SAFE_INT:
            raise JCSError(f"integer {x} outside the IEEE-754 safe range")
        return str(x)
    if math.isnan(x) or math.isinf(x):
        raise JCSError("NaN/Infinity are not JSON")
    if x == 0:
        return "0"
    sign = "-" if x < 0 else ""
    # repr() is the shortest round-trip form; re-lay it out per ECMAScript Number::toString.
    _, digits, exp = Decimal(repr(abs(x))).as_tuple()
    ds = "".join(map(str, digits)).rstrip("0") or "0"
    exp += len("".join(map(str, digits))) - len(ds)
    k = len(ds)
    n = k + exp
    if k <= n <= 21:
        out = ds + "0" * (n - k)
    elif 0 < n <= 21:
        out = ds[:n] + "." + ds[n:]
    elif -6 < n <= 0:
        out = "0." + "0" * (-n) + ds
    else:
        e = n - 1
        es = ("+" if e > 0 else "-") + str(abs(e))
        out = (ds[0] + ("." + ds[1:] if k > 1 else "")) + "e" + es
    return sign + out


_ESC = {'"': '\\"', "\\": "\\\\", "\b": "\\b", "\f": "\\f", "\n": "\\n", "\r": "\\r", "\t": "\\t"}


def _jcs_string(s: str) -> str:
    out = ['"']
    for ch in s:
        o = ord(ch)
        if ch in _ESC:
            out.append(_ESC[ch])
        elif o < 0x20:
            out.append("\\u%04x" % o)
        elif 0xD800 <= o <= 0xDFFF:
            raise JCSError("lone surrogate")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _jcs(v) -> str:
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, (int, float)):
        return _jcs_number(v)
    if isinstance(v, str):
        return _jcs_string(v)
    if isinstance(v, list):
        return "[" + ",".join(_jcs(e) for e in v) + "]"
    if isinstance(v, dict):
        keys = sorted(v.keys(), key=lambda k: k.encode("utf-16-be"))
        return "{" + ",".join(_jcs_string(k) + ":" + _jcs(v[k]) for k in keys) + "}"
    raise JCSError(f"not a JSON value: {type(v).__name__}")


def jcs(v) -> bytes:
    return _jcs(v).encode("utf-8")


def sha256_hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def record_digest(record) -> str:
    """The x-deal-v0 record digest: lowercase hex SHA-256 over the JCS bytes of the record."""
    return sha256_hex(jcs(record))


def jcs_selftest() -> list[str]:
    fails = []
    # RFC 8785 Appendix B: IEEE-754 bit pattern -> expected serialization.
    appendix_b = [
        ("0000000000000000", "0"), ("8000000000000000", "0"),
        ("0000000000000001", "5e-324"), ("8000000000000001", "-5e-324"),
        ("7fefffffffffffff", "1.7976931348623157e+308"),
        ("ffefffffffffffff", "-1.7976931348623157e+308"),
        ("4340000000000000", "9007199254740992"), ("c340000000000000", "-9007199254740992"),
        ("4430000000000000", "295147905179352830000"),
        ("44b52d02c7e14af5", "9.999999999999997e+22"), ("44b52d02c7e14af6", "1e+23"),
        ("44b52d02c7e14af7", "1.0000000000000001e+23"),
        ("444b1ae4d6e2ef4e", "999999999999999700000"), ("444b1ae4d6e2ef4f", "999999999999999900000"),
        ("444b1ae4d6e2ef50", "1e+21"),
        ("3eb0c6f7a0b5ed8c", "9.999999999999997e-7"), ("3eb0c6f7a0b5ed8d", "0.000001"),
        ("41b3de4355555553", "333333333.3333332"), ("41b3de4355555554", "333333333.33333325"),
        ("41b3de4355555555", "333333333.3333333"), ("41b3de4355555556", "333333333.3333334"),
        ("41b3de4355555557", "333333333.33333343"),
        ("becbf647612f3696", "-0.0000033333333333333333"),
        ("43143ff3c1cb0959", "1424953923781206.2"),
    ]
    for bits, want in appendix_b:
        x = struct.unpack(">d", bytes.fromhex(bits))[0]
        got = _jcs_number(x)
        if got != want:
            fails.append(f"Appendix B {bits}: got {got} want {want}")
    for bits in ("7fffffffffffffff", "7ff0000000000000"):
        try:
            _jcs_number(struct.unpack(">d", bytes.fromhex(bits))[0])
            fails.append(f"Appendix B {bits}: NaN/Infinity accepted")
        except JCSError:
            pass
    # RFC 8785 section 3.2.2 (input as parsed JSON) -> section 3.2.3 output.
    src = ('{"numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],'
           ' "string": "\\u20ac$\\u000F\\u000aA\'\\u0042\\u0022\\u005c\\\\\\"\\/",'
           ' "literals": [null, true, false]}')
    want = ('{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],'
            '"string":"€$\\u000f\\nA\'B\\"\\\\\\\\\\"/"}')
    got = jcs(json.loads(src)).decode("utf-8")
    if got != want:
        fails.append(f"section 3.2.2 example: got {got!r} want {want!r}")
    # RFC 8785 section 3.2.3 sorting example: property order by UTF-16 code units.
    sort_src = json.loads('{"\\u20ac":"Euro Sign","\\r":"Carriage Return","\\ufb33":"Hebrew Letter Dalet With Dagesh",'
                          '"1":"One","\\ud83d\\ude00":"Emoji: Grinning Face","\\u0080":"Control",'
                          '"\\u00f6":"Latin Small Letter O With Diaeresis"}')
    order = [json.loads(_jcs_string(k)) for k in sorted(sort_src, key=lambda k: k.encode("utf-16-be"))]
    want_order = ["\r", "1", "\u0080", "ö", "€", "\U0001F600", "דּ"]
    if order != want_order:
        fails.append(f"section 3.2.3 sort order: got {order!r}")
    return fails


# ---------------------------------------------------------------------------
# Fingerprints and commitments
# ---------------------------------------------------------------------------

LEGAL_FORMS = {"llc", "inc", "ltd", "corp", "co", "company", "gmbh", "llp", "plc"}
# Checker-local subset of the Public Suffix List, enough for the fixtures. A producer MUST use
# the full PSL; an unknown TLD falls back to the PSL default rule "*" (one label).
PSL_SUBSET = {"com", "net", "org", "example", "test", "co.uk", "org.uk", "com.au"}


class NormError(ValueError):
    pass


def norm_phone(v: str) -> str:
    v = unicodedata.normalize("NFKC", v).strip()
    v = re.split(r"(?i)\s*(?:ext\.?|x|#)\s*\d+\s*$", v)[0]
    plus = v.startswith("+")
    digits = re.sub(r"\D", "", v)
    if not plus:
        if len(digits) == 10:  # v0 default region: NANP
            digits = "1" + digits
        elif not (len(digits) == 11 and digits.startswith("1")):
            raise NormError("phone without + is not a NANP number")
    if not 8 <= len(digits) <= 15:
        raise NormError("E.164 allows 8..15 digits")
    return "+" + digits


def norm_email(v: str) -> str:
    v = unicodedata.normalize("NFC", v.strip()).lower()
    if v.count("@") != 1 or v.startswith("@") or v.endswith("@"):
        raise NormError("not an email address")
    return v


def norm_domain(v: str) -> str:
    v = unicodedata.normalize("NFC", v.strip()).lower()
    v = re.sub(r"^[a-z][a-z0-9+.-]*://", "", v)
    v = re.split(r"[/?#]", v, maxsplit=1)[0]
    v = v.rsplit("@", 1)[-1]
    v = re.sub(r":\d+$", "", v).rstrip(".")
    try:
        v = v.encode("idna").decode("ascii").encode("ascii").decode("idna")
    except UnicodeError as e:
        raise NormError(f"IDNA: {e}") from e
    labels = v.split(".")
    if len(labels) < 2 or not all(labels):
        raise NormError("not a domain")
    for n in (3, 2, 1):
        if len(labels) > n and ".".join(labels[-n:]) in PSL_SUBSET:
            return ".".join(labels[-n - 1:])
    return ".".join(labels[-2:])


def norm_name(v: str) -> str:
    v = unicodedata.normalize("NFC", v).casefold()
    v = "".join(" " if unicodedata.category(c).startswith("P") else c for c in v)
    toks = v.split()
    if toks and toks[0] == "the":
        toks = toks[1:]
    while toks and toks[-1] in LEGAL_FORMS:
        toks = toks[:-1]
    if not toks:
        raise NormError("empty name")
    return " ".join(toks)


def norm_payee(v: str) -> str:
    if "@" in v:
        return norm_email(v)
    if re.fullmatch(r"\s*\+?[\d\s().-]{7,}\s*", v):
        return norm_phone(v)
    return norm_name(v)


def normalize(kind: str, v: str) -> str:
    if kind == "phone":
        return norm_phone(v)
    if kind == "email":
        return norm_email(v)
    if kind == "domain":
        return norm_domain(v)
    if kind == "name":
        return norm_name(v)
    if kind == "payee":
        return norm_payee(v)
    if kind == "relay_address":
        return unicodedata.normalize("NFC", v.strip()).lower()
    if kind == "profile_id":
        out = unicodedata.normalize("NFC", v.strip())
        if not out:
            raise NormError("empty profile id")
        return out
    raise NormError(f"unknown identifier kind {kind}")


def deal_key(store_secret: bytes, deal_id: str) -> bytes:
    return hmac.new(store_secret, b"x-deal-v0/deal-key\x00" + deal_id.encode("utf-8"), hashlib.sha256).digest()


def fingerprint(dkey: bytes, kind: str, raw: str) -> str:
    msg = b"x-deal-v0/fp\x00" + kind.encode() + b"\x00" + normalize(kind, raw).encode("utf-8")
    return hmac.new(dkey, msg, hashlib.sha256).hexdigest()


def commitment(nonce_hex: str, text: str) -> str:
    """Salted content commitment: SHA-256 over JCS({"nonce": <64 hex>, "text": <text>})."""
    return record_digest({"nonce": nonce_hex, "text": text})


# ---------------------------------------------------------------------------
# Stages
# ---------------------------------------------------------------------------

class StageError(Exception):
    def __init__(self, stage, msg):
        super().__init__(f"[{stage}] {msg}")
        self.stage = stage


_SCHEMA = None
_VALIDATOR = None
SCHEMA_MODE = "unknown"


def _validator():
    global _SCHEMA, _VALIDATOR, SCHEMA_MODE
    if _SCHEMA is None:
        _SCHEMA = json.loads(SCHEMA_PATH.read_text())
        try:
            import jsonschema  # noqa: F401
            from jsonschema import Draft202012Validator
            _VALIDATOR = Draft202012Validator(_SCHEMA)
            SCHEMA_MODE = "jsonschema (Draft 2020-12)"
        except ImportError:
            _VALIDATOR = None
            SCHEMA_MODE = "fallback (jsonschema not installed: reduced structural check)"
    return _VALIDATOR


def stage_canonicalization(rec):
    blk = rec.get("x-deal-v0") if isinstance(rec, dict) else None
    if not isinstance(blk, dict):
        raise StageError("canonicalization", "no x-deal-v0 block")
    c = blk.get("canonicalization")
    if c != "jcs":
        raise StageError("canonicalization", f"canonicalization must be exactly \"jcs\" (got {c!r}); fail closed")
    try:
        jcs(rec)
    except JCSError as e:
        raise StageError("canonicalization", f"not JCS-serializable: {e}") from e
    _no_floats(rec)


def _no_floats(v, path="$"):
    if isinstance(v, float):
        raise StageError("canonicalization", f"floating-point value at {path}; money is integer minor units")
    if isinstance(v, dict):
        for k, x in v.items():
            _no_floats(x, f"{path}.{k}")
    elif isinstance(v, list):
        for i, x in enumerate(v):
            _no_floats(x, f"{path}[{i}]")


def stage_schema(rec):
    val = _validator()
    if val is not None:
        errs = sorted(val.iter_errors(rec), key=lambda e: list(e.absolute_path))
        if errs:
            e = errs[0]
            raise StageError("schema", f"{'/'.join(map(str, e.absolute_path)) or '$'}: {e.message[:200]}")
        return
    # Fallback: the parts of the schema the chain stage depends on.
    if set(rec) != {"x-deal-v0", "body"}:
        raise StageError("schema", "record members must be exactly x-deal-v0 and body")
    blk = rec["x-deal-v0"]
    for k in ("profile", "canonicalization", "deal_id", "record_type", "seq", "at"):
        if k not in blk:
            raise StageError("schema", f"x-deal-v0.{k} is required")
    if blk["record_type"] not in RECORD_TYPES:
        raise StageError("schema", f"x-deal-v0/record_type: {blk['record_type']!r} is not one of {RECORD_TYPES}")
    if not isinstance(blk["seq"], int) or blk["seq"] < 1:
        raise StageError("schema", "seq must be an integer >= 1")
    if not re.fullmatch(r"deal-[0-9a-f]{16,64}", str(blk["deal_id"])):
        raise StageError("schema", "deal_id pattern")
    body = rec.get("body")
    if blk["record_type"] == "baseline" and isinstance(body, dict) and "skill" in body:
        sk = body["skill"]
        if not isinstance(sk, dict) or set(sk) != {"skill_md_digest", "other_copies"} or not re.fullmatch(
            r"[0-9a-f]{64}", str(sk.get("skill_md_digest", ""))
        ) or not isinstance(sk.get("other_copies"), int) or sk["other_copies"] < 0:
            raise StageError("schema", "body/skill must be {skill_md_digest: 64 hex, other_copies: integer >= 0}")
    if "producer" in blk:
        prod = blk["producer"]
        if not isinstance(prod, dict) or set(prod) != {"name", "version", "commit"} or not all(
            isinstance(prod[k], str) and 1 <= len(prod[k]) <= 64 for k in prod
        ):
            raise StageError("schema", "x-deal-v0/producer must be {name, version, commit}: non-empty strings")


_EXEMPT = re.compile(r"^([0-9a-f]{16,}|deal-[0-9a-f]+|\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}Z)?|"
                     r"[a-z0-9-]+/[a-z0-9-]+/\d+\.\d+\.\d+)$")
_PHONE = re.compile(r"\+?\(?\d[\d\s().-]{6,}\d")
_EMAIL = re.compile(r"[^\s@]+@[^\s@]+\.[^\s@]+")


def _strings(v, path="$"):
    if isinstance(v, str):
        yield path, v
    elif isinstance(v, dict):
        for k, x in v.items():
            yield f"{path}.<key>", k
            yield from _strings(x, f"{path}.{k}")
    elif isinstance(v, list):
        for i, x in enumerate(v):
            yield from _strings(x, f"{path}[{i}]")


def stage_personal_data(rec, local_store_values=()):
    for path, s in _strings(rec):
        if _EXEMPT.match(s):
            continue
        for m in _PHONE.finditer(s):
            if len(re.sub(r"\D", "", m.group())) >= 7:
                raise StageError("personal_data", f"raw phone number at {path}")
        if _EMAIL.search(s):
            raise StageError("personal_data", f"raw email address at {path}")
        low = s.casefold()
        for raw in local_store_values:
            if raw and raw.casefold() in low:
                raise StageError("personal_data", f"raw local-store identifier at {path}")


# Words that label or grade a counterparty. The profile never carries them.
_BANNED = {"scam", "scams", "scammer", "scammers", "scammy", "fraud", "frauds", "fraudster",
           "fraudulent", "score", "scores", "scored", "scoring", "rating", "ratings",
           "reputation", "reputational", "blacklist", "blacklisted", "blocklist", "trustworthiness"}


def stage_wording(rec):
    for path, s in _strings(rec):
        for tok in re.split(r"[^a-z]+", s.lower()):
            if tok in _BANNED:
                raise StageError("wording", f"prohibited word {tok!r} at {path}")


def _ref(d):
    return {"type": "deal-record", "digest_alg": "SHA-256", "digest": d}


def check_chain(records):
    """Profile section 6 rules over one deal, in seq order. Raises StageError('chain')."""
    def fail(i, msg):
        raise StageError("chain", f"seq position {i + 1}: {msg}")

    digests = [record_digest(r) for r in records]
    by_digest = {d: i for i, d in enumerate(digests)}
    blk0 = records[0]["x-deal-v0"]
    if blk0["record_type"] != "baseline":
        fail(0, "the first record of a deal must be the baseline")
    allowed_rels = {"evidence": {"about"}, "detail_change": {"source"}, "verdict": {"checks"},
                    "approval": {"approves"}, "action": {"authorized_by"}, "outcome": {"observes"},
                    "close": {"outcome"}, "disclosure": {"authorized_by"}}
    # Absent allowed = no restriction; present and empty = nothing allowed.
    allowed = records[0]["body"]["intent"].get("allowed")
    used_approvals, verdict_for_check = set(), set()
    first_answer = {}  # verdict index -> index of its first approval
    last_outcome = None
    unchecked = 0
    closed_final = False
    for i, rec in enumerate(records):
        b, body = rec["x-deal-v0"], rec["body"]
        t = b["record_type"]
        if closed_final:
            fail(i, "record after a close whose outcome is completed or mismatch (close is terminal)")
        if b["deal_id"] != blk0["deal_id"]:
            fail(i, "deal_id differs from the baseline's")
        if b["seq"] != i + 1:
            kind = "regression" if b["seq"] <= i else "gap"
            fail(i, f"seq {kind}: expected {i + 1}, got {b['seq']}")
        if i > 0:
            if t == "baseline":
                fail(i, "a deal has exactly one baseline")
            if b.get("prev") != _ref(digests[i - 1]):
                fail(i, "prev is not the digest of the previous record")
            if b.get("baseline_ref") != _ref(digests[0]):
                fail(i, "baseline_ref is not the digest of the baseline")
            if b["at"] < records[i - 1]["x-deal-v0"]["at"]:
                fail(i, "at is earlier than the previous record's")
        refs = b.get("refs", [])
        by_rel = {}
        for r in refs:
            if r["rel"] not in allowed_rels.get(t, set()):
                fail(i, f"rel {r['rel']!r} is not allowed on a {t} record")
            j = by_digest.get(r["digest"])
            if j is None or j >= i:
                fail(i, f"ref {r['rel']} does not name an earlier record of this deal")
            by_rel.setdefault(r["rel"], []).append(j)

        def one(rel, types, required=True):
            js = by_rel.get(rel, [])
            if len(js) > 1 or (required and not js):
                fail(i, f"a {t} record carries exactly one {rel!r} ref" if required
                     else f"a {t} record carries at most one {rel!r} ref")
            if js and records[js[0]]["x-deal-v0"]["record_type"] not in types:
                fail(i, f"{rel!r} must point at a {' or '.join(types)} record")
            return js[0] if js else None

        def authorized_check(ja, action, what):
            """Section 6, rule 5: the approval proceeds, is unused, is the
            verdict's first answer, and its check is of this action with no
            change of details since. Returns the check."""
            appr = records[ja]
            if not appr["body"]["proceed"]:
                fail(i, "the referenced approval did not approve proceeding")
            if ja in used_approvals:
                fail(i, "an approval authorizes at most one action or disclosure")
            used_approvals.add(ja)
            jv = by_digest[appr["x-deal-v0"]["refs"][0]["digest"]]
            if first_answer.get(jv) != ja:
                fail(i, f"a{'n' if what == 'action' else ''} {what} is held to the verdict's first answer; a later answer needs a new check")
            jc = by_digest[records[jv]["x-deal-v0"]["refs"][0]["digest"]]
            chk = records[jc]
            if chk["body"]["action"] != action:
                fail(i, f"the {what} differs from the one checked")
            for k in range(jc + 1, i):
                kb = records[k]["x-deal-v0"]
                identity = kb["record_type"] in ("message", "evidence") and (
                    "counterparty" in kb or "counterparty_facts" in records[k]["body"])
                if kb["record_type"] == "detail_change" or identity:
                    fail(i, f"details changed after the check; the {what} needs a new check")
            return chk

        if t == "intent":
            if "allowed" in body:
                allowed = list(body["allowed"])
        elif t == "evidence":
            one("about", ("claim", "baseline"))
        elif t == "detail_change":
            one("source", ("message", "evidence"), required=False)
            kinds = set(b.get("counterparty", {}).get("ids", {}))
            want = {c for c in body["changed"] if c in IDENTIFIER_KINDS}
            if kinds != want:
                fail(i, "counterparty.ids kinds must equal the identifier kinds listed in changed")
        elif t == "verdict":
            j = one("checks", ("check",))
            if j in verdict_for_check:
                fail(i, "a check has at most one verdict")
            verdict_for_check.add(j)
            if body["pack_id"] != records[j]["body"]["pack_id"]:
                fail(i, "verdict pack_id differs from the check's")
        elif t == "approval":
            j = one("approves", ("verdict",))
            first_answer.setdefault(j, i)
            v = records[j]["body"]
            if body["approver"] == "standing_intent":
                chk = records[by_digest[records[j]["x-deal-v0"]["refs"][0]["digest"]]]["body"]
                if v["result"] != "pass":
                    fail(i, "a standing-intent approval needs a passing verdict; a pause needs the user's answer")
                if allowed is not None and chk["action"] not in allowed:
                    fail(i, "standing-intent approval for an action the intent does not allow")
            elif v["result"] == "pause" and body["choice"] not in v["options"]:
                fail(i, "the user's choice is not one of the verdict's options")
        elif t == "disclosure":
            actions = {DISCLOSURE_ACTION[f["class"]] for f in body["fields"]}
            if len(actions) != 1:
                fail(i, "contact details and credentials are separate disclosures, each covered by its own check")
            if body["authority"] == "none":
                if "authorized_by" in by_rel:
                    fail(i, "a disclosure with authority none carries no authorized_by")
            else:
                if "authorized_by" not in by_rel:
                    fail(i, "a disclosure with authority approval references that approval (authorized_by)")
                authorized_check(one("authorized_by", ("approval",)), actions.pop(), "disclosure")
        elif t == "action":
            if "authorized_by" not in by_rel:
                fail(i, "an action must reference the sealed approval that authorized it (authorized_by)")
            chk = authorized_check(one("authorized_by", ("approval",)), body["action"], "action")
            cb = chk["body"]
            if "amount_minor" in body and "amount_minor" in cb and body["amount_minor"] != cb["amount_minor"]:
                fail(i, "the amount differs from the one checked")
            if "currency" in body and "currency" in cb and body["currency"] != cb["currency"]:
                fail(i, "the currency differs from the one checked")
            crail = cb.get("recourse", {}).get("rail")
            if "rail" in body and crail and body["rail"] != crail:
                fail(i, "the payment rail differs from the one checked")
            p_act = b.get("counterparty", {}).get("ids", {}).get("payee")
            p_chk = chk["x-deal-v0"].get("counterparty", {}).get("ids", {}).get("payee")
            if p_act and p_chk and p_act != p_chk:
                fail(i, "the payee fingerprint differs from the one checked")
        elif t == "outcome":
            one("observes", ("action",), required=False)
            if body["status"] == "unchecked_action":
                unchecked += 1
            last_outcome = i
        elif t == "close":
            if last_outcome is None:
                if by_rel.get("outcome"):
                    fail(i, "close references an outcome but none exists")
                if body["outcome"] != "open":
                    fail(i, "without an outcome record a close can only be open")
            else:
                j = one("outcome", ("outcome",))
                if j != last_outcome:
                    fail(i, "close must reference the latest outcome record")
                if body["outcome"] != records[j]["body"]["outcome"]:
                    fail(i, "close outcome differs from the referenced outcome")
            if body["unchecked_actions"] != unchecked:
                fail(i, f"unchecked_actions must count unchecked_action outcomes ({unchecked})")
            closed_final = body["outcome"] != "open"


def check_record(rec, local_store_values=()):
    stage_canonicalization(rec)
    stage_personal_data(rec, local_store_values)
    stage_wording(rec)
    stage_schema(rec)


# ---------------------------------------------------------------------------
# Fixture run
# ---------------------------------------------------------------------------

def load(p):
    return json.loads(Path(p).read_text(encoding="utf-8"))


def local_store_values():
    v = load(FIX / "fingerprint-vectors.json")
    # Identifier raw values, plus any committed text long enough not to collide with tokens.
    return [x["raw"] for x in v["vectors"]] + [x["text"] for x in v["commitments"] if len(x["text"]) >= 24]


def run_fixtures() -> int:
    bad = 0
    vec = load(FIX / "fingerprint-vectors.json")
    secret = bytes.fromhex(vec["store_secret_hex"])
    dk = deal_key(secret, vec["deal_id"])
    if dk.hex() != vec["deal_key_hex"]:
        print("FAIL fingerprint-vectors: deal_key mismatch"); bad += 1
    for x in vec["vectors"]:
        n = normalize(x["kind"], x["raw"])
        fp = fingerprint(dk, x["kind"], x["raw"])
        ok = n == x["normalized"] and fp == x["fp"]
        bad += not ok
        print(f"{'ok  ' if ok else 'FAIL'} fp {x['kind']:<13} {x['normalized']:<28} {fp[:16]}…")
    for x in vec["commitments"]:
        ok = commitment(x["nonce"], x["text"]) == x["commitment"]
        bad += not ok
        print(f"{'ok  ' if ok else 'FAIL'} commitment {x['label']:<18} {x['commitment'][:16]}…")
    store = local_store_values()

    positives = sorted((FIX / "positive").glob("*.json"))
    chain = []
    for p in positives:
        f = load(p)
        rec = f["record"]
        try:
            check_record(rec, store)
            d = record_digest(rec)
            if d != f["expected_digest"]:
                raise StageError("digest", f"recomputed {d} != expected {f['expected_digest']}")
            chain.append(rec)
            check_chain(chain)
            print(f"ok   {p.name:<32} {rec['x-deal-v0']['record_type']:<13} jcs-sha256 {d}")
        except StageError as e:
            bad += 1
            print(f"FAIL {p.name}: {e}")
    types = {load(p)["record"]["x-deal-v0"]["record_type"] for p in positives}
    missing = set(RECORD_TYPES) - types
    if missing:
        bad += 1
        print(f"FAIL positives do not cover record types: {sorted(missing)}")
    else:
        print(f"ok   positives cover all {len(RECORD_TYPES)} record types in one deal ({len(positives)} records)")

    for p in sorted((FIX / "negative").glob("*.json")):
        f = load(p)
        prefix = [load(FIX / "positive" / n)["record"] for n in f.get("chain_prefix", [])]
        prefix += f.get("chain_between", [])
        got = None
        try:
            check_record(f["record"], store)
            if "claimed_digest" in f and f["claimed_digest"] != record_digest(f["record"]):
                raise StageError("digest", "claimed digest is not SHA-256 over the JCS bytes")
            check_chain(prefix + [f["record"]])
        except StageError as e:
            got = e
        ok = got is not None and got.stage == f["expected_stage"] and f["expected_error_contains"] in str(got)
        bad += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {p.name:<40} rejected at {got.stage if got else 'NOT REJECTED'}: "
              f"{str(got)[len(got.stage) + 3:] if got else ''}")
    return bad


def check_files(paths) -> int:
    bad = 0
    for p in paths:
        data = load(p)
        recs = data if isinstance(data, list) else [data]
        recs = [r["record"] if isinstance(r, dict) and "record" in r else r for r in recs]
        try:
            for r in recs:
                check_record(r)
            if isinstance(data, list):
                check_chain(recs)
            print(f"ok   {p}: " + ", ".join(record_digest(r) for r in recs))
        except StageError as e:
            bad += 1
            print(f"FAIL {p}: {e}")
    return bad


# ---------------------------------------------------------------------------
# Fixture generation (deterministic; the jet-ski payee-switch deal)
# ---------------------------------------------------------------------------

def regen(pack_path: Path | None):
    secret = hashlib.sha256(b"x-deal-v0 fixture store secret: public, test only").digest()
    deal_id = "deal-3f9c2a7be4d15068"
    dk = deal_key(secret, deal_id)

    def nonce(label):
        return sha256_hex(b"x-deal-v0 fixture nonce/" + label.encode())

    raw = {  # the local store: never in a record
        ("name", "first"): "Coastal Jet Rentals LLC",
        ("payee", "first"): "Coastal Jet Rentals LLC",
        ("domain", "first"): "https://Book.CoastalJetRentals.example:443/quote?id=7",
        ("phone", "first"): "(555) 010-2000",
        ("phone", "second"): "+1 555-010-2044",
        ("payee", "second"): "M. Torres",
    }
    fps = {k: fingerprint(dk, k[0], v) for k, v in raw.items()}
    texts = {
        "intent-1": "rent me 2 jet skis Saturday, under $450 all in",
        "message-1": "Hi! 2 skis for Saturday are $400 for the day, $200 deposit to hold them.",
        "intent-2": "go ahead and share my number so they can text me pickup details",
        "message-2": "Our office line is down. Text this number, and send the $200 deposit to "
                     "M. Torres on Zelle today to keep the booking.",
        "card-1": "Payee changed since first contact (Coastal Jet Rentals LLC → M. Torres, Zelle) · "
                  "Zelle to a business = no card protection · Site registered 3 weeks ago · "
                  "unverified: has 2 jet skis available Saturday · [Hold] [Call the number I found] [Pay anyway]",
        "said-1": "Hold",
        # What the agent told the new number, kept on the device only.
        "disclosed-phone": "(555) 010-2077",
        "disclosed-pickup": "lakeside marina, slip 14, by the fuel dock",
    }
    com = {k: commitment(nonce(k), v) for k, v in texts.items()}
    pack_digest = None
    if pack_path and pack_path.exists():
        pack_digest = record_digest(json.loads(pack_path.read_text()))
    pack_id = "capsule/marketplace-rentals-safety/0.1.0"

    records, names = [], []
    clock = iter(["2026-10-01T16:00:00Z", "2026-10-01T16:02:10Z", "2026-10-01T16:02:40Z",
                  "2026-10-01T16:05:00Z", "2026-10-01T16:20:00Z", "2026-10-01T16:20:05Z",
                  "2026-10-01T16:20:06Z", "2026-10-01T16:20:06Z", "2026-10-01T16:20:30Z",
                  "2026-10-01T18:41:00Z", "2026-10-01T18:41:20Z", "2026-10-01T18:43:00Z",
                  "2026-10-01T18:43:01Z", "2026-10-01T18:44:30Z", "2026-10-01T18:52:00Z", "2026-10-04T09:00:00Z",
                  "2026-10-04T09:00:05Z"])

    def cp(*keys):
        return {"fp_alg": "hmac-sha256-deal-key", "ids": {k[0]: fps[k] for k in keys}}

    def add(name, rtype, body, channel=None, counterparty=None, refs=None):
        blk = {"profile": "x-deal-v0", "canonicalization": "jcs", "deal_id": deal_id,
               "record_type": rtype, "seq": len(records) + 1, "at": next(clock)}
        if records:
            blk["prev"] = _ref(record_digest(records[-1]))
            blk["baseline_ref"] = _ref(record_digest(records[0]))
        if channel:
            blk["channel"] = channel
        if counterparty:
            blk["counterparty"] = counterparty
        if refs:
            blk["refs"] = [{"rel": rel, **_ref(record_digest(records[j]))} for rel, j in refs]
        records.append({"x-deal-v0": blk, "body": body})
        names.append(f"{len(records):02d}-{name}.json")
        return len(records) - 1

    b = add("baseline", "baseline", {
        "deal_type": "rental", "demo": True,
        "intent": {"verbatim_commitment": com["intent-1"],
                   "asked": {"item": "jet ski", "quantity": 2, "when": "2026-10-03"},
                   "max_total_minor": 45000, "allowed": ["pay"]},
        "terms": {"item": "jet ski", "quantity": 2, "price_minor": 40000, "deposit_minor": 20000,
                  "currency": "USD", "when": "2026-10-03", "place": "lakeside marina"},
        "recourse": {"rail": "card", "refundable": True},
        "counterparty_facts": {"domain_age_days": 21},
    }, channel="marketplace", counterparty=cp(("payee", "first"), ("name", "first"), ("domain", "first"), ("phone", "first")))
    add("message-quote", "message", {"from": "counterparty", "content_commitment": com["message-1"]}, channel="marketplace")
    c = add("claim", "claim", {"text": "has 2 jet skis available Saturday", "source": "counterparty"})
    add("evidence", "evidence", {"source": "listing_photo", "verified": False}, refs=[("about", c)])
    add("intent", "intent", {"verbatim_commitment": com["intent-2"], "allowed": ["pay", "share_contact"]})
    k1 = add("check-share-contact", "check", {"action": "share_contact", "pack_id": pack_id,
                                              **({"pack_digest": pack_digest} if pack_digest else {})})
    v1 = add("verdict-pass", "verdict", {"result": "pass", "pack_id": pack_id, "differences": [], "options": [],
                                         "judge": {"kind": "rules"}}, refs=[("checks", k1)])
    a1 = add("approval-standing", "approval", {"choice": "proceed", "proceed": True, "approver": "standing_intent"},
             refs=[("approves", v1)])
    add("action-share-contact", "action", {"action": "share_contact"}, refs=[("authorized_by", a1)])
    m2 = add("message-switch", "message", {"from": "counterparty", "content_commitment": com["message-2"]},
             channel="sms", counterparty=cp(("phone", "second")))
    add("detail-change-payee", "detail_change", {"source": "counterparty", "changed": ["payee", "phone", "rail", "refundable"],
                                                 "recourse": {"rail": "zelle", "refundable": False}},
        counterparty=cp(("payee", "second"), ("phone", "second")), refs=[("source", m2)])
    k2 = add("check-pay", "check", {"action": "pay", "amount_minor": 20000, "currency": "USD", "seen_item": False,
                                    "recourse": {"rail": "zelle", "refundable": False}, "pack_id": pack_id,
                                    **({"pack_digest": pack_digest} if pack_digest else {})},
             counterparty=cp(("payee", "second")))
    v2 = add("verdict-pause", "verdict", {
        "result": "pause", "pack_id": pack_id,
        "differences": [{"question": "who", "rule": "payee_or_contact_changed", "field": "payee"},
                        {"question": "who", "rule": "payee_or_contact_changed", "field": "phone"},
                        {"question": "recourse", "rule": "business_zelle_or_wire", "field": "rail"},
                        {"question": "safety", "rule": "domain_recent", "field": "domain"}],
        "unverified": ["has 2 jet skis available Saturday"], "notes": ["not_refundable"],
        "options": ["hold", "verify_contact", "proceed"], "card_commitment": com["card-1"],
        "judge": {"kind": "rules"}}, refs=[("checks", k2)])
    a2 = add("approval-hold", "approval", {"choice": "hold", "proceed": False, "approver": "user",
                                           "said_commitment": com["said-1"]}, refs=[("approves", v2)])
    # After the Hold, the agent texts the new number the user's phone and the
    # pickup spot anyway: no check covers it, so the record says authority none.
    add("disclosure", "disclosure", {"to": "counterparty", "authority": "none", "rule": "answer_was_not_proceed",
                                     "fields": [{"class": "phone", "value_commitment": com["disclosed-phone"]},
                                                {"class": "pickup_location", "value_commitment": com["disclosed-pickup"]}]},
        channel="sms", counterparty=cp(("phone", "second")))
    o = add("outcome", "outcome", {"status": "not_received", "outcome": "mismatch",
                                   "differences": [{"question": "delivered", "rule": "not_delivered"}]})
    add("close", "close", {"outcome": "mismatch", "unchecked_actions": 0,
                           "differences": [{"question": "delivered", "rule": "not_delivered"}]}, refs=[("outcome", o)])
    assert a2 and b == 0

    for d in (FIX / "positive", FIX / "negative"):
        d.mkdir(parents=True, exist_ok=True)
        for old in d.glob("*.json"):
            old.unlink()

    def dump(path, obj):
        path.write_text(json.dumps(obj, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")

    stories = {
        "baseline": "First contact sealed: rental of 2 jet skis Saturday from a business on the marketplace; "
                    "who is four fingerprints, terms in minor units, card rail refundable; the user's words by commitment.",
        "message": "A counterparty message; its text stays local, the record holds a salted commitment.",
        "claim": "The counterparty says the skis are available; recorded as a claim, not a fact.",
        "evidence": "The listing photo does not establish the claim; it stays unverified.",
        "intent": "The user widens what the agent may do: sharing the user's own number is now allowed.",
        "check": "The agent runs the deal check before a point of no return; names the pack it runs.",
        "verdict": "The deal check's answer to the check it references.",
        "approval": "What authorizes the next step: a standing intent on a pass, or the user's own answer.",
        "action": "The step actually taken, citing the sealed approval.",
        "detail_change": "The counterparty switches payee, phone and rail after first contact (fingerprints only).",
        "outcome": "Saturday passes; nothing was delivered as agreed.",
        "close": "The deal closes on mismatch, citing the outcome.",
        "disclosure": "After the Hold, the agent texts the user's phone and the pickup spot to the new number with no "
                      "approval covering it: the record names the classes, the recipient and authority none, "
                      "and commits to the values without carrying them.",
    }
    digests = []
    for n, r in zip(names, records):
        d = record_digest(r)
        digests.append((n, d))
        dump(FIX / "positive" / n, {"fixture": n, "expect": "valid",
                                    "story": stories[r["x-deal-v0"]["record_type"]],
                                    "record": r, "expected_digest": d})
    (FIX / "expected-digests.txt").write_text(
        "# SHA-256 over the RFC 8785 (JCS) bytes of each positive record (the `record` member only)\n"
        + "".join(f"{d}  positive/{n}\n" for n, d in digests), encoding="utf-8")

    vec = {"note": "TEST-ONLY. Simulates a device's local store: the secret and raw values below never appear in a record. "
                   "All names, numbers and domains are fictional (555-01xx, .example).",
           "store_secret_hex": secret.hex(), "deal_id": deal_id, "deal_key_hex": dk.hex(),
           "vectors": [{"kind": k[0], "raw": v, "normalized": normalize(k[0], v), "fp": fps[k]} for k, v in raw.items()],
           "commitments": [{"label": k, "nonce": nonce(k), "text": v, "commitment": com[k]} for k, v in texts.items()]}
    extra = [("email", "  Bookings@CoastalJetRentals.example "), ("relay_address", " Reply-7f3@Relay.Marketplace.example"),
             ("profile_id", " mkt:seller:88213 "), ("name", "The Coastal Jet Rentals, Inc."),
             ("payee", "+1 (555) 010-2044")]
    for k, v in extra:
        vec["vectors"].append({"kind": k, "raw": v, "normalized": normalize(k, v), "fp": fingerprint(dk, k, v)})
    dump(FIX / "fingerprint-vectors.json", vec)

    # --- negatives -------------------------------------------------------------------------
    import copy

    def neg(name, stage, contains, record, prefix_upto, desc, **extra_fields):
        dump(FIX / "negative" / f"{name}.json", {
            "fixture": name, "expect": "invalid", "description": desc, "expected_stage": stage,
            "expected_error_contains": contains, "chain_prefix": names[:prefix_upto], "record": record,
            **extra_fields})

    r = copy.deepcopy(records[2]); r["body"]["text"] = "has 2 jet skis, call (555) 010-2044 to confirm"
    neg("neg-raw-phone", "personal_data", "raw phone", r, 2, "A raw phone number inside a claim.")

    r = copy.deepcopy(records[2]); r["x-deal-v0"]["seq"] = 5
    neg("neg-seq-gap", "chain", "seq gap", r, 2, "seq jumps from 2 to 5.")

    r = copy.deepcopy(records[2]); r["x-deal-v0"]["seq"] = 2
    neg("neg-seq-regression", "chain", "seq regression", r, 2, "seq repeats 2 after 2.")

    r = copy.deepcopy(records[8]); del r["x-deal-v0"]["refs"]
    neg("neg-action-without-approval", "chain", "must reference the sealed approval", r, 8,
        "The share_contact action carries no authorized_by ref.")

    r = copy.deepcopy(records[8]); r["x-deal-v0"]["refs"] = [{"rel": "authorized_by", **_ref(record_digest(records[13]))}]
    r["body"] = {"action": "pay", "amount_minor": 20000, "currency": "USD", "rail": "zelle"}
    r["x-deal-v0"]["seq"] = 15; r["x-deal-v0"]["at"] = "2026-10-01T18:45:00Z"
    r["x-deal-v0"]["prev"] = _ref(record_digest(records[13]))
    neg("neg-action-on-hold", "chain", "did not approve proceeding", r, 14,
        "A pay action citing the user's Hold answer.")

    # The user holds, then answers the same verdict again with "Pay anyway": the second answer is
    # sealed (it records what the user chose), but an action is held to the verdict's first answer.
    again = copy.deepcopy(records[13])
    again["body"] = {"choice": "proceed", "proceed": True, "approver": "user",
                     "said_commitment": commitment(nonce("said-2"), "Pay anyway")}
    again["x-deal-v0"].update(seq=15, at="2026-10-01T18:44:50Z", prev=_ref(record_digest(records[13])))
    r = {"x-deal-v0": {**copy.deepcopy(records[8]["x-deal-v0"]), "seq": 16, "at": "2026-10-01T18:45:00Z",
                       "prev": _ref(record_digest(again)), "counterparty": cp(("payee", "second")),
                       "refs": [{"rel": "authorized_by", **_ref(record_digest(again))}]},
         "body": {"action": "pay", "amount_minor": 20000, "currency": "USD", "rail": "zelle"}}
    neg("neg-action-on-second-answer", "chain", "first answer", r, 14,
        "After Hold, a second answer to the same verdict (Pay anyway) is cited by a pay action.",
        chain_between=[again])

    r = copy.deepcopy(records[14]); r["body"]["fields"][0]["class_note"] = "texted (555) 010-2077"
    neg("neg-disclosure-raw-value", "personal_data", "raw phone", r, 14,
        "A disclosure that carries the disclosed phone number itself.")

    r = copy.deepcopy(records[14]); r["body"] = {"to": "counterparty", "authority": "approval",
                                                 "fields": [{"class": "phone", "value_commitment": com["disclosed-phone"]}]}
    r["x-deal-v0"]["refs"] = [{"rel": "authorized_by", **_ref(record_digest(records[7]))}]
    neg("neg-disclosure-reused-approval", "chain", "at most one action or disclosure", r, 14,
        "A disclosure citing the standing approval that already covered the share_contact action.")

    r = copy.deepcopy(records[2]); r["x-deal-v0"]["canonicalization"] = "jcs-n"
    neg("neg-wrong-canonicalization", "canonicalization", "must be exactly", r, 2,
        "The block declares the withdrawn jcs-n construction.")

    r = copy.deepcopy(records[2]); r["body"]["text"] = "has 2 jet skis — lakeside dock"
    non_jcs = sha256_hex(json.dumps(r, sort_keys=True, separators=(",", ":")).encode())
    neg("neg-non-jcs-digest", "digest", "not SHA-256 over the JCS", r, 2,
        "claimed_digest was computed over json.dumps(sort_keys=True) bytes (\\u escapes), not JCS.",
        claimed_digest=non_jcs)

    r = copy.deepcopy(records[2]); r["x-deal-v0"]["record_type"] = "payment"
    neg("neg-unknown-record-type", "schema", "x-deal-v0/record_type", r, 2, "record_type outside the closed set.")

    r = copy.deepcopy(records[12]); r["body"]["differences"][2]["rule"] = "likely_scam"
    neg("neg-wording-label", "wording", "prohibited word", r, 12, "A difference rule that labels the counterparty.")

    r = copy.deepcopy(records[2]); r["body"]["text"] = "seller rating 4.9 on the marketplace"
    neg("neg-wording-grade", "wording", "prohibited word", r, 2, "A claim that carries a grade of the counterparty.")

    r = copy.deepcopy(records[2]); r["body"]["text"] = "has 2 jet skis, ask for M. Torres"
    neg("neg-raw-payee-name", "personal_data", "raw local-store identifier", r, 2,
        "A raw payee name from the local store inside a claim.")

    r = copy.deepcopy(records[1]); r["x-deal-v0"]["counterparty"] = {"fp_alg": "hmac-sha256-deal-key", "ids": {"phone": "+15550102044"}}
    neg("neg-unfingerprinted-id", "personal_data", "raw phone", r, 1,
        "A counterparty id carried as the raw E.164 value instead of a fingerprint.")

    print(f"regenerated {len(records)} positives, negatives and vectors under {FIX}")


def main(argv):
    if "--regen" in argv:
        i = argv.index("--pack") if "--pack" in argv else -1
        pack = Path(argv[i + 1]) if i >= 0 else HERE.parent.parent / "safety-pack-marketplace-rentals-v0" / "pack.json"
        regen(pack)
        return 0
    fails = jcs_selftest()
    for f in fails:
        print("FAIL jcs", f)
    print(f"{'ok  ' if not fails else 'FAIL'} RFC 8785 self-test (Appendix B numbers, 3.2.2 example, 3.2.3 sort order)")
    if "--selftest" in argv:
        return 1 if fails else 0
    _validator()
    print(f"schema stage: {SCHEMA_MODE}")
    files = [a for a in argv if not a.startswith("--")]
    bad = len(fails) + (check_files(files) if files else run_fixtures())
    print("ALL OK" if not bad else f"{bad} FAILURE(S)")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
