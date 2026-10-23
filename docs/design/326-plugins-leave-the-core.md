# Design note: the book and deal plugins leave the neutral repository

capsule-cli#326. Status: draft for review. Checked against `main` at
`df68c51` (after v0.1.0-rc21), evidencebook `v0.7.0`, actionstate-site's
`install/` template and the rc21 install folder, and actionstate-muse `main`.

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
   `plugins/go.mod` also builds against this checkout through
   `replace github.com/action-state-group/capsule-cli => ../`.
2. **One release.** `scripts/release-build.sh` builds
   `capsulectl-<tag>-plugin-{book,deal}-<os>-<arch>` beside the core under
   one `SHA256SUMS` and one attestation. `capsulectl plugin install
   book|deal` fetches them from the core's own release
   (`internal/cli/plugin_install.go`, `pluginReleaseBase`).
3. **The deal skill comes from the tag's source archive.** The install page
   downloads `archive/refs/tags/<tag>.tar.gz` and pins the archive and
   `skills/deal/SKILL.md` (`docs/RELEASING.md`, the box at the top).
4. **Core CI builds the plugins.** `scripts/check-html-render.sh`,
   `scripts/check-portability.sh` and
   `scripts/presentation-goldens/build-fixtures.sh` build both plugins and
   run `deal init` on `skills/deal/demo/retail-checkout` with
   `skills/deal/profile/materiality-predicate/neutral.json`. The
   `presentation-pass`, golden and `interop-at-resolved-versions` jobs run
   in `plugins/`. `skills/capsulectl/scripts/run-scripted-demo.sh` builds
   `plugins/scripts/bookdemo`.

## 2. Placement

| What | Goes to | Why |
|---|---|---|
| `book` plugin | `evidencebook`, as a nested Go module `github.com/action-state-group/evidencebook/capsulectl` | It is the command-line face of the evidencebook library. Its code is evidencebook calls plus core verbs. |
| `deal` plugin, `skills/deal/` (the whole tree), the deal-only scripts | a new company-OSS repository, proposed name `evidencebook-deal` (section 8) | It is an application on books. Its dependencies (capsule-viewer, the AAC presentation contract, the deal skill, profile, demos, confusables data) and its release pace don't belong in a developer library. |

**Checked against the code before accepting it.** `book` and `deal` share
one Go package today (`plugins/internal/plugin`, 225 files, about 43,400
lines outside tests, of which the book files are about 4,700). The split
was tested by compiling copies of the package with each half removed:

