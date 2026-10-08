# capsule-cli

Standalone Go executable `capsulectl`, wrapping `capsule-emit-go`, its optional
artifact SDK, `cll-go` and the `evidencebook` Go library. Applications import
those libraries, not this CLI: everything outside `cmd/` is under Go's
`internal/` and cannot be imported. This repository is a command-line tool, not
an SDK.
No application-specific (investigation/evaluation) semantics, database
migration tools, selective disclosure, or implicit login/default profile are
included.

## Prebuilt binaries

Each `v*` tag publishes `capsulectl` for linux/amd64, linux/arm64 and
darwin/arm64 on the repository's GitHub Releases page, with a `SHA256SUMS` file.
The current release is the pre-release `v0.1.0-rc6`; there is no final `v0.1.0`
yet, so GitHub's "latest release" link does not resolve.
Releases after `v0.1.0-rc2` also carry a GitHub build provenance attestation
for each binary (Sigstore, keyless: there is no release key to hold or
leak). It shows the file was built by this repository's release workflow
from the tagged commit; checking it means trusting GitHub's build attestation
for this repository. `v0.1.0-rc2` and earlier have `SHA256SUMS` only.

Releases from `v0.1.0-rc4` on also publish the attestation bundle itself,
`capsulectl-$V.sigstore.json` (one file covering every binary, listed in
`SHA256SUMS`). Checking a binary against that file needs **no GitHub account
and no sign-in**:

```bash
gh attestation verify "capsulectl-$V-$OS-$ARCH" --bundle "capsulectl-$V.sigstore.json" \
  --repo action-state-group/capsule-cli \
  --signer-workflow action-state-group/capsule-cli/.github/workflows/release.yml
```

That reaches Sigstore's public trust root over the network (no account). To
check with no network at all, fetch the root once with
`gh attestation trusted-root > trusted_root.jsonl` (no account either) and add
`--custom-trusted-root trusted_root.jsonl`. Take the root from Sigstore this
way, never from a release: a root shipped next to the files it vouches for
proves nothing. Without `--bundle`, `gh attestation verify` looks the
attestation up on GitHub, which needs `gh` signed in.

Which check applies (`gh auth status` tells signed in from not):

| `gh` | What to run |
|---|---|
| not installed | The SHA-256 against `SHA256SUMS` is the only check. |
| installed, not signed in | Never sign in for this. Use `--bundle` (`v0.1.0-rc4` and later); for `v0.1.0-rc3`, the SHA-256 is the only check. |
| installed and signed in | `--bundle` (preferred, same as above), or without it: `gh attestation verify capsulectl-$V-$OS-$ARCH --repo action-state-group/capsule-cli --signer-workflow action-state-group/capsule-cli/.github/workflows/release.yml` |

An install report should say which check was used, for example "provenance
checked: attestation verified; checksum matched" or "provenance not checked:
gh not signed in; checksum matched".

Provenance proves a binary was built by the release workflow, not that the
project meant to release it. From `v0.1.0-rc5` on, each release is also
registered in a public transparency log, and `capsulectl release watch`, run
off the release infrastructure, compares that log with the maintainer-signed
tags. What each check covers, and what none of them does (telling you this is
the right repository), is in
[docs/RELEASE-TRANSPARENCY.md](docs/RELEASE-TRANSPARENCY.md).

The binaries are static (`CGO_ENABLED=0`; SQLite is the pure-Go
`modernc.org/sqlite`), so they need no system libraries.

`v0.1.0-rc6` (commit `14bf3bcdeec0c9a83e5537cc02bd6ee349c07d9f`), SHA-256 of
each file, to compare against the downloaded `SHA256SUMS`:

| File | SHA-256 |
|---|---|
| `capsulectl-v0.1.0-rc6-linux-amd64` | `4b1f442a283d8bb5ad463001d4526947865d86a92702be9702b93a5669fe0a82` |
| `capsulectl-v0.1.0-rc6-linux-arm64` | `b105449299f04690aea7bf8c9db3b9d615a5bcd379bf2ef79b5c1d65b2d37601` |
| `capsulectl-v0.1.0-rc6-darwin-arm64` | `6af37d76ca1ec2ca2ad487b372f1360b472b2a527a6265f86c7896d11b17a11a` |
| `capsulectl-v0.1.0-rc6.sigstore.json` (the attestation bundle) | `d759f14f3d60cdb34328f1a724ef4af789d93149d8ffc4142e3773652cc13e07` |

```bash
V=v0.1.0-rc6; OS=linux; ARCH=amd64   # pre-release; or linux/arm64, darwin/arm64
base=https://github.com/action-state-group/capsule-cli/releases/download/$V
curl -fsSL -O "$base/capsulectl-$V-$OS-$ARCH" -O "$base/capsulectl-$V.sigstore.json" -O "$base/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS   # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
# provenance, with no GitHub account (skip if gh is not installed; see the table above):
gh attestation verify "capsulectl-$V-$OS-$ARCH" --bundle "capsulectl-$V.sigstore.json" \
  --repo action-state-group/capsule-cli \
  --signer-workflow action-state-group/capsule-cli/.github/workflows/release.yml
sudo install -m 0755 "capsulectl-$V-$OS-$ARCH" /usr/local/bin/capsulectl
capsulectl --version                        # capsulectl v0.1.0-rc6 (commit 14bf3bcdeec0c9a83e5537cc02bd6ee349c07d9f)
```

Writing to `/usr/local/bin` needs `sudo`. Without it, install into a directory
you own and put that directory on your `PATH`:

```bash
mkdir -p ~/.local/bin && install -m 0755 "capsulectl-$V-$OS-$ARCH" ~/.local/bin/capsulectl
export PATH="$HOME/.local/bin:$PATH"   # add this line to ~/.bashrc or ~/.zshrc to keep it
```

Release builds are reproducible: `scripts/release-build.sh VERSION COMMIT OUTDIR`
is the exact command the release workflow runs, so checking out a tag and running
it with the Go version in `go.mod` gives byte-identical binaries.

## Agent skill (Claude Code and Codex)

`skills/capsulectl/` is one skill in two renderings with the same content:
`SKILL.md` for Claude Code and `AGENTS.md` for Codex, both generated from
`spec.yaml` (see [skills/SKILL-SPEC.md](skills/SKILL-SPEC.md)). The skill calls
an installed `capsulectl`; it does not install one. Take the skill from the same
tag as your binary, so that every verb it lists exists in that binary
(`capsulectl <verb> --help` confirms one).

```bash
V=v0.1.0-rc6   # the same tag as the binary you installed (a pre-release)
git clone --depth 1 --branch "$V" https://github.com/action-state-group/capsule-cli.git

# Claude Code: a personal skill (or .claude/skills/ inside one project)
mkdir -p ~/.claude/skills && cp -r capsule-cli/skills/capsulectl ~/.claude/skills/

# Codex: copy the directory into the project, then point the project's
# AGENTS.md at it (this appends; it never overwrites an existing AGENTS.md)
cp -r capsule-cli/skills/capsulectl /path/to/project/capsulectl-skill
echo 'Before calling capsulectl, read and follow capsulectl-skill/AGENTS.md.' >> /path/to/project/AGENTS.md
```

