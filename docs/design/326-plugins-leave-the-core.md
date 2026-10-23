# Design note: the book and deal plugins leave the neutral repository

capsule-cli#326. Status: draft for review, round 4. Checked against `main` at
`df68c51` (after v0.1.0-rc21), evidencebook `main` (`v0.7.0`), actionstate-site
`main` (`install/` and `scripts/install-*`) and actionstate-muse `main`.

This note covers how the move happens: placement, inventory, contract,
pipelines, the install path and the order of steps. No code moves and no
repository is created until the note converges. The product boundary is
already ruled: Steven's SEAMS ruling of 2026-10-21, gap 5, says the plugins
leave this repository. This note does not change that boundary.

## 1. The problem, and what is already done

capsule-cli is donation-bound: the `capsulectl` core (verify, bundle
verification, contract validation, proof and checkpoint checks) is open and
migrates to a neutral home (`evidencebook-boundary-2026-09-25.md` section 3,
`licensing-and-packaging-2026-08-12.md` section 1). The `book` and `deal`
plugins depend on evidencebook, which is company OSS. Today they live in
this repository's `plugins/` module, and the deal skill lives in
`skills/deal/`.

Part of the work is already done. Since #236, the plugins are a second Go
module (`plugins/go.mod`), and the core module does not require
evidencebook. CI's step "The core builds without evidencebook" fails if
`go list -deps -test ./...` in the core lists evidencebook, or if the
core's `go.mod` names it. At `df68c51` the core's graph holds four
`action-state-group` modules, all neutral: agent-action-capsule,
capsule-emit, capsule-viewer and checkpointed-local-log.

What still ties the plugins to this repository:

1. **Go's `internal` rule.** The plugins import
   `github.com/action-state-group/capsule-cli/internal/cli` (215 distinct
   identifiers outside tests, 277 with tests) and `internal/settlement` (11).
   Go allows an import from `internal/` only from inside the same path
   prefix, so the plugins compile only while they sit under `capsule-cli/`.
   `plugins/go.mod` builds against this checkout through
   `replace github.com/action-state-group/capsule-cli => ../`, with a
   placeholder require.
2. **One release.** `scripts/release-build.sh` builds
   `capsulectl-<tag>-plugin-{book,deal}-<os>-<arch>` beside the core under
   one `SHA256SUMS` and one attestation. `capsulectl plugin install
   book|deal` fetches them from the core's own release
   (`internal/cli/plugin_install.go`, `pluginReleaseBase`).
3. **The deal skill comes from the tag's source archive.** The install page
   downloads `archive/refs/tags/<tag>.tar.gz` and pins the archive and
   `skills/deal/SKILL.md` (`docs/RELEASING.md`, the box at the top).
4. **Core CI and the core skill use the plugins.** `scripts/test.sh` and
   `ci.yml` tidy, vet and test both modules. `check-html-render.sh`,
   `check-portability.sh` and `presentation-goldens/build-fixtures.sh`
   build both plugins and run `deal init` on
   `skills/deal/demo/retail-checkout`. The `presentation-pass`, golden and
   `interop-at-resolved-versions` jobs run in `plugins/`. The `capsulectl`
   skill documents the book verbs (`skills/capsulectl/spec.yaml` from line
   248: `request`, `respond`, `close`, `reconcile`, `result build`, `report
   build`), and its
   scripted demo runs them on a jsonl book profile, built with
   `plugins/scripts/bookdemo`.

## 2. Placement

| What | Goes to | Why |
|---|---|---|
| `book` plugin, and the book half of the `capsulectl` skill | `evidencebook`, as a nested Go module `github.com/action-state-group/evidencebook/capsulectl` | It is the command-line face of the evidencebook library. Its code is evidencebook calls plus core verbs. |
| `deal` plugin, `skills/deal/` (the whole tree), the deal-only scripts | a new company-OSS repository, proposed name `evidencebook-deal` (section 8) | It is an application on books. Its dependencies (capsule-viewer, the AAC presentation contract), its skill, profile, demos and confusables data, and its release pace don't belong in a developer library. |

**Checked against the code before accepting it.** `book` and `deal` share
one Go package today (`plugins/internal/plugin`: 225 `.go` files, about
43,400 lines outside tests; the book files are about 4,700). The split was
tested by compiling copies of the package with each half removed.

- **Book without deal** has 9 dangling references. `requestCommand` and
  `respondCommand` (`book.go:917`, `:1070`) branch to `dealRequest` and
  `dealRespond` on a deal (sqlite) profile, and the deal plugin mounts the
  same two constructors (`commands.go:119`, `dealEvidenceCommands`).
  `backfill.go` uses `currencyCode` and `guardMetadata`. `commands.go`
  registers both plugins' groups.
