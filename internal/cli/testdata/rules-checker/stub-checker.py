#!/usr/bin/env python3
"""A neutral stub rules checker for tests.

It reads one external-check-input/v0 object on stdin and prints one
external-check-result/v0. Its two rules: a per-action limit ($1, minor units)
on the record's spend_authorized_minor (else amount_minor), and a 7-day limit
($2) on that plus the outgoing acts in the history from the last 7 days. With
no history, or history the input says is not complete, the 7-day rule is
not_evaluable. With no per-action limit it refuses the input, as a checker
does: non-zero exit, the cause on stderr.
"""
import json
import sys
from datetime import datetime, timedelta

if len(sys.argv) < 2:
    print("no limit given", file=sys.stderr)
    sys.exit(2)
per_action = int(sys.argv[1])
weekly = int(sys.argv[2]) if len(sys.argv) > 2 else None
doc = json.load(sys.stdin)
if doc.get("schema") != "external-check-input/v0":
    print("not an external-check-input/v0", file=sys.stderr)
    sys.exit(1)


def body(record):
    return record.get("agent_input", {}).get("body", {})


def when(record):
    try:
        return datetime.fromisoformat(record["timestamp"].replace("Z", "+00:00"))
    except (KeyError, ValueError):
        return None


b = body(doc["record"])
value = b.get("spend_authorized_minor", b.get("amount_minor"))
if value is None:
    print("the record has no amount", file=sys.stderr)
    sys.exit(1)
findings = []
fail = value > per_action
findings.append({"id": "per-action", "check": "limit", "verdict": "fail" if fail else "pass",
                 "limit": per_action, "value": value, **({"reason": "over the per-action limit"} if fail else {})})
verdict = "deny" if fail else "allow"
if weekly is not None:
    history, scope = doc.get("history"), doc.get("history_scope", {})
    if history is None or not scope.get("complete") or scope.get("days", 0) < 7:
        findings.append({"id": "weekly", "check": "window", "verdict": "not_evaluable",
                         "reason": "the 7-day total needs every earlier payment of the week"})
        if verdict == "allow":
            verdict = "not_evaluable"
    else:
        now = when(doc["record"])
        total = value
        for r in history:
            # An act sealed without a check carries it under "unchecked".
            t, hb = when(r), body(r).get("unchecked", body(r))
            if hb.get("direction") == "in" or (now and t and t < now - timedelta(days=7)):
                continue
            total += hb.get("amount_minor", 0)
        over = total > weekly
        findings.append({"id": "weekly", "check": "window", "verdict": "fail" if over else "pass",
                         "limit": weekly, "value": total, **({"reason": "over the 7-day limit"} if over else {})})
        if over:
            verdict = "deny"
print(json.dumps({"schema": "external-check-result/v0", "ruleset_id": "stub-rules/0.1.0",
                  "definition_digest": "0" * 63 + "1", "verdict": verdict, "tier": "recomputed",
                  "grade": "self-attested",
                  "findings": findings}))