Verbs that take `--profile` (`discover`, `publish`, `cll append`, `get`, and
the others) need a profile and an initialized store first; the
[Quickstart](#quickstart-no-database-server) below makes one in four commands.
The skill never writes a scope file or a schema for you: `discover --scope`
and `contract validate --schema` always name files you supply.

The two renderings are generated, never edited by hand. `cmd/skillgen`, the
repository's second binary, validates `skills/capsulectl/spec.yaml` against
[`schemas/skill-spec-v0.json`](schemas/skill-spec-v0.json) and writes both
files: run `go run ./cmd/skillgen` from the repository root after editing the
spec, or `go run ./cmd/skillgen --check` to fail when the files on disk differ
from what the spec renders. A test (`TestCapsulectlSkillMatchesItsSpec`) runs
that comparison in CI. The format is described in
[skills/SKILL-SPEC.md](skills/SKILL-SPEC.md). `skills/deal/` is the deal skill,
written by hand; see [skills/deal](skills/deal/README.md).

`skills/capsulectl/scripts/run-scripted-demo.sh` is a contributor check, not an
install step: it builds `capsulectl` from a source checkout, so it needs Go and
this repository, and you do not need it to use the skill.

## Quickstart (no database server)

A SQLite profile keeps everything in one local file. These are the exact
commands (and outputs, with keys and IDs elided) from the v0.1.0 release check
of the linux/amd64 binary on a clean `debian:stable-slim` container:

```console
$ capsulectl key generate --output producer.seed
{"public_key":"<producer-public-key-hex>","signing_key_file":"producer.seed","spec_version":"capsule-cli-result/v1"}
$ capsulectl key generate --output checkpoint.seed
{"public_key":"<checkpoint-public-key-hex>","signing_key_file":"checkpoint.seed","spec_version":"capsule-cli-result/v1"}
$ capsulectl profile create --name demo --type sqlite --sqlite-path /work/capsules.db \
    --namespace demo --log-id demo-log \
    --signing-key-file /work/producer.seed --trusted-key <producer-public-key-hex> \
    --checkpoint-signing-key-file /work/checkpoint.seed --checkpoint-trusted-key <checkpoint-public-key-hex>
{"profile":"demo","spec_version":"capsule-cli-result/v1","status":"saved"}
$ capsulectl store init --profile demo
{"log_id":"demo-log","spec_version":"capsule-cli-result/v1","status":"initialized"}
$ cat request.json
{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"demo-1","ActionType":"fyi","Operator":"example-operator","Developer":"example-developer","Timestamp":"2026-09-27T12:00:00Z"},"payload":{"case":1}}
$ capsulectl seal --profile demo --request request.json --output artifact.json
{"artifact":"artifact.json","capsule_id":"<capsule-id>","spec_version":"capsule-cli-result/v1"}
$ capsulectl cll append --profile demo --capsule artifact.json
{"capsule_id":"<capsule-id>","log_id":"demo-log","sequence":1,"spec_version":"capsule-cli-result/v1"}
$ capsulectl cll checkpoint create --profile demo
{"checkpoint":1,"indexed_sequence":1,"log_id":"demo-log","spec_version":"capsule-cli-result/v1","statement":"<base64 checkpoint>"}
```

`key generate` prints the public key for each seed; pass it to `--trusted-key` /
`--checkpoint-trusted-key`. For MySQL see [Profile setup](#profile-setup).

### JSONL: a directory and its evidence book

A JSONL profile keeps its artifacts in `artifacts.jsonl` and its one log in
`book/`, the profile's evidence book; it also names the `--operator` the book
writes into every record. These commands were run as written against this
branch's `capsulectl` (outputs with keys, IDs and the statement elided):

```console
$ capsulectl profile create --name demo --type jsonl --jsonl-path /work/store \
    --namespace demo --log-id demo-log --operator example-operator \
    --signing-key-file /work/producer.seed --trusted-key <producer-public-key-hex> \
    --checkpoint-signing-key-file /work/checkpoint.seed --checkpoint-trusted-key <checkpoint-public-key-hex>
{"profile":"demo","spec_version":"capsule-cli-result/v1","status":"saved"}
$ capsulectl store init --profile demo
{"log_id":"demo-log","spec_version":"capsule-cli-result/v1","status":"initialized"}
$ capsulectl seal --profile demo --request request.json --output artifact.json
{"artifact":"artifact.json","capsule_id":"<capsule-id>","spec_version":"capsule-cli-result/v1"}
$ capsulectl cll append --profile demo --capsule artifact.json
{"capsule_id":"<capsule-id>","log_id":"demo-log","sequence":1,"spec_version":"capsule-cli-result/v1"}
$ capsulectl cll list --profile demo
{"entries":[{"sequence":1,"capsule_id":"<capsule-id>","appended_at":"<time>","record_type":"published_capsule","record_id":"<book-record-id>","capsule_carried":true}],"log_id":"demo-log","next_after":1,"spec_version":"capsule-cli-result/v1"}
$ capsulectl cll checkpoint create --profile demo
{"checkpoint":4,"indexed_sequence":3,"log_id":"demo-log","spec_version":"capsule-cli-result/v1","statement":"<base64 checkpoint>"}
$ ls /work/store /work/store/book
/work/store:
artifacts.jsonl  book

/work/store/book:
log.jsonl  payloads  records
```

The keys and `request.json` are the ones from the SQLite example above. The
checkpoint covers three log entries (`indexed_sequence`: the published capsule
plus the two index roots the book commits before checkpointing); `checkpoint`
is the MMR size over those three entries, 4, the value
`cll checkpoint status --checkpoint` takes. `cll list` shows only entries that
record a capsule unless given `--all`. A JSONL store
made by an earlier capsulectl, with its log in `cll.jsonl`, is refused until it
is migrated: give its profile an operator, then move the log into the book under
a new log id (checked the same way against a store written by the previous
release):

```console
$ capsulectl profile update --profile demo --operator example-operator
$ capsulectl store migrate --profile demo --log-id <new-log-id>
{"backfilled":1,"log_id":"<new-log-id>","migration_record_id":"<record-id>","retired_log_id":"demo-log","spec_version":"capsule-cli-result/v1"}
```

## Build from source

```bash
make build
./capsulectl --help
```

The artifact SDK and CLL dependencies are pinned to published commits.
`GOWORK=off go build ./cmd/capsulectl` needs no sibling checkout, local
replacement, or Python runtime. Do not commit machine-specific `go.work` files.

This CLI was previously installed as `capsule`, which collided on `PATH` with
the Python `capsule-emit` CLI. If you installed the old binary, remove it so the
stale name does not shadow the new one:

```bash
BIN="$(go env GOBIN)"; GOPATH="$(go env GOPATH)"; rm -f "${BIN:-${GOPATH%%:*}/bin}/capsule"
make install   # installs capsulectl
```

## Commands

```text
capsulectl key generate --output SEED_FILE   # -o SEED_FILE
capsulectl key show-public SEED_FILE
capsulectl profile create --name NAME [configuration flags | --interactive]
capsulectl profile list
capsulectl profile show NAME
capsulectl profile update --profile NAME [configuration flags]
capsulectl profile retire NAME [--move-data NEW_DIR]   # rename to NAME-retired-YYYYMMDD; nothing recorded changes
capsulectl store init --profile NAME
capsulectl seal --profile NAME --request INPUT.json --output ARTIFACT.json
capsulectl emit --profile NAME --request INPUT.json --seal-output ARTIFACT.json
capsulectl get --profile NAME --capsule-id ID [--raw] [--output FILE.json]
capsulectl verify --profile NAME --capsule ARTIFACT.json|CAPSULE.json
capsulectl verify --bundle BUNDLE.json [--witness-directory WITNESSES.json]
capsulectl publish --profile NAME --request INPUT.json
capsulectl cll list --profile NAME --after SEQ [--through SEQ] [--limit 100] [--log-id LOG]
capsulectl cll append --profile NAME --capsule ARTIFACT.json
capsulectl cll verify --profile NAME --proof PROOF.json
capsulectl cll checkpoint create --profile NAME
capsulectl cll checkpoint publish --profile NAME --checkpoint MMR_SIZE
capsulectl cll checkpoint status --profile NAME --checkpoint MMR_SIZE
capsulectl doctor [--profile NAME] [--check-witness]
capsulectl doctor --install-check --profile NAME --expect-version TAG --expect-commit SHA --skills-dir DIR [--expect-skill-sha256 HEX] [--evidence-out FILE]
capsulectl result open FILE [--format text|json]
capsulectl result build --profile NAME --result RESULT.json --out SEALED.json [--contract REF] [--capsule-out RECORD.json]
capsulectl report build --bundle BUNDLE.json --card CARD --out report.html [--presentation P.json] [--permalink] [--base-url URL] [--dry-run]
capsulectl <plugin> [args passed to the plugin]   # a discovered capsulectl-<plugin> launcher
capsulectl plugin ls
capsulectl store migrate --profile NAME [--log-id NEW_LOG_ID]
capsulectl contract validate FILE --schema PATH_OR_URL [--json]
capsulectl contract diff A B [--schema PATH_OR_URL] [--json]
capsulectl discover --profile NAME --scope SCOPE.yaml --seal-output SCAN.json [--effects] [--format table|json]
capsulectl map CONTRACT --schema PATH_OR_URL --discover EFFECTS.json [--format text|json]
capsulectl bundle --profile NAME --root CAPSULE_ID --out BUNDLE.json [--html PAGE.html] [--closure-depth 2] [--producer-key HEX]
capsulectl disclose --profile NAME --root CAPSULE_ID --out BUNDLE.json [--html PAGE.html] [--payloads all|selected] [--suppress agent_input|agent_output] [--producer-key HEX]
capsulectl permalink --profile NAME --root CAPSULE_ID [--payloads all|selected] [--suppress ...] [--base-url URL]
capsulectl countersign request --profile NAME --service URL (--bundle BUNDLE.json | --root CAPSULE_ID [--producer-key HEX]) [--out FILE] [--window LABEL]
capsulectl countersign verify --profile NAME --directory URL_OR_FILE BUNDLE.json
capsulectl request --profile NAME --request FILE --responder NAME --output FILE
capsulectl request --profile NAME --for RECORD_ID (--response FILE --responder-key HEX --responder-checkpoint-key HEX [--witness-directory FILE] | --absent-until TIME)
capsulectl respond --profile NAME --request FILE --requester ID [--policy FILE] --output FILE
capsulectl close --profile NAME --period day|week --counterparty ID [--date YYYY-MM-DD] [--since-last] [--peer FILE --peer-checkpoint-key HEX]
capsulectl reconcile --profile NAME --period day|week --counterparty BOOK_ID --peer FILE --peer-checkpoint-key HEX
capsulectl judge pin FILE
capsulectl judge drift pin FILE_A FILE_B
capsulectl judge drift reports FILE_A FILE_B
capsulectl calibration summarize REPORTS_FILE RATINGS_FILE
capsulectl deal init|open|note|check|close|report|countersign|reconcile|tick --profile NAME [...]
capsulectl backfill run|status --profile NAME [...]
capsulectl canary run --profile NAME [--skill FILE] [--expect-version TAG] [--expect-skill-sha256 HEX]
capsulectl canary watch --log-id ID --expect-every DURATION [--witness URL] [--state FILE]
capsulectl release register --dir DIR --tag TAG --commit SHA [--witness URL --witness-key HEX]
capsulectl release watch --allowed-signers FILE [--repo OWNER/NAME] [--known-unsigned-file FILE] [--trusted-root FILE]
```

`capsulectl <command> --help` gives every flag; this list is the shape of each
command, not its full flag set.

`emit` is `seal`'s v4 name: the same seal/prepare/self-verify/write-to-file
operation, offered under both names (`seal` stays for existing callers).
`doctor` never opens a database connection or requires a profile; it reports
plugin trust, profile presence, signing-key-file permissions, and (only with
`--check-witness`) an unauthenticated reachability probe of the profile's
checkpoint endpoint. `result open` validates and prints a Result v0
document's aggregate/coverage statement as text (or `--format json`); the
Result v0 schema is still DRAFT, so this is a structural check, not schema
validation, and it stands in for the `capsule-viewer` build. `result build`
seals a caller-supplied Result v0 into a jsonl profile's evidence book as an
`evidence_result` record whose statement is the document and whose `cites`
links name every book record the claims cite: it validates against the
vendored `evidence-result-v0` schema (`internal/cli/schema/`, digest pinned
in code), refuses headline values that do not recompute from the claims, a
citation the book does not hold, or a close/reconcile claim whose state or
tallies differ from what the cited Close's links and statement read, and
then checkpoints so `disclose --root <record id>` can build a bundle on it.
Only UNILATERAL close claims can be sealed end to end today: a book holds
only its own records, and its own links to its Close never count, so AGREED
and CONTESTED wait on a way to bring the peer's record into the book.
`report build` verifies a held, disclosed bundle whose root is a sealed
Result (either carrier: the document itself, or the book record header
whose statement is the document), records `--card` and the optional
`presentation/v1` header in the embedded copy, and renders an offline
`report.html` through agent-action-capsule's Go emitter with the vendored
browser runtime (`internal/cli/assets/`, digest pinned; refresh with
`scripts/iife-sync.sh`); `--permalink` mints the viewer fragment over the
same bytes and `--dry-run` writes the page marked draft.

validation, and it stands in for the `capsule-viewer` build. A plugin is an
executable named `capsulectl-<name>` on a trusted plugin root: discovery wires
it up as `capsulectl <name>`, `plugin ls` lists what was found and what was
refused and why, and no core verb is ever replaced by one. The plugin
contract (`cli-plugin/v1`: discovery roots, handshake, and what a plugin can
never change) is all in [docs/PLUGINS.md](docs/PLUGINS.md).

`release register` records a built release in the witness's transparency log;
the release workflow runs it. `release watch` compares the repository's
releases and that log with the intended releases, the tags signed by the keys
in `--allowed-signers`, and is meant to run off the release infrastructure.
What each covers, and what it does not, is in
[docs/RELEASE-TRANSPARENCY.md](docs/RELEASE-TRANSPARENCY.md).

`--html PAGE.html` on `bundle` and `disclose` also writes the bundle as one
self-contained page: the bundle embedded and agent-action-capsule's own
evidence-graph verifier (vendored, `internal/cli/assets/`), which checks it
with no network when the page is opened. The file must be new. A permalink
carries its bundle in the link and takes no `--html`; a deal's page is its
receipt, `deal report --html`.

A page is written only for a bundle `verify --bundle` calls VALID, and only when a
disclosed `report/v1` root shows every row. Otherwise no page is written: `bundle`
still writes the bundle (`verify --bundle` states its verdict), and `disclose` writes
nothing and puts no disclosure on record. A jsonl profile writes no page: its bundle
carries its evidence book's records about what you sealed, not the records themselves.

Every bundle `bundle`, `disclose`, `permalink` and `countersign request --root`
build declares the producer's own Ed25519 key in the `producer-key/v1` bundle
extension, `{"extensions": {"producer-key/v1": {"public_key": "<64 lowercase hex>"}}}`:
by default the public half of the profile's signing key (the key that sealed
its records), or `--producer-key HEX` when the operator countersigns with a
different key of its own. The extension is covered by the bundle digest, so it
is fixed before anyone countersigns. A viewer, and `countersign verify`, reports
a countersignature by the declared key as not independent. A profile with no
signing key declares none.

`countersign verify` resolves each signer against the countersigner directory
named by `--directory`, which is required. It takes an HTTPS URL or a local
file, holding either a bare array of rows (`[{"name": ..., "key_ids": [...]}]`)
or an object with a `countersigners` array. The CLI privileges no list: you
choose the one you trust. A signer the directory does not list is reported as an
unresolved signer, and the command exits partial (3).

`deal` seals a deal's baseline and, when the host calls it, checks each point
of no return (pay, commit, sign, share) against it. The check is advisory: it
holds an action only where the host runs it from a pre-action hook. A deal
profile is witnessed by default on a fixed schedule, its checkpoint cadence:
one checkpoint of hashes per tick (every 5m, give or take 1m, by default),
never on activity. `cll checkpoint cadence --wait-up-to 5m`, scheduled every
5 minutes, is its poll: a run with no tick due publishes nothing.
`deal reconcile` reads the host's execution records afterwards and lists
consequential actions that have no deal record; see
[skills/deal](skills/deal/README.md).

The book verbs (`close`, `reconcile`, `request`, `respond`) and a `jsonl`
profile's log (`publish`, `cll append`, `cll list`) run over an evidence book
from the `evidencebook` Go library (`internal/cli/book.go`,
`internal/cli/booklog.go`). The CLI does not implement an evidence book of its
own: it chooses windows, reads files and writes results, and the library
composes every proof.

`cll list --log-id LOG` reads another log in the profile's store: a deal
profile keeps each deal in its own log, `deal/<deal id>`, and a deal profile
written by an earlier release has no `log_id` at all, so it needs `--log-id`.

Every input error (exit 2) names the flag, field or file at fault and what is
expected. It never prints a secret: a mistyped flag value is not echoed, only
the flag's name.

`backfill` imports an agent host's own records (tool calls, sub-tasks, spend
approvals) after the fact, as a watermark poll by monotonic cursor: every new
row by digest, and the rows near a consequential signal as Capsules in
provenance mode `backfilled`. It shows effects only, never an amount charged
or an approval, and it gives detectability of absence, not prevention; see
[docs/BACKFILL.md](docs/BACKFILL.md).

