#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""One changelog fragment per pull request, folded into CHANGELOG.md at release.

Parallel pull requests that all edit `## Unreleased` conflict on every merge. A pull request
adds `changelog.d/<name>.md` instead; nothing else touches the changelog until a release.

    changelog_fragments.py check   # CI: every fragment is well formed
    changelog_fragments.py fold    # release: append each fragment to its changelog's Unreleased
                                   # section, in file-name order, and delete the fragment

A fragment is the exact Markdown that belongs under `## Unreleased`, in the changelog's own
style. It goes to `CHANGELOG.md` unless its first line is `<!-- changelog: <path> -->`, which
names another changelog (a Rust crate's, Python's). The directory's README.md is not a fragment.
"""
import pathlib
import re
import subprocess
import sys

DIR = "changelog.d"
DEFAULT_TARGET = "CHANGELOG.md"
TARGET = re.compile(r"\A<!-- changelog: (\S+) -->[ \t]*(?:\n|\Z)")
UNRELEASED = re.compile(r"^## Unreleased[ \t]*$", re.M)
H2 = re.compile(r"^## ", re.M)


def root():
    out = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True)
    return pathlib.Path(out.stdout.strip() if out.returncode == 0 else ".")


def parse(path, base):
    """Return (target, body, errors) for one fragment."""
    text = path.read_text(encoding="utf-8")
    errors = []
    target = DEFAULT_TARGET
    m = TARGET.match(text)
    if m:
        target = m.group(1)
        text = text[m.end():]
    body = text.strip()
    if not body:
        errors.append("is empty")
    if H2.search(body):
        errors.append("has a `## ` heading; a fragment is the content under Unreleased, "
                      "so use `###` or bullets")
    if "\t" in body.split("\n", 1)[0]:
        errors.append("starts with a tab")
    t = (base / target)
    if target.startswith("/") or ".." in pathlib.PurePosixPath(target).parts:
        errors.append(f"names a changelog outside the repository: {target}")
    elif not t.is_file():
        errors.append(f"names a changelog that does not exist: {target}")
    elif not UNRELEASED.search(t.read_text(encoding="utf-8")):
        errors.append(f"{target} has no `## Unreleased` heading")
    return target, body, errors


def fragments(base):
    d = base / DIR
    if not d.is_dir():
        return []
    return sorted(p for p in d.iterdir() if p.is_file() and p.name != "README.md" and not p.name.startswith("."))


def check(base):
    bad = 0
    for p in fragments(base):
        if p.suffix != ".md":
            print(f"{DIR}/{p.name}: a fragment is a .md file", file=sys.stderr)
            bad += 1
            continue
        for e in parse(p, base)[2]:
            print(f"{DIR}/{p.name}: {e}", file=sys.stderr)
            bad += 1
    return 1 if bad else 0


def fold(base):
    if check(base):
        return 1
    added = {}
    for p in fragments(base):
        target, body, _ = parse(p, base)
        added.setdefault(target, []).append(body)
    for target, bodies in added.items():
        path = base / target
        text = path.read_text(encoding="utf-8")
        start = UNRELEASED.search(text).end()
        nxt = H2.search(text, start)
        end = nxt.start() if nxt else len(text)
        section = text[start:end].rstrip("\n")
        section = (section + "\n\n" if section.strip() else "\n\n") + "\n\n".join(bodies) + "\n"
        tail = "\n" + text[end:] if nxt else ""
        path.write_text(text[:start] + section + tail, encoding="utf-8")
    for p in fragments(base):
        p.unlink()
    print(f"folded {sum(len(b) for b in added.values())} fragment(s) into {', '.join(sorted(added)) or 'nothing'}")
    return 0


def main(argv):
    if len(argv) != 2 or argv[1] not in ("check", "fold"):
        print(__doc__, file=sys.stderr)
        return 2
    base = root()
    return check(base) if argv[1] == "check" else fold(base)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
