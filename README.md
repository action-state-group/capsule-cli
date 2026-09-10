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

MySQL and SQLite are peer backends; each profile selects one. Several profiles
may share a physical database or reference the same log. The CLI does not bind a
log to a single artifact namespace; callers must consistently select the
intended namespace when writing and reading a log's artifacts.
Log IDs are restricted to lowercase ASCII letters/digits and `._:/-`, starting
with a letter/digit (maximum 191 bytes): this avoids collation aliases in the
current CLL MySQL schema. Namespace and profile names use letters/digits, `_`,
and `-`, maximum 64 characters. TLS defaults to verified TLS (`true`); explicit
`false` is intended only for isolated local development. No insecure TLS fallback.

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

`get` uses the SDK directly, including signature trust, inventory integrity and
bound-original verification. Reading artifacts requires only SELECT access to artifact SDK tables.
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

This read-only profile can run `cll list` against an existing log and read
checkpoint status from CLL witness rows without initialization. It cannot
append, publish, create checkpoints, or initialize storage.

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

```bash
chmod 600 /protected/checkpoint-seed.hex

# One-time: add checkpoint signing and the witness target to an existing profile.
./capsule profile update \
  --profile evaluations \
  --checkpoint-signing-key-file /protected/checkpoint-seed.hex \
  --checkpoint-trusted-key <your-checkpoint-public-key-hex> \
  --checkpoint-endpoint https://witness.agentactioncapsule.org \
  --checkpoint-public-key <pinned-witness-authority-public-key-hex>
  # add --checkpoint-token-env WITNESS_TOKEN only if the service requires a bearer token

# Cut a checkpoint at the current MMR tip; record the returned MMR size.
./capsule cll checkpoint create --profile evaluations
# => {"spec_version":"capsule-cli-result/v1","checkpoint":<MMR_SIZE>,...}

# Submit that checkpoint to the witness and verify the returned receipt.
./capsule cll checkpoint publish --profile evaluations --checkpoint <MMR_SIZE>
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
./capsule cll checkpoint status --profile evaluations --checkpoint <MMR_SIZE>
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
it persists the verified record before appending. The CLI does not bind a
namespace to a log: a caller must select the same namespace it wrote with to
retrieve those artifacts, while CLL-only readers need only the log.