`canary run` plays a scripted DEMO deal on a profile of its own and ticks that
profile's cadence log only when every step succeeded; `canary watch` reads the
log's last checkpoint from the public witness and exits 6 with one line when it
has stopped advancing or its history was rewritten. Absence is the alarm, seen
from outside with no access to the host; see [docs/CANARY.md](docs/CANARY.md).
A harness for test accounts on an agent host (`doctor --install-check`, a
scheduled outside verdict, what each check catches and its limits) is in
[docs/HARNESS.md](docs/HARNESS.md).

`profile show` takes the profile name as its positional argument. Commands
outside profile management that access a configured target require `--profile`.
Connection settings and credentials are accepted only by profile create/update.
Each invocation gets a fresh Cobra/Viper instance. No automatic environment
override is enabled.

## What is in this repository

| Path | What it is |
|---|---|
| `cmd/capsulectl/` | the CLI's entry point |
| `cmd/skillgen/` | the second binary: renders the agent skill from its spec ([Agent skill](#agent-skill-claude-code-and-codex)) |
| `internal/cli/` | every command; not importable (Go `internal/`) |
| `internal/cli/assets/` | what the HTML pages embed: the vendored evidence-graph verifier (with its digest and NOTICE), the deal page's viewer, and the deal profile schema |
| `internal/cli/schemas/` | the embedded evidence-contract schema (a byte copy of capsule-engine's, pinned by digest) |
| `internal/cli/confusables_table.go` | **a security control**, generated: Unicode's TR39 confusables, so the deal skill's share check and page gate match a sensitive value even when it is written with lookalike letters from another script. Built by `scripts/genconfusables` from the unmodified `third_party/unicode/confusables.txt` (digest beside it); `scripts/update-confusables.sh` re-vendors a new Unicode version |
| `schemas/skill-spec-v0.json` | the schema a skill spec is validated against |
| `skills/` | the `capsulectl` skill (generated) and the `deal` skill, each with contributor demo scripts (`skills/capsulectl/scripts/bookdemo` is a small Go program the scripted demo runs); `skills/SKILL-SPEC.md` is the spec format |
| `action.yml` | the `Seal PR Capsule` GitHub Action ([below](#github-action-seal-a-capsule-per-pull-request)); its profile is [docs/PR-CAPSULE-PROFILE.md](docs/PR-CAPSULE-PROFILE.md), and `scripts/pr-capsule-*.sh` are its steps |
| `docs/` | plugins, release transparency, backfill, canary, the test-account harness, and the PR capsule profile |
| `examples/` | workflow files to copy: adopting the PR-capsule action, a canary watch, a deal-harness watch |
| `scripts/` | `test.sh` (`make test`), `release-build.sh` (the reproducible release build), the PR-capsule steps, the confusables tools, `build-evidence-graph-iife.sh`, which rebuilds the vendored verifier, and `check-html-render.sh`, which opens the report example's page in headless Chrome |
| `third_party/unicode/` | Unicode's confusables data, vendored unmodified with its terms of use |

## Profile setup

These identifiers select different layers:

| Setting | Meaning |
| --- | --- |
| `--name` | Local profile name. Commands that operate on a configured target select it with `--profile`; `profile show` takes it positionally. |
| `--type` | Storage backend: `jsonl`, `sqlite` or `mysql`. The three are peers: each holds both the artifact store and the log (for `jsonl`, the log is the profile's evidence book). |
| `--jsonl-path` / `--sqlite-path` / `--mysql-database` | Where the storage lives for the selected type: a JSONL directory, a SQLite database file, or a MySQL database. |
| `--namespace` | Artifact SDK logical grouping within `capsule_store_capsules` and `capsule_store_artifacts`. Records are addressed by namespace and Capsule ID. Not a database/schema or an authorization boundary. |
| `--log-id` | CLL log within shared `cll_*` tables, not a table name. |
| `--trusted-key` | Independently provisioned Ed25519 producer public key used to verify Capsule signatures. Not a password or a private signing key. Never trust a key merely because the artifact supplies it. |

One store (a JSONL directory, a SQLite file or a MySQL database) can contain both
`example-investigation` and `evaluations` namespaces in the same artifact tables. Storage
permissions (database grants, file modes), not namespace names, control access.

For profile `example-investigation`, the default file is
`~/.config/capsule/profiles/example-investigation.yaml`, or
`$XDG_CONFIG_HOME/capsule/profiles/example-investigation.yaml` when `XDG_CONFIG_HOME` is set.
Use `capsulectl profile show example-investigation` to inspect it with secrets redacted.
Use `capsulectl profile list` to list configured profile names without reading
or displaying their contents. Listing discovers filenames only; `profile show`
validates the selected file and its owner-only permissions.

Profiles live in `$XDG_CONFIG_HOME/capsule/profiles/NAME.yaml`, falling back to
`$HOME/.config/capsule/profiles/NAME.yaml`. Files must be owner-only regular files
(mode 0600). Creates never overwrite existing files; updates replace atomically.
Profile show redacts literal secrets. Use protected file or explicitly named
environment references instead of literal secret flags, which may leak through
shell history and process listings.

A SQLite or JSONL profile needs no server (see the
[quickstart](#quickstart-no-database-server)). A MySQL profile:

```bash
capsulectl profile create --name evaluations \
  --type mysql --namespace evaluations --log-id evaluation-results \
  --mysql-host db.example --mysql-database capsules --mysql-user publisher \
  --mysql-password-file /protected/mysql-password \
  --signing-key-file /protected/producer-seed.hex \
  --trusted-key <independently-provisioned-producer-public-key-hex>
capsulectl store init --profile evaluations
```

Signing files contain a 32-byte Ed25519 **seed** encoded as 64 hex characters.
Public trust pins are independently provisioned 32-byte Ed25519 keys in hex,
not keys learned from the Capsule being verified. A publishing profile must
explicitly include its own producer signing key in `trusted_keys`; publish
rejects a missing pin before connecting. Offline seal does not require a pin.
Checkpoint signing and trust
are separate from producer signing and trust.

Equivalent MySQL profile before store initialization (a SQLite or JSONL profile
has `type: sqlite` or `type: jsonl` and only `connection.database`, set to the file
or directory path):

```yaml
name: evaluations
type: mysql
namespace: evaluations
log_id: evaluation-results
connection:
  host: db.example
  port: 3306
  database: capsules
  tls: 'true'
credentials:
  username: publisher
  password:
    file: /protected/mysql-password
signing:
  file: /protected/producer-seed.hex
trusted_keys:
  - <producer-public-key-hex>
checkpoint:
  signing:
    file: /protected/checkpoint-seed.hex
  trusted_keys:
    - <checkpoint-public-key-hex>
  endpoint: https://checkpoint.example
  public_key: <independently-provisioned-witness-public-key-hex>
  token:
    env: CHECKPOINT_TOKEN
```

Each secret supports exactly one of `value`, `file`, or `env`. Flag parity:
`--mysql-password[-file|-env]`, `--signing-key[-file|-env]`,
`--checkpoint-signing-key[-file|-env]`, `--checkpoint-token[-file|-env]`, `--token[-file|-env]`.
Other configuration flags include `--trusted-key`, `--checkpoint-trusted-key`,
`--checkpoint-endpoint`, `--checkpoint-public-key`, `--mysql-tls`, `--read-only`, `--url`.
To change secret source on update, explicitly clear the previous source flag;
conflicting sources are rejected instead of silently taking precedence.

JSONL, SQLite, and MySQL are peer backends (the same three
[cll-go](https://github.com/action-state-group/cll-go) provides); each profile
selects one with `--type`:

- JSONL is an inspectable, single-writer append-only journal. `--jsonl-path`
  sets `connection.database` to a directory holding `artifacts.jsonl` and
  `book/`, the profile's evidence book, which is its one log: `publish`,
  `cll append`, `cll list`, `cll checkpoint`, the bundle verbs and the book
  verbs (`close`, `reconcile`, `request`, `respond`) all use it, and it needs
  `--operator`: `profile create` and `profile update` refuse a jsonl profile
  with a log_id and an empty one. No host, port, or TLS. `cll list` keeps the same fields
  (`capsule_id` is the published capsule) and lists only entries that record a
  capsule; `--all` also lists the book's internal records. `cll list` and
  `cll checkpoint status` read the book's files without opening it, so a
  read-only profile needs no signing secret. A JSONL store made by an earlier
  capsulectl keeps its log in `cll.jsonl`; `store migrate` moves those entries
  into the book once, in order, always under a new `--log-id`, and every other
  command refuses the profile until it has.
- SQLite uses WAL transactions and supports multiple handles in one process.
  Artifact and CLL data share one file, set with `--sqlite-path`.
- MySQL uses transactional row locking and supports multiple processes.
  Artifact and CLL data share one database.

A `mesh-plugin` profile holds no storage. It points `--url` at a mesh node's
management API (for example `http://127.0.0.1:3131`), and the read-only `book`
verbs ask another party's book through that node's evidence-request/1 tool:

```
capsulectl profile create --name my-node --type mesh-plugin --url http://127.0.0.1:3131 --requester-id <node-peer-id>
capsulectl book head    --profile my-node --party <peer-id> [--responder-checkpoint-key <hex>]
capsulectl book get     --profile my-node --party <peer-id> --capsule-id <id>
capsulectl book list    --profile my-node --party <peer-id> --selector <id1..id2> [--page-size N] [--page-token T]
capsulectl book request --profile my-node --party <peer-id> --request request.json
```

`--requester-id` is the node's own full peer id (64 lowercase hex; not the
short id its status shows). Each request names it as `requester_id` unless the
request already names one. The node would otherwise add its own id before
forwarding, and the party would digest a request other than the one sent, so
its refusal would not bind. capsulectl never derives the id: a profile without
one sends nothing until the request names a requester.

Each verb sends the request in canonical form: sorted keys, compact,
printable ASCII, 64-bit integers only. That is the form the party digests
after the node re-encodes it; a request that cannot be sent that way is
rejected before anything is sent. Each verb prints the party's answer
(`response`, re-encoded compactly, so check its signed fields rather than
hashing it) and the SHA-256 of the request it sent (`request_digest`), and
classifies the answer:

- An `artifact` must answer the subject kind that was asked. Its bundles are
  carried, not verified: verify them with the party's keys before relying on
  them. `book get` also checks the bundle's stated `capsule_id`.
- A `refusal` is accepted only if its Ed25519 signature verifies offline and it
  names that exact request. That proves the holder of its `signer` key refused
  this request. It does not prove who that key belongs to: pass `--responder-key`
  (the party's signing key, obtained independently) and a refusal under any other
  key is rejected, and `signer_pinned` is true.

`book head` verifies the signed statement of the newest checkpoint the party
reports and checks every reported field against it. It cannot tell whether the
party is withholding a newer one. With `--responder-checkpoint-key`, it rejects
a head signed by any other key.

An optional node token (`--token[-file|-env]`) is sent as a bearer token only
over https or to a loopback address, and redirects are never followed.
Profiles of this type cannot `store`, `publish` or list a log; those verbs
reject them.
Several profiles may share a physical database or reference the same log. The CLI
does not bind a log to a single artifact namespace; callers must consistently
select the intended namespace when writing and reading a log's artifacts.
Log IDs are restricted to lowercase ASCII letters/digits and `._:/-`, starting
with a letter/digit (maximum 191 bytes): this avoids collation aliases in the
current CLL MySQL schema. Namespace and profile names use letters/digits, `_`,
and `-`, maximum 64 characters. For MySQL, TLS defaults to verified TLS (`true`);
explicit `false` is intended only for isolated local development. No insecure TLS
fallback. SQLite and JSONL profiles are local files and take no TLS setting.

`store init` provisions only the configured artifact SDK and CLL schemas.
Initialization is idempotent. The CLI owns no database tables. Profiles select
the database, artifact namespace, and log ID directly. Existing SDK storage
can be used without CLI initialization. Retries must use the intended target
and signing configuration.

## Seal request and stored artifact

`seal` is offline and creates a new mode-0600 artifact file. The JSON request
uses emit.Input's exported field names for Capsule metadata; fields are validated
by the emitter. The outer request schema is application neutral:

```json
{
  "spec_version": "capsule-seal-request/v1",
  "capsule": {
    "ActionID": "evaluation-42",
    "ActionType": "fyi",
    "Operator": "example-operator",
    "Developer": "example-developer",
    "Timestamp": "2026-09-08T12:00:00Z"
  },
  "payload": {"case": 42, "judgment": "supported"},
  "agent_output": {"next_action": "inspect configuration"}
}
```

Payload and agent output are optional, independent JSON values. Omission differs
from explicit JSON null. Their original JSON bytes are retained with explicit
SDK digest bindings. Optional `model`, `runtime`, and `artifacts` fields support
emitter model metadata and additional SDK artifact records, including effect
preimages. Duplicate artifact names, inconsistent commitments and invalid
signatures fail closed. Composition and selective disclosure are not CLI v1
request forms.

An artifact file is the SDK's `artifact.Record` JSON: `capsule_id`, `capsule`,
`producer_envelope`, and `artifacts`. Byte fields use JSON base64, preserving exact
sealed bytes and exact originals. It is **not** raw Capsule JSON alone. CLL entries
contain only the decoded 32-byte Capsule ID and ordered position/time.

`verify --capsule` also reads a bare Agent Action Capsule, as capsule-emit seals
it: the capsule's own fields with an inline `signature` (the hex COSE_Sign1
producer envelope over `capsule_id`) and `key_id`. The file's shape is read from
its members, never guessed, and named in the output (`shape`: `artifact-record`
or `capsule`); a file with the members of both, or of neither, is refused. A
bare capsule gets the same checks: its `capsule_id` recomputed, its signature
under its `key_id`, and that key held to the profile's trusted keys. It carries
no retained originals, so its committed digests are not rehashed, and the
output says so.

`get` uses the SDK directly, including signature trust, inventory integrity and
bound-original verification. Reading artifacts requires only read access to the store (SELECT on the artifact SDK tables for MySQL; read permission on the file or directory for SQLite and JSONL).
It does not open CLL or require private signing keys.
`--output` writes readable JSON by default, or the exact-byte SDK record with `--raw`; stdout adds `spec_version` alongside the selected representation's fields. Artifact ordering is not meaningful: use names.
Unbound attachments are explicitly not producer-authenticated original content.

### Readable output and exact-byte export

`get` decodes byte fields by default: JSON becomes a JSON value, UTF-8 text
becomes a string, and other binary data becomes an object with `encoding` set
to `base64` and `data` containing the encoded bytes. This applies to both
stdout and `--output`. Artifact binding and retention metadata remain visible.

Use `get --raw --output ARTIFACT.json` for the original SDK record accepted
by `verify` and `cll append`. Readable output is a display representation, not
a round-trip format for signed bytes. `seal --output` remains an exact-byte
SDK export. No stored bytes, digests, or Capsule IDs change.

```bash
capsulectl get --profile example-investigation --capsule-id <capsule-id> | jq '.capsule'
capsulectl get --profile example-investigation --capsule-id <capsule-id> |
  jq '.artifacts[] | select(.name == "payload") | .content'
```

## Example: read a deployment's investigation records

Replace the placeholders with the deployment's database connection, configured
`aac.log-id`, and independently obtained producer public key. The artifact
namespace must match the writer's namespace. This does not trigger an investigation.

```bash
chmod 600 /protected/example-investigation-db-password

./capsulectl profile create \
  --name example-investigation \
  --type mysql \
  --namespace example-investigation \
  --log-id <investigation-log-id> \
  --mysql-host <investigation-db-host> \
  --mysql-database example_investigation \
  --mysql-user <read-only-db-user> \
  --mysql-password-file /protected/example-investigation-db-password \
  --trusted-key <producer-public-key-hex> \
  --read-only

./capsulectl profile show example-investigation

./capsulectl get \
  --profile example-investigation \
  --capsule-id <capsule-id> \
  --raw --output investigation-artifact.json

./capsulectl verify \
  --profile example-investigation \
  --capsule investigation-artifact.json

jq -r '.artifacts[] | select(.name == "payload") | .content' \
  investigation-artifact.json | base64 --decode | jq .
```

The password file contains the database password. The trusted key is the 64-hex
Ed25519 public key, not the private seed. A reader needs neither a signing key nor
`store init`: do not initialize storage just to read existing artifacts. The
exported file is an SDK artifact record, not plain investigation JSON; the last
command decodes its retained payload. Decoding alone is not verification.

For a tunnel, use its local host and `--mysql-port`. Verified TLS must still
match the database certificate; do not disable verification merely for a tunnel.

This read-only profile can run `cll list` against an existing log and read
checkpoint status from CLL witness rows without initialization. It cannot
append, publish, create checkpoints, or initialize storage.

## Example: a self-checking report of records you sealed

Turn records you sealed into one portable report: a bundle (`bundle.json`) and a
self-contained page (`report.html`) that checks itself when opened, with no network.
The report is itself a sealed record, a `report/v1` root whose rows cite the records,
so the page shows each row with the record behind it.

Use a **SQLite** profile with a log. Its bundles carry the sealed records themselves.
A JSONL profile's bundles carry its evidence book's records about them instead, so a
report root's rows cannot be shown from one.

Run this in an empty directory (it needs `jq`). It is the script CI runs:

```bash
public=$(capsulectl key generate --output ./seed | jq -r .public_key)
capsulectl profile create --name example --type sqlite --sqlite-path ./store.db \
  --operator "Example Operator" --signing-key-file ./seed --trusted-key "$public" \
  --log-id example-log --checkpoint-signing-key-file ./seed --checkpoint-trusted-key "$public"
capsulectl store init --profile example

# Two records, each a capsule-seal-request/v1 file; publish prints its capsule_id.
cat > check-config.json <<'EOF'
{"spec_version": "capsule-seal-request/v1",
 "capsule": {"ActionID": "check-config", "ActionType": "fyi", "Operator": "example-operator",
             "Developer": "example-developer", "Timestamp": "2026-10-07T00:00:00Z"},
 "payload": {"check": "config file", "result": "same"}}
EOF
cat > check-permissions.json <<'EOF'
{"spec_version": "capsule-seal-request/v1",
 "capsule": {"ActionID": "check-permissions", "ActionType": "fyi", "Operator": "example-operator",
             "Developer": "example-developer", "Timestamp": "2026-10-07T00:00:00Z"},
 "payload": {"check": "permissions", "result": "different"}}
EOF
config=$(capsulectl publish --profile example --request check-config.json | jq -r .capsule_id)
permissions=$(capsulectl publish --profile example --request check-permissions.json | jq -r .capsule_id)

# The report root cites each record twice: in capsule.References, so the bundle
# carries the record, and in its row, so the page shows it.
jq -n --arg config "$config" --arg permissions "$permissions" '
  def cite($id): {type: "agent-action-capsule", digest_alg: "sha256", digest: $id, citation_purpose: "acted_on"};
  {spec_version: "capsule-seal-request/v1",
   capsule: {ActionID: "report", ActionType: "fyi", Operator: "example-operator",
             Developer: "example-developer", Timestamp: "2026-10-07T00:01:00Z",
             References: [$config, $permissions] | map({Type: "agent-action-capsule", DigestAlg: "sha256",
                                                        Digest: ., CitationPurpose: "acted_on"})},
   payload: {spec_version: "report/v1", title: "Example checks", rows: [
     {row_id: "config", label: "Config file", status: "same", reason: "unchanged since the last run",
      references: [cite($config)]},
     {row_id: "permissions", label: "Permissions", status: "different", references: [cite($permissions)]}]}}' \
  > report-request.json
report=$(capsulectl publish --profile example --request report-request.json | jq -r .capsule_id)

capsulectl cll checkpoint create --profile example
capsulectl disclose --profile example --root "$report" --out bundle.json --html report.html
capsulectl verify --bundle bundle.json
```

**What `verify --bundle` checks.** It says `VALID` (exit 0) when all of these check out:

- the checkpoint's signature;
- the interval coverage;
- each record's place in the log;
- the citation closure;
- the records' signatures;
- every disclosed payload against what its record sealed.

A record or payload changed after sealing makes it `INVALID` (exit 1). `INCOMPLETE`
(exit 3) names what the bundle does not show.

**What VALID rests on.** It means the signatures verify under the keys the bundle
itself carries: each record's `key_id`, and the key in the checkpoint's signed
statement. It does not mean they are keys you already trust. To trust the signer,
compare those keys with the producer's public key, obtained some other way:

```bash
jq -r '.records[].key_id, .checkpoint.key_id' bundle.json | sort -u   # here: "$public"
```

This example has no witness receipt. That does not lower the verdict, and nobody
but the producer has vouched for the checkpoint.

**What the page checks.** In the browser, the page checks the records, the disclosed
payloads, the citation closure and the log proofs. It does **not** check:

- the checkpoint's signature;
- the records' signatures.

It says "Bundle verification passed" for a bundle that `verify --bundle` calls
`INCOMPLETE` too. Run `verify --bundle` for the verdict.

**Order matters.** Publish everything and cut the checkpoint before you disclose:
a bundle proves the records its checkpoint covers. Each `disclose` puts the
disclosure on the log as a sealed capsule (`action_id` `capsulectl-disclosure`,
its input the disclosure record: ids, digests, labels and a random nonce, never
a disclosed byte). A later report carries that capsule, proven in the log, with its input
withheld, so one party's copy never shows what was disclosed to another.

## Publication and recovery

`cll append` accepts an existing artifact file, persists it through the SDK,
then appends its Capsule ID. Missing required originals prevent a new append.

`publish` seals the supplied request, persists the complete record through the
artifact SDK, then calls CLL's identity-idempotent append using the Capsule ID.

For retries, retain the complete request (including ActionID and Timestamp),
signing configuration and target. Repeat the same command. A lost append response
is safe to retry: CLL returns the existing sequence for that Capsule ID. Changed
requests are new publications, not conflicts against an earlier business key.
There is no background recovery guarantee or atomic transaction across storage
and CLL. A failed delivery leaves the persisted artifact available for retry.
Purged originals are never recreated; publish fails rather than bypassing the
artifact SDK's retention checks. Use CLL lookup to inspect prior delivery.

## Checkpoints and verification

Checkpoint creation uses cll-go's runner, signed COSE representation, MMR state
and durable witness rows. The returned `checkpoint` identifier is **MMR size**,
not sequence count. Each checkpoint commits only the selected log's prefix.
CLL persists exact checkpoints and a witness identity derived from the HTTPS
endpoint and pinned witness key. Status and publish select that existing witness
row and verify its checkpoint signature, trusted signer, log and MMR size.
Endpoint/key changes cannot redirect an old delivery: the new target has no row
for that checkpoint, so the command returns not-found (exit 1). Credential
rotation does not change the target. No HTTP success is treated as witnessing:
cll-go verifies the receipt and persists retry/backoff state. Errors persisted by
the submitter suppress service bodies, URLs and credentials.

The latest CLL checkpoint and the statement returned by `checkpoint create`
are available without a configured witness.

`cll verify` takes JSON with `checkpoint` (base64 signed bytes), optional
`capsule_id`, zero-based `leaf_index`, and `path` (array of base64 nodes). It verifies
the profile's expected log, trusted signer, embedded consistency proof and optional
inclusion through cll-go. It does not prove producer signatures or business truth.
No trusted external prefix/time is inferred merely from a self-consistent checkpoint.

`verify --bundle BUNDLE.json` checks an Evidence Bundle (`evidence-bundle/v2`,
draft-mih-zhang-agent-disclosure-bundle-00) offline, from the file alone. No
profile and no network are used. It checks:

- every record's identity;
- every record's producer signature (`signature`: the hex producer envelope;
  `key_id`: the signer's key);
- citation closure to the declared depth;
- the checkpoint: `checkpoint.cose` must verify, and every signed field the
  file states (`log_id`, `mmr_size`, `root`, `key_id`, `timestamp`,
  `prev_size`, `prev_root`) must equal its signed value;
- interval coverage and each record's inclusion under that checkpoint;
- disclosures.

It prints each claim's status and the bundle digest. A `composed/v1` extension is verified
(composed digest, each member, closure, joins) and counts toward the verdict; any other
extension, and any countersignature, is listed as carried but not checked. Exit codes:

- 0: every claim passed;
- 3: nothing failed, but something is not shown (no checkpoint signature, an
  unsigned record, declared-missing citations);
- 1: a claim failed (an edited record, a signature that does not verify, a
  checkpoint field that differs from its signature).

**A deal receipt's text is sealed with it.** A deal receipt's readable text
(the summary lines, each step's line, the amounts and what was told) is sealed
as a record on the deal's log when the receipt is written. The bundle's
`x-deal-v0` extension names that record (`sealed_report`), and its `extensions`
entry is `pass` when the record's disclosed text matches. Editing the text,
removing the extension, naming another record, or replacing the pointer with
text (even with the record's disclosure dropped) makes the bundle INVALID
(`sealed_report_unverified`, `sealed_report_not_named`): a bundle that holds a
sealed report must name it. Each copy seals its own
text and discloses no other copy's. A receipt written before this change carries
its text in the extension itself: `uninterpreted`, with the finding
`extension_unbound`, and its page says the text is not checked.

**composed/v1.** A bundle carrying the `composed/v1` extension
(draft-mih-zhang-agent-disclosure-bundle-01 §7.2: one evidence set built from
several responders' answers to Evidence Requests) is checked too, through the
agent-action-capsule Go library (v0.7.0). Its `extensions` entry has `status`
(`pass`, `withheld` or `fail`) and a `composed` object reporting, separately:

- `composed_digest`: the digest recomputed from the block's declarations, the
  declared one, and whether they match;
- `members`: each member's `outcome` (`artifact`, `refusal` or `absence`),
  `body` (`carried`, `declared_missing` or `absent`), `digest` (`reproduced`,
  `mismatch` or `not_shown`), and, for a carried member bundle, `bundle`: that
  bundle's own full report (its own verdict and claims, never merged with the
  containing bundle's). A refusal's signature is checked when it carries
  `key_id` and `signature` (`ed25519-jcs`: Ed25519 over the JCS of the refusal
  without `signature`); any other refusal is `signature_unverified`, not failed;
- `composition_closure`: `pass`, `withheld` (`declared_incomplete`) or `fail`;
- `joins`: each join's `declared` and re-`derived` state and `result`
  (`derived_matches`, `join_state_mismatch` or `not_derivable`), with each
  member's value at each differing pointer on a derived mismatch;
- `corroboration`: per join, `redundant` (with `reason`: same observer, custody
  domain or key) and the report "redundant, not corroborating", `corroborating`
  (qualified `custody_declared`), or `not_applicable`.

A failed block or member bundle makes the verdict `INVALID`; a declared-missing
member, a non-derivable join or an unverified refusal signature makes it
`INCOMPLETE`. Composition closure covers the declared members only; it is not
completeness of participation.

**Witness receipts.** A checkpoint can carry witness receipts
(`checkpoint.witnesses`). With `--witness-directory WITNESSES.json` (capsule-emit's
`witnesses.json` format: each row names a witness endpoint and its keys), each
receipt is checked against the signed checkpoint under its witness's row, and
reported under `witnesses`:

- `pass`: it verifies under a key in the directory;
- `withheld`: the file carries none, the directory has no row or no key for its
  witness, the row's binding isn't `cll`, or the checkpoint itself did not
  verify;
- `fail`: it does not verify, or is malformed.

The CLI privileges no witness. Without `--witness-directory` no receipt is
checked, so receipts are `withheld`.

capsulectl checks receipts from `cll` witnesses only. A receipt whose row is a
`rekor` or `scrapi` binding is `withheld` here. capsule-emit's verifier checks
those too, so the two can differ on such a file: there it can pass, or fail
and make the file INVALID, where capsulectl reports it as not checked. The bundle draft defines no witness member,
so only a failing receipt changes the verdict; a file without one is judged as
before.

Its verdicts agree with capsule-emit's offline bundle verifier, given the same
witness directory and no `rekor` or `scrapi` receipts. Bundles from `capsulectl bundle` carry the checkpoint's
signed fields and `checkpoint.cose`, and each record's producer signature
inline.

### Example: publish a checkpoint to the witness and confirm it landed

`witness.agentactioncapsule.org` is the live Action State Group Transparency
Service; its `POST /checkpoints` route countersigns a signed CLL checkpoint and
returns a receipt. Configure the witness on the publishing profile. Two distinct
keys are involved: your own checkpoint signer (its public key must be pinned in
`--checkpoint-trusted-key`, mirroring producer trust) and the witness authority
key (`--checkpoint-public-key`). The witness key must be **independently
provisioned** and pinned out of band; do not trust a key merely because the same
host serves it (`GET /anchor/authority-pubkey` is a distribution convenience, not
a trust root: fetching a receipt and its verification key over one untrusted
connection establishes nothing).
A witness directory (the `witnesses.json` format) is one such out-of-band
source: an Ed25519 witness's `key_ids` entry is its raw public key in 64 hex,
which is exactly the `--checkpoint-public-key` value. The endpoint and its key
are set together: `profile create`/`update` refuse an endpoint without a
well-formed key (rather than saving a profile whose every witnessed checkpoint
would fail), and `doctor --check-witness` reports a missing or malformed key as
a failure.

```bash
chmod 600 /protected/checkpoint-seed.hex

# One-time: add checkpoint signing and the witness target to an existing profile.
./capsulectl profile update \
  --profile evaluations \
  --checkpoint-signing-key-file /protected/checkpoint-seed.hex \
  --checkpoint-trusted-key <your-checkpoint-public-key-hex> \
  --checkpoint-endpoint https://witness.agentactioncapsule.org \
  --checkpoint-public-key <pinned-witness-authority-public-key-hex>
  # add --checkpoint-token-env WITNESS_TOKEN only if the service requires a bearer token

# Cut a checkpoint at the current MMR tip; record the returned MMR size.
./capsulectl cll checkpoint create --profile evaluations
# => {"spec_version":"capsule-cli-result/v1","checkpoint":<MMR_SIZE>,...}

# Submit that checkpoint to the witness and verify the returned receipt.
./capsulectl cll checkpoint publish --profile evaluations --checkpoint <MMR_SIZE>
```

The checkpoint identifier is the **MMR size** printed by `create`, not an entry
count. Configure the witness on the profile *before* the checkpoint is first cut:
`create` only enrolls a witness row for the checkpoint it actually cuts, so if the
log tip was already checkpointed (for example by an earlier `create` without a
witness), `create` returns that existing checkpoint without a row for this
service and `publish --checkpoint <that size>` then reports not-found (exit 1).
In that case append a new entry and `create` again so a fresh checkpoint carries
the witness row. `publish` submits the exact signed checkpoint, then cll-go verifies the
returned receipt against the pinned witness key and persists it. Delivery is not
"witnessed" merely because the HTTP call returned 200: a receipt that fails
verification, or a non-receipt response, leaves a durable retry row instead of a
success, and service bodies, URLs, and credentials are suppressed from errors.
Exit 4 (`pending`) means delivery is still in flight; re-run the same command to
continue. Byte-identical resubmission is idempotent on the service, and rotating
the endpoint or key cannot redirect an existing delivery.

Confirm the checkpoint is on the service with no further network call:

```bash
./capsulectl cll checkpoint status --profile evaluations --checkpoint <MMR_SIZE>
# => {"spec_version":"capsule-cli-result/v1","state":"verified","attempts":...,"receipt":{...}}
```

`status` re-verifies the persisted receipt (witness signature, pinned witness
key, log ID, and MMR size) from the CLL witness row, so `"state":"verified"` is
evidence the witness stamped this exact checkpoint rather than a cached success
flag; it reads `pending` before a verified receipt and `failed` after a permanent
rejection. A read-only profile that carries the endpoint and pinned witness key
(but no checkpoint signing key) can run `status`; only a profile with checkpoint
signing can `create` and `publish`. The receipt proves external registration of
the checkpoint statement; it does not by itself establish trusted time or stream
continuity.

## Output and known limitations

Successful stdout is one JSON object:
`{"spec_version":"capsule-cli-result/v1",...}`.
Diagnostics never expose raw driver/config/service errors. Exit codes: 0 success,
1 operational failure, 2 invalid input/profile, 3 partial verification, 4 durable delivery pending,
5 frozen input/target conflict, 6 canary alarm (`canary watch`), 7 paused for the user (a deal step that needs the user's approval first). Inspect the exit code, not only a result object.
Verification lists passed/not-performed checks instead of calling all evidence valid.

Only `store init` calls CLL `Init` and provisions selected library schemas.
Other commands call CLL `Open`, which does not execute DDL or insert metadata.
Read-only profiles allow artifact reads, CLL listing, checkpoint status, and
offline verification; write commands fail before connecting. Missing storage
is an error, never a reason to initialize implicitly.


The pinned CLL dependency allows unrelated application tables to coexist.
It still validates actual CLL state and does not migrate or remove old tables.

The book verbs place records in a period by the book's commit time, which
never goes backwards. When this machine's clock falls behind the book's last
record by less than the profile's `clock_tolerance` (default 5m, at most 1h),
new records are committed at that last time instead, so a record appended up
to an hour before a period ends can be counted in the next period's Close. A
period that such records would leave without any exchange is refused rather
than closed with zero tallies; close the next period with `--since-last` to
count them. A larger gap refuses writing until the clock passes the book's last
record, or until a new log is started.

Checkpoint service success conformance, broad fault-injection coverage and peer
review status are tracked in the implementation handoff, not implied by compilation.
Automated tests use isolated local services, never production.

## Tests

```bash
go test ./...
go vet ./...
CAPSULE_CLI_TEST_MYSQL_PORT=<disposable-local-port> go test -race -timeout=15m ./...
```

Integration tests use only `127.0.0.1` and database `capsule_cli_test`, isolated
namespaces/logs per run. Tests use testify assert/require, real emitter/artifact/CLL
libraries and a dedicated MySQL container. Never point them at production.

## Continuous integration

Run `make test` for the complete suite, including MySQL integration tests,
under the race detector and then every case without it. Locally this requires Docker: the runner creates a temporary
MySQL 8.4 container on a random loopback port and removes it when finished.
CI supplies its own disposable service via `CAPSULE_CLI_TEST_MYSQL_PORT`.
Only set that variable yourself for a disposable test database, never a
production tunnel. Missing Docker or unavailable MySQL fails the run rather
than skipping integration tests.

GitHub Actions runs on pull requests and pushes to `main`, with manual dispatch
also available. It checks formatting, module-file consistency, vet, and the CLI
build. It also builds the report above and opens its page in headless Chrome
(`scripts/check-html-render.sh`): the page must render the report and say it
verified, and a copy with one payload edited must say it failed. Tests run with the race detector against a disposable MySQL 8.4 service;
`CAPSULE_CLI_TEST_MYSQL_PORT` is set so MySQL integration tests are not skipped.
No production credentials or database tunnels are used.

The artifact storage SDK is maintained and tested separately in
[capsule-emit-go](https://github.com/action-state-group/capsule-emit-go), whose CI
sets `CAPSULE_STORAGE_TEST_DSN` for its own isolated MySQL integration tests.
CLI CI tests integration with the SDK version pinned in `go.mod`.

## GitHub Action: seal a capsule per pull request

`action.yml` at the repository root is a composite GitHub Action, `Seal PR
Capsule`. On a `pull_request` event it builds one capsule from public PR
metadata, anchors it to the free public witness, and posts the `capsule_id`
as a PR comment. It seals — it never gates the PR; a red checkpoint does not
fail the build (see "Failure behavior" below).

**What goes into the capsule, and what never does.** The capsule records:
repo, PR number, head SHA, the PR author's declared GitHub login, and three
sha256 digests — of the PR diff, of the CI jobs' conclusions, and (only if
the PR author opted in) of a prompt. It never records the diff text, PR body,
prompt text, or CI log output — those are digested on the runner by
`scripts/pr-capsule-request.sh` and only the digest crosses into the sealed
Capsule, the witness submission, or the PR comment. See "No text leaves the
runner" below for how that boundary is enforced, not just intended.

**Declaring an agent/model (self-attested).** A PR author who wants the
capsule to record which agent/model produced the change opts in with an
HTML-comment block anywhere in the PR body:

```
<!-- capsule-declare
agent_provider: anthropic
agent_model: claude-sonnet-5
prompt_digest_sha256: 3b1f4e...   (64 hex chars; compute this yourself, e.g.
                                   `sha256sum prompt.txt`; the prompt itself
                                   is never sent anywhere by this action)
-->
```

All three fields are optional and independently unverified — they are the PR
author's own claim, carried through as declared. This is not a gap the action
fails to close: the sealed capsule's own `assurance.attestation_mode` field
reads `self_attested` whenever no stronger (compute-attested or countersigned)
evidence is bound in, so the grade is legible in the capsule itself, not just
in this README. A PR with no `capsule-declare` block seals with no `model`
field at all — an ordinary human-authored capsule.

**Configuring the Action.** Six repository settings are required — none of
them are read by this action from anywhere at run time; an operator with repo
admin access provisions them once:

| Name | Kind | What it is |
| --- | --- | --- |
| `CAPSULE_PRODUCER_SIGNING_KEY` | secret | hex Ed25519 seed, `capsulectl key generate` |
| `CAPSULE_PRODUCER_TRUSTED_KEY` | variable | the matching public key, `capsulectl key show-public` |
| `CAPSULE_CHECKPOINT_SIGNING_KEY` | secret | hex Ed25519 seed, a second `capsulectl key generate` |
| `CAPSULE_CHECKPOINT_TRUSTED_KEY` | variable | the matching public key |
| `CAPSULE_WITNESS_PUBLIC_KEY` | variable | the witness authority's public key — see "Pinning the witness key" |

The producer and checkpoint keys are two distinct keypairs (mirroring
"Checkpoints and verification" above): the producer key signs the Capsule
itself; the checkpoint key signs the CLL checkpoint submitted to the witness.
Public keys are not secret — they are repository *variables*, not secrets;
only the two seeds are secrets.

**Pinning the witness key.** The default `witness-endpoint` is the free public
instance, `https://witness.agentactioncapsule.org`. Its authority key is
published at [`/.well-known/did.json`](https://witness.agentactioncapsule.org/.well-known/did.json)
and its `key_id` at [`/health`](https://witness.agentactioncapsule.org/health) —
but per "Checkpoints and verification" above, fetching a key from the same
host it will verify against is a convenience, not a trust root. Confirm the
key out of band (e.g. against the `capsule-anchor` operator, or a second
independent fetch from a different network) before setting
`CAPSULE_WITNESS_PUBLIC_KEY`. Observed 2026-09-22: `key_id 19a9ab3e02fad55c`,
JWK `x` `ObtlTJ3Ar-HA7e8N7_qmkJm4UYg2ybom4EkVNYQPlrU` (base64url Ed25519), hex
`39bb654c9dc0afe1c0edef0deffaa69099b8518836c9ba26e0491535840f96b5` — reconfirm
before pinning; a stable key rotates rarely, but "observed once" is not
"independently pinned."

**Adopting this in another repo.** `examples/adopt-seal-pr-capsule.yml` is a
copy-paste template. `needs:` only resolves jobs within the same workflow
file, so the seal job must live in (or be added to) whatever workflow already
runs your PR checks — it cannot react to a separate CI workflow without a
`workflow_run` trigger, which this v1 does not implement.

**Verifying a PR capsule.** The PR comment names the `capsule_id`; the
capsule's Class-1 payload and full artifact record are also available as this
action's own outputs (`capsule-path`, `artifact-path`) for the run that sealed
it. Two ways to verify, both offline once you have the bytes:

```bash
# Class-1: recompute capsule_id from the payload alone (agent-action-capsule's own tool).
agent-action-capsule verify capsule.json

# Full chain: producer signature + trust + bound artifacts (this repo's own tool).
capsulectl verify --profile <a profile pinning the producer key> --capsule artifact.json
```

A tampered capsule — any single byte changed in `capsule.json`, including one
hex character of a digest reference — fails both: `agent-action-capsule
verify` reports `capsule_id_mismatch` because JCS-canonicalizing the changed
payload no longer reproduces the carried `capsule_id`, and `capsulectl verify`
reports `capsule_and_artifacts: failed` for the same reason plus a producer
signature that verified over the original bytes, not the tampered ones.

To confirm the witness anchor independently: `capsulectl cll checkpoint
status --profile <profile with the checkpoint keys and endpoint configured>
--checkpoint <mmr_size>` re-verifies the persisted receipt against the pinned
witness key — not merely a cached "200 OK".

**Scope (v1).** Each run seals against a fresh, ephemeral local log — there is
no cross-run continuity or durable per-repo CLL; every PR's checkpoint is a
size-1 (or few-entry) tree witnessed independently. That is sufficient for
"one capsule per PR, anchored, externally checkable" but is not the same
guarantee as a continuously-appended log. Fork PRs are not sealed: GitHub
withholds repository secrets from a fork's `pull_request` event, so there is
no signing key to seal with; this is a documented gap, not a silent one.

**Failure behavior.** The action's own steps fail closed (missing/invalid
input, sealing error) but the seal job does not gate the PR — it seals
evidence about the PR, including a failing PR; it is not itself a required
check. If the witness checkpoint does not reach `verified` (network partition,
witness downtime), the script exits 4 and the capsule is still sealed and
locally verifiable — it is simply not yet anchored.

The request is deterministic in everything but wall-clock: `Timestamp` is the
head commit's own commit time (not the time the action ran), so re-running the
workflow for the same head SHA with unchanged CI job conclusions reproduces
the identical `capsule_id` and is a true retry of the same checkpoint, not a
new capsule — `publish` (see "Publication and recovery" above) is
identity-idempotent on that ID. If a re-run's CI conclusions differ from the
first run's (a flaky test now passing, say), that is new evidence, so it
correctly seals as a *different* capsule with its own PR comment — this is by
design, not a rare edge case to work around.

**No text leaves the runner — how this is enforced, not just intended.**
`scripts/pr-capsule-request.sh` pipes `git diff` directly into `sha256sum`
without ever assigning the diff to a shell variable; the PR body is read only
through the `<!-- capsule-declare -->` block's three named fields, never
digested or stored as a whole; CI results are passed in as job-name to
conclusion pairs (`ci-jobs-json`), never as raw log output. Every value the
action receives from the caller's `github` context flows through a step
`env:` mapping, never inline `${{ }}` interpolation inside a `run:` script —
the pattern GitHub's own docs warn is a script-injection vector when a
PR title or body is spliced into shell text directly.

## Artifact-only and CLL-only profiles

The profile format is unchanged. An artifact namespace enables artifact storage;
a log ID enables CLL. Configure either or both. Existing profiles retain both.
Only filled fields are validated when loading; each command requires its own
facilities. `publish` requires both, and `store init` initializes only the
configured facilities. `seal` and offline verification do not open storage.

```bash
# Add your storage flags to each create command: --type sqlite --sqlite-path FILE,
# --type jsonl --jsonl-path DIR, or --type mysql with its host/database/credential flags.
capsulectl profile create --name artifacts --namespace example-investigation --log-id '' ...
capsulectl profile create --name ledger --namespace '' --log-id investigations ...
```

The namespace defaults to `capsule` for backward compatibility; explicitly
clear it with `--namespace ''` for a CLL-only profile. `cll append --capsule`
verifies the supplied raw SDK record; when no artifact namespace is configured,
it appends only the ID and leaves artifact retention to the caller. It does not
provide a backup of the Capsule or originals. With both facilities configured,
it persists the verified record before appending. The CLI does not bind a
namespace to a log: a caller must select the same namespace it wrote with to
retrieve those artifacts, while CLL-only readers need only the log.
