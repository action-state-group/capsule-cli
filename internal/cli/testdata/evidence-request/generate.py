# SPDX-License-Identifier: Apache-2.0
"""Regenerate the evidence-request/1 answer vectors from the Python responder.

Run with capsule-emit==0.8.2 installed, from a scratch directory:

    CAPSULE_WITNESS=stub python generate.py OUT_DIR

Every answer is produced by ``capsule_emit.evidence_request.answer`` itself
over a freshly sealed ledger, so the Go reader is tested against bytes the
responder actually emits rather than bytes written by hand.
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path

from capsule_emit import seal, witness
from capsule_emit.evidence_request import answer

NOW = "2026-09-27T00:00:00Z"


def main(out: Path) -> None:
    out.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp())
    ledger = work / "ledger.jsonl"
    key = work / "producer.key"
    ids = []
    for i in range(3):
        sealed = seal(None, action=f"example-action-{i}", operator="example-operator", anchor=False,
                      ledger=ledger, signing_key_path=key)
        ids.append(sealed.capsule["capsule_id"])
    assert witness.push(str(ledger)) is not None

    requests = {
        "record": {"subject": {"kind": "record", "capsule_id": ids[1]}},
        "range": {"subject": {"kind": "range", "selector": f"{ids[0]}..{ids[2]}"}, "page": {"size": 2}},
        "chain_segment": {"subject": {"kind": "chain_segment", "last": 1}},
        "no_such_record": {"subject": {"kind": "record", "capsule_id": "0" * 64}},
    }
    for name, request in requests.items():
        request_bytes = json.dumps(request, sort_keys=True, separators=(",", ":")).encode()
        result = answer(request_bytes, ledger=ledger, signing_key_path=key, now=NOW)
        (out / f"{name}.request.json").write_bytes(request_bytes)
        (out / f"{name}.answer.json").write_text(json.dumps(result.to_dict(), sort_keys=True, indent=1) + "\n")


if __name__ == "__main__":
    if os.environ.get("CAPSULE_WITNESS") != "stub":
        sys.exit("set CAPSULE_WITNESS=stub: the vectors must never register with a live witness")
    main(Path(sys.argv[1]))
