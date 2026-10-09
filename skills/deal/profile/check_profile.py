#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Checker for the x-deal-v0 deal record profile (skill-local, not registered).

  python3 check_profile.py            validate every fixture, recompute every JCS digest
  python3 check_profile.py --selftest RFC 8785 (JCS) self-test only
  python3 check_profile.py --regen    rewrite fixtures/ deterministically (maintainers only)
  python3 check_profile.py FILE...    check deal records (one JSON record per file, or a
                                      JSON array = one deal in seq order)
  python3 check_profile.py --openings=OPENINGS.json FILE
                                      also check a copy's claim openings (its claim_openings:
                                      record digest, index, nonce and words) against the
                                      commitments FILE's records carry

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
# The typed action records (one schema per type, sharing record-common-v0). A chain
# sealed in them carries x-deal-v0 evidence records and typed records side by side.
RECORDS_DIR = HERE / "records"
# The example materiality predicate the typed fixtures' evaluations name, as capsulectl's
# tests configure it.
EXAMPLE_PREDICATE = HERE / "materiality-predicate" / "neutral.json"
TYPED_TYPES = {"task-authority/v0": "task_authority", "proposed-action/v0": "check",
               "action-evaluation/v0": "verdict", "action-approval/v0": "approval",
               "action-record/v0": "action", "action-outcome/v0": "outcome", "action-report/v0": "report"}
FIX = HERE / "fixtures"

RECORD_TYPES = ["intent", "baseline", "message", "claim", "evidence", "detail_change",
                "check", "verdict", "approval", "action", "outcome", "close", "disclosure"]
# The point of no return whose check covers a disclosed field of each class.
DISCLOSURE_ACTION = {c: "share_contact" for c in ("name", "phone", "email", "home_address", "address",
                                                  "pickup_location", "other_contact")}
DISCLOSURE_ACTION.update({c: "share_credentials" for c in ("credential", "verification_code",
                                                           "payment_card", "id_document")})
# The classes that give a place: a seller gives one out only for an accepted offer.
ADDRESS_CLASSES = {"home_address", "address", "pickup_location"}
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


def is_typed(rec):
    return isinstance(rec, dict) and isinstance(rec.get("type"), str) and "x-deal-v0" not in rec


def record_type_of(rec):
    return rec["type"] if is_typed(rec) else rec["x-deal-v0"]["record_type"]


def typed_view(rec):
    """A typed record as the section 6 rules read it: its header as a block,
    and its body under the names those rules use (an evaluation's disposition
    as result and findings as differences, an approval's authority as its
    approver, an outcome's attempted action as unchecked). Digests are always
    computed over the record itself, never over this view."""
    t = rec["type"]
    body = dict(rec["body"])
    blk = {"profile": t, "canonicalization": rec["canonicalization"], "deal_id": rec["chain_id"],
           "record_type": TYPED_TYPES[t], "seq": rec["seq"], "at": rec["at"], "typed": t}
    if "prev" in rec:
        blk["prev"] = _ref(rec["prev"]["digest"])
    if "chain_root" in rec:
        blk["baseline_ref"] = _ref(rec["chain_root"]["digest"])
    if "refs" in rec:
        blk["refs"] = [{"rel": r["rel"], **_ref(r["digest"])} for r in rec["refs"]]
    if "counterparty" in body:
        blk["counterparty"] = body.pop("counterparty")
    if t == "action-evaluation/v0":
        body["result"] = {"DO": "pass", "ASK": "pause", "DENY": "deny"}[body["disposition"]]
        body["differences"] = body["findings"]
        if "rendering_commitment" in body:
            body["card_commitment"] = body["rendering_commitment"]
    elif t == "action-approval/v0":
        a = body["authority"]
        if a in ("user_approval", "card_answer"):
            body["approver"] = {"user_approval": "user", "card_answer": "agent_card"}[a]
        else:
            blk["record_type"] = a  # platform_approval, policy_change, one_shot_override
    elif t == "action-outcome/v0":
        body["differences"] = body["findings"]
        if "attempted" in body:
            body["unchecked"] = body["attempted"]
    return {"x-deal-v0": blk, "body": body}


def stage_canonicalization(rec):
    blk = rec if is_typed(rec) else (rec.get("x-deal-v0") if isinstance(rec, dict) else None)
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


_TYPED_VALIDATORS = {}


def _typed_validator(type_name):
    if type_name not in _TYPED_VALIDATORS:
        try:
            from jsonschema import Draft202012Validator
            from referencing import Registry, Resource
        except ImportError:
            _TYPED_VALIDATORS[type_name] = None
            return None
        registry = Registry()
        for p in RECORDS_DIR.glob("*.schema.json"):
            doc = json.loads(p.read_text())
            registry = registry.with_resource(doc["$id"], Resource.from_contents(doc))
        doc = json.loads((RECORDS_DIR / (type_name.replace("/", "-") + ".schema.json")).read_text())
        _TYPED_VALIDATORS[type_name] = Draft202012Validator(doc, registry=registry)
    return _TYPED_VALIDATORS[type_name]