- **Book without deal** has 9 dangling references: `book.go` routes
  `request` and `respond` on a deal profile to `dealRequest` and
  `dealRespond`; `backfill.go` uses `currencyCode` and `guardMetadata`; and
  `commands.go` registers both plugins' command groups. The routing goes,
  because on a deal profile the command is `deal request`, and the core's
  transitional alias already forwards there (#238). The two helpers are a
  few lines and go with the book code. `commands.go` splits into one
  registry per plugin.
- **Deal without book** needs `sealResultRecord` and `checkpointCovers`
  (`resultbuild.go`: seal an Evidence Result into a book, about 60 lines
  over `evidencebook.Book`), the thin Result-document wrappers in
  `resultschema.go` (over the core's `cli.DecodeResult`), `bookError` and
  `bookNow`. Both plugins use the Result sealing. Its owner is the
  evidencebook library ("deterministic Result construction", boundary
  section 3), so it moves into evidencebook's public API as a generic
  function, and both plugins call it. The other helpers are small and go
  with whichever plugin uses them. Nothing is copied.

The alternative of one new repository holding both plugins avoids the
package split. It was rejected because it would put evidencebook's own CLI
outside evidencebook, and because the split costs only about a dozen
functions. evidencebook already releases several packages with their own
tag prefixes (`.github/releases.json`), so a `capsulectl/` track does not
tie the library's versions to the CLI's.

## 3. Inventory

### 3.1 What moves

| From (capsule-cli) | To |
|---|---|
| `plugins/cmd/capsulectl-book`, the book files of `plugins/internal/plugin` (`book.go`, `booklog.go`, `bookhost.go`, `backfill.go`, `report.go`, `result.go`, `resultbuild.go`, `resultschema.go`, their tests) | `evidencebook/capsulectl/` (its own `go.mod`): `cmd/capsulectl-book`, `internal/book` |
| `sealResultRecord`, `checkpointCovers` | evidencebook library, as an exported Result-sealing function with its tests |
| `plugins/cmd/capsulectl-deal`, the deal files of `plugins/internal/plugin` (`deal*.go`, `action_records.go`, `materiality.go`, `rules_checker*.go`, `confusables_table.go`, `assets/`, their tests and `testdata/`) | `evidencebook-deal`: `cmd/capsulectl-deal`, `internal/deal` |
| `plugins/scripts/genconfusables`, `third_party/unicode/confusables.txt`, `scripts/update-confusables.sh` | `evidencebook-deal` |
| `plugins/scripts/engine-e2e` | `evidencebook-deal` |
| `plugins/scripts/bookdemo` | `evidencebook/capsulectl` (core's scripted demo stops using it, section 3.4) |
| `skills/deal/` (`SKILL.md`, `README.md`, `COUNTERSIGN.md`, `AUDIT-HOST.md`, `profile/`, `demo/`, `scripts/`) | `evidencebook-deal/skills/deal/` |
| the core's deal-only CI jobs: `presentation-pass`, the goldens step, `portability` ("shared deal copy verifies without the session") | `evidencebook-deal` CI |
| `interop-at-resolved-versions` (Go and Python at the evidencebook version the plugins resolve) | evidencebook's own CI already runs this round trip against its pins; the copy here goes |
| the core's built-in wording pack entries only the deal plugin speaks (section 4.3) | `evidencebook-deal`, as the deal plugin's built-in pack |

History: each new home is created from this repository with
`git filter-repo` on the moved paths, so `git log` and `git blame` follow
the files. The commits keep their DCO sign-offs.

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
plugin pins an exact capsule-cli version (section 4.2). When the core moves
to its neutral home, the module path changes once, and each plugin updates
its imports in the same release.

**What leaves the host package.** `deadcode ./cmd/capsulectl` lists 37
functions in `internal/cli` that only the plugins reach. They split as
follows:

- **Leaves owned by `book remote`, which move to the book plugin:**
  `RemoteBookCommand` and the evidence-door client behind it
  (`evidencedoor.go`; `openEvidenceDoor` and `meshPluginDoor` in
  `meshplugin.go`; the SCITT statement-receipt check in `scitt_receipt.go`
  that they call), about 15 functions.
- **Host API, which stays:** `NewPluginCommand`, `SetBookBackend`,
  `Speak`, `AskText`, `Exit` and `writePlainJSON` (the plugin side of
  `cli-plugin/v1` and the `voice` contract).
- **Thin helpers over core internals, which stay exported** because they
  reach unexported core state (store locks, delivery state):
  `OpenDealTarget`, `RearmRefusedLog`, `WitnessPendingReason`,
  `CheckDealStandaloneReport`, `AppendRecordDigest`, `BytesDigest`, the
  fragment codec, the grade-ladder predicates, `ResultSchemaBytes`,
  `KillGroupOnCancel`, `DecodeJSON`, `BuiltinWordingBytes` and
  `VerifyTrustedPath`.

`internal/settlement` verifies settlement legs. J.8 places that
verification in AAC as a pending move. Until then it is public here, and the
deal plugin follows it when it moves.

### 3.3 Core code that knows about deals and books, and stays

These stay in the core. None of them imports a plugin; each uses the
command line or verifies a portable format.

- **Verification of deal bundles.** `verify_bundle.go` checks the
  `x-cadence-witness/v0` chain (`deal_cadence_verify.go`), a deal's sealed
  report (`deal_sealed_report.go`), a sale's threads
  (`deal_sale_bundle.go`) and a History (`deal_history.go`). Verification is
  the neutral loop's, and SEAMS rulings 3 and 6 register `x-deal/v0` and
  `personal_history_basic/1` as neutral formats. `deal_profile.go` holds
  only the `x-deal-v0` constants and the schema-error formatter. The record
  definition (`skills/deal/profile/PROFILE.md` and its schemas) moves with
  the skill, and the spec lane registers `x-deal/v0` from it. Until then,
  core verifies by the constants it already holds.
- **Delegation by launcher name.** `pluginhost.go` keeps the names `book`
  and `deal`, the hooks `DealBundleRun`, `DealCommandGroup` and
  `DealTickCommand`, and the hand-over of a book profile's verbs. This is
  the CLI-level contract that `docs/PLUGINS.md` documents ("This hand-over
  is the one place the core calls into a plugin for a core verb").
- **`canary`, and `doctor --install-check`'s plugin check.** Both run deal
  verbs or check the deal plugin through the command line. Whether a
  neutral core should keep a deal canary is a separate question (section
  10).

### 3.4 Core CI and fixtures after the move

- `check-html-render.sh`, `check-portability.sh` and the presentation
  fixtures switch to **committed fixture bundles** under `testdata/` with a
  `PROVENANCE.json`, which names the producing `capsulectl-deal` release and
  commit, as Muse's vendored vectors do. They test the core's rendering and
  verification of a deal bundle, which stays core behavior, without building
  a plugin. `evidencebook-deal` CI regenerates them on each release and
  fails when the committed copy here is stale. The fix is a PR here.
- `run-scripted-demo.sh` (the `capsulectl` skill's demo) runs on a plain
  `log: cll` profile, so it needs no book plugin.
- The `go list -deps` check becomes an **allowlist**: every
  `github.com/action-state-group/...` module in the core's graph must be one
  of the neutral modules named in the check. A company-OSS module, or any
  new one, fails until someone adds it on purpose. This is #326's first
  acceptance line.
- A new check fails on any path in the repository that references
  `plugins/`, `skills/deal` or `skills/book`, outside `CHANGELOG.md` and
  `docs/design/`. This is the acceptance proposed in #326's comments.
- `scripts/gradestrings` stays in the core. Each plugin repository runs it
  with `go run github.com/action-state-group/capsule-cli/scripts/gradestrings@<pinned version>`
  on its own files, so the grade vocabulary has one checker.

## 4. The plugin contract and versioning

### 4.1 What stays the same

`cli-plugin/v1` is unchanged on the wire: the same discovery roots, trust
walk, handshake fields, dispatch, `CAPSULECTL_BIN`, `CAPSULECTL_PROFILE`,
`voice` and `say`, and reserved verbs. A plugin built in another repository
is discovered and run exactly like one built here. A plugin still cannot
touch `verify` or `countersign verify`.

### 4.2 The core version a plugin embeds

Today a first-party plugin's handshake `version` equals the core's, because
both come from one build. `plugin install` refuses a mismatch, dispatch
warns, and `doctor --install-check` fails (PLUGINS.md, "Versions"). The
check protects something real: a plugin runs core verbs with the core code
it embeds, so a stale plugin would bundle with older core code than the
`capsulectl` that handed the verb over.

Once the plugins release on their own, their tags are their own. So:

- The handshake gains one optional field, `core_version`: the version of
  the `github.com/action-state-group/capsule-cli` module the plugin was
  built against, read from `runtime/debug.ReadBuildInfo`.
  `NewPluginCommand` fills it in, so plugins do nothing extra. The core
  already parses the handshake with `json.Unmarshal`, which ignores unknown
  fields, so older cores ignore the new field and `plugin_api` stays
  `cli-plugin/v1`.
- The first-party version check compares `core_version` with the running
  core's `CLIVersion`, where it now compares `version`. The rules don't
  change otherwise: a release core refuses at install unless
  `--allow-version-mismatch`, warns at dispatch, and `doctor` fails; a
  development core doesn't check. A first-party plugin with no
  `core_version` is treated as a mismatch on a release core.
- `version` becomes the plugin's own release tag and stays free text.

Every core release therefore needs a plugin release built against it. This
is RELEASING.md step 6, "re-cut the companion plugin", which already exists
for the Muse plugin. Removing the embedding (a backend called over the
command line instead of linked in) would end that pairing, but it would
redesign how book profiles work. It is not needed for this move.

### 4.3 Wording packs

The core's built-in pack (`internal/cli/assets/wording/capsulectl-en.json`,
`capsulectl.wording/v0`, 292 entries) holds mostly deal wording. Of the
`card.*`, `say.*`, `journey.*` and `class.*` keys, the core's own code uses
46 and the plugins use 160.

- The core pack keeps the keys the core speaks.
- The deal plugin embeds its own built-in pack, `deal.wording/v0`, and
  registers it when it starts, the same way it installs its hooks today.
  Inside the plugin process, the active wording is the core pack plus the
  plugin pack, with a profile's pinned pack over both.
- A pinned pack must still hold every built-in key of the process that
  speaks. In the plugin process, that is the plugin's keys too. Muse's pack
  is already a complete overlay, so it carries both sets.

## 5. Release pipelines

Both new homes reuse what capsule-cli's `release.yml` and
`scripts/release-build.sh` already prove. Each pipeline has:

- a reproducible build (`CGO_ENABLED=0`, `-trimpath`, no VCS stamp, empty
  build id), for linux/amd64, linux/arm64 and darwin/arm64;
- `SHA256SUMS`, and a GitHub build provenance attestation with its bundle
  published as an asset;
- a person approving every run in a deployment environment, with the same
  two release reviewers, and agents never approving;
- tags that only maintainers can create and nobody can move, and immutable
  releases;
- every Action pinned by commit SHA.

| | `evidencebook-deal` | `evidencebook/capsulectl` |
|---|---|---|
| Trigger | a pushed `v*` tag, as in capsule-cli | a merged version bump of the `capsulectl` package in `.github/releases.json` (tag prefix `capsulectl/v`, which is also Go's tag form for the nested module); `tag-release.yml` dispatches `publish-capsulectl.yml` |
| Environments | `release` and `release-candidate`, as in capsule-cli | one environment accepting only `capsulectl/v*` tags, in the same way `crates-io` accepts only `crates/*-v*` |
| Assets | `capsulectl-deal-<tag>-<os>-<arch>`; `deal-skill-<tag>.tar.gz`; `SHA256SUMS`; `capsulectl-deal-<tag>.sigstore.json` | `capsulectl-book-<version>-<os>-<arch>`; `SHA256SUMS`; `capsulectl-book-<version>.sigstore.json` |
| Version check before publishing | the binary's handshake `core_version` equals the capsule-cli version in `go.mod` | the same |

**The deal skill becomes a release asset.** `deal-skill-<tag>.tar.gz` is
built deterministically: sorted entries, a fixed mtime, owner `0:0`, and
`gzip -n`. `SHA256SUMS` and the attestation cover it. The tag's source
archive is not guaranteed to stay byte-stable, which is why RELEASING.md
step 4 downloads it twice. A checksummed asset removes that step and the
"change the skill before the tag" box, which becomes the same rule every
release asset already follows.

Each plugin repository's `go.mod` requires capsule-cli at the core's release
tag, with no `replace`. Between releases it may track a capsule-cli
pseudo-version on `main`, but a plugin release always requires a capsule-cli
release tag. The version check above enforces that.

## 6. The install path

### 6.1 The core

`capsulectl plugin install book|deal` (install by name) goes. It fetches
from this repository's release, and a neutral core should not name company
repositories. After the cut-over, a release core refuses the name form with
one line saying the plugin now ships from its own release, and gives the
file form `capsulectl plugin install ./capsulectl-deal`. Install from a file
keeps every check it already runs: the trust walk, the handshake, the name
rules, the version check (now on `core_version`), the shadow check and the
atomic rename. Downloading, checking the checksum and verifying the
attestation move to whatever fetched the file, which on Muse is the install
skill.

### 6.2 actionstate-site's install template

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
- `install.sh.tpl` makes the same two changes (the `DEAL_SRC` lookup reads
  the skill asset).
- `release.json`: `capsulectl.deal_plugin`, `capsulectl.source_archive` and
  `capsulectl.deal_skill` move to a new top-level `deal` block with its own
  `tag`, `commit`, `platform`, `plugin`, `attestation_bundle`,
  `skill_archive` and `skill`. Removing fields from the `capsulectl` block
  breaks the format, so `format` becomes `actionstate-install-release/2`,
  with `release.schema.json` to match. Published folders (rc21 and older)
  stay as they are.
- The install-folder name gains the deal tag. The site picks the exact
  form, for example `rc22-d<dealtag>-p0.0.16-s0.0.16-r1`.

### 6.3 actionstate-muse

- `CAPSULECTL_PIN` in `.github/workflows/ci.yml` holds
  "`<version> <core sha256> <deal plugin sha256>`". It splits into
  `CAPSULECTL_PIN` ("`<version> <core sha256>`") and `CAPSULECTL_DEAL_PIN`
  ("`<deal tag> <deal plugin sha256>`"). The scripts that read the third
  field read the new variable instead. `scripts/receipt-e2e.sh
  CAPSULECTL CAPSULECTL_DEAL` already takes the two binaries separately.
- `wording/capsulectl/PROVENANCE.json` records each vendored file's
  `source_repo`: `capsulectl-en.json` from capsule-cli, and the new
  `deal-en.json` from `evidencebook-deal`. `wording/capsulectl.go` builds
  Muse's overlay over both. Each core or deal bump vendors its pack again,
  as each capsulectl bump does today (Muse README, "presentation install").
- New test vectors name both producers in `PROVENANCE.json` ("capsulectl
  vX, capsulectl-deal vY"). Existing vectors stay byte-for-byte.

## 7. Sequencing and cut-over

Nothing published changes. The v0.1.0-rc21 and older releases, their
`plugin-deal` assets, their source archives and the rc21 install folder
stay frozen and keep installing. The install page moves only once the whole
new set (core, deal plugin, deal skill, Muse plugin) has been verified,
which is RELEASING.md's release-triple rule.

1. **capsule-cli, a PR before the rc22 tag** (no change users can see):
   promote `cli` and `settlement`; add the `core_version` handshake field
   and the new version check (still equal-version in practice, since the
   plugins still build here); add the module allowlist and the path check
   (the path check starts in warn mode while `plugins/` is still here);
   switch the core fixtures to committed bundles.
2. **evidencebook, a PR:** add the Result-sealing function to the library,
   with tests, and release it as a patch.
3. **Ethan or Steven creates `evidencebook-deal`:** public, Apache-2.0,
   with the release environments, tag rulesets and immutable releases
   applied by its own `scripts/release-settings.sh`.
4. **Moves, one PR per destination:** `evidencebook-deal` takes the
   filtered history of the deal paths, and `evidencebook/capsulectl` takes
   the book paths. Each builds against capsule-cli `main` by pseudo-version
   and passes its own CI, including the moved goldens and portability jobs.
5. **capsule-cli, the removal PR:** delete `plugins/`, `skills/deal/`, the
   deal-only scripts, `third_party/unicode/` and the plugin builds in
   `release-build.sh` and the `Makefile`; turn the path check to fail mode;
   make install by name refuse; update `docs/PLUGINS.md`,
   `docs/RELEASING.md`, `README.md`, `docs/CANARY.md`, `docs/HARNESS.md`
   and the `CHANGELOG.md` upgrade note. It merges before the rc22 tag only
   if steps 2 to 4 are done. Otherwise the plugins ship from here once more
   in rc22, and the removal goes into rc23.
6. **The release:** tag the core, then release `capsulectl-deal` and
   `capsulectl-book` built against that tag (`go.mod` bumped to the tag).
   Muse vendors both wording packs and bumps both pins, the site adds the
   new install folder, and the render gate and a fresh-install acceptance
   run follow RELEASING.md steps 7 to 9.

Users already on rc21 or older: a profile, deal store or book written by
rc21 is read by the moved code unchanged, because the code moves without
changes. The upgrade note says that `plugin install deal` no longer fetches,
and that the install page now installs the plugin from a file. A user who
upgrades only the core gets the dispatch warning (a missing or mismatched
`core_version`) until the plugin is reinstalled, as with any mismatch today.

The transitional aliases (#238) keep their v0.2.0 removal. The core still
forwards them by launcher name.

## 8. The new repository's name

Proposed: **`evidencebook-deal`**.

- It says what the repository is: the deal application on evidence books,
  in the evidencebook family (`evidencebook`, `evidencebook-skills`).
- It avoids the `capsule-*` mark, which the naming rule of record keeps for
  the donation-bound family (licensing note, "Naming of record").
- It doesn't collide with "Agent Deal Records", the neutral spec name from
  SEAMS ruling 3. A repository called `agent-deal` would read as that spec's
  home.
- The `actionstate-*` prefix belongs to the private repositories (Muse, the
  site).

The binary stays `capsulectl-deal`, and the command stays `capsulectl
deal`. Users see no rename.

## 9. One-way doors

| Door | Why it is one-way |
|---|---|
| The repository name `evidencebook-deal` | It appears in release URLs, attestation signer identities and pins |
| The Go import paths `github.com/action-state-group/capsule-cli/cli` and `.../settlement`, `github.com/action-state-group/evidencebook-deal`, and `github.com/action-state-group/evidencebook/capsulectl` | Public import paths. The capsule-cli ones change once more at the neutral home, which is already known |
| Asset names and URLs in the new releases, and `actionstate-install-release/2` | Install folders and Muse pins freeze them |
| The handshake field `core_version` | It joins the published `cli-plugin/v1` contract |
| Removing `plugin install book\|deal` | A command users can see stops fetching |

## 10. Out of scope, and follow-ups to file after convergence

- Whether a neutral core keeps a deal canary (`canary run`) and a deal check
  in `doctor --install-check`, or `canary` becomes `capsulectl deal
  canary`.
- Making the hand-over of a book profile generic (a profile names the
  plugin that owns its log), so the core stops naming `book` and `deal`
  before donation.
- Moving settlement verification to AAC (J.8), after which the deal plugin
  drops `capsule-cli/settlement`.

## 11. Acceptance, as #326 states it

| Acceptance line | Where this note meets it |
|---|---|
| The core builds with no company-OSS module in its graph, checked in CI | already true; section 3.4 makes the check an allowlist |
| The plugins build and release from their own repositories | sections 2, 3.1 and 5 |
| The install page and Muse pins point at the new release assets | section 6 |
| From the comments: `skills/deal/` lives with the plugins, core has no path that references it (CI), core checks run on neutral fixtures, and `docs/RELEASING.md` is updated | sections 3.1, 3.4 and 7 (step 5) |
