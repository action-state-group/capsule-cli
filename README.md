# capsule-cli

Standalone Go executable `capsule`, wrapping `capsule-emit-go`, its optional
artifact SDK, and `cll-go`. Applications import those libraries, not this CLI.
No Alchemy/evaluation semantics, database migration tools, selective disclosure,
or implicit login/default profile are included.

## Development status

Implementation and tests are present. Publication is not yet authorized for
this checkout. The artifact SDK dependency is pinned to its published commit;
ordinary `GOWORK=off go build ./cmd/capsule` needs no sibling checkout, local
replacement or Python runtime. Do not commit machine-specific go.work files.

## Commands

```text
capsule profile create --name NAME [configuration flags | --interactive]
capsule profile show --profile NAME
capsule profile update --profile NAME [configuration flags]
capsule store init --profile NAME
capsule seal --profile NAME --request INPUT.json --output ARTIFACT.json
capsule get --profile NAME --capsule-id ID [--output ARTIFACT.json]
capsule verify --profile NAME --capsule ARTIFACT.json
capsule publish --profile NAME --request INPUT.json --idempotency-key KEY
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

`store init` provisions library schemas plus CLI coordination metadata and pins
the existing/generated `store_id` into the profile. MySQL DDL itself is not
transactional; each initialization step is idempotent and log readiness is
recorded last. Normal log operations never invent a missing identity. Initialized
profiles cannot change log ID/namespace through update; create another profile.
Endpoint/credential changes must still resolve the pinned physical store ID.
An independent database clone requires deliberate identity/journal reprovisioning
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
opens CLL or needs private signing keys. `--output` writes the raw SDK record; otherwise stdout
contains it in the result envelope. Artifact ordering is not meaningful: use names.
Unbound attachments are explicitly not producer-authenticated original content.

## Publication and recovery

`publish` uses an authoritative shared MySQL journal keyed by physical store ID,
namespace, log ID and exact idempotency key. The transaction claims the operation
before sealing, writes exact artifact bytes with SDK `PutTx`, and commits their
Capsule ID together. No append happens before that commit. A crash before commit
rolls back the whole claim; after commit, retries load the stored artifact and
never reseal it. Changed input bytes (even whitespace) or signing identity conflict.

Append and journal completion are reconciled against the actual CLL entry. Lost
append acknowledgments are safe because the underlying CLL append is explicitly
identity-idempotent. A process/host/profile-alias retry uses the same shared journal,
not a local cache. Keep the same request file, key and target. `cll append` accepts
an existing artifact file, persists it via the SDK first, then appends its identity.
Missing required originals prevent a new append. If the entry already exists,
retry reconciles completion without appending again or resurrecting purged originals.
If the journal records completion but the log entry is absent, publish returns a
conflict and never recreates missing log history.

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
`{"spec_version":"capsule-cli-result/v1","result":...}`.
Diagnostics never expose raw driver/config/service errors. Exit codes: 0 success,
1 operational failure, 2 invalid input/profile, 3 partial verification, 4 durable delivery pending,
5 frozen input/target conflict. Inspect the exit code, not only a result object.
Verification lists passed/not-performed checks instead of calling all evidence valid.

Current cll-go MySQL `Open` always performs initialization DDL and metadata writes;
it has no exported open-existing/read-only constructor. Consequently **all CLL
commands explicitly reject read_only profiles before connecting**. This is not
solved by secretly escalating permissions or duplicating library SQL. Artifact
`get` and offline verification work independently. Strict read-only CLL range
reading needs an upstream API addition before deployment to such accounts.

The currently pinned CLL dependency also refuses databases containing its legacy
`ledger_metadata` table. Artifact-only get is unaffected. Coexistence requires an
upstream fix; this CLI does not bypass that guard or drop legacy tables.

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