def stage_schema(rec):
    if is_typed(rec):
        if rec["type"] not in TYPED_TYPES:
            raise StageError("schema", f"type: {rec['type']!r} is not a typed action record")
        val = _typed_validator(rec["type"])
        if val is not None:
            errs = sorted(val.iter_errors(rec), key=lambda e: list(e.absolute_path))
            if errs:
                e = errs[0]
                raise StageError("schema", f"{'/'.join(map(str, e.absolute_path)) or '$'}: {e.message[:200]}")
            return
        for k in ("type", "canonicalization", "chain_id", "seq", "at", "prev", "chain_root", "body"):
            if k not in rec:
                raise StageError("schema", f"{k} is required")
        return
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
    records = [typed_view(r) if is_typed(r) else r for r in records]
    # A chain sealed in the typed action records opens with the baseline and then
    # the user's task authority; its authority steps are typed records.
    typed_chain = len(records) > 1 and records[1]["x-deal-v0"].get("typed") == "task-authority/v0"
    task_idx = 1 if typed_chain else 0
    task_at_verdict, platform_for_verdict = {}, {}
    blk0 = records[0]["x-deal-v0"]
    if blk0["record_type"] != "baseline":
        fail(0, "the first record of a deal must be the baseline")
    allowed_rels = {"evidence": {"about", "confirms"}, "detail_change": {"source"}, "verdict": {"checks"},
                    "approval": {"approves"}, "action": {"authorized_by", "reverses"}, "outcome": {"observes"},
                    "close": {"outcome"}, "disclosure": {"authorized_by"},
                    "task_authority": {"source", "approves"}, "platform_approval": set(), "policy_change": set(),
                    "check": {"supersedes"}, "counterparty_acceptance": set()}
    # The user's limits in force. Absent allowed = no restriction; present and
    # empty = nothing allowed. An intent may narrow them; only the user's
    # confirm_limits answer to an intent that asks for more puts a new version
    # in force (section 6).
    allowed = records[0]["body"]["intent"].get("allowed")
    max_total = records[0]["body"]["intent"].get("max_total_minor")
    # The side of the deal the user is on: set when it opens (absent = buyer), never changed.
    party_role = records[0]["body"]["intent"].get("party_role", "buyer")
    # The floor in force, as the bounds_commitment that stated it: the floor itself is never in a
    # record, so it cannot be compared. After an intent states a new floor the producer applies it
    # only if it is higher, which this checker cannot see: the floor in force is then unknown
    # (bounds_known False) until a confirmation states it again.
    bounds, bounds_known = records[0]["body"]["intent"].get("bounds_commitment"), True

    def limits_in_force():
        out = {k: v for k, v in (("max_total_minor", max_total), ("allowed", allowed)) if v is not None}
        if bounds_known and bounds is not None:
            out["bounds_commitment"] = bounds
        return out

    def without_unknown_bounds(d):
        return d if bounds_known else {k: v for k, v in d.items() if k != "bounds_commitment"}
    confirmed_intents = set()
    used_approvals, verdict_for_check = set(), set()
    reversed_actions = set()
    first_answer = {}  # verdict index -> index of its first approval
    last_outcome = None
    unchecked = 0
    # A seller's offers: each later offer supersedes the one before, and only the
    # latest can be accepted. accepted is the index of the acceptance of the latest
    # offer, None until there is one.
    latest_offer, accepted = None, None
    closed_final = None  # index of the final close, once there is one
    for i, rec in enumerate(records):
        b, body = rec["x-deal-v0"], rec["body"]
        t = b["record_type"]
        if b.get("typed") and not typed_chain:
            fail(i, "a typed record in a chain that did not open with a task authority")
        late_answer = t == "approval" and body.get("choice") == "confirm_limits" and not body.get("proceed")
        if typed_chain and not b.get("typed") and t in ("check", "verdict", "approval", "action", "outcome") and not late_answer:
            fail(i, f"in a chain of typed records a {t} step is a typed record")
        if typed_chain and t == "disclosure" and body.get("authority") == "approval":
            fail(i, "in a chain of typed records an approved disclosure is an action-record/v0")
        if t == "one_shot_override":
            fail(i, "one_shot_override is reserved: no rule in this profile defines an override")
        confirms = [r for r in b.get("refs", []) if r["rel"] == "confirms"]
        if closed_final is not None:
            # Close is terminal: after it, only later evidence that confirms
            # that close (and commits to its digest) may follow.
            if t != "evidence" or len(confirms) != 1 or confirms[0]["digest"] != digests[closed_final]:
                fail(i, "record after a close whose outcome is final (completed, mismatch or not_selected; close is terminal): "
                        "only an evidence record that confirms that close may follow")
        elif confirms:
            fail(i, "a confirms ref names the deal's final close; this deal is not closed")
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

        def offer_check_of(k):
            """The check an authorizing record (a DO evaluation, or an approval of
            one) rests on."""
            if records[k]["x-deal-v0"]["record_type"] == "approval":
                k = by_digest[records[k]["x-deal-v0"]["refs"][0]["digest"]]
            return by_digest[records[k]["x-deal-v0"]["refs"][0]["digest"]]

        def seller_needs_acceptance(what):
            """A seller commits, or gives out a place, only for the latest offer the
            counterparty accepted, with no change of details since."""
            if latest_offer is None:
                fail(i, f"no offer is on record: a seller's {what} rests on an offer the other party accepted")
            if accepted is None:
                fail(i, f"the latest offer has no recorded acceptance: the {what} needs one")
            for k in range(accepted + 1, i):
                kb = records[k]["x-deal-v0"]
                identity = kb["record_type"] in ("message", "evidence") and (
                    "counterparty" in kb or "counterparty_facts" in records[k]["body"])
                if kb["record_type"] == "detail_change" or identity:
                    fail(i, f"details changed after the other party accepted; the {what} needs the offer made and accepted again")

        def authorized_check(ja, action, what):
            """Section 6, rule 5: the approval proceeds, is unused, is the
            verdict's first answer, and its check is of this action with no
            change of details since. Returns the check."""
            appr = records[ja]
            if appr["body"]["choice"] == "confirm_limits":
                fail(i, "a limits confirmation authorizes no action or disclosure; the step needs its own check")
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

        def authorize_typed(jt, action, what):
            """Typed chains (section 9): a DO evaluation authorizes the step
            on the task authority alone; an ASK is answered only by the user's own
            approval (a card answer does not satisfy it). Returns (evaluation,
            approval or None, check)."""
            if records[jt]["x-deal-v0"]["record_type"] == "verdict":
                if records[jt]["body"]["result"] != "pass":
                    fail(i, "only a DO evaluation authorizes on its own; an ASK needs the user's own approval")
                if jt in used_approvals:
                    fail(i, f"an evaluation authorizes at most one action or disclosure")
                used_approvals.add(jt)
                jc = by_digest[records[jt]["x-deal-v0"]["refs"][0]["digest"]]
                chk = records[jc]
                if chk["body"]["action"] != action:
                    fail(i, f"the {what} differs from the one checked")
                if allowed is not None and action not in allowed:
                    fail(i, "the task authority does not allow this action")
                for k in range(jc + 1, i):
                    kb = records[k]["x-deal-v0"]
                    identity = kb["record_type"] in ("message", "evidence") and (
                        "counterparty" in kb or "counterparty_facts" in records[k]["body"])
                    if kb["record_type"] == "detail_change" or identity:
                        fail(i, f"details changed after the check; the {what} needs a new check")
                return jt, None, chk
            if records[jt]["body"].get("approver") != "user":
                fail(i, "the deal check asked (it paused); an ask needs the user's own approval, and a card answer does not satisfy it")
            chk = authorized_check(jt, action, what)
            return by_digest[records[jt]["x-deal-v0"]["refs"][0]["digest"]], jt, chk

        def verify_basis(jv, ja, amount):
            """The executed phase of the check contract: the evaluation relied on
            (evaluation_ref) still valid, and every authority layer in order, each a
            ref to its record: the task authority the evaluation named; on an ASK,
            the user's answer it cites; every platform approval observed for that
            evaluation (scope mismatch exactly when it stated another amount),
            never as the answer; no one_shot_override."""
            v = records[jv]["body"]
            if b["at"] > v["valid_until"]:
                fail(i, f"the evaluation was valid until {v['valid_until']}: a step after that needs a new check")
            if body.get("evaluation_ref", {}).get("digest") != digests[jv]:
                fail(i, "evaluation_ref is not the evaluation the step relied on")
            basis = body.get("authority_basis") or []
            types = [e["type"] for e in basis]
            if "one_shot_override" in types:
                fail(i, "one_shot_override is reserved: no rule in this profile defines an override")
            if not types or types[0] != "task_authority" or types.count("task_authority") != 1:
                fail(i, "authority_basis starts with exactly one task_authority")
            if basis[0]["ref"]["digest"] != v["task_authority_ref"]["digest"]:
                fail(i, "task_authority is not the evaluation's task_authority_ref")
            asa = [e for e in basis if e["type"] == "user_approval"]
            if ja is not None:
                if len(asa) != 1 or asa[0]["ref"]["digest"] != digests[ja]:
                    fail(i, "the user_approval entry is the user's answer the step cites (authorized_by)")
            elif asa:
                fail(i, "a DO evaluation needs no approval: the step rests on the task authority")
            seen = []
            for e in basis:
                if e["type"] != "platform_approval":
                    continue
                jp = by_digest.get(e["ref"]["digest"])
                if jp is None or jp >= i or records[jp]["x-deal-v0"]["record_type"] != "platform_approval":
                    fail(i, "a platform_approval entry names an earlier platform approval")
                pb = records[jp]["body"]
                mismatch = amount is not None and "amount_minor" in pb and pb["amount_minor"] != amount
                if mismatch != (e.get("scope") == "mismatch"):
                    fail(i, "scope mismatch is stated exactly when the platform's approval stated another amount")
                seen.append(jp)
            if sorted(seen) != sorted(platform_for_verdict.get(jv, [])):
                fail(i, "authority_basis lists every platform approval observed for the evaluation, and only those")
            want = ["task_authority"] + (["user_approval"] if ja is not None else []) + ["platform_approval"] * len(seen)
            if types != want:
                fail(i, "authority_basis lists task_authority, then the user's answer, then platform approvals")

        if t == "task_authority":
            # The user's task authority: as asked at the baseline, or a new version
            # the user confirmed in their words (previous_ref names the one replaced).
            if one("source", ("baseline",), required=False) is not None:
                if i != 1:
                    fail(i, "the opening task authority directly follows the baseline")
                intent0 = records[0]["body"]["intent"]
                for k in ("max_total_minor", "allowed"):
                    if body.get(k) != intent0.get(k):
                        fail(i, f"the task authority's {k} is not the one asked at the baseline")
                if ("bounds_commitment" in body) != ("bounds_commitment" in intent0):
                    fail(i, "the task authority states a floor exactly when the baseline does")
            else:
                j = one("approves", ("intent",))
                prop = records[j]["body"]
                if j in confirmed_intents or any(records[k]["x-deal-v0"]["record_type"] == "intent" for k in range(j + 1, i)):
                    fail(i, "a proposal replaced by a later intent, or already confirmed, gives no new task authority")
                if body["previous_ref"]["digest"] != digests[task_idx]:
                    fail(i, "previous_ref is not the task authority in force")
                in_force = {k: v for k, v in (("max_total_minor", max_total), ("allowed", allowed)) if v is not None}
                new = dict(in_force)
                for k in ("max_total_minor", "allowed"):
                    if prop.get(k) is not None:
                        new[k] = prop[k]
                if {k: body[k] for k in ("max_total_minor", "allowed") if k in body} != new:
                    fail(i, "the new task authority is not what the proposal asked for")
                if "bounds_commitment" in prop and body.get("bounds_commitment") != prop["bounds_commitment"]:
                    fail(i, "the new task authority's floor is not the one the proposal stated")
                more = (prop.get("max_total_minor") is not None and max_total is not None
                        and prop["max_total_minor"] > max_total) or \
                       (prop.get("allowed") is not None and allowed is not None
                        and any(a not in allowed for a in prop["allowed"]))
                if not more:
                    fail(i, "the proposal asks for no higher limit and no new action: nothing to confirm")
                confirmed_intents.add(j)
                max_total, allowed = new.get("max_total_minor"), new.get("allowed")
            task_idx = i
        elif t == "platform_approval":
            # An observation of another platform's approval interaction for a proposed
            # action: listed beside that action's evaluation, never as its answer.
            j = by_digest.get(body["proposed_action_ref"]["digest"])
            if j is None or j >= i or records[j]["x-deal-v0"]["record_type"] != "check":
                fail(i, "proposed_action_ref names no earlier proposed action of this chain")
            jv = next((k for k in range(j + 1, i) if records[k]["x-deal-v0"]["record_type"] == "verdict"
                       and records[k]["x-deal-v0"]["refs"][0]["digest"] == digests[j]), None)
            if jv is None:
                fail(i, "a platform approval observation follows the evaluation of its proposed action")
            if body["observed_at"] > b["at"]:
                fail(i, "observed_at is later than the record")
            platform_for_verdict.setdefault(jv, []).append(i)
        elif t == "check" and body.get("action") == "offer":
            if party_role != "seller":
                fail(i, "an offer is made on a deal where the user sells (party_role seller)")
            j = one("supersedes", ("check",), required=latest_offer is not None)
            if j != latest_offer:
                fail(i, "an offer supersedes exactly the latest earlier offer")
            latest_offer, accepted = i, None
        elif t == "check":
            if "supersedes" in by_rel:
                fail(i, "only an offer supersedes an earlier offer")
        elif t == "counterparty_acceptance":
            # An observation that the counterparty accepted one exact offer. It
            # authorizes nothing by itself; a seller's commit rests on it.
            j = by_digest.get(body["proposed_action_ref"]["digest"])
            if j is None or j >= i or records[j]["x-deal-v0"]["record_type"] != "check" \
                    or records[j]["body"]["action"] != "offer":
                fail(i, "proposed_action_ref names no earlier offer of this chain")
            if j != latest_offer:
                fail(i, "a later offer superseded that one: only the latest offer can be accepted")
            if not any(records[k]["x-deal-v0"]["record_type"] == "action" and records[k]["body"]["action"] == "offer"
                       and any(r["rel"] == "authorized_by" and offer_check_of(by_digest[r["digest"]]) == j
                               for r in records[k]["x-deal-v0"].get("refs", []))
                       for k in range(j + 1, i)):
                fail(i, "that offer was checked but never made: the offer action comes before its acceptance")
            if body["observed_at"] > b["at"]:
                fail(i, "observed_at is later than the record")
            accepted = i
        elif t == "policy_change":
            pass  # its bindings are structural (the schema); no chain step depends on it yet
        elif t == "intent":
            if body.get("party_role", party_role) != party_role:
                fail(i, f"an intent cannot change the deal's party_role ({party_role}, set when it opened)")
            # An intent narrows the limits in force; asking for more is a
            # proposal that applies only once the user confirms it.
            if body.get("max_total_minor") is not None and (max_total is None or body["max_total_minor"] < max_total):
                max_total = body["max_total_minor"]
            if body.get("bounds_commitment") is not None and body["bounds_commitment"] != bounds:
                bounds_known = False
            if body.get("allowed") is not None:
                allowed = [a for a in body["allowed"] if allowed is None or a in allowed]
        elif t == "evidence":
            one("about", ("claim", "baseline"))
            one("confirms", ("close",), required=False)
            if "resolves_obligation" in body:
                j = by_digest.get(body["resolves_obligation"]["digest"])
                if j is None or j >= i or "obligation" not in records[j]["body"]:
                    fail(i, "resolves_obligation must name an earlier record holding an obligation")
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
            # pack_id is optional (records sealed before v0.1.0-rc7 carry it); when
            # either side names one, the two must agree.
            if body.get("pack_id") != records[j]["body"].get("pack_id"):
                fail(i, "verdict pack_id differs from the check's")
            if b.get("typed"):
                # The check contract on the evaluation: the ProposedAction it
                # evaluated and the task authority in force; its proposed-phase
                # basis is that authority.
                task_at_verdict[i] = task_idx
                if body["proposed_action_digest"] != digests[j]:
                    fail(i, "proposed_action_digest is not the digest of the proposed action this evaluation evaluates")
                if body["task_authority_ref"]["digest"] != digests[task_idx]:
                    fail(i, "task_authority_ref is not the task authority in force at the evaluation")
                if body["authority_basis"] != [{"type": "task_authority", "ref": body["task_authority_ref"]}]:
                    fail(i, "the evaluation's authority_basis is its task_authority_ref")
        elif t == "approval" and body["choice"] == "confirm_limits":
            j = one("approves", ("intent",))
            prop = records[j]["body"]
            # A late answer (the proposal was replaced by a later intent, or is
            # already confirmed) is sealed as said but changes nothing.
            if j in confirmed_intents or any(records[k]["x-deal-v0"]["record_type"] == "intent" for k in range(j + 1, i)):
                if body["proceed"]:
                    fail(i, "a proposal replaced by a later intent, or already confirmed, cannot be confirmed (proceed must be false)")
                continue
            if not body["proceed"]:
                fail(i, "a confirmation of a standing proposal proceeds")
            in_force = limits_in_force()
            if without_unknown_bounds(body["limits"]["previous"]) != in_force:
                fail(i, "limits.previous is not the limits in force")
            new = dict(in_force)
            for k in ("max_total_minor", "allowed", "bounds_commitment"):
                if prop.get(k) is not None:
                    new[k] = prop[k]
            got = body["limits"]["new"] if "bounds_commitment" in prop else without_unknown_bounds(body["limits"]["new"])
            if got != new:
                fail(i, "limits.new is not what the intent proposed")
            # A floor cannot be compared (only its commitment is sealed): a proposal that states a
            # floor other than the one in force may be asking for a lower one, so it needs confirming.
            more = (prop.get("max_total_minor") is not None and max_total is not None
                    and prop["max_total_minor"] > max_total) or \
                   (prop.get("allowed") is not None and allowed is not None
                    and any(a not in allowed for a in prop["allowed"])) or \
                   (prop.get("bounds_commitment") is not None and prop["bounds_commitment"] != bounds)
            if not more:
                fail(i, "the intent asks for no higher limit and no new action: nothing to confirm")
            confirmed_intents.add(j)
            max_total, allowed = new.get("max_total_minor"), new.get("allowed")
            bounds, bounds_known = body["limits"]["new"].get("bounds_commitment"), True
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
            elif v["result"] in ("pause", "deny") and body["choice"] not in v["options"]:
                fail(i, "the user's choice is not one of the verdict's options")
            # The card the answer was given on is committed under the verdict's own card nonce,
            # so equal commitments mean the card shown is the card checked.
            if "card_commitment" in body:
                if "card_commitment" not in v:
                    fail(i, "the approval commits to a shown card, but the verdict it answers rendered none")
                elif body["card_commitment"] != v["card_commitment"]:
                    fail(i, "the card shown is not the card checked: the approval's card_commitment differs from the verdict's")
            if b.get("typed") and "rendering_commitment" in body and body["rendering_commitment"] != v.get("card_commitment"):
                fail(i, "what was shown (rendering_commitment) is not what the evaluation rendered")
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
            if b.get("typed"):
                jv, ja, chk = authorize_typed(one("authorized_by", ("approval", "verdict")), body["action"], "action")
                verify_basis(jv, ja, body.get("amount_minor"))
                if "disclosed" in body:
                    actions = {DISCLOSURE_ACTION[f["class"]] for f in body["disclosed"]["fields"]}
                    if actions != {body["action"]}:
                        fail(i, "the disclosed classes are not the ones this action covers")
                if party_role == "seller":
                    if body["action"] == "commit":
                        seller_needs_acceptance("commit")
                    elif any(f["class"] in ADDRESS_CLASSES for f in body.get("disclosed", {}).get("fields", [])):
                        seller_needs_acceptance("address")
            else:
                chk = authorized_check(one("authorized_by", ("approval",)), body["action"], "action")
            cb = chk["body"]
            if "amount_minor" in body and "amount_minor" in cb and body["amount_minor"] != cb["amount_minor"]:
                fail(i, "the amount differs from the one checked")
            if "cancelled_amount_minor" in body and "cancelled_amount_minor" in cb and body["cancelled_amount_minor"] != cb["cancelled_amount_minor"]:
                fail(i, "the cancelled amount differs from the one checked")
            if "currency" in body and "currency" in cb and body["currency"] != cb["currency"]:
                fail(i, "the currency differs from the one checked")
            crail = cb.get("recourse", {}).get("rail")
            if "rail" in body and crail and body["rail"] != crail:
                fail(i, "the payment rail differs from the one checked")
            p_act = b.get("counterparty", {}).get("ids", {}).get("payee")
            p_chk = chk["x-deal-v0"].get("counterparty", {}).get("ids", {}).get("payee")
            if p_act and p_chk and p_act != p_chk:
                fail(i, "the payee fingerprint differs from the one checked")
            # Section 6, rule 8: an action that returns money names the pay it
            # reverses, once, with the same amount and currency.
            j = one("reverses", ("action",), required=False)
            if j is not None:
                undone = records[j]["body"]
                if undone["action"] != "pay" or undone.get("direction", "out") != "out":
                    fail(i, "reverses must name an earlier pay action")
                if body.get("direction") != "in":
                    fail(i, "an action that reverses a payment carries direction in")
                if body.get("amount_minor") != undone.get("amount_minor") or body.get("currency") != undone.get("currency"):
                    fail(i, "a reversal returns the amount and currency of the pay it reverses")
                if j in reversed_actions:
                    fail(i, "a payment is reversed at most once")
                reversed_actions.add(j)
            elif body.get("direction") == "in" and "amount_minor" in body:
                # A cancel that returns no sealed payment carries direction in
                # with its amount as cancelled_amount_minor: nothing returned,
                # nothing to name.
                fail(i, "an action with direction in names the pay it reverses (reverses)")
        elif t == "outcome":
            one("observes", ("action",), required=False)
            if body["status"] == "unchecked_action":
                unchecked += 1
            if body["outcome"] == "not_selected" and any(
                    records[k]["type"] == "action-record/v0" if is_typed(records[k])
                    else records[k]["x-deal-v0"]["record_type"] == "action" for k in range(i)):
                fail(i, "not_selected means nothing was done on the deal, and an action is on record")
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
            if body["outcome"] != "open":
                closed_final = i
            for c in body.get("carried_obligations", []):
                j = by_digest.get(c["obligation"]["digest"])
                held = records[j]["body"].get("obligation", {}) if j is not None else {}
                date = "due_by" if "due_by" in c else "cancel_by"
                if j is None or j >= i or held.get(date) != c[date]:
                    fail(i, "carried_obligations must name earlier records holding that date")


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
    for x in load(FIX / "commercial-bounds-vectors.json")["vectors"]:
        ok = jcs(x["document"]).decode("utf-8") == x["text"] and commitment(x["nonce"], x["text"]) == x["bounds_commitment"]
        bad += not ok
        print(f"{'ok  ' if ok else 'FAIL'} commercial-bounds/v0 {x['document']['min_total_minor']:<16} {x['bounds_commitment'][:16]}…")
    store = local_store_values()

    # Two positive chains: x-deal-v0 throughout, and one sealed in the typed action
    # records beside x-deal-v0 evidence records. Each is checked as its own chain.
    positives = []
    for chain_dir in ("positive", "positive-typed"):
        chain = []
        for p in sorted((FIX / chain_dir).glob("*.json")):
            positives.append(p)
            f = load(p)
            rec = f["record"]
            try:
                check_record(rec, store)
                d = record_digest(rec)
                if d != f["expected_digest"]:
                    raise StageError("digest", f"recomputed {d} != expected {f['expected_digest']}")
                chain.append(rec)
                check_chain(chain)
                print(f"ok   {chain_dir}/{p.name:<34} {record_type_of(rec):<20} jcs-sha256 {d}")
            except StageError as e:
                bad += 1
                print(f"FAIL {chain_dir}/{p.name}: {e}")
    types = {record_type_of(load(p)["record"]) for p in positives}
    missing = (set(RECORD_TYPES) | set(TYPED_TYPES) - {"action-report/v0"}) - types
    if missing:
        bad += 1
        print(f"FAIL positives do not cover record types: {sorted(missing)}")
    else:
        print(f"ok   positives cover all {len(RECORD_TYPES)} x-deal-v0 record types and the sealed typed action "
              f"records ({len(positives)} records)")

    for p in sorted((FIX / "negative").glob("*.json")):
        f = load(p)
        prefix = [load(FIX / f.get("chain_dir", "positive") / n)["record"] for n in f.get("chain_prefix", [])]
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


