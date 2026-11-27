#!/usr/bin/env bash
# python-verifiers.sh -- print the path of a python that has the Python
# verifiers pinned in scripts/python-verifiers.txt, building a venv for them on
# first use. scripts/test.sh hands it to the Go/Python agreement tests as
# CAPSULECTL_TEST_PYTHON.
#
# The venv lives under ${XDG_CACHE_HOME:-$HOME/.cache}/capsule-cli, named by a
# digest of the lock file and the python3 version, so a changed pin or python
# gets a new venv. It is built in a temporary directory and renamed into
# place under a lock, so a failed or concurrent build never leaves a
# half-installed venv in use, and one that no longer imports the verifiers is
# rebuilt. Installs with
# --require-hashes; needs python3 with venv, and access to PyPI on the first
# run. To start over, delete that directory.
set -euo pipefail
cd "$(dirname "$0")/.."

if (($# > 0)); then
  echo "usage: scripts/python-verifiers.sh" >&2
  exit 2
fi

lock=scripts/python-verifiers.txt
command -v python3 >/dev/null || {
  echo "python-verifiers: python3 is required" >&2
  exit 1
}
key=$(python3 -c 'import hashlib, sys
h = hashlib.sha256(open(sys.argv[1], "rb").read())
h.update(sys.version.encode())
print(h.hexdigest()[:16])' "$lock")
cache=${XDG_CACHE_HOME:-$HOME/.cache}/capsule-cli
venv=$cache/python-verifiers-$key

probe='import capsule_emit.evidence_file, agent_action_capsule.bundle'
# Fast path: a venv that runs the probe is used without locking.
if ! "$venv/bin/python" -c "$probe" 2>/dev/null; then
  # Repair under an exclusive lock on fd 9, so one run never deletes a venv
  # another run just built: flock holds while this shell keeps fd 9 open, and
  # the probe is rerun once the lock is held. A venv that no longer runs the
  # probe (its base python was removed, say) is rebuilt.
  mkdir -p "$cache"
  exec 9>"$venv.lock"
  python3 -c 'import fcntl; fcntl.flock(9, fcntl.LOCK_EX)'
  if ! "$venv/bin/python" -c "$probe" 2>/dev/null; then
    rm -rf "$venv"
    build=$(mktemp -d "$cache/python-verifiers-build.XXXXXX")
    trap 'rm -rf "$build"' EXIT
    python3 -m venv "$build" >&2
    "$build/bin/python" -m pip install --quiet --disable-pip-version-check \
      --require-hashes -r "$lock" >&2
    mv "$build" "$venv"
  fi
  exec 9>&-
fi

"$venv/bin/python" -c "$probe"
printf '%s\n' "$venv/bin/python"
