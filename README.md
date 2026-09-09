# capsule-cli

Standalone Go executable `capsule`, wrapping `capsule-emit-go`, its optional
artifact SDK, and `cll-go`. Applications import those libraries, not this CLI.
No Alchemy/evaluation semantics, database migration tools, selective disclosure,
or implicit login/default profile are included.

## Build from source

```bash
make build
./capsule --help
```

The artifact SDK and CLL dependencies are pinned to published commits.
`GOWORK=off go build ./cmd/capsule` needs no sibling checkout, local
replacement, or Python runtime. Do not commit machine-specific `go.work` files.

## Commands

```text
capsule profile create --name NAME [configuration flags | --interactive]
capsule profile show --profile NAME
capsule profile update --profile NAME [configuration flags]
capsule store init --profile NAME
capsule seal --profile NAME --request INPUT.json --output ARTIFACT.json
capsule get --profile NAME --capsule-id ID [--raw] [--output FILE.json]
capsule verify --profile NAME --capsule ARTIFACT.json
capsule publish --profile NAME --request INPUT.json
capsule cll list --profile NAME --after SEQ [--through SEQ] [--limit 100]
capsule cll append --profile NAME --capsule ARTIFACT.json
capsule cll verify --profile NAME --proof PROOF.json
capsule cll checkpoint create --profile NAME
capsule cll checkpoint publish --profile NAME --checkpoint MMR_SIZE
capsule cll checkpoint status --profile NAME --checkpoint MMR_SIZE
```

Only `profile create`, help and version omit `--profile`. Connection settings
and credentials are accepted only by profile create/update. Each invocation
gets a fresh Cobra/Viper instance. No automatic environment override is enabled.

## Profile setup

These identifiers select different layers:

| Setting | Meaning |
| --- | --- |
| `--name` | Local profile name selected by subsequent `--profile` flags. |
| `--mysql-database` | MySQL database containing the storage tables. |
| `--namespace` | Artifact SDK logical grouping within `capsule_store_capsules` and `capsule_store_artifacts`. Records are addressed by namespace and Capsule ID. Not a MySQL database/schema or an authorization boundary. |
| `--log-id` | CLL log within shared `cll_*` tables, not a table name. |
| `--trusted-key` | Independently provisioned Ed25519 producer public key used to verify Capsule signatures. Not a password or a private signing key. Never trust a key merely because the artifact supplies it. |

One MySQL database can contain both `alchemy` and `evaluations` namespaces in
the same artifact tables. Database permissions, not namespace names, control access.

For profile `alchemy`, the default file is
`~/.config/capsule/profiles/alchemy.yaml`, or
`$XDG_CONFIG_HOME/capsule/profiles/alchemy.yaml` when `XDG_CONFIG_HOME` is set.
Use `capsule profile show --profile alchemy` to inspect it with secrets redacted.

Profiles live in `$XDG_CONFIG_HOME/capsule/profiles/NAME.yaml`, falling back to
`$HOME/.config/capsule/profiles/NAME.yaml`. Files must be owner-only regular files
(mode 0600). Creates never overwrite existing files; updates replace atomically.
Profile show redacts literal secrets. Use protected file or explicitly named
environment references instead of literal secret flags, which may leak through
shell history and process listings.

```bash
capsule profile create --name evaluations \
  --type mysql --namespace evaluations --log-id evaluation-results \
  --mysql-host db.example --mysql-database capsules --mysql-user publisher \
  --mysql-password-file /protected/mysql-password \
  --signing-key-file /protected/producer-seed.hex \
  --trusted-key <independently-provisioned-producer-public-key-hex>
capsule store init --profile evaluations
```

Signing files contain a 32-byte Ed25519 **seed** encoded as 64 hex characters.
Public trust pins are independently provisioned 32-byte Ed25519 keys in hex,
not keys learned from the Capsule being verified. A publishing profile must
explicitly include its own producer signing key in `trusted_keys`; publish
rejects a missing pin before connecting. Offline seal does not require a pin.
Checkpoint signing and trust
are separate from producer signing and trust.