def check_openings(recs, openings) -> None:
    """Each claim opening (a copy's claim_openings: the record digest, the
    baseline claim's index if any, and the nonce and words of the claim and
    of its source note) must recompute to the commitment the sealed record
    carries. Raises StageError('openings')."""
    by_digest = {record_digest(r): r for r in recs}
    for o in openings:
        rec = by_digest.get(o["record_digest"])
        if rec is None:
            raise StageError("openings", f"no record has digest {o['record_digest']}")
        claim = rec["body"]["claims"][o["index"]] if "index" in o else rec["body"]
        pairs = [("text", "text_commitment"), ("source", "source_ref_commitment")]
        for member, field in pairs:
            if member not in o:
                if field in claim:
                    raise StageError("openings", f"{field} of {o['record_digest']} has no opening")
                continue
            if field not in claim:
                raise StageError("openings", f"an opening for {field}, which {o['record_digest']} does not carry")
            if commitment(o[member]["nonce"], o[member]["text"]) != claim[field]:
                raise StageError("openings", f"the opening does not match {field} of {o['record_digest']}")


def check_files(paths, openings=None) -> int:
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
            if openings is not None:
                check_openings(recs, openings)
                print(f"ok   {p}: {len(openings)} claim opening(s) match")
            print(f"ok   {p}: " + ", ".join(record_digest(r) for r in recs))
        except StageError as e:
            bad += 1
            print(f"FAIL {p}: {e}")
    return bad


