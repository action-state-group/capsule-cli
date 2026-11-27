#!/usr/bin/env bash
# check-env-files.sh -- no env file is committed. .gitignore excludes .env and
# .env.*; this catches one committed anyway (git add -f, a rename). Example
# files (*.example) are allowed. Run by CI's quality job and scripts/check.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

# ls-files on its own line, so a failure fails the check instead of reading as
# "no env files".
tracked="$(git ls-files)"
bad="$(printf '%s\n' "$tracked" | grep -E '(^|/)\.env($|\.)' | grep -vE '\.example$' || true)"
if [[ -n "$bad" ]]; then
  printf '::error::env file(s) committed; remove them from the repository and rotate any secret they held:\n%s\n' "$bad"
  exit 1
fi