Equivalent profile before store initialization:

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
`--checkpoint-signing-key[-file|-env]`, `--checkpoint-token[-file|-env]`.
Other configuration flags include `--trusted-key`, `--checkpoint-trusted-key`,
`--checkpoint-endpoint`, `--checkpoint-public-key`, `--mysql-tls`, `--read-only`.
To change secret source on update, explicitly clear the previous source flag;
conflicting sources are rejected instead of silently taking precedence.

MySQL is the only supported backend. Several profiles may use MySQL, share a
physical database, or alias one log. Each physical log has one artifact namespace.
Log IDs are restricted to lowercase ASCII letters/digits and `._:/-`, starting
with a letter/digit (maximum 191 bytes): this avoids collation aliases in the
current CLL MySQL schema. Namespace and profile names use letters/digits, `_`,
and `-`, maximum 64 characters. TLS defaults to verified TLS (`true`); explicit
`false` is intended only for isolated local development. No insecure TLS fallback.

`store init` provisions only the configured library schemas plus required CLI coordination metadata and pins
the existing/generated `store_id` into the profile. MySQL DDL itself is not
transactional; each initialization step is idempotent and log readiness is
recorded last. Normal log operations never invent a missing identity. Initialized
profiles cannot change log ID/namespace through update; create another profile.
Endpoint/credential changes must still resolve the pinned physical store ID.
An independent database clone requires deliberate identity reprovisioning
by an operator; automatic clone recovery is not provided.

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

`get` uses the SDK directly, including signature trust, inventory integrity and
bound-original verification. An unpinned reader profile needs only the artifact
tables. If the profile contains an explicit `store_id` (as store init adds), get
also requires SELECT on the CLI identity row and verifies that pin. Neither mode
opens CLL or needs private signing keys. `--output` writes readable JSON by default, or the exact-byte SDK record with `--raw`; stdout adds `spec_version` alongside the selected representation's fields. Artifact ordering is not meaningful: use names.
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
capsule get --profile alchemy --capsule-id <capsule-id> | jq '.capsule'
capsule get --profile alchemy --capsule-id <capsule-id> |
  jq '.artifacts[] | select(.name == "payload") | .content'
```

## Example: read an Alchemy investigation

Replace the placeholders with the deployment's database connection, configured
`aac.log-id`, and independently obtained producer public key. The artifact
namespace must match the writer's namespace. This does not trigger an investigation.

```bash
chmod 600 /protected/alchemy-db-password

./capsule profile create \
  --name alchemy \
  --type mysql \
  --namespace alchemy \
  --log-id <alchemy-log-id> \
  --mysql-host <alchemy-db-host> \
  --mysql-database alchemy \
  --mysql-user <read-only-db-user> \
  --mysql-password-file /protected/alchemy-db-password \
  --trusted-key <producer-public-key-hex> \
  --read-only

./capsule profile show --profile alchemy

./capsule get \
  --profile alchemy \
  --capsule-id <capsule-id> \
  --raw --output investigation-artifact.json

./capsule verify \
  --profile alchemy \
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

This read-only profile can run `cll list` against an existing log without DDL or
CLI initialization. Explicit `store_id` pins are checked when configured. It
cannot append, publish, create checkpoints, or initialize storage. Checkpoint
status reads additionally need existing CLI checkpoint archive tables and
store-identity metadata (`capsule_cli_identity`).

## Publication and recovery

The unreleased CLI no longer accepts `--idempotency-key` or returns the
`idempotency_key` JSON field. `capsule_cli_operations` is unused and may be
dropped after checking for unfinished operations. Initialization does not
silently delete existing tables.

`cll append` accepts an existing artifact file, persists it through the SDK,
then appends its Capsule ID. Missing required originals prevent a new append.

`publish` seals the supplied request, persists the complete record through the
artifact SDK, then calls CLL's identity-idempotent append. It does not maintain
an operation journal or accept a business idempotency key.

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
The CLI archives exact checkpoints and freezes the witness identity derived from
HTTPS endpoint and pinned witness key. Endpoint/key changes conflict with existing
deliveries; credential rotation does not. No HTTP success is treated as witnessing:
cll-go verifies the receipt and persists retry/backoff state. Errors persisted by
the submitter suppress service bodies, URLs and credentials.