# ---------------------------------------------------------------------------
# Fixture generation (deterministic; the jet-ski payee-switch deal)
# ---------------------------------------------------------------------------

def regen():
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
        "said-limits": "yes, share my number with them",
        "said-1": "Hold",
        # What the agent told the new number, kept on the device only.
        "disclosed-phone": "(555) 010-2077",
        "disclosed-pickup": "lakeside marina, slip 14, by the fuel dock",
    }
    com = {k: commitment(nonce(k), v) for k, v in texts.items()}

    records, names = [], []
    clock = iter(["2026-10-01T16:00:00Z", "2026-10-01T16:02:10Z", "2026-10-01T16:02:40Z",
                  "2026-10-01T16:05:00Z", "2026-10-01T16:06:00Z", "2026-10-01T16:20:00Z", "2026-10-01T16:20:05Z",
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
    # The user's "go ahead and share my number" is sealed by the agent as an
    # intent that proposes a new action; it applies only once the user's own
    # confirm_limits answer seals the new version of the limits.
    p1 = add("intent", "intent", {"verbatim_commitment": com["intent-2"], "allowed": ["pay", "share_contact"]})
    add("approval-confirm-limits", "approval", {
        "choice": "confirm_limits", "proceed": True, "approver": "user", "said_commitment": com["said-limits"],
        "limits": {"previous": {"max_total_minor": 45000, "allowed": ["pay"]},
                   "new": {"max_total_minor": 45000, "allowed": ["pay", "share_contact"]}}}, refs=[("approves", p1)])
    k1 = add("check-share-contact", "check", {"action": "share_contact", "disclosing": ["phone"],
                                              "disclosing_to": "counterparty"})
    v1 = add("verdict-pass", "verdict", {"result": "pass", "differences": [], "options": [],
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
                                    "recourse": {"rail": "zelle", "refundable": False}},
             counterparty=cp(("payee", "second")))
    v2 = add("verdict-pause", "verdict", {
        "result": "pause",
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
        "intent": "The user asks the agent to share their number: sealed as a proposal of a new action, "
                  "which applies only once the user confirms it.",
        "check": "The agent runs the deal check before a point of no return.",
        "verdict": "The deal check's answer to the check it references.",
        "approval": "What authorizes the next step: a standing intent on a pass, or the user's own answer; "
                    "or the user's confirmation of limits an intent proposed (a new version naming the previous one).",
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

    # --- the typed action records: the merchant-changed counterexample ----------------------
    # The merchant changes from Example Air to travel-super-discount.example, so the deal
    # check asks. The platform's own gate asks "Approve $558.80 purchase" and the user
    # approves it there: recorded as the platform's approval, it does not answer the check
    # (paying on it alone is an unchecked action). The user's own answer, on the card the
    # check rendered, does; the payment then lists every authority layer it relied on.
    chain_id = "deal-7a1e5c0d9b2f4e68"
    dk1 = deal_key(secret, chain_id)
    raw1 = {("name", "first"): "Example Air", ("payee", "first"): "Example Air", ("domain", "first"): "air.example",
            ("payee", "second"): "Travel Super Discount", ("domain", "second"): "travel-super-discount.example"}
    fps1 = {k: fingerprint(dk1, k[0], v) for k, v in raw1.items()}
    texts1 = {"flight-intent": "book the SJC to HOU flight under $600",
              "flight-message": "Checkout continues on travel-super-discount.example for this fare.",
              "flight-card": "Payee changed since first contact (Example Air → Travel Super Discount) · [Hold] [Pay anyway]",
              "flight-platform": "Approve $558.80 purchase",
              "flight-platform-user": "Approve",
              "flight-said": "yes, the new site is fine"}
    com1 = {k: commitment(nonce(k), v) for k, v in texts1.items()}
    # The digest of the materiality predicate the check evaluated (here the example one);
    # regenerate these fixtures when it or the rule table changes (a Go test keeps them equal).
    materiality = record_digest(load(EXAMPLE_PREDICATE))
    rules = {"evaluator": "capsulectl deal check", "materiality_digest": materiality, "rules": [
        {"question": "asked", "rules": ["agent_picked", "not_asked", "over_limit", "under_floor"]},
        {"question": "who", "rules": ["payee_or_contact_changed", "first_disclosure"]},
        {"question": "terms", "rules": ["terms_changed"]},
        {"question": "recourse", "rules": ["recourse_changed", "irreversible_rail"]},
        {"question": "safety", "rules": ["pay_before_seeing", "credentials_requested", "verification_code_request",
                                         "off_platform_early", "domain_recent", "materiality_changed"]}]}
    records1, names1 = [], []
    clock1 = iter([f"2026-10-06T09:{m:02d}:00Z" for m in (2, 2, 10, 11, 13, 13, 14, 15, 16, 17)])

    def tref(j):
        return {"type": "record", "digest_alg": "SHA-256", "digest": record_digest(records1[j])}

    def add_v0(name, rtype, body, channel=None, counterparty=None, refs=None):
        blk = {"profile": "x-deal-v0", "canonicalization": "jcs", "deal_id": chain_id,
               "record_type": rtype, "seq": len(records1) + 1, "at": next(clock1)}
        if records1:
            blk["prev"] = _ref(record_digest(records1[-1]))
            blk["baseline_ref"] = _ref(record_digest(records1[0]))
        if channel:
            blk["channel"] = channel
        if counterparty:
            blk["counterparty"] = {"fp_alg": "hmac-sha256-deal-key", "ids": {k[0]: fps1[k] for k in counterparty}}
        if refs:
            blk["refs"] = [{"rel": rel, **_ref(record_digest(records1[j]))} for rel, j in refs]
        records1.append({"x-deal-v0": blk, "body": body})
        names1.append(f"{len(records1):02d}-{name}.json")
        return len(records1) - 1

    def add_typed(name, type_name, body, refs=None):
        r = {"type": type_name, "canonicalization": "jcs", "chain_id": chain_id, "seq": len(records1) + 1,
             "at": next(clock1), "prev": tref(len(records1) - 1), "chain_root": tref(0)}
        if refs:
            r["refs"] = [{"rel": rel, **tref(j)} for rel, j in refs]
        r["body"] = body
        records1.append(r)
        names1.append(f"{len(records1):02d}-{name}.json")
        return len(records1) - 1

    def cpt(*keys):
        return {"fp_alg": "hmac-sha256-chain-key", "ids": {k[0]: fps1[k] for k in keys}}

    fb = add_v0("baseline", "baseline", {
        "deal_type": "purchase",
        "intent": {"verbatim_commitment": commitment(nonce("flight-baseline"), texts1["flight-intent"]),
                   "max_total_minor": 60000, "allowed": ["pay"]},
        "terms": {"item": "SJC-HOU flight", "price_minor": 55880, "currency": "USD"},
        "recourse": {"rail": "card", "refundable": True},
    }, channel="web", counterparty=[("payee", "first"), ("name", "first"), ("domain", "first")])
    fta = add_typed("task-authority", "task-authority/v0", {
        "verbatim_commitment": com1["flight-intent"], "max_total_minor": 60000, "allowed": ["pay"]}, refs=[("source", fb)])
    fm = add_v0("message-site-switch", "message", {"from": "counterparty", "content_commitment": com1["flight-message"]},
                channel="web", counterparty=[("domain", "second")])
    add_v0("detail-change-merchant", "detail_change", {"source": "counterparty", "changed": ["payee", "domain"]},
           counterparty=[("payee", "second"), ("domain", "second")], refs=[("source", fm)])
    fk = add_typed("proposed-action", "proposed-action/v0", {
        "action": "pay", "amount_minor": 55880, "currency": "USD", "recourse": {"rail": "card", "refundable": True},
        "counterparty": cpt(("payee", "second"))})
    fv = add_typed("action-evaluation", "action-evaluation/v0", {
        "disposition": "ASK", "findings": [{"question": "who", "rule": "payee_or_contact_changed", "field": "payee"}],
        "options": ["hold", "proceed"], "rendering_commitment": com1["flight-card"], "judge": {"kind": "rules"},
        "proposed_action_digest": record_digest(records1[fk]), "task_authority_ref": tref(fta),
        "ruleset_digest": record_digest(rules), "materiality": "predicate", "materiality_digest": materiality, "valid_until": "2026-10-06T09:28:00Z",
        "authority_basis": [{"type": "task_authority", "ref": tref(fta)}]}, refs=[("checks", fk)])
    fp = add_typed("platform-approval-observation", "action-approval/v0", {
        "authority": "platform_approval", "kind": "platform-approval-observation",
        "platform": "example-platform", "mechanism": "native-gate",
        "displayed_text_digest": com1["flight-platform"], "returned_user_text_digest": com1["flight-platform-user"],
        "proposed_action_ref": tref(fk), "observed_at": "2026-10-06T09:13:40Z",
        "amount_minor": 55880, "currency": "USD"})
    add_typed("outcome-platform-only", "action-outcome/v0", {
        "status": "unchecked_action", "outcome": "mismatch",
        "findings": [{"question": "asked", "rule": "no_sealed_approval"}],
        "attempted": {"action": "pay", "amount_minor": 55880, "currency": "USD", "rail": "card"}})
    fa = add_typed("approval-on-the-card", "action-approval/v0", {
        "authority": "user_approval", "choice": "proceed", "proceed": True,
        "said_commitment": com1["flight-said"], "rendering_commitment": com1["flight-card"]}, refs=[("approves", fv)])
    add_typed("action-record-pay", "action-record/v0", {
        "action": "pay", "amount_minor": 55880, "currency": "USD", "rail": "card", "counterparty": cpt(("payee", "second")),
        "evaluation_ref": tref(fv),
        "authority_basis": [{"type": "task_authority", "ref": tref(fta)},
                            {"type": "user_approval", "ref": tref(fa)},
                            {"type": "platform_approval", "ref": tref(fp)}]}, refs=[("authorized_by", fa)])
    stories1 = {
        "baseline": "First contact: the user asks for the SJC-HOU flight under $600 from Example Air.",
        "task-authority/v0": "The user's task authority as asked: their words by commitment, the limit and the allowed action.",
        "message": "Checkout moves to another site.",
        "detail_change": "The merchant changes: payee and domain (fingerprints only).",
        "proposed-action/v0": "The payment about to be made, exactly as checked.",
        "action-evaluation/v0": "The deal check asks (ASK): the payee changed since first contact. It names the proposed action, "
                                "the task authority, the rule table and materiality predicate, and how long it is valid.",
        "action-approval/v0": "An approval artifact of a stated authority: here an observation of the platform's own approval "
                              "interaction (what it displayed and the text it returned, by commitment; it authorizes nothing "
                              "and never answers the check), or the user's own answer on the card the check rendered.",
        "action-outcome/v0": "Paying on the platform's approval alone is an unchecked action: it did not answer the ask.",
        "action-record/v0": "The payment, with the evaluation it relied on and every authority layer, in order.",
    }
    (FIX / "positive-typed").mkdir(parents=True, exist_ok=True)
    for old in (FIX / "positive-typed").glob("*.json"):
        old.unlink()
    for n, r in zip(names1, records1):
        d = record_digest(r)
        digests.append((f"positive-typed/{n}", d))
        dump(FIX / "positive-typed" / n, {"fixture": n, "expect": "valid", "story": stories1[record_type_of(r)],
                                          "record": r, "expected_digest": d})
    (FIX / "expected-digests.txt").write_text(
        "# SHA-256 over the RFC 8785 (JCS) bytes of each positive record (the `record` member only)\n"
        + "".join(f"{d}  {n if '/' in n else 'positive/' + n}\n" for n, d in digests), encoding="utf-8")
    texts.update(texts1)
    com.update(com1)
    # A claim's words and its source note, committed as a claim record from
    # claim_commit on seals them (text_commitment, source_ref_commitment).
    for k, v in {"claim-text-1": "they have 2 jet skis for Saturday", "claim-source-1": "seller_message"}.items():
        texts[k] = v
        com[k] = commitment(nonce(k), v)

    vec = {"note": "TEST-ONLY. Simulates a device's local store: the secret and raw values below never appear in a record. "
                   "All names, numbers and domains are fictional (555-01xx, .example).",
           "store_secret_hex": secret.hex(), "deal_id": deal_id, "deal_key_hex": dk.hex(),
           "vectors": [{"kind": k[0], "raw": v, "normalized": normalize(k[0], v), "fp": fps[k]} for k, v in raw.items()],
           "commitments": [{"label": k, "nonce": nonce(k), "text": v, "commitment": com[k]} for k, v in texts.items()]}
    extra = [("email", "  Bookings@CoastalJetRentals.example "), ("relay_address", " Reply-7f3@Relay.Marketplace.example"),
             ("profile_id", " mkt:seller:88213 "), ("name", "The Coastal Jet Rentals, Inc."),
             ("payee", "+1 (555) 010-2044")]
    for k, v in extra + [(k[0], v) for k, v in raw1.items()]:
        vec["vectors"].append({"kind": k, "raw": v, "normalized": normalize(k, v), "fp": fingerprint(dk, k, v)})
    dump(FIX / "fingerprint-vectors.json", vec)

    # commercial-bounds/v0: the private document holding a floor, and the bounds_commitment a
    # record seals to it. Go recomputes the same values (internal/cli/deal_bounds_test.go).
    bounds = {"description": "bounds_commitment = SHA-256 over the RFC 8785 (JCS) bytes of {\"nonce\", \"text\"}, "
                             "with text the JCS bytes of the commercial-bounds/v0 document (commit_alg sha256-jcs-nonce256).",
              "vectors": []}
    for label, floor in (("bounds-1", 170000), ("bounds-2", 0), ("bounds-3", 9007199254740991)):
        doc = {"type": "commercial-bounds/v0", "min_total_minor": floor}
        text = jcs(doc).decode("utf-8")
        bounds["vectors"].append({"document": doc, "nonce": nonce(label), "text": text,
                                  "bounds_commitment": commitment(nonce(label), text)})
    dump(FIX / "commercial-bounds-vectors.json", bounds)

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

    r = copy.deepcopy(records[16]); r["body"] = {"status": "not_selected", "outcome": "not_selected", "differences": []}
    neg("neg-not-selected-after-an-action", "chain", "nothing was done on the deal", r, 16,
        "An outcome that says the other side was not chosen, on a deal where an action was taken.")

    r = copy.deepcopy(records[4]); r["body"]["party_role"] = "seller"
    neg("neg-intent-changes-party-role", "chain", "cannot change the deal's party_role", r, 4,
        "An intent note on a deal that opened with no party_role (a buyer's) names seller.")

    r = copy.deepcopy(records[2]); r["x-deal-v0"]["seq"] = 2
    neg("neg-seq-regression", "chain", "seq regression", r, 2, "seq repeats 2 after 2.")

    r = copy.deepcopy(records[9]); del r["x-deal-v0"]["refs"]
    neg("neg-action-without-approval", "chain", "must reference the sealed approval", r, 9,
        "The share_contact action carries no authorized_by ref.")

    r = copy.deepcopy(records[9]); r["x-deal-v0"]["refs"] = [{"rel": "authorized_by", **_ref(record_digest(records[14]))}]
    r["body"] = {"action": "pay", "amount_minor": 20000, "currency": "USD", "rail": "zelle"}
    r["x-deal-v0"]["seq"] = 16; r["x-deal-v0"]["at"] = "2026-10-01T18:45:00Z"
    r["x-deal-v0"]["prev"] = _ref(record_digest(records[14]))
    neg("neg-action-on-hold", "chain", "did not approve proceeding", r, 15,
        "A pay action citing the user's Hold answer.")

    # The user holds, then answers the same verdict again with "Pay anyway": the second answer is
    # sealed (it records what the user chose), but an action is held to the verdict's first answer.
    again = copy.deepcopy(records[14])
    again["body"] = {"choice": "proceed", "proceed": True, "approver": "user",
                     "said_commitment": commitment(nonce("said-2"), "Pay anyway")}
    again["x-deal-v0"].update(seq=16, at="2026-10-01T18:44:50Z", prev=_ref(record_digest(records[14])))
    r = {"x-deal-v0": {**copy.deepcopy(records[9]["x-deal-v0"]), "seq": 17, "at": "2026-10-01T18:45:00Z",
                       "prev": _ref(record_digest(again)), "counterparty": cp(("payee", "second")),
                       "refs": [{"rel": "authorized_by", **_ref(record_digest(again))}]},
         "body": {"action": "pay", "amount_minor": 20000, "currency": "USD", "rail": "zelle"}}
    neg("neg-action-on-second-answer", "chain", "first answer", r, 15,
        "After Hold, a second answer to the same verdict (Pay anyway) is cited by a pay action.",
        chain_between=[again])

    r = copy.deepcopy(records[15]); r["body"]["fields"][0]["class_note"] = "texted (555) 010-2077"
    neg("neg-disclosure-raw-value", "personal_data", "raw phone", r, 15,
        "A disclosure that carries the disclosed phone number itself.")

    r = copy.deepcopy(records[15]); r["body"] = {"to": "counterparty", "authority": "approval",
                                                 "fields": [{"class": "phone", "value_commitment": com["disclosed-phone"]}]}
    r["x-deal-v0"]["refs"] = [{"rel": "authorized_by", **_ref(record_digest(records[8]))}]
    neg("neg-disclosure-reused-approval", "chain", "at most one action or disclosure", r, 15,
        "A disclosure citing the standing approval that already covered the share_contact action.")

    # The proposal without the user's confirmation: the share_contact check passes on standing
    # intent as if the intent note had widened what the user allowed. It did not.
    r = copy.deepcopy(records[8]); r["x-deal-v0"]["seq"] = 8
    k_unconfirmed = copy.deepcopy(records[6]); k_unconfirmed["x-deal-v0"].update(seq=6, prev=_ref(record_digest(records[4])))
    v_unconfirmed = copy.deepcopy(records[7]); v_unconfirmed["x-deal-v0"].update(
        seq=7, prev=_ref(record_digest(k_unconfirmed)), refs=[{"rel": "checks", **_ref(record_digest(k_unconfirmed))}])
    r["x-deal-v0"].update(prev=_ref(record_digest(v_unconfirmed)), refs=[{"rel": "approves", **_ref(record_digest(v_unconfirmed))}])
    neg("neg-standing-on-unconfirmed-proposal", "chain", "does not allow", r, 5,
        "An intent note proposes share_contact; with no confirm_limits answer from the user, a standing-intent "
        "approval of a share_contact check is refused: the limits in force are unchanged.",
        chain_between=[k_unconfirmed, v_unconfirmed])

    # A confirmation that claims to raise the limit the intent never asked to raise.
    r = copy.deepcopy(records[5]); r["body"]["limits"]["new"]["max_total_minor"] = 90000
    neg("neg-confirm-limits-not-the-proposal", "chain", "not what the intent proposed", r, 5,
        "A confirm_limits answer whose new version raises the limit, which the intent it approves did not propose.")

    # An answer given on a card other than the one the check rendered: its card_commitment, under
    # the verdict's card nonce, differs from the verdict's.
    r = copy.deepcopy(records[14])
    r["body"]["card_commitment"] = commitment(nonce("card-1"), "Pay M. Torres $200 by Zelle: all checks passed.")
    neg("neg-approval-card-not-checked", "chain", "card shown is not the card checked", r, 14,
        "The Hold answer commits to a card text other than the one its verdict rendered.")

    # agent_card: a click on a card the agent composed. It carries no words of the user's, so a
    # said_commitment on it is a schema error, and it can never confirm the user's limits.
    r = copy.deepcopy(records[14]); r["body"]["approver"] = "agent_card"
    neg("neg-agent-card-with-words", "schema", "said_commitment", r, 14,
        "An agent_card answer that carries a said_commitment: words are only ever the user's (approver user).")
    r = copy.deepcopy(records[5]); r["body"]["approver"] = "agent_card"; del r["body"]["said_commitment"]
    neg("neg-agent-card-confirms-limits", "schema", "user", r, 5,
        "A confirm_limits answer sealed as agent_card: raising the user's limits takes their own words.")

    # A confirmation cited as the authority for an action.
    r = copy.deepcopy(records[9]); r["x-deal-v0"]["refs"] = [{"rel": "authorized_by", **_ref(record_digest(records[5]))}]
    neg("neg-confirm-limits-as-authority", "chain", "authorizes no action", r, 9,
        "The share_contact action cites the user's limits confirmation instead of the approval of its own check.")

    # Section 6, rule 8 (reversals). Each negative continues the deal after
    # its share action (records[:10]) with a standing-approved pay, then a
    # cancel the user approves, and ends in the reversal under test.
    def continue_after(upto, specs):
        """Records continuing the deal after records[:upto]: specs are
        (record_type, body, [(rel, target)]), target a record or a list index."""
        built, previous = [], records[upto - 1]
        for n, (rtype, body, refs) in enumerate(specs):
            blk = {"profile": "x-deal-v0", "canonicalization": "jcs", "deal_id": deal_id, "record_type": rtype,
                   "seq": upto + n + 1, "at": "2026-10-01T16:%02d:00Z" % (30 + n),
                   "prev": _ref(record_digest(previous)), "baseline_ref": _ref(record_digest(records[0]))}
            if refs:
                blk["refs"] = [{"rel": rel, **_ref(record_digest(built[t] if isinstance(t, int) else t))} for rel, t in refs]
            previous = {"x-deal-v0": blk, "body": body}
            built.append(previous)
        return built

    def reversal_run(cancel_minor, reversal, extra_reversal=None):
        pay = [
            ("check", {"action": "pay", "amount_minor": 20000, "currency": "USD"}, []),
            ("verdict", {"result": "pass", "differences": [], "options": [], "judge": {"kind": "rules"}}, [("checks", 0)]),
            ("approval", {"choice": "proceed", "proceed": True, "approver": "standing_intent"}, [("approves", 1)]),
            ("action", {"action": "pay", "amount_minor": 20000, "currency": "USD", "direction": "out"}, [("authorized_by", 2)]),
        ]

        def cancel(at):
            return [
                ("check", {"action": "cancel", "amount_minor": cancel_minor, "currency": "USD"}, []),
                ("verdict", {"result": "pause", "differences": [{"question": "asked", "rule": "not_asked", "field": "action"}],
                             "options": ["hold", "proceed"], "card_commitment": com["card-1"], "judge": {"kind": "rules"}},
                 [("checks", at)]),
                ("approval", {"choice": "proceed", "proceed": True, "approver": "user", "said_commitment": com["said-1"]},
                 [("approves", at + 1)]),
            ]
        specs = pay + cancel(4)
        if extra_reversal is not None:
            specs += [extra_reversal(6)] + cancel(8)
        specs.append(reversal(len(specs) - 1))
        return continue_after(10, specs)

    good_in = {"action": "cancel", "amount_minor": 20000, "currency": "USD", "direction": "in"}
    for name, contains, run, desc in [
        ("neg-reversal-amount-mismatch", "returns the amount and currency",
         reversal_run(15000, lambda g: ("action", {**good_in, "amount_minor": 15000}, [("authorized_by", g), ("reverses", 3)])),
         "A cancel that reverses a 200.00 payment but returns 150.00."),
        ("neg-reversal-without-reverses", "names the pay it reverses",
         reversal_run(20000, lambda g: ("action", good_in, [("authorized_by", g)])),
         "An action with direction in that names no payment it reverses."),
        ("neg-reversal-twice", "reversed at most once",
         reversal_run(20000, lambda g: ("action", good_in, [("authorized_by", g), ("reverses", 3)]),
                      extra_reversal=lambda g: ("action", good_in, [("authorized_by", g), ("reverses", 3)])),
         "A second cancel reversing the same payment again."),
        ("neg-reversal-of-non-pay", "earlier pay action",
         reversal_run(20000, lambda g: ("action", good_in, [("authorized_by", g), ("reverses", records[9])])),
         "A reversal naming the share_contact action, not a payment."),
        ("neg-reversal-direction-out", "carries direction in",
         reversal_run(20000, lambda g: ("action", {**good_in, "direction": "out"}, [("authorized_by", g), ("reverses", 3)])),
         "An action that reverses a payment but says its money went out."),
    ]:
        neg(name, "chain", contains, run[-1], 10, desc, chain_between=run[:-1])

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

    r = copy.deepcopy(records[13]); r["body"]["differences"][2]["rule"] = "likely_scam"
    neg("neg-wording-label", "wording", "prohibited word", r, 13, "A difference rule that labels the counterparty.")

    r = copy.deepcopy(records[2]); r["body"]["text"] = "seller rating 4.9 on the marketplace"
    neg("neg-wording-grade", "wording", "prohibited word", r, 2, "A claim that carries a grade of the counterparty.")

    r = copy.deepcopy(records[2]); r["body"]["text"] = "has 2 jet skis, ask for M. Torres"
    neg("neg-raw-payee-name", "personal_data", "raw local-store identifier", r, 2,
        "A raw payee name from the local store inside a claim.")

    r = copy.deepcopy(records[1]); r["x-deal-v0"]["counterparty"] = {"fp_alg": "hmac-sha256-deal-key", "ids": {"phone": "+15550102044"}}
    neg("neg-unfingerprinted-id", "personal_data", "raw phone", r, 1,
        "A counterparty id carried as the raw E.164 value instead of a fingerprint.")

    # --- negatives on the typed chain (section 9) ------------------------------------
    def negt(name, stage, contains, record, prefix_upto, desc, **extra_fields):
        dump(FIX / "negative" / f"{name}.json", {
            "fixture": name, "expect": "invalid", "description": desc, "expected_stage": stage,
            "expected_error_contains": contains, "chain_dir": "positive-typed", "chain_prefix": names1[:prefix_upto],
            "record": record, **extra_fields})

    pay = records1[-1]
    last = len(records1) - 1
    r = copy.deepcopy(pay)
    r["refs"] = [{"rel": "authorized_by", **tref(fp)}]
    r["body"]["authority_basis"] = [e for e in r["body"]["authority_basis"] if e["type"] != "user_approval"]
    negt("neg-typed-platform-approval-as-the-answer", "chain", "must point at a approval or verdict record", r, last,
         "The payment cites the platform's approval as its authority: a platform approval never answers the deal check.")

    card = copy.deepcopy(records1[fa])
    card["body"] = {"authority": "card_answer", "choice": "proceed", "proceed": True, "rendering_commitment": com1["flight-card"]}
    r = copy.deepcopy(pay)
    r["prev"] = {"type": "record", "digest_alg": "SHA-256", "digest": record_digest(card)}
    r["refs"] = [{"rel": "authorized_by", "type": "record", "digest_alg": "SHA-256", "digest": record_digest(card)}]
    r["body"]["authority_basis"][1]["ref"]["digest"] = record_digest(card)
    negt("neg-typed-card-answer-answers-the-ask", "chain", "a card answer does not satisfy it", r, fa,
         "A card answered with no words of the user's, cited as the answer to the deal check's ask.", chain_between=[card])

    r = copy.deepcopy(records1[fa])
    r["body"]["rendering_commitment"] = commitment(nonce("flight-card"), "Pay Example Air $558.80 · [Pay]")
    negt("neg-typed-shown-not-what-was-checked", "chain", "is not what the evaluation rendered", r, fa,
         "The user's answer commits to a card other than the one the evaluation rendered.")

    r = copy.deepcopy(pay)
    r["body"]["authority_basis"] = r["body"]["authority_basis"][:2]
    negt("neg-typed-platform-approval-left-out", "chain", "every platform approval observed", r, last,
         "The payment leaves out the platform approval observed for the same evaluation: layers accumulate.")

    r = copy.deepcopy(pay)
    r["body"]["authority_basis"].append({"type": "one_shot_override", "ref": tref(fa)})
    negt("neg-typed-one-shot-override", "chain", "one_shot_override is reserved", r, last,
         "one_shot_override is reserved until a rule defines its own override.")

    r = copy.deepcopy(pay); r["at"] = "2026-10-06T09:40:00Z"
    negt("neg-typed-act-after-valid-until", "chain", "a step after that needs a new check", r, last,
         "The payment is sealed after the evaluation's valid_until.")

    r = copy.deepcopy(pay); r["body"]["evaluation_ref"] = tref(fk)
    negt("neg-typed-evaluation-ref", "chain", "evaluation_ref is not the evaluation", r, last,
         "The payment's evaluation_ref names the proposed action, not the evaluation.")

    r = copy.deepcopy(records1[fp]); r["body"].update(choice="proceed", proceed=True)
    negt("neg-typed-observation-claims-a-decision", "schema", "body", r, fp,
         "A platform approval observation that states a choice: an observation records bytes, never a decision.")

    r = copy.deepcopy(records1[fp]); r["body"]["proposed_action_ref"] = tref(fm)
    negt("neg-typed-observation-not-of-a-proposed-action", "chain", "proposed_action_ref names no earlier proposed action", r, fp,
         "A platform approval observation whose proposed_action_ref names a message.")

    r = copy.deepcopy(records1[fv]); del r["body"]["materiality_digest"]
    negt("neg-typed-materiality-missing", "schema", "materiality_digest", r, fv,
         "An evaluation without materiality_digest: no predicate is stated as null with mode none_fail_safe, never by absence.")

    r = copy.deepcopy(records1[fv]); r["body"]["materiality"] = "none_fail_safe"
    negt("neg-typed-materiality-mode-with-a-digest", "schema", "materiality_digest", r, fv,
         "Mode none_fail_safe (no predicate configured) with a predicate digest.")

    r = copy.deepcopy(records1[fv]); r["body"]["proposed_action_digest"] = record_digest(records1[fm])
    negt("neg-typed-proposed-action-digest", "chain", "proposed_action_digest is not the digest of the proposed action", r, fv,
         "The evaluation names a ProposedAction other than the one it evaluates.")

    r = copy.deepcopy(records1[fv]); r["body"]["task_authority_ref"] = tref(fm)
    r["body"]["authority_basis"] = [{"type": "task_authority", "ref": r["body"]["task_authority_ref"]}]
    negt("neg-typed-task-authority-ref", "chain", "task_authority_ref is not the task authority in force", r, fv,
         "The evaluation's task_authority_ref names a message instead of the task authority in force.")

    v0_verdict = {"x-deal-v0": {"profile": "x-deal-v0", "canonicalization": "jcs", "deal_id": chain_id, "record_type": "verdict",
                                "seq": fv + 1, "at": records1[fv]["at"], "prev": _ref(record_digest(records1[fk])),
                                "baseline_ref": _ref(record_digest(records1[0])), "refs": [{"rel": "checks", **_ref(record_digest(records1[fk]))}]},
                  "body": {"result": "pass", "differences": [], "options": [], "judge": {"kind": "rules"}}}
    negt("neg-typed-x-deal-verdict-in-a-typed-chain", "chain", "a verdict step is a typed record", v0_verdict, fv,
         "An x-deal-v0 verdict in a chain sealed in the typed action records.")

    print(f"regenerated {len(records)} + {len(records1)} positives, negatives and vectors under {FIX}")


def main(argv):
    if "--regen" in argv:
        regen()
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
    openings = None
    for a in argv:
        if a.startswith("--openings="):
            openings = load(a.split("=", 1)[1])
    bad = len(fails) + (check_files(files, openings) if files else run_fixtures())
    print("ALL OK" if not bad else f"{bad} FAILURE(S)")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