- **Deal without book** needs `sealResultRecord` and `checkpointCovers`
  (`resultbuild.go`: seal an Evidence Result into a book, about 60 lines
  over `evidencebook.Book`), the Result-document wrappers in
  `resultschema.go` (over the core's `cli.DecodeResult`), `bookError`,
  `bookNow`, and the two command constructors.

The split works like this:

- `request` and `respond` become one command per plugin. Each keeps the
  flags, input checks (response keys, the witness directory) and output it
  has today. The argument reading they share is evidence-request carrier
  handling, and it goes to evidencebook's library with the Result sealing
  below. Tests on both sides pin the two command trees as they are now.
- The Result sealing is used by both plugins (`book result build`, and the
  deal's sealed terms Result). Its owner is the evidencebook library
  ("deterministic Result construction", boundary section 3), so it becomes
  an exported evidencebook function, released in a minor version. That is a
  new public API commitment in evidencebook.
- `currencyCode`, `guardMetadata`, `bookError` and `bookNow` are a few
  lines each and go with the plugin that uses them. `commands.go` splits
  into one registry per plugin.

Nothing is copied. The split is made inside this repository first (section
7, step 3), so CI proves both full command trees before anything moves.

The alternative of one new repository holding both plugins would avoid the
split. It was rejected because it would put evidencebook's own CLI outside
evidencebook, and the split costs only about a dozen functions.

## 3. Inventory

### 3.1 What moves

| From (capsule-cli) | To |
|---|---|
| `plugins/cmd/capsulectl-book`, the book files of `plugins/internal/plugin` (`book.go`, `booklog.go`, `bookhost.go`, `backfill.go`, `report.go`, `result.go`, `resultbuild.go`, `resultschema.go`, their tests) | `evidencebook/capsulectl/` (its own `go.mod`): `cmd/capsulectl-book`, `internal/book` |
| `sealResultRecord`, `checkpointCovers`, the shared request and response reading | evidencebook library, exported, with tests |
| the book verbs' part of `skills/capsulectl` (spec entries for `request`, `respond`, `close`, `reconcile`, `result build`, `report build`; demo steps 12-15; `references/` text on them), `plugins/scripts/bookdemo`, and `docs/BACKFILL.md` (the book plugin's `backfill`) | `evidencebook/capsulectl`, as that plugin's skill. This is public skill text, so Sherlock2 reviews it. The core skill keeps the core verbs, and its demo runs on a plain `log: cll` profile |
| `plugins/cmd/capsulectl-deal`, the deal files of `plugins/internal/plugin` (`deal*.go`, `action_records.go`, `materiality.go`, `rules_checker*.go`, `confusables_table.go`, `assets/`, their tests and `testdata/`) | `evidencebook-deal`: `cmd/capsulectl-deal`, `internal/deal` |
| `plugins/scripts/genconfusables`, `third_party/unicode/`, `scripts/update-confusables.sh`, `plugins/scripts/engine-e2e` | `evidencebook-deal` |
| `skills/deal/` (`SKILL.md`, `README.md`, `COUNTERSIGN.md`, `AUDIT-HOST.md`, `profile/`, `demo/`, `scripts/`) | `evidencebook-deal/skills/deal/` |
| `presentation-pass`, the deal receipt-page checks of `check-html-render.sh` (`deal report --html`), and the share, export and delete flow of `portability` | `evidencebook-deal` CI |
| `interop-at-resolved-versions`, with `scripts/interop-resolved.sh` and its pinned Python requirements | both plugin repositories. Each resolves its own module graph, which differs from evidencebook's own pins; that difference is why the job exists (`interop-resolved.sh` lines 5-10) |

**Test files.** About 31 test files in `plugins/internal/plugin` exercise
core behavior through a plugin tree: `canary_test.go`,
`doctor_install_test.go`, `doctor_user_root_test.go`,
`corehelpers*_test.go`, `page_gate_test.go`, `epistemic_test.go`,
`fragment_codec_test.go`, `producer_key_test.go`,
`profile_operator_test.go`, `jsonl_storage_test.go` and others. They are
classified one by one in step 3. Tests of core behavior are rewritten
against a stub launcher, as `doctor_install_test.go`'s `dealPluginAt`
already does, and stay in the core, so core behavior is not tested only in a
company repository. Plugin tests that read core files through relative
paths (`binaries_test.go:15`, `presentation_golden_test.go:41`,
`report_test.go:73`) read them from the pinned capsule-cli module instead
(`go list -m -f '{{.Dir}}' github.com/action-state-group/capsule-cli`;
module zips include `testdata/`). That copy is read-only, so nothing writes
there. `presentation_golden_test.go` today rewrites the `page.html`
goldens and snapshots in `internal/cli/testdata/presentation` when its
update variables are set (lines 34-37, 165, 175). After the move:

- `evidencebook-deal` owns the goldens of the pages it builds, and updates
  them in its own tree.
- The core keeps its own committed fixtures and goldens for its own tests
  (`presentation_fixtures_test.go` and others). It updates them only when a
  format or coverage changes, not each time a deal page changes.

So a change to a deal page's data needs no capsule-cli PR. A change to the
page's script or words still does, because `deal-view` and the wording pack
are core code until #428 moves them (sections 3.3 and 4.3).

**Wording key coverage.** `TestEveryWordingKeyTheCodeNamesIsInThePack`
(`narration_test.go:56-74`) globs `../../plugins/internal/plugin/*.go` as
well as the core. Once that directory is gone the glob returns nothing and
the deal half passes silently. So each plugin repository runs the same
check over its own code against `cli.BuiltinWording()` from its pinned
capsule-cli, and the check fails when its file glob matches nothing.
Step 6 removes the plugin glob from the core test.

**History.** capsule-cli stays public and keeps the full history. Each new
home starts from one import commit naming the capsule-cli commit it was cut
from. This is simpler than `git filter-repo`: evidencebook does not allow
merge commits, so an unrelated history cannot arrive by PR; the deal code's
history runs through older paths (`internal/cli`, before #263); and one
commit in that history has no sign-off, which the DCO check would refuse.

### 3.2 Go dependencies: a public host package, not a copy

Each plugin binary is a whole `capsulectl` command tree plus its own
commands (`cli.NewPluginCommand`, `cli.Run`). The core runs a book
profile's `publish`, `cll`, `store init` and `bundle` verbs in
`capsulectl-book`, and `bundle --deal` and `cll checkpoint cadence` in
`capsulectl-deal` (`internal/cli/pluginhost.go`). So the plugins need the
core's command tree and most of its machinery: profiles, the artifact
store, seal, bundle assembly, witness and checkpoint handling, the error and
`voice` output contract, and the wording packs.

Three ways were weighed:

- **Copy the code** into the plugin repositories. Rejected: it would give
  verify-adjacent code a second source of truth.
- **A narrow facade package** re-exporting what the plugins use. Rejected:
  with 215 identifiers it is no narrower than the package. It would be a
  second surface to keep in step, and a new SDK built only to relocate code.
- **Make the two packages public.** Chosen. `internal/cli` becomes
  `github.com/action-state-group/capsule-cli/cli` and `internal/settlement`
  becomes `github.com/action-state-group/capsule-cli/settlement`. The
  package names don't change, so the plugins' diff is the import path. The
  identifiers are already exported for the plugins; only the path changes.

The package documentation says: this is the host package the first-party
plugins build against; before v1 it carries no compatibility promise; a
plugin pins an exact capsule-cli release (section 4.2). Making it public
also exports, into the donation-bound API, the names that exist only for
the company plugins (`DealBundleRun`, `DealCommandGroup`,
`DealTickCommand`, `SetBookBackend`, `OpenDealTarget`,
`CheckDealStandaloneReport`). That is acceptable before v1, and removing
them is a donation blocker, filed as #428 (section 10). When the core moves
to its neutral home, the module path changes once, and each plugin updates
its imports in the same release.

**Plugin-only code in the core.** `deadcode ./cmd/capsulectl` lists 37
functions in `internal/cli` that only the plugins reach. They stay in the
host package, because almost all of them wrap unexported core state (store
locks, delivery state, the evidence-door transport and its answer checks,
`verifyRefusal` and `verifyChainHead`). Moving them would mean
exporting more. The one exception is `scitt_receipt.go`
(`statementEntryHash`, `verifyStatementReceipt`): it has no callers and no
tests, so step 1 deletes it.

`internal/settlement` verifies settlement legs. J.8 places that
verification in AAC as a pending move. Until then it is public here, and the
deal plugin follows it when it moves.

### 3.3 Core code that knows about deals and books, and stays

None of this imports a plugin. Each item either runs a plugin through the
command line, or verifies or renders a portable format.

**Deal formats the core verifies** (`verify_bundle.go`), each with its spec
home:

| Format | Core code | Spec home |
|---|---|---|
| `x-deal-v0` bundle extension: deal steps, the sealed report pointer | `deal_profile.go` (constants), `deal_sealed_report.go` | Agent Deal Records spec, ops u451 `[agent-deal-records-spec]`, registering `x-deal/v0` (SEAMS 3) |
| `x-cadence-witness/v0` chain | `deal_cadence_verify.go` | Agent Deal Records spec (u451) |
| a sale's threads (`sale_threads` in `x-deal-v0`, the transport extension) | `deal_sale_bundle.go` | Agent Deal Records spec (u451) |
| a History (`x-history-selection/v0`) | `deal_history.go` | `personal_history_basic/1` in AAC (SEAMS 6, #264); `x-history-selection/v0` retires one release after |

**Donation gate:** a format that still has no spec text at donation either
gets spec text, or its verify code leaves the core (#428).

**The record definition.** `skills/deal/profile/PROFILE.md` and its
schemas move with the skill for now. Once the Agent Deal Records draft
exists, the spec is the source of `x-deal/v0`, and
`evidencebook-deal`'s `PROFILE.md` cites it, not the other way round.

**Other deal-aware core code:**

| Code | Why it stays for now |
|---|---|
| `deal.go`, `deal_ids.go`, `deal_share.go`, `deal_sale.go`, `deal_rules.go`, `deal_errors.go`, `deal_cadence.go` | reachable from the core's own commands (`deadcode`): verify, bundle, the cadence, `canary`, and the error classes `SafeError` maps for every binary |
| `materiality.go`, `assets/materiality/neutral.json` | the core evaluates a predicate a profile or pack supplies; it holds no deal policy |
| `rules_checker*.go` | the pinned external-check seam the core runs |
| `capsulectl.deal-view/v0` (`assets/deal-view.{js,css,manifest.json}`) | the core's own page module for a deal bundle (#325). It requires `x-deal-v0` and carries product wording, so it is not basic rendering under J.8, which keeps company modules out of capsule-viewer. It leaves the core before donation (#428). Where it goes, the deal plugin as a plugin presentation module or capsule-viewer once `x-deal/v0` is a neutral spec and the product wording is gone, is a boundary question under #428 |
| the deal entries of the built-in wording pack | see section 4.3 |
| `pluginhost.go`: launcher names `book` and `deal`, the three deal hooks, the hand-over of book-profile verbs | the CLI-level contract `docs/PLUGINS.md` documents. It becomes generic before donation (#428) |
| `canary`, the deal-plugin check in `doctor --install-check` | they run deal verbs or check the deal plugin through the command line (#428) |

### 3.4 Core CI and fixtures after the move

- **Fixtures.** The core's render, portability and presentation fixtures
  become committed bundles under `testdata/`, built once with a fixed key
  and a frozen clock (no due dates relative to today), each with a
  `PROVENANCE.json` naming the producing `capsulectl-deal` release and
  commit. `build-fixtures.sh` makes fresh keys and real dates on every run
  (lines 46 and 217-218), so a byte comparison against a fresh build could
  never pass. Committed fixtures change only when a format or coverage
  changes.
  - The core checks that it verifies and renders the committed fixtures,
    including the offline verification of a committed shared copy, which
    is the core's half of `portability`.
  - `evidencebook-deal` CI checks the other direction: the capsulectl it
    pins, and capsule-cli `main`, both verify and render what the newest
    deal build produces.
- **Module checks.** The `go list -deps` check becomes an **allowlist**:
  every `github.com/action-state-group/...` module in the core's graph must
  be one of the neutral modules the check names. A company-OSS module, or
  any new module, fails until someone adds it on purpose. This is #326's
  first acceptance line. The second module's tidy, vet and test steps go
  from `ci.yml` (lines 46-51 and 64-67) and from `scripts/test.sh`
  (lines 74-76).
- **Path check.** CI fails if the `plugins/` or `skills/deal/` directory
  exists, or if a tracked file references `skills/deal/` or a
  `plugins/(cmd|internal|scripts)/` path. The pattern is anchored to those
  repository paths. It exempts vendored files (`evidence-graph.iife.js`, the
  presentation `page.html` goldens), `CHANGELOG.md` and `docs/design/`, and
  it doesn't match URL paths such as `/api/plugins/...`.
- **`scripts/gradestrings`** is a contributor tool, not a CI gate. Step 6
  drops the plugin paths from it (`main.go:46`, `:55`), so it checks the
  core's files. The plugin repositories don't get a port of it in this
  move. Running it there would need explicit inputs (its fixed read of
  `deal-view.js` at lines 109-112 and its path-based classification), plus a
  refusal of an empty scan. That waits until someone needs it.

## 4. The plugin contract and versioning

### 4.1 What stays the same

`cli-plugin/v1` is unchanged on the wire: the same discovery roots, trust
walk, handshake fields, dispatch, `CAPSULECTL_BIN`, `CAPSULECTL_PROFILE`,
`voice` and `say`, and reserved verbs. A plugin built in another repository
is discovered and run exactly like one built here. A plugin still cannot
touch `verify` or `countersign verify`.

### 4.2 Versions

Today a first-party plugin's handshake `version` equals the core's, because
both come from one build. `plugin install` refuses a mismatch, dispatch
warns, and `doctor --install-check` fails (PLUGINS.md, "Versions"). The
check protects something real: a plugin runs core verbs with the core code
it embeds, so a stale plugin would bundle with older core code than the
`capsulectl` that handed the verb over.

Once the plugins release on their own, a plugin binary carries two
versions:

- **`cli.CLIVersion` and `cli.CLICommit`** stay the core version the
  binary embeds. A plugin release build sets them with `-X`: the version is
  the capsule-cli version its `go.mod` requires (`go list -m`), and the
  commit is that version's commit (`.Origin.Hash` from `go mod download
  -json`, as `ci.yml`'s interop job already reads it). Inside capsule-cli,
  `release-build.sh` already sets them to the release tag and commit, so
  builds from here don't change.
- **`cli.PluginVersion` and `cli.PluginCommit`**, two new host-package
  variables, are the plugin's own release tag and commit. Release builds
  set them with `-X`; builds from capsule-cli leave them empty.

The handshake:

- `version` is `PluginVersion`, or `CLIVersion` when that is empty. It stays
  free text.
- A new optional field, `core_version`, is always `CLIVersion`.
  `NewPluginCommand` fills both, so a plugin does nothing extra. The core
  parses the handshake with `json.Unmarshal` (`plugin.go:476`), which
  ignores unknown fields, so older cores ignore it and `plugin_api` stays
  `cli-plugin/v1`.

The first-party check compares `core_version` with the running core's
`CLIVersion`. When `core_version` is missing, it falls back to `version`.
Every plugin released so far (rc21 and older) set `version` to its core's
version, so the fallback keeps their behavior. A plugin built inside
capsule-cli during the transition (an rc22 that still ships the plugins
from here) reports the rc tag in both fields and passes. Otherwise the
rules are unchanged: a release core refuses at install unless
`--allow-version-mismatch`, warns at dispatch, and `doctor` fails; a
development core doesn't check.

**The producer stamp names the deal build.** Sealed deal records name
the producer as `capsulectl <CLIVersion> (<CLICommit>)`
(`deal_rules.go:861`). The schema defines `producer` as "the software build
that sealed this record" (`x-deal-v0.schema.json:167`), and after the move
that build is `capsulectl-deal`. If the stamp kept the core's tag, two deal
releases built against the same core would seal identical stamps, and
sealed records keep them for good. So, once `PluginVersion` is set, the
stamp is `capsulectl-deal <PluginVersion> (<PluginCommit>)`. The schema
already accepts any non-empty name, version and commit (lines 168-172), and
`check_profile.py` checks only that, so this changes a value, not the
format.

The page needs a matching change, because `deal-view.js` (lines 436-445)
compares every step's producer version with `page_version` as capsule-cli
tags, and links capsule-cli's release notes:

- the deal report's `page_built_by` and `page_version` (`deal_report.go:292`)
  take the same stamp;
- deal-view compares versions only between producers with the same name,
  and treats a bare `capsulectl` stamp (records sealed before the move) as
  older than any `capsulectl-deal`;
- the release-notes link comes from the page data the plugin writes (a new
  `release_notes` field), so the core asset names no company repository.

Step 1 makes this change, with tests. The stamp's new value is a one-way
door (section 9).

Every core release still needs a plugin release built against it. This is
RELEASING.md step 6, "re-cut the companion plugin", which already exists
for the Muse plugin. Removing the embedding (a backend called over the
command line instead of linked in) would end that pairing, but it would
redesign how book profiles work. It is not needed for this move.

### 4.3 Wording packs: unchanged by this move

The core's built-in pack (`assets/wording/capsulectl-en.json`,
`capsulectl.wording/v0`, 292 entries) holds mostly deal wording. Some keys
are spoken by core code, some by the plugins, some are built at run time,
and some by `deal-view.js`. Pinning a pack runs in the core, which refuses
a pack that hides a `card.terms.*` term (`narration.go`, `hidingTerms`).
Splitting the pack would need that check to see the deal plugin's keys
across processes.

So the move keeps the pack whole, in the core. Nothing changes for pinning,
for Muse's vendored copy (`wording/capsulectl/PROVENANCE.json`) or for
pages. The cost is a known coupling: a new deal string needs a capsule-cli
PR first, and then a core release, which the plugin pairs with anyway.
Moving the deal words to their owner, with a pin check that still refuses a
hiding pack, is part of #428.

## 5. Release pipelines

Both new homes reuse what capsule-cli's `release.yml` and
`scripts/release-build.sh` already prove:

- a reproducible build (`CGO_ENABLED=0`, `-trimpath`, no VCS stamp, empty
  build id), for linux/amd64, linux/arm64 and darwin/arm64;
- `SHA256SUMS`, and a GitHub build provenance attestation with its bundle
  published as an asset;
- a person approving every run in a deployment environment, with the same
  two release reviewers, and agents never approving;
- the release created as a draft, uploaded, then published, with a
  published release never changed;
- tags that only maintainers can create and nobody can move, and immutable
  releases;
- every Action pinned by commit SHA.

| | `evidencebook-deal` | `evidencebook/capsulectl` |
|---|---|---|
| Trigger | a maintainer pushes a `v*` tag, as in capsule-cli | a maintainer pushes a `capsulectl/v*` tag (Go's tag form for the nested module), and `publish-capsulectl.yml` runs on it. Not through `release.py` and `tag-release.yml`: those handle only manifest-versioned packages and create a published release before the publisher runs, which immutable releases would then lock |
| Settings | `release` and `release-candidate` environments, tag rulesets and immutable releases, applied by its own `scripts/release-settings.sh` | a settings PR in evidencebook: a ruleset for `capsulectl/v*` tags (evidencebook has no tag ruleset today, RELEASING.md "Safeguards"), immutable releases, and an environment with the release reviewers that accepts only `capsulectl/v*` tags |
| Assets | `capsulectl-deal-<tag>-<os>-<arch>`; `deal-skill-<tag>.tar.gz`; `SHA256SUMS`; `capsulectl-deal-<tag>.sigstore.json` | `capsulectl-book-<version>-<os>-<arch>`; `SHA256SUMS`; `capsulectl-book-<version>.sigstore.json` |

**Checks before publishing,** in both pipelines:

- the module's `go.mod` requires capsule-cli at a version that matches the
  release tag policy (`plugin_install.go:38`), with no `replace`, and that
  capsule-cli release exists and is published;
- the built binary's handshake reports that version as `core_version`, and
  the plugin's own tag as `version`.

The nested module requires a tagged evidencebook root version, so a book
change that needs a library change takes two releases: the library, then
`capsulectl/`.

**The deal skill becomes a release asset.** `deal-skill-<tag>.tar.gz` is
built deterministically: sorted entries, a fixed mtime, owner `0:0`, and
`gzip -n`. `SHA256SUMS` and the attestation cover it. The tag's source
archive is not guaranteed to stay byte-stable, which is why RELEASING.md
step 4 downloads it twice. A checksummed asset removes that step and the
"change the skill before the tag" box, which becomes the same rule every
release asset already follows.

## 6. The install path

### 6.1 The core

`capsulectl plugin install book|deal` (install by name) goes. It fetches
from this repository's release, and a neutral core should not fetch from
company repositories. After the cut-over, a release core refuses the name
form with one line. That line names no repository or URL: it says to
install the plugin file from its release, and shows the file form,
`capsulectl plugin install ./capsulectl-deal`. Install from a file keeps
every check it already runs: the trust walk, the handshake, the name rules,
the version check (section 4.2), the shadow check and the atomic rename.
Downloading, checking the checksum and verifying the attestation move to
whatever fetched the file, which on Muse is the install skill.

With install by name gone, the removal PR also handles:

- **Advice that points at install by name:** `pluginhost.go:172`,
  `doctor_install.go:208` and `:214`, and the version warning at
  `plugin_install.go:831`, with their tests. Each points at the file form
  instead.
- **Docs and skill text:** `skills/capsulectl/spec.yaml:148`,
  `SKILL.md:48`, `AGENTS.md:45`, `references/capsule-cli.md:225`,
  `docs/RELEASE-TRANSPARENCY.md:52`, and the comment at
  `release.yml:65-67`.
- **Code that dies:** `fetchFirstParty`, `pluginReleaseBase` and the
  `--release-url` flag.

### 6.2 actionstate-site

Template and page text, which is public wording and goes to Sherlock2:

- `install/actionstate-install.SKILL.md.tpl`, step 4, today runs
  `capsulectl plugin install deal` and then checks the installed file's
  SHA-256. It becomes:
  1. download `capsulectl-deal-<dealtag>-PLATFORM` from the
     `evidencebook-deal` release and check its SHA-256;
  2. when `gh` works, run `gh attestation verify --repo
     action-state-group/evidencebook-deal --signer-workflow
     action-state-group/evidencebook-deal/.github/workflows/release.yml`;
  3. run `capsulectl plugin install ./capsulectl-deal`.

  The restore-on-failure rule stays the same.
- Step 5 (the deal skill) downloads `deal-skill-<dealtag>.tar.gz`, checks
  the pinned asset SHA-256, unpacks it outside `SKILLS_DIR` and checks
  `SKILL.md`'s pinned SHA-256. The one-copy rule and its check don't change.
- `install.md` and `install.html` list the new assets (today
  `install.md:65-86`).

The generator and its checks:

- `install.sh.tpl`: the plugin download and checks, the `DEAL_SRC` lookup
  (it reads the skill asset), and the summary JSON. Today the summary
  records the deal plugin and skill with `"version":"$TAG"` (lines
  267-270), and step 11's repin parses it (line 342). Those entries take the
  deal tag. Whether that bumps the summary format is the site's call; it is
  listed as a one-way door.
- `scripts/install-fill.sh` (lines 18, 51, 59, 75-80) and
  `install-finalize.sh` take two release identities, core and deal, instead
  of reading the deal plugin from the core's `SHA256SUMS` and the core's
  source archive.
- `scripts/install-apply.sh` (lines 498-512) writes the new `release.json`,
  and `install-release-check.py` (lines 48-61) checks the new URLs.
- `install-check-latest.sh` and `install-latest.yml` watch both the
  capsule-cli and `evidencebook-deal` releases.
- `release.json`: `capsulectl.deal_plugin`, `capsulectl.source_archive` and
  `capsulectl.deal_skill` move to a new top-level `deal` block with its own
  `tag`, `commit`, `platform`, `plugin`, `attestation_bundle`,
  `skill_archive` and `skill`. Removing fields from the `capsulectl` block
  breaks the format, so `format` becomes `actionstate-install-release/2`,
  with `release.schema.json` to match. The check keeps accepting `/1` for
  the folders already published. Published folders stay as they are.
- The install-folder name gains the deal tag. The site picks the exact
  form.

### 6.3 actionstate-muse

- `CAPSULECTL_PIN` in `.github/workflows/ci.yml` holds
  "`<version> <core sha256> <deal plugin sha256>`". It splits into
  `CAPSULECTL_PIN` ("`<version> <core sha256>`") and `CAPSULECTL_DEAL_PIN`
  ("`<deal tag> <deal plugin sha256>`").
- `scripts/seller-sale-e2e.sh` (lines 49-51) and `buyer-exits-e2e.sh`
  (lines 48-50) read the third field of `CAPSULECTL_PIN` and exit 2 without
  it. They read `CAPSULECTL_DEAL_PIN` instead. When run on their own,
  without the binaries `real-capsulectl-test.sh` (lines 63-67) passes in,
  they also build the deal asset's name and download it from the core
  release (`seller-sale-e2e.sh` lines 56 and 86, `buyer-exits-e2e.sh` lines
  53 and 91). Those downloads switch to the `evidencebook-deal` release,
  and that standalone path is tested.
- `scripts/real-capsulectl-test.sh`, `demo-acp.sh`, `demo-ucp.sh`,
  `staged-install-test.sh` and `.github/workflows/install.yml` build the
  deal asset's name and URL from the capsule-cli release, check it against
  the core's `SHA256SUMS`, and fetch the core's source archive for the
  skill. Each switches to the `evidencebook-deal` release: its asset names,
  its `SHA256SUMS` and the skill asset.
- `scripts/receipt-e2e.sh CAPSULECTL CAPSULECTL_DEAL` already takes the two
  binaries separately.
- The wording provenance doesn't change, since the pack stays in the core
  (section 4.3).
- New test vectors name both producers in `PROVENANCE.json`. Existing
  vectors stay byte-for-byte.

## 7. Sequencing and cut-over

Nothing published changes. The v0.1.0-rc21 and older releases, their
`plugin-deal` assets, their source archives and the rc21 install folder
stay frozen and keep installing. The install page moves only once the whole
new set (core, deal plugin, deal skill, Muse plugin) has been verified,
which is RELEASING.md's release-triple rule.

1. **capsule-cli, preparation PR** (no change users can see):
   - promote `cli` and `settlement`, and update every path that names the
     old package: the `-X` package in `release-build.sh` (`PKG`, line 14;
     `-X` silently ignores a symbol that no longer exists), `ci.yml:326`,
     `scripts/build-evidence-graph-iife.sh:15`, and the comment at
     `command.go:310`;
   - add `PluginVersion`, `PluginCommit` and `core_version`, with the
     fallback, and a test that builds a plugin with the `replace` in place;
   - delete `scitt_receipt.go`;
   - add the module allowlist, and the path check in warn mode;
   - commit the fixed-key fixtures, and rewrite the core halves of
     `check-html-render.sh` and `check-portability.sh` onto them;
   - make the producer stamp and deal-view's comparison changes in section
     4.2.
2. **evidencebook, two PRs:** the library functions (Result sealing,
   request and response reading) with tests, released as a minor version;
   and the settings for the `capsulectl/v*` track.
3. **capsule-cli, the split in place:** split `plugins/internal/plugin`
   into `plugins/internal/book` and `plugins/internal/deal`, on the
   evidencebook release from step 2, with one `request` and `respond` per
   plugin; classify the test files and move the core-behavior tests into
   the core against a stub launcher. CI runs both full command trees.
4. **`evidencebook-deal` exists.** Ethan decided on 2026-10-22 to create
   it without a Steven ruling, and it was created that day: public,
   Apache-2.0, squash and rebase merges only, and a "release tags" ruleset
   that blocks updating or deleting a tag. The release environments and
   immutable releases come with its first pipeline PR.
5. **Freeze and import:** a capsule-cli CI check refuses any change under
   `plugins/` or `skills/deal/` from the cut commit on, so daily deal PRs
   can't create two sources of truth. `evidencebook-deal` and
   `evidencebook/capsulectl` each import their part of the cut commit in one
   commit that names it. Each builds against capsule-cli by pseudo-version
   and passes its own CI, including the moved jobs. Deal work resumes in the
   new repository.
6. **capsule-cli, the removal PR:** delete exactly the frozen trees, the
   deal-only scripts, `third_party/unicode/`, the plugin builds in
   `release-build.sh` and the `Makefile`, and the freeze check; turn the
   path check to fail mode; make install by name refuse, with the advice,
   doc and dead-code changes in section 6.1; move the book verbs out of the
   `capsulectl` skill; drop the plugin glob from `narration_test.go:60`;
   move `scripts/presentation-goldens/build-fixtures.sh` (which builds the
   fixtures from a deal build and reads
   `plugins/internal/plugin/testdata/deal/`) to `evidencebook-deal`, and
   delete the deal halves of `check-html-render.sh` and
   `check-portability.sh`, which moved in step 5;
   point the comment at `deal_profile.go:11` away from
   `skills/deal/profile/PROFILE.md`; update `docs/PLUGINS.md`, `docs/RELEASING.md`,
   `README.md`, `docs/CANARY.md`, `docs/HARNESS.md` and the `CHANGELOG.md`
   upgrade note. It merges before the rc22 tag only if steps 2 to 5 are
   done. Otherwise the plugins ship from here once more in rc22 (section 4.2
   covers that build), and the removal goes into rc23.
7. **The release:** tag the core, then release `capsulectl-deal` and
   `capsulectl-book` built against that tag (each `go.mod` bumped to it).
   Muse bumps both pins and its download sources, the site adds the new
   install folder, and the render gate and a fresh-install acceptance run
   follow RELEASING.md steps 7 to 9.

Users already on rc21 or older: a profile, deal store or book written by
rc21 is read by the moved code unchanged, because the move changes no
format. The upgrade note says that `plugin install deal` no longer fetches,
and that the install page installs the plugin from a file. A user who
upgrades only the core gets the dispatch warning until they reinstall the
plugin, as with any mismatch today.

The transitional aliases (#238) keep their v0.2.0 removal. The core still
forwards them by launcher name.

## 8. The new repository's name

**`evidencebook-deal`.** It was proposed here and created on 2026-10-22
(section 7, step 4).

- It is in the evidencebook family (`evidencebook`, `evidencebook-skills`)
  and says what the repository is: the deal application on evidence books.
- It doesn't read as the home of "Agent Deal Records", the neutral spec name
  from SEAMS ruling 3, which a repository called `agent-deal` would.
- It follows the plugin naming the move already uses: `capsulectl-deal` in
  `evidencebook-deal`, and `capsulectl-book` in `evidencebook/capsulectl`.

The naming rule of record keeps the `capsule-*` mark off the paid product
only. Company-OSS `capsule-engine` carries it, so the mark doesn't rule out
a name like `capsule-deal`; the reasons above do.

The binary stays `capsulectl-deal`, and the command stays `capsulectl
deal`. Users see no rename.

## 9. One-way doors

| Door | Why it is one-way |
|---|---|
| The repository name `evidencebook-deal` | It appears in release URLs, attestation signer identities and pins |
| The Go import paths `github.com/action-state-group/capsule-cli/cli` and `.../settlement`, `github.com/action-state-group/evidencebook-deal`, and `github.com/action-state-group/evidencebook/capsulectl` | Public import paths. The capsule-cli ones change once more at the neutral home, which is already known |
| The `capsulectl/v` tag prefix in evidencebook | The Go module proxy keeps a tag permanently |
| The new exported evidencebook functions | Public library API |
| Asset names and URLs in the new releases, `actionstate-install-release/2`, and the install summary entries | Install folders, the repin and Muse pins freeze them |
| The handshake field `core_version` | It joins the published `cli-plugin/v1` contract |
| The producer stamp `capsulectl-deal <tag> (<commit>)` | Sealed records keep it |
| Removing `plugin install book\|deal` | A command users can see stops fetching |

## 10. Follow-ups

- **#428, a donation blocker:** before donation, the core names no company
  plugin. That covers the launcher names, hooks and plugin-only exports; a
  generic hand-over; `canary` and the deal check in `doctor`; the deal
  wording (with a pin check that still refuses a hiding pack) and the
  deal-view module moving to their owners; and spec text, or removal, for
  each deal format the core verifies.
- Moving settlement verification to AAC (J.8), after which the deal plugin
  drops `capsule-cli/settlement`.

## 11. Acceptance, as #326 states it

| Acceptance line | Where this note meets it |
|---|---|
| The core builds with no company-OSS module in its graph, checked in CI | already true; section 3.4 makes the check an allowlist |
| The plugins build and release from their own repositories | sections 2, 3.1 and 5 |
| The install page and Muse pins point at the new release assets | section 6 |
| From the comments: `skills/deal/` lives with the plugins, core has no path that references it (CI), core checks run on neutral fixtures, and `docs/RELEASING.md` is updated | sections 3.1, 3.4 and 7 (step 6) |
