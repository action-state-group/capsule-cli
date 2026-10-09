#!/usr/bin/env python3
"""A composed/v1 bilateral bundle with one mismatch, for the presentation goldens.

Built with agent-action-capsule's own composed/v1 vector generator
(python/scripts/generate_bundle_composed_vectors.py at the commit go.mod
pins), so every digest and signature follows the draft exactly as the
published vectors do. It is the "agree" case with one change: the two
responders' records carry different transcript digests, so the join is
declared, and derives, "mismatch". The keys are the generator's published
test seeds.

  composed_mismatch.py GENERATOR.py > bundle.json
"""

from __future__ import annotations

import importlib.util
import json
import sys


def load(path: str):
    spec = importlib.util.spec_from_file_location("composed_vectors", path)
    if spec is None or spec.loader is None:
        sys.exit(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> None:
    g = load(sys.argv[1])
    rec_a = g.observation("responder-a", "responder-a.example", g.RESPONDER_A, 10, transcript="shared transcript")
    rec_b = g.observation("responder-b", "responder-b.example", g.RESPONDER_B, 11, transcript="a different transcript")
    members = [g.artifact_member("responder-a", "obs-a", rec_a), g.artifact_member("responder-b", "obs-b", rec_b)]
    observers = [
        {"id": "obs-a", "role": "responder", "custody_domain": "custody-a.example"},
        {"id": "obs-b", "role": "responder", "custody_domain": "custody-b.example"},
    ]
    composition = g.block(members, observers, [g.join("responder-a", "responder-b", "mismatch")])
    print(json.dumps(g.container(composition), indent=1))


if __name__ == "__main__":
    main()
