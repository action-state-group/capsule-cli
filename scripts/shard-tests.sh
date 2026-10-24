#!/usr/bin/env bash
# Prints the test names of shard i of n, for scripts/test.sh. Reads the unique
# test names on stdin and the seconds each took from a weights file ("seconds
# name" lines, # for comments; scripts/test-weights). Each test, heaviest first,
# goes to the shard with the least time so far. A test not in the file counts as
# CAPSULE_TEST_DEFAULT_WEIGHT seconds (default 2), and no test counts for less
# than 0.01s, so tests that timed at zero still spread over the shards. The
# split depends only on the names and the file, so every shard computes the
# same one and together they run each name exactly once.
#
#   shard-tests.sh i n weights-file < names
set -euo pipefail

awk -v w="$3" -v d="${CAPSULE_TEST_DEFAULT_WEIGHT:-2}" '
  BEGIN {
    while ((getline l < w) > 0) if (l !~ /^#/) { split(l, a, " "); wt[a[2]] = a[1] }
  }
  { t = ($0 in wt ? wt[$0] : d); print (t < 0.01 ? 0.01 : t), $0 }' |
  LC_ALL=C sort -k1,1gr -k2,2 |
  awk -v i="$1" -v n="$2" '
    { m = 1; for (k = 2; k <= n; k++) if (load[k] < load[m]) m = k
      load[m] += $1; if (m == i) print $2 }'
