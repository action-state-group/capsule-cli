# Contributing to capsule-cli

Commits are signed off (`git commit -s`, the Developer Certificate of Origin).
CI checks formatting, module files, vet, the build, and the suite under the
race detector (slow single-goroutine case loops sampled), then every case
without it; see "Tests" and "Continuous integration" in the README.

## Running the tests the way CI does

```bash
make test                                    # MySQL; race detector, then every case without it
bash scripts/test.sh -tags=actionstate       # the same for the plugin-dispatch build
CAPSULE_TEST_RACE_FULL=1 make test           # every case under the race detector
```

Under the race detector the slowest single-goroutine case loops (random
rewritings through the share gate, end-to-end runs over fixed case lists) run a
fixed sample: the detector makes them about ten times slower and finds nothing
in them. The second pass runs every case. `CAPSULE_TEST_RACE_FULL=1` runs every
case under the race detector too; see `internal/cli/race_sample_test.go`.

If you call `go test -race` yourself, pass `-timeout=15m` as the script does.
Without it, `go test` stops a package after 10 minutes and reports a failure.

## Race failures

A -race failure that does not reproduce is not resolved, it is recorded. A PR
can't clear review on "two reruns passed": it must name the failing test, link
a reproduction, link the hunt that recorded it, or argue the failure mode
can't occur in the changed code.

Capture the full output, not the summary: a race report is printed above the
`FAIL` line, and a report from a goroutine that outlived its test names no
test. `GORACE='halt_on_error=1 log_path=/tmp/race'` keeps the first report
intact.
