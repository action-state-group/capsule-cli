# Contributing to capsule-cli

Commits are signed off (`git commit -s`, the Developer Certificate of Origin).
CI checks formatting, module files, vet, the build, and the full suite under
the race detector; see "Tests" and "Continuous integration" in the README.

## Running the tests the way CI does

```bash
make test                                                  # race detector + MySQL, -timeout=15m
go test -tags actionstate -race -count=1 -timeout=15m ./...  # the plugin-dispatch build
```

Pass `-timeout=15m` as CI does. Without it, `go test` stops a package after
10 minutes and reports a failure. The race-enabled `internal/cli` package takes
3 to 6 minutes on a CI runner and up to about 8.5 on a busy 8-core machine, so
the default leaves little room.

## Race failures

A -race failure that does not reproduce is not resolved, it is recorded. A PR
can't clear review on "two reruns passed": it must name the failing test, link
a reproduction, link the hunt that recorded it, or argue the failure mode
can't occur in the changed code.

Capture the full output, not the summary: a race report is printed above the
`FAIL` line, and a report from a goroutine that outlived its test names no
test. `GORACE='halt_on_error=1 log_path=/tmp/race'` keeps the first report
intact.
