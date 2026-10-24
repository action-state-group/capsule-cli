#!/usr/bin/env bash
# check-moved-paths.sh [--fail] -- the first-party plugins and the deal skill
# leave this repository (capsule-cli#326, docs/design/326-plugins-leave-the-core.md
# section 3.4). Reports the plugins/ and skills/deal/ trees if they exist, and
# any tracked file outside them that references a path in them. Without
# --fail it only warns, while the trees are still here.
set -euo pipefail
cd "$(dirname "$0")/.."

level=warning
[ "${1:-}" = --fail ] && level=error
found=0
for dir in plugins skills/deal; do
  if [ -e "$dir" ]; then
    echo "::$level::$dir/ still exists"
    found=1
  fi
done
# Only repository paths into the moved trees count. A URL (a word holding
# "://", such as https://host/api/plugins/cmd/x) and an absolute path (a word
# starting with "/", such as an agent host's /path/to/skills/deal/SKILL.md)
# are not; vendored files and history, which name other repositories' paths,
# are skipped.
moved='(skills/deal/|plugins/(cmd|internal|scripts)/)'
while IFS= read -r hit; do
  echo "::$level::${hit%%:*} references a moved path: ${hit#*:}"
  found=1
done < <(git grep -nE "$moved" -- \
  ':!plugins/' ':!skills/deal/' ':!CHANGELOG.md' ':!docs/design/' \
  ':!cli/assets/evidence-graph.iife.js' ':!cli/testdata/presentation/*/page.html' \
  ':!scripts/check-moved-paths.sh' |
  MOVED="$moved" perl -ne 'my ($f, $n, $text) = split /:/, $_, 3; for my $w (split /[\s"\x27`<>]+/, $text) { if ($w =~ /$ENV{MOVED}/ && $w !~ m{://} && $w !~ m{^/}) { print; last } }' |
  cut -c1-200 || true)
if [ "$level" = error ]; then exit "$found"; fi