`cll verify` takes JSON with `checkpoint` (base64 signed bytes), optional
`capsule_id`, zero-based `leaf_index`, and `path` (array of base64 nodes). It verifies
the profile's expected log, trusted signer, embedded consistency proof and optional
inclusion through cll-go. It does not prove producer signatures or business truth.
No trusted external prefix/time is inferred merely from a self-consistent checkpoint.

## Output and known limitations

Successful stdout is one JSON object:
`{"spec_version":"capsule-cli-result/v1",...}`.
Diagnostics never expose raw driver/config/service errors. Exit codes: 0 success,
1 operational failure, 2 invalid input/profile, 3 partial verification, 4 durable delivery pending,
5 frozen input/target conflict. Inspect the exit code, not only a result object.
Verification lists passed/not-performed checks instead of calling all evidence valid.

Only `store init` calls CLL `Init` and provisions selected library schemas.
Other commands call CLL `Open`, which does not execute DDL or insert metadata.
Read-only profiles allow artifact reads, CLL listing, checkpoint status, and
offline verification; write commands fail before connecting. Missing storage
is an error, never a reason to initialize implicitly.


The pinned CLL dependency allows unrelated application tables to coexist.
It still validates actual CLL state and does not migrate or remove old tables.

Checkpoint service success conformance, broad fault-injection coverage and peer
review status are tracked in the implementation handoff, not implied by compilation.
Automated tests use isolated local services, never production.

## Tests

```bash
go test ./...
go vet ./...
CAPSULE_CLI_TEST_MYSQL_PORT=<disposable-local-port> go test -race ./...
```

Integration tests use only `127.0.0.1` and database `capsule_cli_test`, isolated
namespaces/logs per run. Tests use testify assert/require, real emitter/artifact/CLL
libraries and a dedicated MySQL container. Never point them at production.

## Continuous integration

Run `make test` for the complete suite, including MySQL integration tests and
the race detector. Locally this requires Docker: the runner creates a temporary
MySQL 8.4 container on a random loopback port and removes it when finished.
CI supplies its own disposable service via `CAPSULE_CLI_TEST_MYSQL_PORT`.
Only set that variable yourself for a disposable test database, never a
production tunnel. Missing Docker or unavailable MySQL fails the run rather
than skipping integration tests.

GitHub Actions runs on pull requests and pushes to `main`, with manual dispatch
also available. It checks formatting, module-file consistency, vet, and the CLI
build. Tests run with the race detector against a disposable MySQL 8.4 service;
`CAPSULE_CLI_TEST_MYSQL_PORT` is set so MySQL integration tests are not skipped.
No production credentials or database tunnels are used.

The artifact storage SDK is maintained and tested separately in
[capsule-emit-go](https://github.com/action-state-group/capsule-emit-go), whose CI
sets `CAPSULE_STORAGE_TEST_DSN` for its own isolated MySQL integration tests.
CLI CI tests integration with the SDK version pinned in `go.mod`.

## Artifact-only and CLL-only profiles

The profile format is unchanged. An artifact namespace enables artifact storage;
a log ID enables CLL. Configure either or both. Existing profiles retain both.
Only filled fields are validated when loading; each command requires its own
facilities. `publish` requires both, and `store init` initializes only the
configured facilities. `seal` and offline verification do not open storage.

```bash
# Add your MySQL host/database/credential flags to each create command.
capsule profile create --name artifacts --namespace alchemy --log-id '' ...
capsule profile create --name ledger --namespace '' --log-id investigations ...
```

The namespace defaults to `capsule` for backward compatibility; explicitly
clear it with `--namespace ''` for a CLL-only profile. `cll append --capsule`
verifies the supplied raw SDK record; when no artifact namespace is configured,
it appends only the ID and leaves artifact retention to the caller. It does not
provide a backup of the Capsule or originals. With both facilities configured,
it persists the verified record before appending. A log's artifact namespace is
bound when artifact-backed initialization first occurs; CLL-only readers do not
need to know that namespace.
