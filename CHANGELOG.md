# Changelog

## Unreleased

- **Fixed:** the "pending" texts of a witness delivery that failed or was refused
  said it is "retried at every tick". A tick only attempts a delivery whose wait
  has passed, and that wait grows up to an hour. The texts now say it is tried
  again later while capsulectl runs, with the wait growing up to an hour, and a
  test ties that to the library's backoff cap (capsule-cli#397).
- **Changed:** the pages capsulectl writes (`presentation build`, and `bundle` or
  `disclose --html`) embed capsule-viewer's released page runtime, `go/pageruntime`
  in capsule-viewer/go v0.3.0, pinned by its digest `0758f29b…`. capsule-cli's own
  build of it is gone (capsule-cli#472, evidencebook#144 move 3). That covers
  `cli/assets/evidence-graph.iife.js` and its digest, NOTICE and third-party
  notices, which now ship with `go/pageruntime`. It also covers
  `scripts/build-report-runtime.mjs`, `build-evidence-graph-iife.sh`,
  `iife-sync.sh` and `check-viewer-without-webcrypto.mjs`, `make iife-check`, and
  the `iife` CI job. The page's "Verified by" line names viewer commit
  `3fe9ad7b0737`; the verifier is still agent-action-capsule 0.8.0. From the
  viewer's fixes since: the summary line names only the signature checks the page
  did not run, such as "checkpoint signature not checked here"
  (capsule-viewer#118), and the banner's frame check is tighter (capsule-viewer#117,
  #125).
- **Removed:** `cli.EvidenceGraphIIFE`, `EvidenceGraphIIFESHA256`,
  `EvidenceGraphIIFEDigest`, `CardExtension` and `PresentationExtension`, with no
  alias. Use `pageruntime.IIFE`, `pageruntime.SHA256`,
  `presentation.CardExtension` and `presentation.PresentationExtension` from
  capsule-viewer/go. The runtime exports 45 names to presentation modules (its
  `browser.ts` contract), not every name agent-action-capsule's browser entry
  exports. A page bootstrap that called another name, such as
  `buildVerifiedBundleContext`, calls `renderEvidenceGraph` with the bundle
  instead (evidencebook-deal#49).

- **Changed:** the deal words leave the core's built-in wording pack for the
  deal plugin's own pack (capsule-cli#428). The core's pack keeps the 24 keys
  core commands speak (`say.verify.*`, `say.settlement.*`, `say.error.*`,
  `say.doctor.issues.*` and the card frame `cli.AskText` prints); key names
  do not change. A plugin adds its pack to its process with
  `cli.RegisterWording`, and its `cli-plugin/v1` handshake carries it as the
  optional `wording` member. A pinned pack must hold the core's keys and,
  for a profile whose log a plugin keeps, that plugin's: `profile update
  --wording` and `doctor` read the plugin's pack from its handshake, and a
  pack whose `card.terms.*` entries could hide a term is still refused,
  whichever pack holds the term. A pack made from the core's pack alone is
  refused with a line naming the plugin whose keys it lacks. A pack pinned
  before this change holds every key and is still used.
  `profile update --wording` on a profile whose plugin is not installed is
  refused. `BuiltinWordingBytes` is now the core's pack alone.
- **Added:** `verify --profile NAME` with no file flag verifies the profile's
  store: every CLL entry, the artifact record it names (the `--capsule`
  checks), and agent-action-capsule's store checks once over the whole log.
  Failed checks (exit 1) and checks that could not be made (exit 3) are listed
  apart, each by sequence and capsule id (capsule-cli#477, step 4 of moving
  off the ledger).
- **Moved:** the deal view (`capsulectl.deal-view/v0`) is the deal plugin's own
  presentation module (capsule-cli#428 step 4): `cli/assets/deal-view.*` and the
  `DealViewJS`, `DealViewJSSHA256` and `DealViewManifestJSON` exports are gone.
  `presentation list` no longer offers a deal view of its own, and drops the rule
  that yielded it to an installed module: a deal bundle resolves among the
  built-ins and the installed plugins' modules like any other, so with no plugin
  module installed it falls back to `aac.builtin.no-aggregate/v0`. A plugin
  binary states its modules in the `cli-plugin-metadata` handshake through
  `PluginMetadata.Presentations` (capsule-cli#478).
- **Fixed:** the vendored page runtime and `capsule-viewer/go` move to capsule-viewer
  389c1bac (a pseudo-version, no release): the banner integrity check no longer
  discards a module's render when the page grows a classic scrollbar
  (capsule-viewer#113; every deal page was shown as a refused presentation in
  Linux Chrome CI, evidencebook-deal#40).
- **Removed:** the core names no company plugin (capsule-cli#428). Gone: the
  transitional aliases (`close`, `reconcile`, `request`, `respond`, `compose`,
  `report`, `backfill`, `result open`, `store migrate`), which moved to the
  plugins and were due in v0.2.0 (use `capsulectl book ...` and `capsulectl
  deal ...`); the `book` and `deal` placeholders that said what to install; the
  `--deal`, `--share` and `--to` flags of `bundle`, `disclose` and `permalink`
  (the deal plugin adds them with `cli.ExtendBundle`); `canary` (now `capsulectl
  deal canary run|watch`, with its guide and workflow templates in the deal
  plugin's repository); and, from the host package, `BookPluginName`,
  `DealPluginName`, `AliasRemovedIn`, `SetBookBackend`, `BookBackend`,
  `BookLog`, `BookBundleRequest`, `RemoteBookCommand`, `ErrBookProfile`,
  `Target.Book`, `DealBundleRun`, `DealCommandGroup`, `DealTickCommand`,
  `OpenDealTarget`, `DealStoreWriter`, `ErrDealProfileWrite`, `IsBook`,
  `IsShelf` and `LogBook` (use `RegisterLogBackend`, `LogBackend`, `OwnedLog`,
  `LogBundleRequest`, `RemoteCommand`, `ErrPluginLogProfile`, `Target.Owned`,
  `OpenOwnedTarget`, `PluginStoreWriter`, `ErrPluginStoreWrite`, `ExtendBundle`
  and `GraftCommand`).
- **Changed:** a jsonl profile with a `log_id` must say who keeps its log
  (`profile create --log cll|<plugin>`); the core no longer reads one that
  says nothing as a book. A profile saved without one stops its log commands
  with `capsulectl profile update --profile NAME --log <cll or the plugin>`,
  which is accepted once. A deal profile saved without `log:` is no longer read
  as the deal plugin's: `profile update --profile NAME --log deal` declares it
  (accepted while the store carries only the older deal marker).
  `cll checkpoint cadence` runs in the plugin that keeps the selected profile's
  log, healing a loosened per-user plugin root for that plugin's launcher as it
  did for the deal plugin's; on a profile the core keeps it says it has none.
  A plugin's version is checked only when it declares `core_version` (its own
  `version` is no longer read as a core version), and `plugin install` has no
  first-party special case. `doctor --install-check` looks for the skill named
  like the plugin that keeps the profile's log (for a deal profile, `deal`,
  as before), reports it under `skills` and `skill_name` instead of
  `deal_skills`, and checks that plugin's version. `bundle` on a store a
  plugin owns says its bundles come from that plugin. `profile retire
  --move-data` is refused for a profile whose log a plugin keeps, and the
  running-command check no longer looks at a book's `book/log.jsonl`. A profile whose log a plugin keeps must carry an `--operator`, as a
  book's always had to (it was the book's rule; it now holds for every plugin's
  log). The exit class `ErrAlarm` (code 6) is named "alarm".
- **Added:** `cli.MarkStoreOwned`, `Target.MarkOwned` on a sqlite handle the
  plugin already holds (capsule-cli#428), for a plugin session that writes
  without ever opening a target. Same rules: the writer lock, one owner, no
  write to a store that already records it.
- **Changed:** `cli.GraftCommand` takes a function that builds the command
  (capsule-cli#428), not the command: the core calls it for each command tree
  it builds, because a cobra command keeps the flag values of its last run and
  a shared one printed help after any earlier `--help`. It landed in #463
  (unreleased) with no caller outside tests.
- **Changed:** names the host package gave a plugin go generic
  (capsule-cli#428): `LogBundleRequest`, `RemoteCommand`, `ErrPluginLogProfile`
  and the `Target.Owned` field replace `BookBundleRequest`, `RemoteBookCommand`,
  `ErrBookProfile` and `Target.Book`; the old names stay as deprecated aliases
  (`Target.Book` is set and released with `Target.Owned`). The
  refusal of `countersign request` on a profile whose log a plugin keeps no
  longer says "book" (it keeps its hint to pass `--bundle`), and any plugin
  that returns `ErrBookProfile` now shows the same generic wording.
  `profile create|update` reads a sqlite store for its owner only when the
  profile is new or its log owner or database file changes; `Target.MarkOwned`
  writes nothing to a store that already records its owner.
- **Changed:** a sqlite store a plugin owns is written by that plugin only
  (capsule-cli#428). The plugin opens it with `cli.OpenOwnedTarget` while it
  holds the store's writer lock and records its ownership with
  `Target.MarkOwned`; from then on every core write verb refuses the store
  whatever the profile says, including a profile that declares `log: cll`
  over it. The older deal store marker (the `deal_shelf` table) keeps
  protecting stores the plugin has not marked yet. `profile create|update`
  refuses a `log:` that disagrees with the owner a store records. The refusal
  is `cli.ErrPluginStoreWrite` (`ErrDealProfileWrite` stays as another name for
  it), and its text no longer names a plugin or one of its commands. `OpenDealTarget`,
  `DealStoreWriter` stay as deprecated names.
- **Changed:** `DecodeBundleJSON`, which every command that reads a bundle
  or a bare capsule uses (`verify --bundle` and `verify` of a capsule,
  `countersign`, `presentation build` and `list`, and the page gate), now
  decodes with agent-action-capsule/go's `bundle.Decode` (evidencebook#144).
  A file with data after its JSON object, or with invalid UTF-8 or an
  unpaired surrogate escape, is now refused as not valid JSON (exit 2), as
  agent-action-capsule's capsule decoder and every permalink decoder already
  did. `go.mod` requires agent-action-capsule/go at 1d1752b.

- **Changed:** `bundle`, `disclose` and `permalink` hand their whole command
  line to the plugin that keeps the selected profile's log, before the core
  reads any flag, so a plugin's own flags (`--deal`, `--share`, `--to`) reach
  it (capsule-cli#428). A process that has the plugin's backend, or is the
  plugin, serves the command itself; otherwise a plugin-kept profile always
  goes to its plugin, and a plugin that is not installed fails the command
  instead of the core running it. A sqlite profile
  with a namespace can now name its plugin in `log:`; a deal profile saved
  without one still reads as the deal plugin's. Plugin authors get
  `cli.ExtendBundle` (flags and a run step for those commands) and
  `cli.GraftCommand` (mount a command under a core one, such as
  `cll checkpoint cadence`); the `DealBundleRun` and `DealTickCommand` hooks
  stay until the plugins move to them.
- **Added:** `presentation build --format fragment` writes the page as a
  fragment permalink. The file holds the `aac.presentation-fragment/v0` token,
  or the whole URL with `--viewer-url`, plus a newline. The token is packaged
  within a budget of 65,536 characters by default (`--max-fragment-length`,
  at most 1,046,528). The whole URL is at most 1 MiB. A page over either
  limit is refused with `unavailable.code` `fragment-too-large`, and nothing
  is written. `unavailable.subject` says which limit (`fragment token` or
  `permalink URL`), with its `length` and `limit`. A written fragment's
  result reports `fragment_length` and `fragment_limit`, which is always
  the token budget. A monthly report is over the
  default budget. The fragment carries the page's settings and bundle and
  pins its code: capsule-viewer's `OfflineHTMLFromFragment` hands back
  exactly the page `--format html` writes (capsule-cli#254).
- **Added:** `presentation build --format embedded` writes a snippet a host
  page inlines where the presentation goes. It holds the runtime, the
  modules, the bundle as a JSON data element and a bootstrap. The result
  lists `script_sources` and `style_sources`, the CSP hash sources the host's
  `script-src` and `style-src` must allow; no `'unsafe-inline'` is needed. The
  snippet takes the host page's title, so `--title` does not change it
  (capsule-cli#254).
- **Added:** `presentation list` reports `packagings`: for the bundle,
  `--audience` and build's settings (`--depth`, `--title`,
  `--max-fragment-length`, `--viewer-url`; the default module set, as
  `list` resolves among every installed module), each target in the order html,
  fragment, embedded, static. Each is either available or unavailable with
  a `reason` (`fragment-too-large`, or `no-document` for static). A target is
  available exactly when `presentation build` writes it with the same
  settings. `packagings` is independent of `--format`, which still names
  only the format the module is resolved for. A bundle no page may be written for lists none and says why in
  `packagings_refused` (capsule-cli#254).
- **Changed:** `go.mod` pins capsule-viewer `go/v0.2.0`, and the vendored
  browser runtime moves with it, to capsule-viewer `cdc83dc` with the
  agent-action-capsule `ts/v0.8.0` verifier.
- **Changed:** a profile's `log:` can name any plugin that keeps its log, not
  only `book` (capsule-cli#428): `profile create --log <plugin>`, then the
  core runs the log commands in that plugin. A plugin serves its own profiles
  by registering a backend with `cli.RegisterLogBackend` (`SetBookBackend` and
  `BookBackend` stay as deprecated names). No behavior change for `book`.
- **Added:** `verify --bundle` and cadence verify report, for each witness
  receipt, the checked `registration_time` (the `iat` the witness signed),
  the verifying `key_id`, and `registration_time_reason` when there is no
  time (chips D4, capsule-cli#402). Needs agent-action-capsule `go/v0.14.0`,
  which `go.mod` now pins.
- **Added:** `doctor` lists, under `plugins.doctors`, the command of each
  trusted plugin whose metadata declares a `doctor` subcommand, such as
  `capsulectl book doctor` (evidencebook#130). It reads the plugin's
  `subcommands` and names no plugin itself.
- **Changed:** the pages capsulectl writes with its vendored viewer
  (`presentation build`, and `bundle` or `disclose --html`) draw the
  verification banner in the presentation contract's words
  (capsule-viewer#80): one of checking, verified, incomplete, failed or
  unable, with a details line naming the findings. Old to new: "Contents
  checked" becomes "✓ Unchanged since it was sealed" only when every check ran
  on the page and passed; a bundle whose checks pass but some were not run,
  such as one with unsigned records or an unsigned checkpoint, now reads "None
  of the bundle's checks failed, but some were not run" (with ': see "Not
  checked on this page" below' when the page lists them). "Check failed"
  becomes "✗ A check failed: do not rely on this page", or "✗ This bundle did
  not verify: do not rely on it" when the records are withheld. The vendored
  browser runtime is rebuilt from capsule-viewer `d100e32` and
  agent-action-capsule `ts/v0.7.0` (`cd68af7`), and `go.mod` names the same
  capsule-viewer commit.
- **Changed:** `presentation list` resolves with capsule-viewer's
  `go/presentation`, which moved there from agent-action-capsule unchanged
  (agent-action-capsule#379). Resolution is the same, except that a valid
  producer-selected bundle now resolves its module instead of a refusal, as
  the browser page already did (agent-action-capsule#395).

## v0.1.0-rc23

### Upgrade note: the book and deal plugins and the deal skill ship from their own releases (#326)

- **Changed:** `capsulectl-book` now builds and releases from evidencebook
  (the nested module `capsulectl/`, tags `capsulectl/vX.Y.Z`), and
  `capsulectl-deal` and the deal skill from evidencebook-deal (tags
  `vX.Y.Z`). This repository no longer holds `plugins/`, `skills/deal/`,
  `third_party/unicode/`, `docs/BACKFILL.md` or the deal-only scripts, and its
  releases no longer carry `capsulectl-<tag>-plugin-<name>-<os>-<arch>`
  assets. Published releases are unchanged.
- **Removed:** `capsulectl plugin install book|deal` (install by name). It
  refuses and gives the file form: download the plugin file built for your
  capsulectl release from the plugin's own release, check it, then install
  the downloaded file (`capsulectl plugin install
  ./capsulectl-deal-<version>-<os>-<arch>`); install names the plugin by its
  handshake. The hidden `--release-url`
  flag is gone. The missing-plugin, version-mismatch and `doctor
  --install-check` messages give the file form.
- **Changed:** the `capsulectl` skill lists the core verbs only; the book
  verbs (`request`, `respond`, `close`, `reconcile`, `report build`, and
  `result build` on a book profile) are the book plugin's. `result build` on
  a plain profile stays a core verb. Its scripted demo runs on a sqlite
  profile, now including `result build`, and passes again (it needed the book
  plugin since #236).
- **Added:** `TestPresentationFixturesVerifyOffline` checks every committed
  presentation fixture, deal copies included, verifies to its recorded
  verdict with the core alone. The deal page and portability checks run in
  evidencebook-deal.
- **Changed:** CI fails on any path into the trees that moved out
  (`scripts/check-moved-paths.sh --fail`, which now also fails when its
  `git grep` cannot run), and drops the plugin jobs.

### The deal page compares builds by name (#448)

- **Changed:** the deal page's version note compares a step's build with the
  build that made the page only when both have the same name. A page from a
  `capsulectl-deal` release (evidencebook-deal#3) names that build; a step
  `capsulectl` sealed before the move, with or without a recorded version, is
  said apart: "Some steps were sealed by capsulectl vX, the build that sealed
  deals before capsulectl-deal did; capsulectl-deal vY made this page." The
  note links the page's own `release_notes` when it is an https URL, else
  capsule-cli's releases. A page without the new `page_producer` and
  `release_notes` data reads exactly as before.

### The book and deal plugins are separate packages (#326)

- **Changed:** `plugins/internal/plugin` is split into `plugins/internal/book`
  (capsulectl-book) and `plugins/internal/deal` (capsulectl-deal), so each
  plugin can move to its own repository (docs/design/326-plugins-leave-the-core.md,
  step 3). Neither imports the other.
- **Changed:** `request` and `respond` keep one implementation of their flags,
  input checks and output shape, in the host package
  (`cli.EvidenceRequestCommand`, `cli.EvidenceRespondCommand`); each plugin
  runs its own mode on what they read. On a deal profile,
  `capsulectl book request` and `book respond` already refused and pointed to
  `capsulectl deal request` and `deal respond`; capsulectl-book no longer
  carries the deal code behind them. `deal respond --policy` now refuses the
  flag before reading the file, so a missing or malformed policy file gets
  the same refusal instead of a read or JSON error.
- **Changed:** the guard on identifier fields of host-supplied records (no
  card-number-like or phone-like values) is one function, `cli.GuardMetadata`,
  which `book backfill` and `deal audit-host` both apply.
- **Changed:** sealing an Evidence Result (`book result build`, `deal
  terms-result`) goes through evidencebook's `Book.SealResult`
  (evidencebook#103); the plugins module requires evidencebook at that commit.

### The plugins' host package is public (#326)

- **Changed:** `internal/cli` is now `github.com/action-state-group/capsule-cli/cli`
  and `internal/settlement` is `.../settlement`, so the first-party plugins
  can build outside this repository (docs/design/326-plugins-leave-the-core.md).
  Before v1 they carry no compatibility promise. Release builds set
  `-X github.com/action-state-group/capsule-cli/cli.CLIVersion` and `.CLICommit`;
  the old `internal/cli` path no longer exists, and `-X` silently ignores it.
- **Added:** the `cli-plugin/v1` handshake carries an optional `core_version`,
  the capsulectl release a first-party plugin embeds. The first-party version
  check (install, the dispatch warning, `doctor --install-check`) compares it,
  and falls back to `version` for plugins without it, which is every plugin
  released so far. A plugin built on its own reports its own tag as `version`.
- **Changed:** the version-mismatch messages (install, the dispatch warning,
  doctor) now name the release the plugin is built for: "capsulectl-deal is
  built for capsulectl X, this capsulectl is Y".
- **Changed:** CI checks that every action-state-group module in the core's
  build graph is on a neutral list (`scripts/check-neutral-deps.sh`), and warns
  about paths that leave with the plugins (`scripts/check-moved-paths.sh`).
- **Removed:** an unused SCITT statement-receipt check with no callers.

### CI shards the tests by measured time (#407)

- **Changed:** `scripts/test.sh` cuts its shards by the seconds each test took
  (`scripts/test-weights`, refreshed from a CI run by `scripts/test-weights.sh`)
  instead of by name, and the race pass runs on eight jobs instead of six, so
  no shard sits near go test's 15-minute limit.

### verify --bundle: producer signatures and the checkpoint from AAC's strict verifier (#438)

- **Changed:** `verify --bundle` reads each record's producer signature
  from AAC's strict verifier (`VerifyStrict`'s `signatures` claim) instead
  of its own copy. `producer_signatures` and `record_signatures` keep their
  shape and values. A record whose `signature` and `key_id` are both
  present as `null` (or as anything but hex strings) was read as unsigned
  and the bundle INCOMPLETE; it is now an invalid signature and the bundle
  INVALID. A record edited under a kept signature now also fails
  `producer_signatures` (it was INVALID already). A record whose carried
  `capsule_id` alone was edited now reads `authored`, since the envelope
  signs the Capsule ID recomputed from its content; `record_identity` still
  fails it, so the bundle stays INVALID. The claim lists invalid records
  before unsigned ones. A non-object entry in `records` no longer adds an
  unsigned finding with an empty id, and a `records` list with no object
  in it fails the claim instead of withholding it. A composed member that
  is not an evidence-bundle/v2 reports `not_an_evidence_bundle_v2` as its
  signature claim, with no per-record verdicts. Each of these bundles was
  INVALID already.
- **Changed:** the `checkpoint` claim is AAC strict's
  (`bundle.VerifyCheckpoint`) instead of the CLI's own. Its shape and
  finding codes are unchanged. A checkpoint whose JSON copy has no
  `log_id`, which only its completeness certificate states, was VALID and
  is now INVALID (`checkpoint_field_missing:log_id`). A `mmr_size` or
  `prev_size` written as a float (`0e0`, `1.0`) is not the signed integer,
  and fails `checkpoint_field_mismatch`. A `prev_size` written that way was
  VALID before. A `checkpoint.cose` must be strict unpadded base64url: one
  with a line break, or with non-zero trailing bits, passed the CLI's claim
  and now fails it as `checkpoint_signature_malformed`, as does a `cose`
  that is not a string, which was read as absent. Each was INVALID already,
  through interval coverage. Findings list every mismatch before any
  missing field, so a checkpoint with no `log_id` beside a certificate
  naming another log reports both. Every bundle capsulectl and AAC write
  carries the checkpoint's own `log_id`, integer sizes and strict base64url,
  so none of them is affected.
- **Changed:** witness receipts and a deal's cadence chain are checked only
  against a checkpoint whose claim passed, as AAC strict checks them. So on
  a bundle whose checkpoint claim newly fails, `witnesses` reads `withheld`
  (`checkpoint_unverified`) with no receipts. A deal History reports
  `deal_log_unbound` for such a carried deal. A producer-selected bundle
  whose certificate names another log has its membership withheld
  (`checkpoint_unverified`) instead of checked. Each of these bundles is
  INVALID.
- **Changed:** witness receipts, on the bundle and on a deal's cadence
  chain, are AAC's `witness.VerifyReceipts` results as they stand. The CLI's
  own well-formedness check and tally are gone; AAC go/v0.13.1 makes the
  same entries malformed (agent-action-capsule#386). The shape and codes
  are unchanged. Findings list malformed entries first, then one per
  receipt that did not pass, where the CLI listed them in entry order. A
  passing receipt's `reason` is AAC's text (`ok`, or the grade note), not
  "verified under <row>'s key". A `receipt_b64` wrapped across lines, or
  with non-zero trailing bits, is not canonical standard base64, so the
  entry is `witness_receipt_malformed` and fails. With a directory row for
  its witness it was `witness_receipt_invalid`, and INVALID already. Without
  a usable row, or as a stub, it was withheld: a bundle that was VALID is
  now INVALID (exit 1), and the receipt is no longer listed.
- **Changed:** `go.mod` (root and `plugins/`) moves
  `agent-action-capsule/go` to v0.13.1: v0.13.0 for the checkpoint claim,
  and v0.13.1 for the witness rule. v0.13.0 also caps AAC's Class 1 number
  findings (agent-action-capsule#387):
  `verify --capsule` lists at most 64 `float_in_digest_field` and 64
  `unsafe_integer_in_digest_field` codes, and a quoted path is cut at 256
  characters. Verdicts don't change.

### Report pages: the failed-bundle notes and repeated record ids (capsule-viewer#77)

- **Changed:** the pages capsulectl writes take capsule-viewer at
  `4b08aad`. The two report notes on tampering say what a bundle that did
  not verify gets: no report, the red "Check failed" callout, the checks
  that failed and each record's id and status, with no record content. A
  capsule id the bundle lists more than once has one entry in "Records in
  this bundle", with one address and a note of how many times it is listed;
  the summary counts ids and names the listings when they differ.
- **Changed:** the vendored runtime (`evidence-graph.iife.js`) is rebuilt
  from capsule-viewer `4b08aad` and agent-action-capsule `1d655a1`
  (`ts/v0.6.1`). `go.mod`'s `capsule-viewer/go` moves to the same viewer
  commit (`agent-action-capsule/go` moves to v0.13.1 with #438 above). The presentation
  goldens' pages and snapshots, and the presentation pass's build
  snapshots, are re-emitted from their unchanged bundles.

## v0.1.0-rc22

### Report pages: one record view, conversations in place, the layout pass (capsule-viewer#73, #74, #75, #76)

- **Changed:** the pages capsulectl writes take capsule-viewer at
  `4e4d553`. The check section's "Records in this bundle" lists every
  record once, each with its own `#record-<capsule id>` address, kind,
  times, seal status and disclosed content (capsule-viewer#76). A report's
  conversation opens in place inside its card; on a static page the card
  links to the record instead. The mock-judge line is a mark under the
  report title, the provenance line opens the first section card, and
  every panel starts at one inset (capsule-viewer#75). The verification
  page lists `contract-report/v1` as interpreted when the contract-report
  module drew the page (capsule-viewer#73). A bundle that did not verify
  gets a red callout on every page: "Check failed", the check section's
  counts and a link to it (capsule-viewer#74).
- **Changed:** the vendored runtime (`evidence-graph.iife.js`) is rebuilt
  from capsule-viewer `4e4d553` and agent-action-capsule `1d655a1`
  (`ts/v0.6.1`). `go.mod`'s `capsule-viewer/go` moves to the same viewer
  commit; `agent-action-capsule/go` stays at v0.12.0. The presentation
  goldens' pages and snapshots, and the presentation pass's build
  snapshots, are re-emitted from their unchanged bundles.

### Report pages take capsule-viewer's contract-report module (capsule-viewer#58, #66, #70)

- **Changed:** the pages capsulectl writes take capsule-viewer at
  `20f815d`. A Result bundle that opts into `contract-report/v1`, as the
  EU AI Act and airline-support-outcomes packs now do
  (evidencebook-skills#39), opens as capsule-viewer's contract-report
  page:
  - a callout at the top says the contents checked, with the counts and a
    link to "How this page was checked";
  - the sealed coverage and its buckets come first, then the per-case
    answers;
  - provenance folds to one line;
  - rule and test rows open in place, with no script needed;
  - a Result whose claims the card cannot place one by one is drawn as the
    generic Result page.

  Bundles that carry only the earlier `outcome-report/v1` or
  `eu-ai-act-compliance/v1` block keep their cards, which now share the
  same layout. A page built with no `--depth` now opens at L0, the first
  screen with its rows closed, where it opened fully before; `--depth L2`
  still opens every section, but each entry under "Records in this bundle"
  stays closed until it is opened (capsule-viewer#76).
- **Fixed:** the "Verified by" line names the capsule-viewer commit the page
  came from ("at 20f815da0688"), where it read "at commit not stated"
  (#363): `scripts/build-report-runtime.mjs` defines
  `__CAPSULE_VIEWER_COMMIT__` as the pinned commit.
- **Changed:** `agent-action-capsule/go` moves from v0.11.0 to v0.12.0 in
  both modules, so the Go resolver knows `contract-report/v1` as the page
  does: v0.12.0 registers it and adds its built-in manifest (the seven
  built-ins are vendored from that release), and its interop verifier
  accepts `out_of_scope` in the pair invariant. `result build` and this
  tool's Result schema are unchanged. `presentation list` names the
  contract-report module by the id the page registers in place of
  agent-action-capsule's built-in, `capsuleviewer.contract-report/v0`.
- **Changed:** the vendored runtime (`evidence-graph.iife.js`) is rebuilt
  from capsule-viewer `20f815d` and agent-action-capsule `17f0c9f`
  (`ts/v0.6.0`), the verifier the viewer now builds against. `go.mod`'s
  `capsule-viewer/go` moves to the same commit. The presentation goldens'
  pages and snapshots, and the presentation pass's build snapshots, are
  re-emitted from their unchanged bundles.

### Evidence Bundles come from AAC's producer (#353)

- **Changed:** `bundle`, `disclose`, `permalink`, `countersign request`
  and the deal commands build their Evidence Bundles with
  agent-action-capsule's `BuildEvidenceBundle`, the producer AAC's
  libraries share, instead of capsulectl's own assembler. Book profiles
  are unchanged. A new bundle differs from an rc21 one in five ways:
  `checkpoint` carries the signed checkpoint once, as `cose`, without the
  older `statement` copy; `checkpoint` no longer repeats the signing
  body's `kind` and `v`; a countersign request bundle states
  `payloads_mode` `selected` (the draft's value) instead of `none`; a
  bundle that reveals nothing has no `disclosures` member instead of an
  empty one; and `completeness.missing` lists missing citations in the
  order they are reached. Bundles made earlier still verify.

### A pickup_area holds only what a public area does (#399)

- **Changed (breaking):** a `pickup_area` may hold only letters, spaces
  and `, . - ' ’ ( ) /`, each word in one script, with a number only as a
  5-digit ZIP at the end of the value, or as a number of up to 3 digits:
  an ordinal (`5th`), right after district, ward, zone, exit, route or
  highway and ending its part (`Exit 12, Commerce Dr`), or right before
  block(s), mile(s), min(s) or km. Anything else is refused as written:
  a house number, a unit, a ZIP+4, another country's postal code, two
  numbers joined by a hyphen, markup, `#`, `|`, `&`, an escape, a control
  character, or a word mixing scripts. So lookalike letters, marks and
  markup no longer hide a street address. Values that passed before and
  are now refused include `78701-1234`, `Pier 39`, `Exit 12 Commerce Dr`
  (it needs a comma) and `<b>Hyde Park</b>`.

### Docs

- A design note says how the book and deal plugins leave this repository
  (`docs/design/326-plugins-leave-the-core.md`, capsule-cli#326). No
  behavior change.

## v0.1.0-rc21

### Every page has a margin, a "Contents checked" pill and a folded check section (capsule-viewer#60, from #408)

- **Changed:** the pages capsulectl writes (`disclose --html`, `deal
  report --html`, `presentation build`, `book report build`) take
  capsule-viewer's page shell at `c48de50`:
  - every page has a width and padding; receipts keep their 760px theme;
  - the verdict is a small pill at the top, "Contents checked" or "Check
    failed", in place of the "Bundle verification passed" line;
  - the verification details (digests, checkpoint root, the ten checks,
    completeness, extensions) fold into one closed "How this page was
    checked" section, whose line says how many checks passed, how many
    records the file carries and what was not checked here. It opens by
    itself for a bundle that did not verify.
- **Changed:** the same pin brings capsule-viewer's report-card redesign
  (capsule-viewer#41, #42, #48) to `book report build`'s outcome and EU AI
  Act cards:
  - one contract-report view with wording packs;
  - two levels in plain words;
  - who judged, who witnessed and what verified, on the front;
  - a rule-by-rule table with a "could not decide" column.

  A conversation the judge could not decide is now counted as neither
  resolved nor missed: the outcome sample reads 4 missed and 1 undecided
  where it read 5 missed. The old "The terms", "What ran" and "Verification
  details" sections move into the card's own proofs section and the folded
  check section.
- **Changed:** `capsule-viewer/go` moves from v0.1.1 to the pseudo-version
  of `c48de50`, the same commit the vendored runtime (`evidence-graph.iife.js`)
  is built from, so the shell's style hashes and the runtime move together. A
  test now holds the two to one commit; the earlier check that hashed the
  runtime's stylesheets from the vendored file cannot, as the runtime now
  builds them from parts.

### The deal page shows the errand's journey (capsule-cli#370)

- **Added:** `deal report --html` shows the errand's journey under "How it
  went", after "What you asked": one row per move, in order, `<local time> ·
  You | Your agent | The other side · <label>`. A time shows only when it
  changes, and each row opens to its sealed step. A shared copy carries no
  journey, and a report sealed before this release shows no section.
- **Added:** the owner copy's sealed report carries `journey`: for each move,
  who made it, the move, its wording key, its step and its facts. It holds no
  wording-pack words: a reason is its token (plus an "other" reason's own
  detail) and the classes told are their tokens, in spoken order. This shape
  is part of every owner report sealed from this release on. The JSON
  `journey` gains each move's `key`, and skills/deal/README.md documents the
  member.
- **Changed:** the built-in `journey.agent.paused` reads "Waited for your OK".

### The user's OK is their own journey move (capsule-cli#410)

- **Added:** the user's answer to a pause is their own move, `approved` or
  `declined`, at its own step: after the pause and before the act it allowed.
  It is worded from the check it answered: "OK'd paying {amount}" / "Said no
  to paying {amount}", "OK'd sharing your {classes}" / "Said no to sharing
  your {classes}", or "OK'd it" / "Said no". A pause with no sealed answer, a
  `confirm_limits` answer (still the `raised` move) and an answer sealed with
  none of the user's words (`agent_card`) are not this move.
- **Changed:** `chose` names the item when the move recorded one: "Chose
  {item} for you".
- **Added:** eleven `journey.*` wording keys for these moves. A pinned pack
  needs them, or the built-in pack is used.

### A tapped option's label is the user's answer (capsule-cli#416)

- **Changed:** the deal skill says a tapped option's label is the user's
  answer, passed verbatim as `--said`, and a typed answer is passed as typed.
  `--said` is left out only for a card nobody answered in words: it is sealed
  `agent_card`, is not the user's approval, authorizes no post, and in a typed
  deal satisfies no ask. `deal note` already sealed it so; only the words
  change. A post's refusal now names the tapped label as `--said`, and the
  trail line for an `agent_card` answer no longer says "you approved this
  card".

### A sale's listing post, and the classes its listings state (capsule-engine#187, #190)

- **Added:** `deal check --deal sale-…` takes a listing post: `{"action":
  "post", "posting": {"text", "classes", "platform"}}`, classed
  `communication.publish`, sealing the words as `posting.text_commitment`
  and the classes and platform (the site's domain) in the clear. Every post
  check asks with capsulectl's own `public_post` difference, on a card that
  quotes the exact words and names the classes ("This listing states the
  item's condition and price"). No rules checker runs on it, pinned or not:
  the verdict's rules status is the new `not_applicable` (and the typed
  evaluation carries `rules_status: not_applicable`), which `deal list`
  counts under `totals.checks.rules.not_applicable` (always present).
- **Added:** `deal note --deal sale-…` takes the user's answer to a post
  check and the post act (`{"action": "post", "posting": ...}`). Only the
  user's own approval, in their words, on the card shown, authorizes the
  post, of exactly the words checked; anything else is refused and nothing
  is sealed. A sale's own log admits a check, evaluation, approval and
  action for `post` only.
- **Added:** a post whose words say, outside the seller's attributed
  template ("The seller says … (unverified)", one sentence), a word that
  reads as an authenticity claim (authentic*, genuine*, legit*,
  counterfeit*, replica*, fake*, knockoff, knock-off or knock off,
  certified, original, in any case, read through NFKD, format characters
  and look-alike letters) is refused, naming the word and both ways out.
  `authenticity` is in the post's classes exactly when such a sentence
  claims it: either one without the other is refused. A task authority's
  `allowed`, `allowed_actions` and `preconditions` never name `post`
  (`record-common-v0` `plan_action`).
- **Added:** each offer and commit check on a sale's thread seals the rule
  input `listing_classes`: the intersection over the latest post act per
  platform on the sale's log when the check is made (`[]` when they share
  no class, absent with no post). The x-deal-v0 and typed schemas
  (`record-common-v0` `post`, `proposed-action-v0` `posting` and
  `listing_classes`), PROFILE.md (section 6 rule 15, section 8) and
  `check_profile.py` follow: given a sale's log beside its threads, it
  recomputes each `listing_classes`, and reports one it cannot order (a post
  act in the same second, or a copy of the sale's log that ends before the
  check) as not independently established.

### The proposal a seller's offer answers (capsule-cli#364, capsule-engine#193)

- **Added:** `deal check` takes `answering` on an offer only: the capsule id
  of the buyer's message the offer answers, a `message` from the counterparty
  sealed earlier in the same deal. The check seals a `source` ref to it
  (x-deal-v0, and the typed `proposed-action/v0` refs), and the rules checker
  is given `proposal_at` on the offer: the message's `sent_at` when sealed,
  else its sealed `at`. `answering` on any other action, or naming a message
  from the user or the agent, one in another deal or one not sealed, is
  refused; nothing is sealed.
- **Added:** an offer on a deal with no message from the counterparty sealed
  before it carries the rule input `no_proposal: true`, decided by
  capsulectl from its own log and never from the input file, and never beside
  a `source` ref. An offer naming no message on a deal that has buyer
  messages carries neither, so a rule on the proposal's age still cannot
  establish it.
- **Added:** a `message` note may carry `sent_at`, the platform's own time
  for the message (RFC 3339, sealed in UTC at whole seconds, never in the
  future), beside the note's own `at`.
- **Changed:** the x-deal-v0 and typed record schemas, PROFILE.md (sections
  3, 6 rule 9 and 8), `external-check-input/v0`'s `proposal_at`,
  `check_profile.py` (a check's `source` names a counterparty message, on an
  offer only, never beside `no_proposal`; `no_proposal` never after such a
  message; a message's `sent_at` is a real UTC time at or before its `at`)
  with fixtures, and the deal skill (note each buyer message as it
  arrives, with `sent_at` when visible; `answering` on offers). A commit keeps
  the accepted offer's time.

### A seller states how the item reaches the buyer (capsule-engine#192)

- **Added:** `terms.handover`, `in_person` or `shipped`: how a sold item
  reaches the buyer, a closed member of the x-deal-v0 `terms` (both schema
  copies; a typed record's `terms` stay open and the profile documents the
  same rule for them). A seller states it on an offer and the commit that
  rests on it, and on a sale; the seller rules read it at the commit, where
  cash passes only in person. It is not a material or offer field, so no
  count or basis moves.
- **Added:** capsulectl refuses `terms.handover` on a deal where the user
  buys, and, where the user sells, anywhere but an offer or commit check or
  the opening (another check such as a share, pay or represent, an intent's
  `asked`, a change, a close's `delivered`); a value other than `in_person`
  or `shipped` is refused. Nothing is sealed. The profile checker holds the
  same rule (PROFILE section 6, rule 14), with six negative fixtures and the
  sale fixture stating `in_person`.
- **Changed:** the deal skill tells the agent to state the rail and the
  handover on a seller's offer and commit.

### A buyer's acceptance releases a seller's details only while the deal is live (capsule-engine#179)

- **Added:** on a sale's thread, after the seller cancels the buyer's
  acceptance (a `cancel` act after it), every later share check asks,
  until the buyer accepts again: a new built-in difference
  `acceptance_called_off` (question `safety`), with the card reason "the
  buyer's acceptance was called off" (wording key
  `card.why.acceptance_called_off`). A cancel calls off only the
  acceptances before it; a cancel checked but never recorded calls off
  nothing. The rule joins the rule table, so `ruleset_digest` changes and
  the profile's typed fixtures are regenerated.
- **Added:** the check of that cancel asks "The buyer backed out: record
  their acceptance as called off?" (wording key
  `card.question.cancel.acceptance`).
- **Added:** when every commit of a sale thread is older than the cutoff
  of the 31-day history the rules checker was given, the card on a
  seller's share to the thread's buyer says why. When the acceptance rule
  (`release_on_acceptance`, s13) fails and the rules refuse, it reads "Why
  I stopped: your rules don't allow this, because the buyer's acceptance
  is older than 31 days. Ask them to accept again." (wording key
  `card.why.rules_deny.acceptance_too_old`). When s13 could not be
  evaluated and nothing refused, the reason is "the buyer's acceptance is
  older than 31 days; ask them to accept again"
  (`card.why.acceptance_too_old`). Both take `{days}`. A refusal that s13
  did not fail keeps the plain reason. Never on an escalation, a finding
  of another rule, or a share of only the public `pickup_area`; the
  refusal is always said, after at most one reason above it. capsulectl
  holds the whole thread, so it tells this from no acceptance at all.
- **Changed:** the deal skill has the agent record the seller's `cancel`
  in the thread when a buyer who accepted withdraws, rather than closing
  it, and, when the acceptance is too old, make the offer again, record
  the acceptance and check and record the commit before sharing.

### An approval agrees only to the terms its card named (capsule-cli#380)

- **Changed (breaking):** a check's card names every term the check changes
  from what was agreed, whatever the action, in one new line: `Saying yes
  also agrees to <terms>.` (new `card.terms.*` keys: a pinned pack needs
  them, or the built-in pack is used; a pack whose `card.terms.*` entry is
  blank or uses other placeholders than the built-in entry is refused, as
  it could hide a term, and one pinned before is not used). A term that had
  an agreed value says it, `(was ...)`, in the agreed currency. The terms
  are the ones `material_fields_changed` counts, the payee aside.
- **Changed (breaking):** an approved proceed adopts a changed term into what
  was agreed only when the card it was given on named it, with the value the
  check proposed; a named drop applies (a condition dropped is no longer
  agreed). An approval with no card (`standing_intent`, on a check that
  passed) adopts none, so the next check carrying that term still counts it
  and r09 asks at the pay. A term stated for the first time is still
  adopted. PROFILE.md section 6 rule 13 and section 8 say so.
- **Added:** a check sealed under this rule states `rule_inputs_version: 3`
  in its record (x-deal-v0 check body and `proposed-action/v0`), so a
  verifier knows which rule an approval of it adopts by. Each approval
  adopts by its own check's rule: a check without the member re-derives and
  verifies as before, in a deal that mixes versions too. The profile checker
  refuses a check that states it without `material_fields_changed` and
  `material_fields_basis`.

### Producer-selected bundles read as AAC strict reads them (capsule-cli#301)

- **Changed:** a producer-selected Evidence Bundle
  (`completeness.selection` `producer-selected`, no certificate range) no
  longer reads INVALID: `verify --bundle` reads it as INCOMPLETE (exits 3,
  as for any partial bundle), as agent-action-capsule's strict verifier
  does. `interval_coverage` is withheld with `selection_not_checkable`, and
  `per_record_membership` checks each record's own inclusion proof against
  the signed checkpoint. As in AAC strict, such a bundle's checkpoint must
  carry its own `log_id`; one stated only in the completeness certificate
  fails `checkpoint`. A composed/v1 member that is such a bundle is read the
  same way. Every other bundle keeps its verdict, and a bundle without a
  selection whose completeness certificate has no range (`first_seq`,
  `last_seq`) still reads as malformed.

### The user's own authenticity claim, said as theirs (capsule-engine#189)

- **Added:** a `represent` check of class `authenticity` may carry
  `representing.attributed_to: "user"`: the words are the user's own claim,
  said by the agent as theirs and marked unverified. capsulectl accepts it
  only in one fixed template, one sentence: "The seller says … (unverified)",
  e.g. "The seller says it's authentic (unverified)", with no full stop,
  `!`, `?` or line break before "(unverified)", in any script (the set is in
  PROFILE §6 rule 12). Any other words, any other class and any other value
  are refused, and nothing is sealed. The attribution is sealed in the
  x-deal-v0 and typed `proposed-action/v0` check body, and the checker reads
  it there.
- **Added:** every attributed check asks, with capsulectl's own finding
  `attributed_claim`, even when the user ordered authenticity. Its card
  quotes the exact words and says they go out as the user's claim. Only the
  user's own approval, in their words, authorizes the claim: an answer on the
  card with no words of theirs (`agent_card`) or a standing intent never
  does, and their hold leaves it unchecked. The profile (PROFILE.md and
  `check_profile.py`, with a new negative fixture), the wording pack
  (`card.question.represent.attributed`, `card.why.attributed_claim`) and the
  deal skill follow.

### A share check says what was told to its recipient before (capsule-engine#188)

- **Added:** two rule inputs on a share check, from capsulectl's memory of
  what it told whom: `repeat_disclosures`, the classes of `disclosing`
  already told to this same recipient, in this deal or another, and
  `recipient_told_before`, `true` when this recipient was told any class
  before. Told means a sealed disclosure, never a dealing: a party the user
  dealt with but told nothing was not told before. Both are absent when there
  is no memory or the recipient has no identity to remember them by. They are
  in the x-deal-v0 and typed `proposed-action-v0` schemas, the profile
  (PROFILE.md section 8 and `check_profile.py`'s typed seller fixture) and the
  rules checker's input, so the everyday rules (0.3.8) need not ask again
  for a repeat. The input file cannot set them.
- **Changed:** a share the user refused, or data given without authority (a
  disclosure sealed `authority: none`), is not told: only what was given
  with the user's rules or approval counts, for both rule inputs and for
  capsulectl's own first-telling pause.
- **Changed:** a class that went to a recipient before without the user's
  OK is never called a first time. The check's difference reads `Your
  <class> already went to <recipient> without your OK`. The card asks `Your
  <class> already went to <recipient> without your OK. Share it again?`
  (`card.question.share.unapproved_before`; `… Share them again?`,
  `.several`, for more than one such class), with the reason `it went out
  without your OK, so I can't count <recipient> as already told`
  (`card.why.unapproved_before`). When the share gives other classes too,
  the question names every one: `Your <class> already went to <recipient>
  without your OK. This also shares your <told class>, which they already
  have, and your <new class>, which would be new to them. Share both?`
  (`card.question.share.unapproved_also`, `… Share all of them?` with
  `.all` for more than two classes; the clauses `card.question.share.also_told`
  and `card.question.share.also_new` appear only when they have a class),
  whatever reasons the card keeps; `first time they get your <new class>`
  stays the reason. A held disclosure note naming such a class says `You
  haven't OK'd telling <recipient> your <class> before.`
  (`disclosure.held.unapproved`), then `This also shares …`
  (`disclosure.held.also`) with the same clauses. A held first telling
  names the other classes the same way: `First time telling <recipient>
  your <class>, and no check the user approved named it. This also shares
  your <told class>, which they already have.`

### An answer without your words is not what you approved (capsule-cli#421)

- **Fixed:** a payment check answered with none of the user's words
  (`agent_card`) no longer counts as what they approved in the
  merchant-email comparison, so the deal page, the receipt email and the
  report no longer say "You approved (the amount on the card that was
  approved)". The row compares the charge with what the user did approve:
  their latest approval in their own words, before or after the card, or a
  passing check before it, otherwise their limit ("You approved (your
  limit)"), otherwise the agreed price. After an `agent_card` answer, which
  adopts its card's price, neither a later passing check on that price nor
  the agreed price counts; with no limit, the row shows no "You approved"
  line and the charge is not flagged `charged_differs`. A charge over the
  limit is still flagged.

### Tests

- The composed presentation golden is built with the real `deal compose`
  (a buyer gathers both halves of one deal over evidence requests) and
  rendered with `presentation build` (capsule-cli#149).
- CI splits the race pass across six jobs and the plain pass across two, as
  the plugin tests outgrew go test's per-package timeouts (capsule-cli#403).
  A History privacy test now reads the History as JSON, field by field,
  skipping only cryptographic fields, so a random digest no longer reads as
  another deal's amount.

## v0.1.0-rc20

### The receipt moments of the Muse chat contract (capsule-cli#373)

- **Added:** the first receipt says up to two more lines, once per profile:
  the first act or final close when no other deal in the store has an act or
  a close. Every first receipt gets a keep line, with its own wording for a
  called-off errand and for a seller's first receipt (the thread's close); a
  paid purchase with a sealed counterparty name (not an id or a domain) also
  gets a contrast line. It is read from the sealed steps; nothing new is
  stored. A seller's offer or commit is never a receipt moment.
- **Added:** a `called_off` or `not_selected` close, and `deal report` on such
  a deal, says "The receipt shows nothing paid and nothing shared.", or
  "…nothing paid." when a disclosure was noted. A seller's thread says
  neither.
- **Added:** `deal list --json` gives each deal a `say` line (the agreed
  item, how the deal ended, the amount paid, an ended sale's listing, the
  local date with the year when not this year's; demos marked; no id), with
  or without a range.
- **Changed:** the page message's label is `report.private` ("PRIVATE: your
  own copy."), not "View details"; the chat line still ends with "View
  details". 20 new wording-pack keys (`say.receipt.first.*`,
  `say.close.nothing_done`, `say.close.nothing_paid`, `say.list.*`,
  `report.private`): a pinned pack needs them, or the built-in pack is used.
- **Changed:** the deal skill sends the page in its own message with the
  label as its only words; says when a receipt goes out by itself (a
  payment, an errand ending arrived or called off, a sale thread closing,
  an updated receipt only on a material change); closes an errand that a
  hold or deny ended as `called_off` when no act is on record; answers "show
  my receipts" from `deal list --json`, never memory, and re-attaches one
  with `deal report --html`.

### A period's checks and agent anomalies in `deal list` (capsule-cli#385)

- **Added:** `deal list --since/--until --json` adds two members to
  `totals`, both present on every range: `checks`
  (`verdicts`: `pass`/`pause`/`deny`; `rules`: `evaluated`,
  `partly_unanswered`, `not_evaluated`, `not_configured`, `unrecorded`;
  `rulesets`: `[{ruleset_id, definition_digest, checks}]` for the checks whose
  rules were evaluated) and `anomalies`, at most one for each act,
  disclosure or claim sealed in the range that has one (`skipped_check`,
  `acted_against_answer`, `unsealed_approval`, `differs_from_check`,
  `unapproved_disclosure`, `unapproved_representation`). Every step sealed
  in the range counts, sales' own logs included. A known key with nothing
  to count is zero (`rulesets` is `[]`). In `checks`, a verdict or rules word
  outside the known ones is counted under its own key, never dropped; an act
  sealed without a valid check whose rule is none of these counts as
  `skipped_check`, as `deal report` names it. `acted_against_answer` is an act that
  went ahead after the user answered not to proceed; `deal report` lists it
  under `unsealed_approval` (two keys in `totals`, one heading in
  `deal report`).
- **Changed:** the agent answering an ask's card itself
  (`ask_needs_your_words`) is `unsealed_approval` in `deal report` too,
  where it read `skipped_check`; the check was made.

### A 403 at witness delivery is retried (capsule-cli#335)

- **Fixed:** an answered HTTP 403 at witness delivery is stored as
  retryable, not permanent, so the delivery is tried again on the delivery
  backoff (up to an hour apart) and goes through once the host lets the
  request out; if the witness itself keeps refusing, it stays pending. Its
  pending reason is `refused`, and its pending text says the request was
  refused (HTTP 403) by the witness or by a network gate on this host, then
  how to grant the witness's site. 407 and transport errors stay
  `network_consent_needed`.
- **Fixed:** a delivery that an earlier version stored as permanent after a
  403 is made due again, once: `cll checkpoint publish` re-arms its own row,
  and the deal cadence re-arms every such row on its log at the next tick.
- **Changed:** a delivery that failed for good (a 409, a receipt that does
  not verify) now starts "not delivered: " and says it is not retried; it
  was said to be retried at every tick.
- **Added:** `reason` (tick, cadence and terms-seal JSON) and the deal
  receipt's `witness_reason` take the new value `refused`. The deal skill
  and its README name the new pending text.

### Plugin install heals a group-writable home (capsule-cli#354)

- **Changed:** `plugin install` puts back `chmod go-w`, as the checkpoint
  cadence does (#339), on the directories of the plugin root's path that the
  process's user owns under `$HOME` and whose only fault is group or other
  write. Anything else stays refused, now with `not healed: <why>`. The
  result carries `plugin_root_healed` (each directory, mode before and
  after) when it changed anything; a failed install after a heal names the
  directories it tightened. README and docs/PLUGINS.md drop the manual
  `chmod go-w` step.

### `say.error.before` says what the agent did (capsule-cli#377)

- **Changed:** the built-in wording for `say.error.before` is "I couldn't
  record that step, so I stopped before paying or sharing anything." It said
  "… so I've stopped. Nothing was paid or shared."

### A listing's public pickup area may be told as its own class (capsule-engine#181)

- **Added:** disclosure class `pickup_area`, the public area a listing shows
  (a neighborhood, city, region or ZIP, never a street address), distinct
  from `pickup_location`, the exact spot. It is a `share_contact` class in
  `deal check`'s `disclosing`, `deal note --kind disclosure`, the x-deal-v0
  and typed record schemas, the profile (PROFILE.md and
  `check_profile.py`), the wording pack (`class.pickup_area`) and the deal
  skill, with a seller's typed fixture chain in the profile. It is not an
  address class: capsulectl's own acceptance gate does not hold a seller's
  share of it for an accepted offer. The seller rules still hold it from a
  buyer before acceptance until capsule-engine#186.
- **Added:** a disclosure note refuses a `pickup_area` value that reads as
  a street address (a house number before a street type such as St, Ave or
  Pkwy, or a numbered unit such as Apt 4 or #5), naming `pickup_location`
  for an exact spot; nothing is sealed.

## v0.1.0-rc19

### The deal page's as-of line (capsule-cli#362)

- **Changed:** the deal page's as-of line is `This report states the deal
  as of its checkpoint at <time> (the producer's time).` The clause naming
  where a witness is checked is cut; the full witness display is deferred
  (capsule-cli#356).

### One deal per errand: journey records (capsule-cli#340)

- **Added:** the deal opens at the user's first ask, with no merchant (`who`
  `{}`) or way to pay (`recourse` `{}`) yet. Agent moves are notes, one kind
  per label: `searched`, `options` (Showed you N options), `recommended`,
  `chose` (Chose for you) and `couldnt` (Couldn't), sealed as `agent_move`
  records with every text as a commitment. `deal report` lists the errand's
  `journey`, each move labelled from the wording pack (new `journey.*` keys:
  a pinned pack needs them, or the built-in pack is used).
- **Added:** an intent note's `move` (`narrowed`, `switched`, `raised`,
  `picked`) is derived by capsulectl and sealed; the agent never states it.
  An intent that names another merchant (`who`) starts a new leg of the same
  deal: the ask and limits carry over, no approval from the earlier leg
  covers a step in it (nor does an approval given before a new pick: an
  intent naming an item, or the agent's `chose`, in capsulectl and the
  profile checker), and a payee or contact change is compared with that
  leg's first contact, so the user's switch never reads as one while a
  seller's change within the leg still does. Counterparty memory, deal list,
  shared copies, the audit and merchant-email checks read the leg in force.
- **Added:** `delegated: ["item"]` with the user's words (`delegated_said`):
  the agent's `chose` of the item reads "I chose … for you" and does not
  pause as its pick; adding a delegation later is a proposal.
- **Added:** close status `called_off` (actor `user`, reason `called_off`),
  never "nothing delivered".
- **Changed (breaking):** an intent note that changes nothing is refused
  (the user's words go in a message). A note's `asked` adds to the ask in
  force, field by field.
- **Changed:** the limit binds on the largest of the price, the amount
  charged (tax included) and the authorized maximum; quantity 1 and a free
  shipping or delivery option are not the agent's picks, while "free
  shipping" in any other field still is; a postal code or region added to
  the place asked for is not a difference. Rule inputs version 2 counts
  `material_fields_changed` against the leg's agreement (a check sealed
  under version 1 keeps its count).
- **Changed:** the deal skill opens the deal at the first ask, keeps one
  deal per errand, seals a switch as the user's intent, puts tax in the
  amount, starts a sell with `deal sale new`, never offers to change the
  user's rules after a deny, and finds deals with `deal list`.
- **Changed:** `x-deal-v0` adds the `agent_move` record type, an intent's
  `move`, `proposed`, `delegated` (with `delegated_commitment`) and
  `why_commitment`, and `called_off` to the close status, reason and
  outcome enums. A baseline may omit `counterparty` and `recourse` when the
  deal opened before a merchant was in play.

### A platform is a recipient of its own (capsule-cli#348)

- **Added:** a share check may say `"disclosing_to": "platform"` with a
  `recipient`, for a marketplace, booking site or mail relay between the user
  and the counterparty; a `disclosure` note says `"to": "platform"` with
  `who`. The check seals `recipient_role: "platform"`, which everyday 0.3.7
  and seller 0.1.4 allow for the user's email only (capsule-engine#173). A
  platform is known by its registrable domain, a marketplace's included, so
  its approval and its first telling are its own: the first email to a
  platform pauses, and another platform pauses again. With the counterparty
  memory, a repeat to the same platform does not pause as a first telling;
  without it, every email to a platform pauses as one. A shared copy says
  "Told a platform", and `deal list` counts a platform under `to_others`,
  once per platform.
- **Fixed:** in typed records, a disclosure covered by a passing check's own
  evaluation (no approval needed) is matched to that check's recipient, so a
  repeat telling to the same recipient is no longer sealed as
  `approval_for_another_party`.
- **Changed:** `platform` joins the `disclosing_to`, disclosure `to` and
  `recipient_role` enums of `x-deal-v0` and the typed records. `other`
  still seals `third_party`.

### Merchant email keys over DNS-over-HTTPS (capsule-cli#343)

- **Added: a DNS-over-HTTPS fallback for DKIM and DMARC records.** `deal
  note --email` and `deal tick` read them over DoH (RFC 8484) in the same call
  when the system resolver hands out placeholder addresses (198.18.0.0/15 or
  IPv6 unique local addresses), cannot be reached, or finds no DKIM key. A
  proxied resolver is asked last, and counts only when it finds every key.
  The default endpoints are Cloudflare and Google, by name and then by
  address. This sends the looked-up names (`selector._domainkey.<signing
  domain>`, `_dmarc.<From domain>`) to the DoH provider, which also sees
  the IP address the query comes from (the device's, or its proxy's or
  VPN's), so it learns which merchants email the user and can tie them to
  that address. DoH is a fallback only. `CAPSULECTL_DOH` sets the
  endpoints, and `CAPSULECTL_DOH=off` turns DoH off.
- **Added: `key_resolver` and `key_dnssec` on `merchant_email`.** A new
  record says whether its records came from the system resolver or over DoH
  (`system` | `doh`), and whether the DoH resolver reported that it validated
  DNSSEC (`resolver_validated` | `not_validated`; capsulectl does not check
  the chain itself). Records sealed before keep their bytes and have neither
  field. The DoH endpoint stays on the device.
- **Changed: a deferred email states its reason.** `deal note` and each
  `waiting` entry of `deal tick` give the `reason` the records could not be
  read.

### Every command says what to tell the user; the check card; one verdict on the receipt (capsule-cli#345, #344, #341)

- **Added:** every result object carries `voice` (`silent`, `ask`, `line`,
  `report` or `error`) and, when it speaks, `say`: the words to tell the
  user, verbatim, with values filled from the sealed steps (#349). Words come
  from an `aac.wording-pack/v0` pack: the built-in neutral pack, or a
  complete pack the profile pins with `profile update --wording FILE`. A
  pinned pack that cannot be used falls back to the whole built-in pack and
  says so (`"wording": "built-in"`, and in `doctor`).
- **Added:** a command that fails with nothing printed writes one object
  with `voice: "error"`, `hold: true` and `say`; the stderr line and the exit
  code are unchanged. `--html` on `deal note --kind act`, `deal close` and
  `deal note --kind evidence --email` writes the updated receipt in the same
  step.
- **Changed (breaking):** the check card (`approval_text`, `card`) is one tag,
  one question, the money facts, at most two reasons, at most one "Good to
  know" and "Next": no bullets, rule ids or ruleset. Buttons carry the amount
  ("Pay $1.90"); "anyway" is only over a limit or floor the user set, never
  for a rule that asks; a deny says what will not happen, with one "OK"
  button. A pay that a share card promised to ask about pauses for it
  (built-in rule `promised_ask`, so `ruleset_digest` changes; checks
  sealed before keep the old digest), and the answer is sealed (#344).
- **Added:** `external-check-result/v0` findings may carry `disposition`
  (`ask` or `deny`), sealed with the finding; each failing rule's words follow
  it, so a rule that only asks never reads "Not allowed" under a deny (#341).
- **Changed:** the deal page states no verification of its own: no witness
  line, no list of checks and no signature line. The core's verification
  section is the one verdict (agent-action-capsule#346, #347 track the core's
  words and its reading of the cadence witness). Times are the reader's
  local time; a step is listed once, and an act's or a close's own step is
  not said again under it; what the user approved on a check is no anomaly
  of the agent's, while a signal from the other side (a changed payee, a
  code asked for) stays one (#341).

### `result build` in the core: a Result page with no plugin (capsule-cli#324)

- **Added:** `capsulectl result build --profile NAME --result FILE` seals an
  Evidence Result v0 into the profile's log as one Capsule whose payload is
  the Result and whose references cite, `acted_on`, every record its claims
  cite. It checks the document against the vendored `evidence-result-v0`
  schema, refuses headline values that do not recompute from the claims and
  any cited record the profile's log or artifact store does not hold, and
  seals nothing new for the same document. `--out` writes the canonical
  Result and `--capsule-out` the sealed Capsule. Then `cll checkpoint create`,
  `disclose --root` and `presentation build --bundle` write the Result page,
  with only the core installed.
- **Changed:** `capsulectl result` is no longer only an alias of
  `book result`. On a book profile `result build` still forwards to
  `book result build`, with a notice, until v0.2.0; `result open` still
  forwards to `book result open`.
- **Changed:** `book result build` and `deal terms-result` check and
  canonicalize a Result with the core's code, so all three write the same
  bytes for the same document. Their output does not change.
- **Changed (breaking):** the vendored `evidence-result-v0` schema is the
  one from agent-action-capsule#354 (for agent-action-capsule#353): every
  claim cites at least one record (`minItems: 1` on a claim's `evidence`).
  `result build` refuses a claim whose `evidence` is empty, including a
  story claim about a recorded absence, which cites the absence record
  instead. This breaks a caller of `result build` whose Result has a
  claim that cites no record; `book result build` already refused one.
  The same schema adds the optional (PROPOSED) aggregate member
  `coverage_declaration` (agent-action-capsule#312), which `result build`
  now accepts, and description-only edits to the claim types of its §4.1.

### `deal list --since/--until`: a period's deals, totals and who was told what (capsule-cli#342)

- **Added:** `deal list --since DATE --until DATE` lists the deals with a
  step sealed in the range, each as it stood at the range's end. A bound is
  a date (the start of a UTC day) or an RFC 3339 time with its offset;
  `--since` is inclusive, `--until` exclusive, and either may be left out.
  An unreadable date or an empty range is refused.
- **Added:** with `--json` and a range, the output is one object: the
  range, its deals, `totals` (deals by result, sales, and per currency what
  was paid out, fees included, what was returned, and what sale threads
  closed for) and `disclosures` (for each class, how many distinct
  recipients were told it, as sellers, buyers or others; classes and counts
  only). It is read from the sealed steps and adds no
  evidence. Without a range the output is the array it was. The shape is in
  the deal skill's README, under "A period".
- **Added:** each `deal list --json` row of a deal carries `result`:
  `bought`, `sold`, `called_off`, `not_chosen` or `open`.

### A seller's representations carry their authority (capsule-cli#269)

A seller's deal, or a sale and its threads, opened from this version
follows these rules. Deals and sales opened before keep the old ones.

- **Added: `ordered_representations`.** The intent and task authority gain
  this field: the representation classes the user's words order the agent to
  state ("tell them about the scratch" is `condition`). It is empty when
  nothing is ordered. Adding or dropping a class is a proposal that applies
  only on the user's `confirm_limits`. On a sale, the order is fixed for all
  its threads.
- **Added: the `represent` check.** It runs before the agent tells the buyer
  anything: `deal check {"action": "represent", "representing": {"class",
  "text"}}`. An ordered class passes, and its claim is sealed with
  `authority_basis` `[task_authority]` and no approval. Any other class asks,
  and only the user's approval of those exact words authorizes the claim. The
  claim is noted with `deal note --kind claim --check CHECK_ID`.
- **Added: unchecked claims.** An agent claim that nothing authorized is
  still sealed, with `unchecked: true` and the rule why. A change of details
  after the check leaves it unchecked too (`changed_after_check`).
- **Added: the words are bound.** An authorized claim is sealed under its
  check's nonces, so its commitments equal the checked ones. Approving the
  words approves no change of terms.
- **Added: ordered statements come first.** While an ordered class is
  unstated in a deal (on a sale, in each thread), an offer, commit, sign, pay
  or share check asks with no way to proceed. A cancel is never held.
- **Added: the rules checker sees the basis.** Its `deal_claims` entries carry
  each claim's `authority_basis` or `unchecked`. A `represent` check gets
  `representing_opening`, the exact words. After a `confirm_limits` it gets
  the confirmed `task_authority_record`.
- **Breaking:** where these rules apply, these are refused:
  - an agent claim with no class;
  - a `merchant`, `platform` or `external` claim with no `source`;
  - agent claims in a seller's `deal open` input. Note them after open,
    each against a `represent` check.
- **Changed (fixtures):** presentation goldens 8 and 9 are rebuilt with
  `represent` checks. Golden 8 now carries `x-deal-seller/v0`, so the
  viewer's seller receipt is selected for it.

### The checkpoint cadence heals a home a host made group-writable again (capsule-cli#336)

- **Fixed:** a host may recreate the home directory as `2770`. The trust
  walk then refused the deal plugin, and the scheduled checkpoint
  cadence stopped with nothing in its log. `cll checkpoint cadence` and
  `doctor` now put back the `chmod go-w` that plugin install required, but
  only on directories the process's user owns under `$HOME` whose only
  fault is group or other write. Anything else stays refused. The trust
  walk is unchanged. `docs/PLUGINS.md` lists the exact conditions.
- **Added (output):** the cadence writes `plugin_root_healed` (each
  directory, with its mode before and after) or `plugin_root_heal_failed`
  to its log. When the deal plugin was not run it writes
  `plugin_unavailable`, and when it was killed or could not be started it
  writes `plugin_stopped`, each with the error that stderr shows, so a
  stopped cadence is no longer silent. `doctor` adds
  `plugins.user_root`, and `doctor --install-check` adds `user_root`.

### A seller's later evidence chains to the close and shows under it (capsule-cli#266)

- **Added (fixture):** a seller's sale that closes with its delivery still
  due, then takes two later records, each chained `confirms` to the close:
  the carrier's delivery scan, checked, which resolves the carried due date,
  and the buyer's refund request, a claim and so unverified. It is the deal
  plugin's `testdata/deal/seller-later-evidence` and presentation golden
  `9-seller-later-evidence`; its bundle verifies VALID.
- **Fixed:** in "Where this deal stands", a record linked to the close read
  "confirms the close: after the close (confirms it): …". It now says the
  relation once: "confirms the close: evidence (buyer_refund_request), unverified: …".
- **Changed (scripts):** `scripts/presentation-goldens/build-fixtures.sh`
  builds the capsulectl-deal plugin and runs it from a plugin root of its
  own, so the deal fixtures build again since the deal commands moved into
  the plugin.

### Report pages take their runtime from capsule-viewer; a bundle may be up to 256 MiB

- **Changed (build).** The browser runtime every `report build`, `deal
  report --html` and `presentation build` page embeds
  (`internal/cli/assets/evidence-graph.iife.js`) is now built from
  capsule-viewer's page code (main @ `1d39351`, the commit
  `capsule-viewer/go v0.1.1` tags) with agent-action-capsule's verifier
  (main @ `4593d42`, the commit `ts/v0.5.0` tags, which carries
  agent-action-capsule#317; it was `6ffe7c1`), by the new
  `scripts/build-report-runtime.mjs`. Until now it was aac's own copy of
  the views, which no longer moves since the pages moved to capsule-viewer
  (capsule-viewer#33). The page global keeps every name it had, so the deal
  view's verifier calls still resolve. `scripts/iife-sync.sh` and `make
  iife-check` take a capsule-viewer checkout before the aac one
  (`make iife-check CV=… AAC=…`), and CI rebuilds from both pinned commits
  and refuses either if it is not on its repository's main. The rendered
  text of every golden is unchanged; only the runtime's script hash in each
  page's CSP moves. The runtime still opens with `"use strict";`, as on
  main: both repositories build and test it under `"strict": true`, and the
  build refuses an output without it.
- **Known gap until the next capsule-viewer bump.** agent-action-capsule#317's
  transcript display does not reach the pages yet. When a bundle withholds
  every judge report, the model at `4593d42` can mark a conversation's
  transcript `cited`, and the outcome card at capsule-viewer `1d39351`
  knows only `verified`: it shows "Not shown: …" with the reason and no
  turns, where main showed "Not in this bundle …". It never shows such a
  transcript as verified. No golden withholds every report, so the goldens
  do not cover this text. The capsule-viewer release that carries #317's
  view brings the display.
- **Changed.** `report build`, `presentation build`, `presentation list` and
  `verify --bundle` read a bundle of up to 256 MiB (`MaxBundleInput`), not
  12 MiB: a book's bundle carries its transcripts, and a week-per-book
  bundle with them is 15-20 MB, a month 106-142 MB. The bound also caps
  memory: rendering the 142 MB month peaked at about 0.9 GB resident.
  Every other input keeps the 12 MiB limit.

## v0.1.0-rc18

### Releases are approved in GitHub, not signed on a laptop

- **Changed (release procedure).** Release tags are no longer SSH-signed. A
  release run now waits for a person to approve it in a GitHub deployment
  environment: `release` for a full release, where whoever pushed the tag
  cannot approve it, and `release-candidate` for a pre-release, where they
  can. A tag ruleset limits who can create `v*` tags and stops anyone,
  admins included, from moving or deleting one. `docs/RELEASING.md` has the
  procedure; `scripts/release-settings.sh` has the repository settings.
- **Removed (breaking): `capsulectl release register` and `capsulectl
  release watch`.** Releases are no longer registered in the witness's
  transparency log, and no release-statement files (`.release.json`,
  `.release.cose`, `.release.registration.json`, `.release.sigstore.json`)
  are published from this version on. What keeps a release nobody meant
  from reaching users is the reviewed change that pins its hashes on the
  install page; the build attestations stay, and are logged in Sigstore's
  public Rekor. `docs/RELEASE-TRANSPARENCY.md` says what each covers. A
  monitor that ran `release watch` can be retired.
- **Removed:** `release/known-unsigned.txt`.

### A jsonl profile's bundles verify VALID, so `report build` writes their page (capsule-cli#148, #312)

- **Fixed:** `bundle`, `disclose`, `permalink` and `close --bundle-out` on a
  jsonl profile now carry each record's producer signature, the one stored
  when the record was sealed, so `verify --bundle` calls them VALID instead
  of INCOMPLETE ("producer_signatures withheld"). `report build` now writes
  the page for a Result disclosed from a `result build` record. Nothing is
  re-signed. Each record also shows the key that signed it.
- **Changed:** `close --bundle-out` now declares the profile's signing key as
  its producer key, as `bundle` and `disclose` already did. A
  countersignature by that key on a Close bundle now reads as not
  independent.
- **Changed (build):** evidencebook moves to v0.7.0.
- The tests and their approach are adapted from Jody Edmondson's
  capsule-cli#231, and Jody diagnosed capsule-cli#148.

## v0.1.0-rc17

### Before you upgrade: there is no going back to an older version

After this version opens your deals, keep using this version or a newer one.
An older capsulectl may refuse to open your deals, and copies it makes can
show when a deal was shared.

- **Want the option to go back?** Make a backup of your capsulectl data
  before you upgrade. First pause anything that runs `deal tick` on a
  schedule (cron, launchd), and make sure no capsulectl command is running.
  For a deal profile set up the usual way, your data is two things: the
  folder you gave `deal init --dir` (the deal skill uses
  `~/.local/share/capsule-deal`), and your profile file,
  `~/.config/capsule/profiles/deal.yaml` (named after your profile, under
  `$XDG_CONFIG_HOME/capsule/` if you set it). If your profile points at files
  outside that folder, such as a rules file given to `--materiality` or keys
  moved with `profile update`, back those up too. For example:

  ```
  mkdir ~/capsulectl-backup
  cp -Rp ~/.local/share/capsule-deal ~/.config/capsule/profiles/deal.yaml ~/capsulectl-backup/
  ```

  To go back, with nothing running, move the upgraded folder aside rather
  than copying over it, then copy the backup in:

  ```
  mv ~/.local/share/capsule-deal ~/.local/share/capsule-deal.after-upgrade
  cp -Rp ~/capsulectl-backup/capsule-deal ~/.local/share/
  cp -p ~/capsulectl-backup/deal.yaml ~/.config/capsule/profiles/deal.yaml
  ```

  Then run the older version. Anything recorded after the backup is not in
  the restored copy; it stays in the folder you moved aside.
- **Sending receipts or deal copies?** Ask the people you send them to to
  update too. An older capsulectl shows copies made by this version as
  INVALID, even though they are valid.
- **Using capsulectl through Muse?** Nothing to do. Muse updates its own
  copy.

Details, for developers. Three changes make the upgrade one-way:

- **The shelf (capsule-cli#242):** the first deal command that writes moves
  the store onto its evidencebook shelf, once. An older build can still run
  on it, but its ticks show how many book deals the store holds.
- **Standalone reports (capsule-cli#294):** a copy an older build makes puts
  the deal's reports back in its log, where they show when the deal was
  shared. An older capsulectl also reports new copies as INVALID: verify them
  with this version or later.
- **A closed sale (capsule-cli#271):** an older build cannot load a closed
  sale's log, and if this version crashed at the wrong moment, an older build
  refuses to open the whole store.

### Dependencies pinned at release tags

- **Changed (build):** agent-action-capsule/go is pinned at `go/v0.11.0`
  (from main 81e804a) and capsule-viewer/go at `go/v0.1.1` (from main
  42ee2e5), in both modules. Each tag is a fast-forward of the commit pinned
  before. agent-action-capsule adds its bundle producer, with
  `bundle-produce` and `bundle-verify` modes in its cross-language check
  command; `bundle.DecodeFragment` now says "trailing data after the JSON
  value" when it refuses a fragment for that reason, and accepts and refuses
  the same inputs as before. capsule-viewer's only change is in its Python
  package. The vendored presentation manifest schema is unchanged.

### A deal's shared copy is checked with capsulectl alone (capsule-cli#243)

- **Added (tests, CI):** a `portability` job runs a deal on a shelf book with
  typed records, shares it with its counterparty and exports its records, then
  deletes the session (home, profile and keys, deal store, plugin root). With
  capsulectl alone, it opens the shared page offline in headless Chrome, runs
  `verify --bundle`, and checks that each exported typed record is the exact
  payload a bundle record commits to, that the chain links, and that the page
  shows what each step's record says. An edited bundle, a dropped record and
  an edited exported record must each fail. The Chrome render helpers move to
  `scripts/lib/page-checks.sh`.

### Presentation manifests follow presentation contract revision 1 (capsule-cli#293)

- **Fixed:** capsulectl checks plugin presentation manifests against
  presentation contract revision 1 (agent-action-capsule 81e804a): a
  declarative field may name the `member` it reads (`agent_input` or
  `agent_output`), and wording packs document the reserved `page.title` and
  `module.title` keys. This only widens what is accepted.

### Dispatch runs the launcher it checked (capsule-cli#296)

- **Fixed (security):** the trust walk judged a launcher's resolved path,
  but the handshake and dispatch executed the name it was found by, so a
  symlink hop on that name could be repointed between the check and the
  exec. The handshake, dispatch and the rules checker now execute or read
  the checked resolved path. The walk also checks the hops below the root:
  every directory a name is looked up in, and the owner of every link
  followed.
- **Existing installs:** a launcher reached through a symlink in a directory
  another user can write, or through another user's symlink, is refused,
  with the directory and the fix named. A plain launcher, or one linked
  within a safe root, runs as before. A shell-script launcher's `$0` is now
  its resolved path; argv[0] is unchanged.

### A deal's copies no longer show when it was shared (capsule-cli#294)

- **Changed (privacy):** a deal on the shelf whose log holds no sealed report
  seals each copy's report as a standalone signed record, carried in the copy
  (`x-deal-report/v0`) and on no log, bound to the deal checkpoint the copy is
  made at. Such a deal cuts a checkpoint at every step, at the step's time, so
  a copy cuts none. Its log, its copies and its History no longer show when or
  how often the deal was shared, and a copy carries no other copy's report.
- **Limits:** a deal whose log already holds a report, and every legacy deal,
  keeps its reports in its log, where they still show when it was shared. A
  report can be bound to an earlier state of the deal, and is shown as of that
  checkpoint's time. A `deal respond` time floor later than the deal's last
  step cannot be met. A standalone report rests on the step-signing key alone.
  After a signing-key change, a deal can be copied again only once it has a
  step under the new key. A sale's own log still records each sale bundle's
  cut, so a sale's bundle shows when and how often it was made. A copy
  stripped of both its deal section and its report is its steps only and
  verifies as such: it shows no report text.
- **Compatibility:** verify new copies with this version or later: an older
  capsulectl reports them as INVALID. Do not run an older build on the store
  (see "Before you upgrade" above). A deal an earlier build made with no
  report on its log, and steps after its last checkpoint, is covered at its
  last step's time by its next command or `deal tick`; until then `deal
  history` refuses it.

### A shared copy keeps the bounds back by field, and reads text, not code (capsule-cli#247, #286)

- **Changed (privacy):** structured amounts are kept back by field, never by
  value. The private bounds (`intent.max_total_minor`,
  `intent.min_total_minor` and their openings) stay out of every counterparty
  copy, and the floor stays out of every shared copy. A final close's amount
  and currency openings travel whenever the close commits money terms, also
  when the amount equals a bound. A merchant charge in the deal's currency, or
  naming none, counts as an amount paid, so a receipt charging exactly the
  limit is shared. Free text (notes, page and report text, merchant prose) is
  still matched by value.
- **Fixed:** the share gate reads a page's markup outside every `<script>`
  and `<style>`, its bundle and its page data, never script or style code.
  A currency string in the vendored verifier no longer refuses a share. It
  fails closed: a copy that does not start with an HTML doctype, or a page
  with no bundle script, is read whole, less the two vendored scripts where
  they match byte for byte; a data script whose value does not decode is
  read whole.

### A merchant email's seal claims only the seal time (capsule-cli#289, #306)

- **Fixed (copy):** the receipt email's note and the deal page said our seal
  shows this device "kept these exact bytes from that time on", and the skill
  "from this time on", which a seal does not show. All three now say it held
  them no later than the seal time. The email note and the page add that who
  signed them is checked by the signing domain's DKIM signature, not by our
  seal; the DKIM signature is described as the signing domain's (the
  merchant's, or its mail provider's), showing what that domain signed.
- **Fixed (copy):** the labels around it say the same. The receipt email and
  the deal page label the signature "DKIM signature:" and its key "Signing
  domain's key", where both said the merchant's. An email whose key can't be
  read yet says "the email's signature will be checked later". The skill and
  README name the signing domain's key, and our seal's words in `we_say` say
  "no later than this time". An email is still called merchant-confirmed only
  when a passing DKIM signature's domain is aligned with the sender's.

### Pages say what they checked; the outcome report counts its criteria (capsule-cli#147, #150)

- **Changed (pages):** the browser viewer every page embeds is rebuilt from
  agent-action-capsule main 6ffe7c1 (it was 6f5b48f). Every page's
  verification section now lists what it "Checked on this page" and what it
  did "Not checked on this page" (the checkpoint signature and producer
  signatures), and points to "a full verifier" rather than "the CLI".
- **Fixed (outcome report):** a resolved conversation reads "all four
  criteria met" for a pack that judges four, not always "nine"; the page fits
  a 390px viewport.
- **Changed (build):** capsule-viewer/go moves to main 42ee2e5, whose
  Content-Security-Policy lists the outcome report's new stylesheet.

### `verify --history`, and deal steps under the History profile's headers (capsule-cli#264)

- **Added (verify):** `capsulectl verify --history FILE [--holder-key HEX]...`
  checks a one-file History at the Basic level of the Personal-Agent History
  Profile v0 (`personal_history_basic/1`) with agent-action-capsule's
  verification. The output keeps the profile's order: what a Basic History
  never establishes, coverage, each deal's state and records (committed and
  claimed times apart), the checks not run, and the limits. `--holder-key`
  pins the holder's keys; without one it names the keys the envelopes carry
  and exits 3 if nothing else failed. It exits 0 when the History is
  accepted, 1 when it is not, and 2 for a file that is not the profile's
  container or for conflicting flags.
- **Changed (deal):** readers admit a deal step sealed with the History
  profile's headers, `record_type` `step_completed` or `step_failed` with
  `correlation.exchange_id` the deal id, as a step: the step source, `deal
  history` and the terms reconcile's half admission. A step correlated to
  another deal is refused.
- **Compatibility:** this build still seals `deal_step`; it reads the new
  headers one release before a writer emits them. `verify --bundle` on an
  `x-history-selection/v0` file is unchanged.
- **Changed (build):** agent-action-capsule/go moves to main 81e804a.

### A sale can close, and then leaves the cadence (capsule-cli#271)

- **Added:** `deal sale close --sale ID` ends a sale: it seals a
  `sale_close` record on the sale's log (a new x-deal-v0 record type), after
  first sealing any thread opening a crash left out. No buyer's thread opens
  on the sale afterwards; a thread already open goes on and closes on its
  own. A closed sale leaves the cadence's live set once a witnessed tick
  holds its last checkpoint, so ticks stop re-checkpointing it; a later
  `deal sale bundle` cut brings it back until the next one. `deal list` and
  the sale bundle's summary show the sale closed. The command prints that the
  sale "stops taking new buyers" and how many negotiations are still open
  (`status`, `open_negotiations`).
- **Compatibility:** existing sale bundles verify unchanged, and an older
  `capsulectl verify --bundle` skips the new record. An older profile checker
  refuses it. An older capsulectl cannot load a closed sale's log: its `deal
  open --sale`, `deal sale bundle` and `deal sale export` on that sale fail
  with the store's conflict error, and if this version crashed after indexing
  a record on a closed sale but before appending it, an older build refuses to
  open the whole store (see "Before you upgrade" above).

### Dispatch checks the per-user plugin root's parents (capsule-cli#270)

- **Changed (security):** the trust walk now checks the per-user root's
  ancestry, from `$HOME` down to `~/.local/lib/capsulectl/plugins`, where it
  used to trust the root's parents. Every directory a name is looked up in
  while the path resolves, through every symlink hop, the resolved root,
  and the parents of each up to `$HOME` (up to `/` outside `$HOME`) must be
  owned by the user or root and neither group- nor other-writable. The
  user's own group is not exempt.
- **Existing installs:** a plugin installed by hand under a group-writable
  `~/.local` (stock Ubuntu, umask 002) no longer runs; `capsulectl plugin ls`
  lists it as refused, with the fix:
  `chmod go-w ~/.local ~/.local/lib ~/.local/lib/capsulectl`. The same
  holds for presentations and the rules checker, which use the walk, and for
  a `~/.local` symlinked into a directory another user can write.
- **Fixed:** running a refused plugin, or `deal` or `book` when theirs is
  refused, prints the refusal and its fix, where it said only "operation
  failed" or that the plugin was not installed. The fix's `chmod go-w` names
  each path as one shell word, so it runs as pasted. A refused launcher whose
  name is not a plugin name is listed by `plugin ls` only, and a refusal's
  paths are printed with control characters escaped.

### A held object on a buyer's deal (capsule-cli#250)

- **Added (deal):** `deal note --kind evidence --object FILE --object-type TYPE
  [--octets as-transmitted|jcs] [--headers FILE]`, or `--object-digest HEX`,
  seals an object the user holds on a buyer's deal kept on a book. The record
  carries one entry in the settlement-records wrapped-object shape (`type`,
  `SHA-256`, digest; never the content), `wrapped_octets`, `wrapped_held` and a
  salted `carrier_headers_commitment`. The octets and headers stay in the
  step's local record in the artifact store (at most 4 MiB). A type not in the
  settlement-records registry is sealed as the sealer's label and reported
  `type_registered: false`; a registered type is held to its octets rule.
- **Added (deal):** `deal verify-object` checks a held object and its headers
  against their step, or (without `--deal`) any object against a record's
  digest; `--out` writes the held bytes back out. It exits 1 on a mismatch.
- **Changed (profile):** x-deal-v0 `evidence` records gain the optional
  `wrapped`, `wrapped_octets`, `wrapped_held` and `carrier_headers_commitment`
  members, and the `wrapped_object` source.

### `deal terms-result`: both halves of a deal's terms, reconciled and sealed (capsule-cli#242, #278)

- **Added (deal):** `deal terms-result --deal ID --response RECORD --openings
  FILE --out FILE` reconciles the profile's own final close with the
  counterparty's, read from the copy recorded with `deal request --response`
  and opened with the `field_openings` of the counterparty's `disclose
  --share counterparty` file. It writes an `evidence-result-v0` Result with
  two `type: reconcile` claims, amount and currency, cited against
  `ec:deal-reconcile@1`. It prints the claims' verdicts and tallies and the
  sealed record, never a per-deal reconcile state. `--from`/`--to` set the
  period, by default from the earlier close to one second after the later.
  A half that is not signed under the expected key, not a step of the deal,
  not covered by a checkpoint or not final refuses the reconcile.
- **Added (deal):** every Result is sealed before its file is written, as an
  `evidence_result` record on the deal's results book, `deal/<id>/results`,
  never on the deal's own book; a retry returns the same record. `deal
  terms-result --share counterparty --recipient WHO [--record ID] --out FILE`
  shares one Result, with only its payload disclosed, and records the share.
  No deal copy carries a results record.
- **Added (deal):** `--check RESULT [--sent FILE]` checks a Result against
  the inputs given: `equal`, `differs` (exit 5) or `cannot_reproduce` (exit
  2). A third party both parties name checks one from files alone, with no
  profile or store: `--check RESULT --copy FILE --copy FILE --party-key
  SIGNER:CHECKPOINT --party-key SIGNER:CHECKPOINT`.
- **Changed (disclose):** `disclose --deal ID --share counterparty --to WHO`,
  and `--share adjudicator --to WHO`, carry the openings of the party's own
  final close when it commits money terms, and the output says whether the
  file carries them and, if not, why. Respond copies, report pages, the user's
  own bundle and History carry none.
- **Changed:** `result build` refuses a claim under `ec:deal-reconcile@1`,
  and `result open` lists such claims as not recomputed.
- **Changed (privacy):** a Result names the counterparty's long-lived key.

### `deal reconcile` is now `deal audit-host` (capsule-cli#281)

- **Changed (deal):** `deal reconcile` is renamed `deal audit-host`. It lists
  host actions that have no deal record, and seals nothing.
  `skills/deal/RECONCILE.md` is now `AUDIT-HOST.md`.
- **Compatibility:** `deal reconcile` runs `deal audit-host` for one more
  release, hidden from help, with a warning on stderr; its output is
  unchanged. `book reconcile`, `type: reconcile`, `ec:deal-reconcile@1` and
  `deal terms-result` keep the word.

### The deal store is an evidencebook shelf; a new deal is a book (capsule-cli#242)

- **Changed (deal store):** the deal store is an evidencebook shelf, opened
  by one writer under the lock file a released build takes. The witness
  cadence is evidencebook's: a tick checkpoints the live logs only, and
  always holds as many leaves as logs the profile ever had, plus one, with
  fillers. A deal is live until it is finally closed and witnessed, and again
  after a later write; a sale is live until it is closed (`deal sale close`)
  and witnessed.
- **Added (deal):** a deal opened from now on is a book on the shelf. Each
  step is a record the deal profile seals, carrying `evidence_record_header`
  (`record_type` `deal_step`; `committed_at` the step's own time). Its reports
  are sealed off its log (see capsule-cli#294 above). A step's local event,
  and a received final close's amount and currency, are salted field
  commitments whose openings stay in the artifact store. A new sale and each
  new thread are books too. A deal opened before keeps its log and reads as
  before.
- **Changed:** only `capsulectl deal` writes a deal profile. Core `publish`,
  `cll append`, `cll checkpoint create|publish`, `store init` and generic
  `disclose` are refused on a deal profile, and on any store moved onto a
  shelf; reads are unchanged.
- **Changed:** every deal command refuses a deal profile without its
  checkpoint key, naming `profile update --checkpoint-signing-key-file` and
  `--checkpoint-trusted-key`. `deal tick` retries every pending witness
  delivery, whether or not a tick is due.
- **Changed (privacy):** `get`, plain and `--raw`, never prints a field
  opening (an artifact named `opening.*`), and sealing refuses a request
  artifact with that name on every profile.
- **Changed (build):** the deal plugin pins evidencebook v0.6.0.
- **Compatibility:** the first deal command that writes moves a store a
  released build wrote onto its shelf, once. Prepared steps are recovered, and
  the cadence continues from the released build's ticks and due time. A
  read-only command (`deal list`, `deal checkpoint status`) never moves a
  store, and on one not yet moved it says to run a deal command first. A
  released build can still run on a moved store, and the next command takes in
  what it wrote, but its ticks show how many book deals the store holds (see
  "Before you upgrade" above). A store whose mutation counter row is missing
  is refused, with "restore from a backup". A request with an artifact named
  `opening.*` must rename it. Verify a new deal's copies with this version or
  later (see capsule-cli#294 above).

### The core builds without evidencebook; the book and the deals are plugins (capsule-cli#236)

- **Changed (build):** the repository is two Go modules. The root module is
  the core, and no package of it imports evidencebook (`go list -deps ./...`
  names none; CI checks it). The `plugins/` module builds two plugins,
  `capsulectl-book` and `capsulectl-deal`, and pins evidencebook. Releases
  ship them as `capsulectl-<version>-plugin-book-<os>-<arch>` and
  `capsulectl-<version>-plugin-deal-<os>-<arch>`, under the same
  `SHA256SUMS`, attestation and release statement; install each as
  `capsulectl-<name>` on a plugin root (docs/PLUGINS.md).
- **Added (plugins):** `capsulectl plugin install book|deal|./FILE` and
  `capsulectl plugin remove NAME` manage `~/.local/lib/capsulectl/plugins`.
  - **By name:** fetches the first-party plugin from the core's own release.
    The asset is checked against `SHA256SUMS`, and its provenance with
    `gh attestation verify` when `gh` is installed. The output lists the
    checks that ran.
  - **Safety:** refuses to run through sudo, and refuses a plugin root on a
    path another user could change. On stock Ubuntu that means a one-time
    `chmod go-w ~/.local ~/.local/lib ~/.local/lib/capsulectl`, which it
    prints.
  - **Versions:** a first-party plugin must match the core's version unless
    `--allow-version-mismatch` is given.
  - **`make install`** installs the core and both plugins, through the core
    `go install` just wrote (never one found on PATH).
  - **Missing plugin:** the error prints the install command.
  - **Drift:** a release core warns when a first-party plugin's version
    differs from its own.
  - **`doctor --install-check`** checks the plugin the profile needs.
  - **Release workflow:** refuses a tag outside the release tag policy.
  - **Skills:** they tell the agent to show the install command to the
    user, never to run it.
  - **Docs:** docs/PLUGINS.md, "Installing plugins".
- **Changed (profiles):** a profile declares who keeps its log, `log: cll`
  or `log: book` (`profile create --log`), once. A plain profile's CLL is the
  core's, with no plugin. A book profile's log is its evidence book, and the
  core runs `publish`, `cll`, `store init` and `bundle`/`disclose`/`permalink`
  on it in `capsulectl-book`, or refuses, naming the plugin, when it is not
  installed. A jsonl profile with a log_id saved earlier reads as a book.
- **Changed (book records):** `publish` on a book profile seals the capsule
  as the record itself: it carries its record header
  (`evidence_record_header`), is signed by the profile's key, and its id is
  the log entry, of record type `own_capsule`. The artifact store holds it
  once, with its originals, which the record commits. The capsule's id
  therefore differs from the one `seal` gives the same request; that id is
  the record's subject_ref, so publishing the request again returns the same
  record, and `publish` reports it as `sealed_id` beside `capsule_id`.
  Claims cite the record's id: `result build` refuses the sealed id with
  the id to cite, `report build` credits only the record id (as the neutral
  viewer does), and `get` by the sealed id says which id to use. A bundle
  discloses an own capsule's originals as its `agent_input` and
  `agent_output` members, which a neutral verifier matches. `cll append` of a capsule sealed elsewhere wraps it in a
  `published_capsule` record that names it and commits the capsule and its
  envelope as its payloads, so a bundle carries the capsule; `book migrate`
  backfills each retired capsule the same way. On a book profile,
  `disclose` carries an own capsule's originals as its record's payloads;
  `--suppress agent_input` and `--suppress agent_output` withhold them (with
  `--payloads selected`).
- **Changed (storage):** a book profile keeps no `book/payloads/`. The first
  time its book opens, the directory's bytes move into the profile's
  artifact store, as each record's originals, and the directory is removed;
  no leaf, record or checkpoint changes.
- **Changed (replies):** a reply recorded with `request --response` is kept,
  as received, in the profile's artifact store under the record that
  observed it, on book and deal profiles. `deal compose --member` takes the
  request record id alone and reads the reply from there; `ID=FILE` remains
  for a reply recorded before.
- **Changed (commands):** `close`, `reconcile`, `request`, `respond`,
  `result`, `report`, `backfill` and `store migrate` are now `capsulectl book
  ...` (`book migrate`); reading another party's book is `capsulectl book
  remote list|get|head|request`; `compose` is `capsulectl deal compose`, and
  a deal profile's `request` and `respond` are `capsulectl deal request` and
  `respond`. The old names stay as aliases until v0.2.0 (capsule-cli#238):
  each says on stderr what replaced it and runs it with the same arguments,
  and its help is marked deprecated.
- **Fixed:** a plugin's exit code is `capsulectl`'s, as docs/PLUGINS.md
  says; it was 1. `capsulectl` adds nothing to a failed plugin's own message.
- **Added:** `--profile` given before a plugin's name also reaches the
  plugin as `CAPSULECTL_PROFILE`, which a core command the plugin runs back
  uses when it names no `--profile` of its own.
- **Fixed (capsule-cli#234):** a plugin gets `CAPSULECTL_BIN`, the path of the
  capsulectl that ran it (symlinks resolved where they can be), unless the
  caller's environment already sets it to a value. A plugin that calls the
  core back no longer fails with `capsulectl-unavailable` when capsulectl was
  run by its full path and its folder is not on PATH. docs/PLUGINS.md
  documents it.

### The deal profile checker's record types come from its schema

- **Fixed:** without the Python `jsonschema` package, `skills/deal/profile/check_profile.py`
  rejected `counterparty_profile`, `thread`, `thread_opened` and `sale_cut` records: its reduced
  check kept its own list of record types, which lacked them. It now reads the `record_type` enum
  from the schema it ships with. Its fixture run names the four types the positive fixtures do not
  carry yet, where it counted against the old list.

### Settlement fee findings from agent-action-capsule

- **Changed (build):** agent-action-capsule's Go module moves to v0.10.0. Its settlement verifier
  now names the leg whose fee is in another asset in `fee_asset_differs`
  (action-state-group/agent-action-capsule#303), so capsulectl's own mapping of that finding to a
  leg is removed. The vendored settlement vectors come from agent-action-capsule 626074f.
- **Changed (output):** `settlement status` and `deal settlement` name the legs of a
  `fee_asset_differs` finding as before, except in two cases. When both fees differ, the finding
  names the payer leg and then the payee leg, where it named only the payee leg. When only the
  routing fee differs and the payer leg was passed more than once, every live copy of it is named,
  as for the payee leg and every other finding, where only the compared copy was.

### `x-` members in an Evidence Result

- **Changed (validation):** `result build` validates against Evidence Result v0 as of
  agent-action-capsule 627dd8c. It admits members named `x-...` on the document and on a claim
  (action-state-group/agent-action-capsule#297, spec section 8a). Every other member of every
  level stays closed, including a capitalised `X-...`. It also admits an optional document member,
  `coverage_report` (action-state-group/agent-action-capsule#157, PROPOSED), and a Result without
  one validates as before. The vendored schema and its test vectors come from that commit. No
  output of this CLI changes.

### Verification on agent-action-capsule's Go APIs

- **Changed (build):** agent-action-capsule's Go module moves to v0.9.1, and capsulectl's own
  copies of its verifiers are removed (action-state-group/agent-action-capsule#280): witness
  receipt checks and witness directories (`verify --bundle --witness-directory`, deal copies,
  cadence receipts) use `go/witness`; countersign receipts and SCITT statement receipts use
  `witness.VerifyCOSEReceipt`; `settlement status` and `deal settlement` derive states with
  `go/settlement`. Output shapes, labels and exit codes are unchanged.
- **Changed (validation):** witness receipts with a `rekor` or `scrapi` binding are checked, and
  can pass or fail, where they were withheld. A directory row with a key id that is not 64
  lowercase hex characters is unusable as a whole (its receipts are withheld) where the bad id was
  skipped. A stub receipt (`is_stub`) is withheld. `leaf_index` and `tree_size` must be present but
  are no longer read. COSE receipts must be strictly encoded, must carry their payload detached (a
  receipt with its payload attached is refused, since its signature does not cover the rebuilt
  root), and may not mark an unknown header critical; a countersign or statement receipt may carry
  more than one inclusion proof.
- **Changed (witness directory and receipts):** agent-action-capsule reads a `witnesses.json` and
  a `receipt_b64` strictly, and capsule-cli no longer softens them. Two kinds of change:
  - Withheld where they used to be checked: a row whose `binding` is empty or null, whose
    `public_keys` is null, or whose field names are cased otherwise (`Endpoint`) is no row AAC
    uses, so its receipts are withheld.
  - Fail where they used to pass: a `public_keys` entry wrapped across lines is skipped, so the
    row's key id is read as a raw Ed25519 key and the receipt fails. A `receipt_b64` wrapped
    across lines also fails.

  capsule-cli writes no directories and writes its receipts unwrapped.
- **Changed (settlement):** a side with more than one observed leg that no supersedes chain
  orders is compared leg by leg (mismatch or agreed) instead of `unjoined` with
  `observed_leg_not_unique`; a supersedes link counts only under the superseded leg's own key; the
  `in_progress` finding is no longer reported. A conflated pair that otherwise matches is still
  reported `unjoined`. A delivered leg with an empty `delivery.carrier` is accepted, and a
  wrapped entry of an unregistered type is `settlement_malformed`.
  A wrapped SD-JWT re-signed with the leg's own key is still `wrapped_resigned` with or without
  disclosures: v0.9.1 reads the issuer JWS before the first `~`, as capsule-cli did (v0.9.0 missed
  `JWT~disclosure~`).

### The page emitter from capsule-viewer

- **Changed (build):** the offline pages capsulectl writes (`report build`, `deal report --html`,
  `presentation build` and the bundle commands' HTML output) take the page emitter from `github.com/action-state-group/capsule-viewer/go/emitter` v0.1.0
  instead of `github.com/action-state-group/agent-action-capsule/go/emitter`
  (action-state-group/agent-action-capsule#281). The package is the same code with the same API,
  held to the same golden pages, so pages are byte-identical. `go/presentation` and the rest of
  agent-action-capsule's Go module stay at v0.8.0. The vendored browser IIFE is unchanged.

### evidencebook on agent-action-capsule's Evidence Request package

- **Changed (build):** the evidencebook pin moves from `d3ae0fe` to `717f21e`
  (action-state-group/evidencebook#20), which takes the Evidence Request protocol from
  agent-action-capsule's `go/evidencerequest` instead of its own copy, and agent-action-capsule's Go
  module moves to `v0.8.0` (from the `a726b9f` pseudo-version). No CLI code changes are needed:
  evidencebook's request and reply types keep their fields, and its `ErrInvalid` (now the
  package's sentinel, message `evidencerequest: invalid`) still matches every protocol error.
- **Changed (validation):** v0.8.0 carries agent-action-capsule's stricter verifier checks, so
  verification now rejects some inputs it accepted before: non-strict base64url or invalid UTF-8
  in bundle fragments, non-string `domain`/`provenance`, and Producer Envelopes whose CBOR is not
  strictly encoded.

### capsule-emit from its released module path

- **Changed (build):** capsulectl imports `github.com/action-state-group/capsule-emit/go`
  v0.3.0, the first release of the module that replaces the archived
  `github.com/action-state-group/capsule-emit-go`. `go.mod` no longer replaces the old path with
  an unreleased pseudo-version, and no longer requires the old path at all: evidencebook is pinned
  at `d3ae0fe`, which imports the new one (action-state-group/evidencebook#19). New capsules are
  still sealed `-05`, as before.

### Book evidence requests follow evidence-interop/0

The evidencebook pin moves from `b970240` to `d3ae0fe`, whose `request` and `respond` implement
the evidence interoperability profile `evidence-interop/0` over the request map of
`draft-mih-agent-evidence-request-00`. On a book (jsonl) profile:

- **Breaking (request input):** `request --request FILE` takes the draft's request map. The
  subject is a map with one member naming its form (`{"full_history":null}`,
  `{"record":"<digest>"}`, `{"range":[a,b]}` with zero-based log positions,
  `{"correlation":"<id>"}`); `expected_pin` is the digest of an anchor the requester holds;
  `min_freshness` is a log size or an RFC 3339 UTC time. The old shape (`subject.kind`,
  `expected_pin` as `{root, mmr_size}`, `min_freshness` as `{size, time}`) is refused. Book
  requests are still sent as JCS JSON.
- **Breaking (reply):** `respond --output` writes the profile's signed reply, an envelope or a
  refusal, not `{"artifact": ...}` or `{"refusal": ...}`. An answer is the canonical Artifact the
  profile defines (a record's own octets with its CLL proofs, or a derivation), never an Evidence
  Bundle. `request --response` reads that reply.
- **Breaking (authorization):** `respond --requester ID` is the identity you authenticated the
  request's sender as, and it is the only input to the book's relationship gate. Without it,
  only public subjects (`checkpoints`, `history_card/1`) are answered; every other request gets a
  signed `not_authorized` refusal. A malformed or unauthenticated request is answered without a
  book record and never makes the book checkpoint, so `respond` may print no `record_id` or
  `relationship`.
- **Changed (records):** a recorded response's statement may carry `not_checked`, and an answer
  that fails its checks is recorded as `artifact_verification_failed`; a signed refusal with an
  unregistered reason as `refusal_nonconformant`. A recorded absence's statement is the profile's
  absence record (`outcome`, `request_digest`, `route`, `window`, `recorded_at`).
- **Changed (sealed bytes):** a new book record's links are no longer sealed as `references[]`
  entries carrying `citation_purpose` (the link type). Every record with links (a close,
  acknowledgement, rebuttal, disclosure, `evidence_result`, recorded response or absence) gets a
  capsule and record id that differ from what the previous build sealed for the same entry. Records already
  in a book keep verifying and keep their ids; the header still names every link's type.
- **Breaking (pending requests):** a request a book recorded before this upgrade holds the old
  request shape, which evidencebook no longer reads back: `request --response` and
  `request --absent-until` for it fail with a corrupt-record error, and an upgraded responder
  refuses its bytes as `request_malformed`. Record the outcome of every pending request before
  upgrading, or ask again after it.
- **Changed:** `request --witness-directory` applies to a deal copy only; on a book profile it is
  refused, since the profile reply carries no witness receipts.

Deal (sqlite) profiles are unchanged: a deal copy request, its answer, the records a deal keeps
and a composed/v1 member's `response` keep their bytes. evidencebook no longer writes that
pre-profile wire, so capsulectl holds its types itself (`internal/cli/deal_wire.go`).

## v0.1.0-rc16

### The presentation pass, a CI job of its own

- **Added (tests, CI):** `presentation-pass` drives the presentation goldens' bundles through
  `presentation list` and `presentation build` at 390px, 1280px and in print, with semantic
  snapshots, in the pinned Chrome and fonts. See `internal/cli/testdata/presentation/README.md`
  for what it holds, and for the two gaps it pins: build does not carry the deal view, and a
  page's own gate does not check producer signatures.

### One file for the deals you pick, each checked on its own

- **Added:** `deal history --deal ID [--deal ID ...] --out FILE --to WHO` writes a History: one
  file holding the deals you pick from `deal list`. It is an Evidence Bundle of a log of its own,
  `deal-history/<nonce>`, where the nonce is the selection record's. Each deal has its own log, so
  one bundle over several deals could not prove its records against one checkpoint. Instead,
  `deal history` seals one selection record as that log's only entry, signed by your key, naming
  who it is for (`recipient`, from `--to`), the picked deals and each deal copy's digest. The
  bundle's `x-history-selection/v0` extension carries each deal's own bundle.
- One log per History: a History carries no other History's selection, not even withheld, so its
  recipient learns nothing about earlier ones (how many, when, under which key).
- A History may be sent to anyone, so each deal is carried as its **history** copy by default
  (`--audience history`). Every record's payload is withheld, and each record shows only what its
  capsule states: its time and, for an act, its effect's status. A deal's outcome and its acts'
  action classes are in payloads next to the item, the amounts and the other party's details. A
  payload is disclosed whole or not at all, so a History does not prove them: verify says
  `"not_proven": ["outcome", "action_class"]`, and the History is still VALID.
  - The copies `deal report --share` writes for someone in the deal (`counterparty`,
    `adjudicator`) are refused for a History.
  - `--audience keep` gives your own copy, with every private value in it, and is never one to
    send. It takes no `--to`; every other History needs it.
- `verify --bundle FILE` checks that the selection record is disclosed, matches its seal and is
  signed, and that the selection the file carries is the sealed one. For each picked deal it then
  checks that the deal is carried, is the bundle its digest names, is of that deal's own log, is
  the copy for the sealed audience, and verifies on its own. Nothing unpicked may be carried. A
  History sealed for any audience but history or keep fails, and so does one carrying a history
  copy with any payload disclosed, one sealed for history with no `recipient`
  (`selection_recipient_missing`), and one whose log is not `deal-history/` + its selection's nonce
  (`not_a_history_log`).
- Verify reports the History's `holder_key` and `signer_keys` (each key with the deals it signed).
  It fails a History whose deals are signed by more than one key, or by a key other than the one
  that signed the selection. A finding names its deal.
- `deal history` writes no deal's log. It seals no report and cuts no checkpoint on any picked
  deal, and it does not put the share on a deal's disclosures log: the selection record on the
  History's own log is your record of what was sent and to whom.
  - Each deal is copied through its latest checkpoint as it stands. Steps after that checkpoint
    (a refund, a dispute) are not in the copy. The output counts them, and so does the deal's
    entry in the sealed selection (`{deal_id, bundle_digest, steps_after_checkpoint}`); verify
    reports each deal's count.
  - `recipient` and `steps_after_checkpoint` are your own statements, signed with the selection
    and checked against no deal: verify lists them as `"holder_stated": ["recipient",
    "steps_after_checkpoint"]`.
  - For keep, a deal with sealed reports is refused unless its newest report of your own covers
    every step shown (seal one with `deal report --deal ID --bundle FILE`). A history copy carries
    no report.
  - A step not yet on the deal's log is refused too, not recovered.
- **Known limit:** a History shows that the holder of one key K picked these deal copies and
  signed every record in them. It does not prove K's holder made the deals. A record's signature
  is outside its Capsule ID, so whoever holds copies can sign them again under their own key.
- **Known limit:** your deals from before a signing-key change carry the old key, so a History
  mixing deals from before and after the change fails (`deal_signers_differ`).
- **Known limit:** a nested deal's `x-deal-v0` section (a history copy's, or a keep copy's inline
  report with no `sealed_report`) is not sealed on its own. Editing it leaves that nested deal
  VALID; only the History's `bundle_digest` catches the edit. A nested deal taken out of a History
  is no stronger than an unsealed deal report.
- The deal view's manifest forbids `x-history-selection/v0`, so it never presents a History.
- `presentation list` says a History is verified only when `verify --bundle` calls it VALID. The
  bundle verifier it runs does not check the nested deals. `presentation build` writes a History's
  page under the same rule.

### A rules checker may be pinned to a set of rulesets

- **Added:** `profile update --rules-checker FILE` accepts `"definition_digests": ["<64 hex>", …]`
  in `FILE`: a set of rulesets, any of which the checker may report, for a checker whose ruleset
  depends on the deal. A check compares the digest the checker reports against the set as it
  compared it against the single digest; one outside the set is still not evaluated, cause
  `ruleset_changed`. `profile show` (`RulesChecker.DefinitionDigests`) and `doctor`
  (`profile.rules_checker.definition_digests`) print every pinned digest.
- **Unchanged:** one digest, given as `definition_digest` or as a set of one, is stored as
  `definition_digest` exactly as before, so a profile pinned to one ruleset reads, saves and shows
  byte for byte as it did. An earlier capsulectl refuses to load a profile pinned to two or more
  (it does not know `definition_digests`), rather than checking against only one of them.

### A deal's copy over evidence requests, and `compose` from what was recorded

- **Added:** on a deal profile, `request`, `respond` and `request --response` carry a deal's
  shared copy. The subject is `{"kind":"correlation","value":"<deal id>"}`: the deal id both
  parties hold (`deal open --deal-id`).
  - `request --request` records the ask on the deal's requests log (`deal/<id>/requests`) and
    writes the canonical request bytes. Their digest is the one evidencebook gives the same
    request.
  - `respond --requester WHO` answers with the copy `disclose --deal --share counterparty` makes,
    in an evidencebook signed artifact response. The share is sealed on the disclosure log as
    before, and its record names the request's digest. The artifact is the copy's JCS bytes, so
    the signed `artifact_digest` is its bundle digest.
  - Under `coverage.min_freshness` the copy is fresh: a report is sealed and a checkpoint cut.
    Under `coverage.expected_pin` (`{root, mmr_size}`) the copy is re-served at exactly that
    checkpoint, with nothing new sealed. That works only while the deal's log ends at the pin
    with a copy's report as its newest record. Otherwise, for example once the deal has moved on,
    the answer is a signed `coverage_unsatisfiable` refusal.
  - `request --response` records the answer under the pinned responder keys. A signed copy that
    does not verify VALID is recorded `artifact_failed_verification`, never as a grant; so is one
    that is not anchored under the responder's checkpoint key, not on the requested deal's log,
    or not at the pinned or fresh enough checkpoint. `--absent-until` records an absence.
- **Added:** `capsulectl compose --deal ID --member REQUEST_RECORD_ID=RESPONSE_FILE ...` (two or
  more members; `REQUEST_RECORD_ID` alone for a recorded absence) writes a composed/v1 bundle from
  recorded answers only. It refuses:
  - a member whose request this profile never recorded;
  - a request still pending;
  - response bytes other than the ones recorded.

  Each member's `request_digest` is the recorded request's digest, and its observer is the
  responder key the answer verified under. A copy's member carries `response`: the responder's
  signed response without its artifact bytes, which binds the copy to the request it answered.
  Each pair of members is joined on the deal id (`pre_agreed_identifier`, `pointer: "/deal_id"`),
  declared in the state the verifier derives. The containing bundle's root is a composition
  record sealed on `deal/<id>/compositions`.
- **Added (wire):** every deal step's Capsule carries the deal id as its top-level `deal_id`
  member (x-deal-v0 section 1), so two parties' copies join on it. A step an earlier release
  prepared, without it, is still recovered as that release prepared it.
- **Changed:** `verify --bundle` reads a composed/v1 block holding a refusal member as
  INCOMPLETE (the member reports `reads_as: "INCOMPLETE"`): what was asked for was not served.
  A refusal is still no failed claim, so it never makes the verdict INVALID. A copy's carried
  `response` is checked under profile `evidencebook-artifact-response`: an invalid signature, or
  one under a key other than the member's observer, fails.
- **Fixed:** `verify --bundle` checks a carried refusal in evidencebook's own form (`key_id`, and
  `sig` over `{request_digest, reason, issued_at}`), the form `respond` sends, and reports
  profile `evidencebook-refusal`. Before this fix, such a refusal read `invalid`.

### A statement that a sealed merchant email's signature checked out, without the email

- **Added:** `deal verifier-statement --deal ID [--step N] --out FILE` re-checks a sealed merchant
  email's DKIM signature over the full sealed message, against the key records sealed with it,
  and only when a signature passes writes a signed statement: `dkim_domain`, `message_date`
  (only when the passing signature covers the Date header; otherwise left out and shown as "date
  not covered by the signature". A covered Date that is repeated, where DKIM signs the bottom-most
  and a reader takes the first, or unreadable gets no statement), `email_digest` (the `message_digest` the deal record already
  seals, with its algorithm named), `key_records_digest`, `key_source` (`dns`), `checked_at`,
  `verifier` and an Ed25519 `signature` over UTF8(`x-deal-v0/verifier-statement/v1`) || 0x00 ||
  JCS(the rest). Those signed bytes begin with the tag, which a capsule's or a countersignature's
  signed bytes never do, so neither verifies as the other under the same key. A test pins one
  signing input and signature, cross-checked with a second Ed25519 implementation. No
  email, header value, selector or signed-header list is in it. There is no statement when the
  signature fails or is absent, and none over a key record supplied with `--key-record`.
- **Added:** `deal verify-statement FILE [--key HEX]` checks the statement offline and prints "V's statement:
  DKIM signature from D verified over a message dated T, checked by the holder's own tool (key
  K) at T2". It does not re-check DKIM (there is no email) and says so. It exits 1 when the
  statement does not verify. The statement names its own key: `--key` (the holder's public key)
  checks that it is the holder's; without it the result says the key is not matched to any holder.
- The verifier is the profile's own signing key. A statement is therefore no more independent
  than the holder's own seal: it discloses less. It is never worded as independent, third-party
  or a verified receipt.
- **Schema:** `x-deal-v0.schema.json` gains `$defs/verifier_statement` (not a record type; no
  record shape changes). A copy of the old schema does not know the definition, so the schema
  ships with this release.
- The deal SKILL.md is unchanged (digest still `643ac179237d9a5f30f9bbd373b93db84f4cff0428fa0edcfa66461bf042d29a`): the agent is not offered the
  statement.

### Offer, commit, sign and cancel acts seal their disposition and counterparty

- **Changed:** an approved `offer`, `commit`, `sign` or `cancel` act is sealed as a `decide`
  Capsule, as a `pay` is, with the same disposition rule (the typed record's own
  `authority_basis`; the approval in `x-deal-v0`) and its effect type: `make_offer`
  (`two_way`), `accept_agreement`, `sign_document`, `cancel_commitment`. These four effect
  types are **provisional**: proposed for agent-action-capsule's registry, held for
  ratification and not yet registered, and provisional here until they are registered (the deal
  profile's PROFILE.md marks each one). Before, only a `pay` had a disposition, so a reader could not tell an accepted
  commit from a repeat, and after a sale's first commit every later commit check in it was
  refused.
- **Changed:** the same acts seal the counterparty of the check they rest on (an act names no
  payee), exactly as that check sealed it, so a rule keying earlier acts on who they were with
  (a repeated booking in one deal) finds it in the sealed record, not only beside it in the
  checker's history.
- Steps sealed before keep their bytes: each change is marked on the step (`act_effects`,
  `act_counterparty`), and a step without the mark re-derives as it was sealed. An unchecked act
  stays `fyi`. The deal skill's SKILL.md is unchanged, so its digest is too.

### The witness's grant text states, it does not instruct

- **Changed:** the witness's network-grant text, in `doctor --check-witness`'s `witness.consent` and
  in a tick's `network_consent_needed` pending text, now states what the grant is and why ("Ticks
  run on a schedule, with nobody present to answer a network prompt, so they need a standing grant
  ("Always allow this site") for agentactioncapsule.org, not a one-time one ("allow once")…")
  instead of telling the reader what to choose. An agent reads this output, and an agent host may
  treat instructions in tool output as prompt injection: Muse flagged an install whose output
  carried them. The facts are unchanged; the agent's skill still tells the person what to choose.

### A merchant email whose key cannot be read now is sealed at the next tick

- **Changed:** when `deal note --kind evidence --email` cannot reach DNS for the merchant's DKIM
  key or DMARC records (a timeout or unreachable resolver; an absent record is still sealed as
  absent), it no longer refuses with a way on that asked for the record to be looked up some
  other way. It answers `"state": "deferred"` with `"say": "the merchant's signature will be
  checked later"`, seals nothing, and keeps the email and its note on this device
  (`deal_deferred_emails`, local only). Each `deal tick` first reads the records again with the
  store unlocked and, when it can, seals the email on its deal as the deal stands then, with
  `key_source: dns` and a checkpoint at once. The step's time is when the key was read; after a
  close, the email is later evidence linked to the close. The tick's result lists the queue under
  `merchant_emails`: `sealed` (with the same fields `deal note` reports), `waiting` (each email's
  deal and when it was queued; there is no retry limit) and `refused` (an email its deal no longer
  takes, such as a cancel-by date on a deal closed meanwhile: kept on the device, never retried,
  listed at every tick). A failure on one email is listed under `failed` and never stops the
  tick. An email whose exact bytes its deal already holds is not sealed twice. No record shape
  changes.
- A capture that fails for any other reason (the email is not raw RFC 822 with its headers
  intact, or its DKIM-Signature headers cannot be read at all) is still refused at once. A
  signature that is merely malformed fails and is sealed as failing, as before. `--key-record` / `--dmarc-record` are unchanged.
- The deal skill no longer tells the agent to have the user look up a key record: it says only
  that the merchant's signature will be checked later. This changes the deal SKILL.md digest
  (sha256 `b5d4d9aec20328a746684283ba3ec7a0237b7fd6a11b5ecef348c39ae8722fa3` to
  `643ac179237d9a5f30f9bbd373b93db84f4cff0428fa0edcfa66461bf042d29a`).

### `deal list --json`: a plain summary per deal

- **Added:** each row of `deal list --json` carries `summary`: `what` (the user's own asked
  words, as `deal report` gives them), `counterparty` (the name the opening step sealed as
  `who.name`, or `"not recorded"`: never read out of a message or other free text), `amount`
  (`amount_minor` and `currency` as recorded, with `basis` `"paid"` for the net of the sealed acts
  in the first act's currency, `deal report`'s `net_minor`, acts in another currency counted in
  `other_currency_acts` and never summed; or `"opening_terms"` for the opening step's price when
  no act moved money, a later change of terms not read; `null` when neither is recorded),
  `outcome` (the close step's own outcome word, or `"open"`) and `text`, the four as one line.
  It is read from the sealed steps the list already reads: the list still seals nothing and
  cuts no checkpoint, which `deal report` does even without a file flag. The summary is the
  user's own view and private; no shared copy (`deal report --share`) changes. The tab-separated
  output is unchanged.

### A presentation page carries only the modules for its audience

- **Fixed (privacy):** `presentation build` put every installed plugin's presentation module in
  the page, whatever `--audience` said, so a page built for the other party carried the script of
  a module meant only for the user's own copy. That module could never render there, but its code
  travelled with the page. The page now carries only the modules whose manifest audiences include
  `--audience` (or `"*"`), the same rule the page's resolver applies. Each module left out is
  listed under `not_included`, its reason naming the module's audiences, and `--module` naming one
  is refused, with no page written. A module for `"*"` is carried as before.
- The builder's ambiguity and duplicate-id checks now look only at the modules the page carries,
  so two modules that could match one bundle are refused only on a page that would carry both.
  When one module is left, its wording pack is the page's, as for any one-module page.

### A built-in materiality predicate, pinned by name

- **Added:** `deal init --materiality builtin:neutral` and `profile update --materiality builtin:neutral`
  pin the materiality predicate built into capsulectl ("quantity, size, variant and delivery picks",
  version 1.0.0, digest `05ec66b4a21025a66288cafc74e7fa1c540cc62695c5f536df84f4a7d7f66d4a`). With
  it, a quantity of 1 the agent filled in does not pause a check; another quantity, or a size,
  variant or delivery the agent picked, still does. The profile keeps the name `builtin:neutral`
  and the digest, not a path, so the pin survives an upgrade or a move. A check still refuses when
  the pinned digest is not the one this capsulectl ships. A predicate given as a FILE is pinned by
  its absolute path, as before; with none, every pick still pauses. The example
  `skills/deal/profile/materiality-predicate/neutral.json` is unchanged. The deal skill's SKILL.md
  names the built-in, which changes its digest.

### The approval prompt is one block, each fact once

- **Changed:** `deal check` returns one block to show the user, built from the check's own
  fields: the headline (what, how much, to whom, by what rail, refundable or not), the rules line
  once, then one line per distinct finding. A first dealing, the profile being new and every
  detail told for the first time are one line. A pick the check paused on is its own line,
  beside what the user asked for and the agent's other picks. Unverified claims are one line.
  The block lists no options (`options` stay the host's to render), and the rules line names no
  CLI syntax. Before, the agent was told to show both `approval_text` and `card`, so the rules
  line printed three times, every finding twice, and the buttons appeared again as text.
- On a pause, `card` (the text sealed as `shown_card`) is that same block, so `--shown-card`
  takes the `approval_text` shown. A pass still has no card. Records sealed before keep their
  card as it was. The deal skill now has one show-it-verbatim rule naming `approval_text`, which
  changes its SKILL.md digest.
- **Changed:** the proceed option is an override ("Pay anyway", "Share anyway") only when something
  the user set was broken: a rule of theirs that failed and asks for approval, or a limit they gave.
  Rules not checked or not fully checked, and findings such as a changed payee or a first telling,
  now offer "Pay" / "Share". A deny still offers only "Hold".
- **Changed:** in the block, the rules line is a bullet like the findings, and what the user asked
  for and what the agent picked are separate bullets. A paused share in a deal that may still pay,
  with no pay sealed yet, says `Step 1 of 2: I will ask again before paying $X.` after the headline,
  with the deal's total.
- **Changed (deal skill):** the agent may put one sentence of its own before the block, and nothing
  else but one plain question when the host has no buttons. It never prints an option label as
  text, and offers `options` as buttons where the host supports them. After a share's "Step 1 of
  2", it asks the user before paying, even when the pay check passes.

### `presentation list`: which presentation module a bundle resolves to

- **Added:** `capsulectl presentation list --bundle <file> [--audience <token>] [--format html|fragment|embedded] [--text]`
  says which presentation module the bundle resolves to among every module this tool can put in a
  page: the page's built-ins, its deal view (`capsulectl.deal-view/v0`) and the modules the
  installed plugins offer through the `presentations[]` handshake, each listed with its source,
  trust class, tier and whether it matches. agent-action-capsule's Go resolver (`go/presentation`)
  makes every decision: the bundle's descriptor, the runtime's refusals (each refused module is
  shown with its reason; one that matched, with the resolver's refusal line), the ambiguity test at registration and the
  resolution. Exit 0 for a module or for no presentation; 1 when the bundle does not pass the
  viewer's verification gate (nothing is matched); 2 when two modules of one tier can match one
  bundle (the resolver's named ambiguity, never first-wins) or the registry refuses a manifest.
  The JSON report is the default; `--text` prints a short summary.

## v0.1.0-rc15

### Release tags are signed from v0.1.0-rc15

- **Changed (release procedure).** From `v0.1.0-rc15` on, every release tag is SSH-signed by the
  maintainer's key, whose public key is in the release monitor's allowed-signers file. The tag
  step in `docs/RELEASING.md` is now two parts: prepare (record the merge commit) and one line
  for the maintainer that signs and pushes the tag, with SSH pinned for the signature.
- **Added: `release/known-unsigned.txt`**, a reference copy of the tags made before signing
  began, exactly `v0.1.0-rc1` to `v0.1.0-rc14`, frozen. The monitor's operator copies it once
  from the signed `v0.1.0-rc15` tag and checks its SHA-256 against the value in
  `docs/RELEASE-TRANSPARENCY.md`, section 5. The monitor never reads the repository's file, and
  a later edit of it is itself a red flag. An unsigned tag made after signing began is an
  alarm, whatever its name.

### A report opened outside a secure context verifies

- **Fixed:** a valid deal report opened where the browser gives no WebCrypto (a non-secure or
  opaque origin, such as an app's attachment viewer on iPhone) said it did not verify: the page hashed only
  through `crypto.subtle`, which exists only in a secure context, so every digest, membership
  and range check failed. The vendored runtime is rebuilt from agent-action-capsule with a plain
  SHA-256 fallback (and `@action-state-group/cll` 0.2.1 for the MMR proofs); the ten checks
  now pass with no `crypto` at all. `scripts/check-viewer-without-webcrypto.mjs PAGE.html` runs a
  page's own verifier that way and prints its checks.
- **Changed:** without WebCrypto the deal page lists the countersignatures and the cited signers
  under "Checked only by capsulectl verify": it cannot check an Ed25519 signature there.
- **Added:** a viewer with no SHA-256 at all shows "Unable to verify in this viewer" and the
  `capsulectl verify` command for the file, never the failure banner.

### A sale registers its threads, and the user's own sale bundle proves the list is whole

- **Added (wire, additive; records sealed before re-derive unchanged).** `deal open --sale` seals a
  `thread` record on the sale's own log before the thread opens. It holds the thread's deal id as
  a commitment only. The thread's `task-authority/v0` names that record with a `registration`
  ref. A thread's task authority that should name a registration and does not is refused, never
  sealed. An open that failed after registering reuses its registration on the next try.
- **Added: `deal sale bundle --sale ID --out FILE`,** the user's own copy of a sale:
  - the sale log's Evidence Bundle, whose completeness certificate covers every registration;
  - each thread's own copy, in the `x-deal-sale/v0` extension;
  - `sale_threads`: for each registration, its opening and whether its thread is `present`,
    `missing` or `never_opened`.
- **Added (verify).** `verify --bundle` checks a sale bundle's binding. A missing thread, or a sale
  with threads opened before registration, is INCOMPLETE. Any mismatch, or a carried thread that
  fails, is INVALID. A bundle that is not a sale's is `not_a_sale_bundle`. No buyer's copy carries
  any of it.

### A sale bundle is cut fresh, and a sale's log says which threads opened

- **Added (wire, additive; records sealed before re-derive unchanged).** When a sale's thread
  opens, the sale's log seals a `thread_opened` record: its registration, and its task authority
  by commitment. `deal sale bundle` seals each thread's report, then a `sale_cut` naming each
  opened thread's last record by commitment, then the sale log's checkpoint. The bundle's
  certified interval then covers every thread's acts up to the bundle. Present `sale_threads`
  entries carry the openings (`opened`, `head`).
- **Added (verify).** A present thread must open to its `thread_opened`, and its last record must
  be its head in the latest cut, so a copy cut short is INVALID. A `never_opened` registration has
  neither; when a withheld record could be its opening, it is INCOMPLETE. A sale copy without
  these records reads INCOMPLETE; any opening or head that does not match is INVALID.

### `verify --bundle`: one name per sale-cut finding, against the latest cut only

- **Fixed.** In a sale bundle, `no_sale_cut` (no readable cut) is said once, not once per thread,
  and a readable cut without a present thread's head says `thread_not_in_the_cut` for that thread.
  Only the latest cut counts: a cut is read only when its disclosure matches what its record
  sealed and no record this copy cannot read follows it on the sale's log. A withheld or edited
  latest cut is `no_sale_cut`, never a silent fall-back to an older one.

### A rules checker sees who an earlier act was with

- **Added (checker input only; nothing sealed changes).** Each act in a check's `history` now
  carries `counterparty`: the counterparty the act's check sealed, exactly as sealed and keyed per
  deal, beside the act record (which names none). A rule keyed on the counterparty sees a repeated
  booking in one deal as a repeat on acts, as on checks; `counterparty_profile` remains the key
  across deals. Absent for an act with no check behind it, or a check that names no counterparty.

### A seller's check tells the rules checker what the agent stated earlier

- **Added (checker input only; nothing sealed changes).** Where the user sells, the checked record
  given to the rules checker carries `deal_claims`: the deal's agent claims that state a class,
  sealed before the checked step, in seal order. Each is its sealed claim record's own values, with
  the words as a commitment only. A rule on a required disclosure can then find a statement the
  agent made before the offer. Absent when there is none; a buyer's checker input is unchanged.

### The deal page says which checks ran on it and which only `capsulectl verify` runs

- **Changed:** the deal page lists, in plain words, what the page itself checked and what only
  `capsulectl verify --bundle` checks, with the command to run.
  - **On the page:** each record's digest, its place in the log, the range proof, opened words, a
    countersignature's signature, and the agent's signature only on the records the page cites as
    signers.
  - **Only by `capsulectl verify`:** the checkpoint's signature when the page could not check it
    (the page says it shows the bundle as passing without it), the agent's signature on every
    record, the witness receipt, and that the log only grew since its earlier checkpoint.

  The lists are read from what the page's verification recorded and what the file carries.
- **Changed:** the command includes `--witness-directory` when the file carries a witness receipt.
- **Changed:** the page says none of these checks says who the agent or its user is, or that the
  file is the latest copy. "This page checked itself" is gone.

### A deal page's title says what that copy shows, with or without JavaScript

- **Changed:** a deal page's `<title>`, which a tab or a host's preview shows even with JavaScript
  off, now says what that copy shows in the clear:
  - on the user's own copy, the words they asked, only when checked against their sealed
    commitment when the page was built (whitespace folded, at most 120 characters);
  - on a shared copy, only whom it is for.

  It never carries a structured field such as a floor or a limit. The share gate scans it with the
  rest of a shared page.
- **Not in this change:** a `<noscript>` line in the page body, which waits on a slot in
  agent-action-capsule's page shell.

### `deal list`: every deal in the profile's store, read-only

- **Added:** `capsulectl deal list [--json]` prints one line per deal in the profile's store, in
  deal id order, tab-separated:
  - `deal_id`;
  - `kind`: `deal`, or `sale` for a `deal sale new` log. A sale's buyer threads are deals and are
    listed as `deal`. A sale never closes, so its `state` is always `open`;
  - `state`: `open`, or `closed` once a final close is in the index (as `deal deadlines` decides);
  - `opened_at` and `last_record_at`: the `at` of the deal's first and last step;
  - `counterparty_fingerprint`: the payee fingerprint of the latest `counterparty_profile` step,
    or `-` when there is none;
  - `log_id`.

  `--json` prints the same fields as one JSON array, in that key order, with `null` for no
  fingerprint. A store with no deal yet prints `[]` with `--json` and nothing without, and exits 0.
- **Reads the store's index, not the deal logs:**
  - Every query is a SELECT, under the store's shared lock when its lock file exists, like
    `deal checkpoint status`. It works on a `read_only` profile.
  - Each row gets the index checks a deal command makes before it reads a deal: one strict record
    whose deal, step number and kind are the row's, chained to the step before it by digest. A row
    that fails them is refused as a conflict.
  - The steps are not re-verified against the deal's log; `deal export` and `deal report` do that.
    A last step that was indexed but never reached the log (a crash between the two writes) is
    listed, and the deal's next write command recovers it.
  - On a store whose last writer closed cleanly, every file under the store is byte-for-byte the
    same after the list.

### The deal page is built through the emitter's slots, under a per-page CSP

- **Changed:** `deal report --html` builds its page through agent-action-capsule's emitter
  options (agent-action-capsule `fa49b4d`, with the browser runtime rebuilt from it):
  - the title;
  - the deal view's stylesheet (in the theme slot, until module stylesheets can be pinned);
  - the deal view as a digest-pinned module;
  - a bootstrap that builds the verified context once.

  The page's Content-Security-Policy lists the digest of every inline script and style and allows
  no network. Nothing is spliced into the page any more.
- **Changed: the deal view reads only the verified context.** It no longer verifies the bundle,
  reads `window.__BUNDLE__` or the bundle's disclosures or records, or recomputes a commitment.
- **The words a page shows are checked when it is built.** These are the user's words and each
  statement the agent made. capsulectl recomputes each against its sealed commitment and writes no
  page whose words do not match. The page now says they were "checked when this page was built".
- **The sealed-report refusal** stays in the build-time page gate.
- **Interim:** the deal view's stylesheet rides in the theme slot until the emitter can pin a
  module's own stylesheet (agent-action-capsule #214).
- **Not in this change:**
  - the static, no-script page: capsulectl does not produce it, and will report it as unavailable
    (`no-document`) for now;
  - the share gate's removal of the two vendored scripts before it scans a shared page is retired in
    a later change.

### A hex id or digest is never read as money

- **Fixed.** The share gate no longer reads a currency code inside a run of hex as money ("…3cad8…"
  as 8 Canadian dollars, a digest's "8aed…" as 8 dirham), and the spending-limit and floor check
  takes out the deal's own ids and every digest, key and signature before it reads a copy. A deal's
  share could be refused at random when such a run matched the user's limit; a code written against
  an amount as a word of its own ("1700usd", "USD1,700") still counts.

### The deal page's view is a registered presentation module

- **Changed:** `deal report --html` puts the deal view in the page as the presentation module `capsulectl.deal-view/v0`, with its own manifest. The page's runtime selects it for a verified deal bundle. It reads only the verified context and what capsulectl checked when it built the page.
- **Changed:** the deal view's stylesheet is its own, pinned in the page's Content-Security-Policy by its manifest. The theme slot holds theme tokens only.
- **Checked when the page is built:** capsulectl writes no deal page whose view the page's runtime would refuse, or that could be ambiguous with a built-in view.
- **Changed:** a deal page whose bundle does not verify shows the runtime's refusal and no deal section.
- **Not in this change:** the deal view's words in a wording pack; the share gate on the page builder's inputs; the static, no-script page, which stays unavailable (`no-document`).

### The deal page says first what the witness covers

- **Changed:** the deal page opens with one line saying what its witness receipt covers, read from the bundle's witness block only:
  - "Witnessed through step K of N at `<time>` (receipt attached); later steps: witness receipt pending the next tick", when the receipt covers part of the deal;
  - "Witnessed through the last step this copy holds at `<time>` (receipt attached)", when it covers every step;
  - "Not witnessed yet: witness receipt pending the next tick", before the first tick;
  - "Not witnessed.", when there is no receipt.

  The line never says the whole deal is witnessed. It is on every copy, shared ones included.

### A plugin may offer presentations

- **Added:** the cli-plugin/v1 handshake may carry `presentations`.
  - **What each entry holds:** an `aac.presentation-manifest/v0` manifest (agent-action-capsule's
    presentation contract) and the files it names by role (`script`, `style`, `wording`) with their
    SHA-256.
  - **Compatibility:** an older capsulectl ignores the member. A module's compatibility is its
    manifest's `presentation_api` and `runtime_min`, not cli-plugin/v1.
- **How each file is checked:** capsulectl reads it itself, from the launcher's own directory,
  under the launcher's trust rules, at most 1 MiB. It must hash to both its entry's digest and the
  digest the manifest pins.
  - The manifest and any wording pack are checked against the contract's schema.
  - A plugin's code module is carried in the module slot.
  - Any failure refuses that plugin's presentations; its subcommands still run. `plugin ls` reports
    the refusal as `presentations_refused: {reason, file, detail}`, with a machine-readable
    reason: `digest_mismatch`, `path_escape`, `not_regular_file`, `writable`, `oversize`,
    `invalid_manifest`,
    `invalid_files`, `invalid_wording`, `unsupported_carrier`, `duplicate_id`, `missing_file`,
    `untrusted_root` or `unreadable`.
- **Layout:** a plugin keeps its files under `<launcher>.d/presentations/<module>/`, user-owned
  `0644` files in `0755` directories.
- **Presentations load only from the two built-in plugin roots.** `CAPSULECTL_PLUGIN_ROOTS` never
  makes a directory a source of them.
- **Not in this change:** putting presentations into a page.

### `presentation build`: the offline page with the installed plugins' presentation modules

- **Added:** `capsulectl presentation build --bundle FILE --out FILE [--audience A] [--depth L] [--module ID]` writes the offline evidence page with the installed plugins' trusted-executable presentation modules in its module slots. The page's own runtime decides which module renders.
- **Checked before anything is written:**
  - the page's runtime (`aac.presentation-api/v0`, version `0.1.0`) would refuse no module given to it (`presentation_api_unsupported`, `runtime_too_old`);
  - no two manifests the page would hold, the built-ins included, are of one tier and can match the same bundle (`ambiguous`);
  - no id is given twice (`duplicate_id`), and no manifest can never match (`dead_manifest`).

  A refusal writes no page, and the result names the module and the reason.
- **The page is written only when `verify --bundle` calls the bundle VALID.**
- **What the result reports:** each module carried, and what was left out (`not_included` for a declarative module; `plugins_refused` for a plugin whose presentations were refused).
- **Not in this change:**
  - `presentation list`;
  - the fragment and embedded packagings;
  - the static page, which is reported as unavailable (`no-document`).

### A seller's offer is classed `marketplace.offer`

- **Changed (wire; records sealed before re-derive unchanged).** An offer is now classed
  `marketplace.offer`; before this, `external_commitment.other`. It is the taxonomy-version-6 class a
  seller's rules select on: under the old class no seller rule reached an offer, so an offer below the
  floor was not held to it.
- **No taxonomy change.** Version 6 already has `marketplace.offer`. Each step keeps its own class:
  an offer sealed before this change re-derives as `external_commitment.other`.

### A sale thread's check names the thread's buyer

- **Changed (wire; records sealed before re-derive unchanged).** A check on a sale's thread that
  names no one now seals the thread's buyer as its `counterparty`: the keyed fingerprints the
  thread's opening sealed, per deal, as a purchase's check carries them (typed records:
  `body.counterparty`, `hmac-sha256-chain-key`). Rules keyed on the counterparty, such as one
  commitment per sale, now have a target on a seller's deal.
- Shared copies carry the fingerprints, never the buyer's identifiers. No profile-keyed companion is
  sealed for such a check.

### A seller's commit tells the rules checker when the accepted offer was made

- **Added (checker input only; nothing sealed changes).** The check of a seller's `commit` that rests
  on an accepted offer now carries `record.proposal_at`. That is the offer's proposed-action record's
  sealed `at`, so a rule on an offer's expiry can be evaluated: before, it could not be checked on
  any commit.
- It is the latest offer, with a recorded acceptance and no change of details since: the same
  acceptance the commit gate requires. It is absent on a buyer's deal, on any other action, with no
  acceptance in force, and on history entries.
- `record` is open in `external-check-input-v0.schema.json`, so a checker that predates the member
  ignores it.

### A history act on a sale's thread carries the sale's item reference

- **Added (checker input, additive).** In `external-check-input/v0`, each `history` entry whose
  act was on a sale's thread carries `item_ref`: the sale's item reference, the same plain value
  the top-level `item_ref` carries on a check of that sale's threads. A rule on a second buyer's
  thread can see the first thread's acceptance of the same item. An act of any other deal carries
  none. It is beside the record, as `counterparty_profile` is, never in it: no record changes.
  History entries are open objects, so a checker that does not read it is unaffected.

### A claim's class is one of a seller's fourteen representation classes

- **Changed (schema, additive; records sealed before re-derive unchanged).** A claim's optional
  `class` is one of `price`, `condition`, `features`, `authenticity`, `availability`,
  `delivery_date`, `service_scope`, `warranty`, `refund_terms`, `payment_methods`, `pickup`,
  `deadline`, `address` or `other`: the representation classes of capsule-engine's
  `capsule_engine/guards/wickets/catalog_defs/required_disclosure.yaml` at `21975ce45837`, in
  the same order. Any other class is refused.
- **`delivery_promise` becomes `delivery_date`.** An input that says `delivery_promise` is sealed
  as `delivery_date`; capsulectl never seals `delivery_promise` again. A claim sealed with it
  stays valid in the profile schema, re-derives, and is labelled as before on the page.
- The page labels every class; the deal skill and the profile list them.

### Every copy of a seller's deal engages `x-deal-seller/v0`

- **Added (bundle packaging; no record changes).** Every copy of a deal the user sells (the
  user's own, the counterparty's and an adjudicator's, a sale's threads included) engages the
  bundle extension kind `x-deal-seller/v0`, with an empty block, so a viewer can choose a
  seller's presentation from the engaged kinds. It carries no value and is never authority:
  which side the user is on stays the opening intent's sealed `party_role`. A buyer's deal
  engages nothing new.

### Every amount-bearing check names the payee, not only a pay

- **Changed (wire, additive; records sealed before re-derive unchanged).** Where the user buys, a
  `commit`, `sign` or `cancel` check that states an `amount_minor` now names the deal's payee as a
  `pay` check does (`counterparty.ids.payee`, the per-deal fingerprint), and is followed by its
  `counterparty_profile` companion (the payee keyed per profile, withheld from every shared copy).
  A check without an amount, a share, and a seller's `commit` or `offer` name none. In x-deal-v0
  and typed records alike.
- **The rules checker sees one merchant as one payee on these checks too:** the checked record and
  each earlier act in its history carry the `counterparty_profile` of the check they rest on, so a
  repeated booking in one deal reads as a repeat, and a merchant booked before is not new in a
  later deal.

### A close seals who ended the deal and why

- **Added (wire, additive; records sealed before re-derive unchanged).** A close's outcome and
  close records (and, in typed records, its `action-outcome/v0`) carry `actor` (`user`, `agent`
  or `platform`) and `reason` from a closed set: `completed`, `delivered_mismatch`,
  `awaiting_delivery`, `not_selected`, `user_closed`, `agent_could_not_complete`,
  `merchant_rejected`, `payment_failed`, `cancelled_in_window`, `not_delivered`, `other`. The
  reason follows from the status where it can; a close with `not_received` must state it.
- **Changed (CLI).** `deal close` with `status: not_received` and no `reason` is refused. `actor`
  defaults to `agent` (`user` for `user_closed`); a reason or actor the close contradicts is
  refused.
- The report and page read a close as an actor-marked line ("You closed this deal", "The agent
  could not complete this deal").

### Both parties' copies of a deal can carry one deal id

- **Added.** `deal open --deal-id deal-…` opens your side of a deal the other party opened, under the
  deal id their `deal open` minted. Both copies then seal one `deal_id`, so a `composed/v1` bundle
  joins them as one deal. The id is opaque (`deal-` and 16 hex), each party keeps its own per-deal
  key, and an id the profile already holds is refused. Without the flag a deal gets a new random id,
  as before.

### `verify --bundle` prints the same bytes on every run

- **Fixed.** Each `disclosures` array in `verify --bundle`'s result is now ordered by capsule id,
  then member, then status. That covers the containing bundle's array and each composed member
  bundle's. Before, a record that withheld both `agent_input` and `agent_output` listed them in
  either order, so the same bundle could print differently from run to run. The verdict was
  never affected.

## v0.1.0-rc14

### A typed deal's history acts carry the profile payee key

- **Fixed (checker input only; nothing sealed changes).** A history act in a deal recorded with typed
  records now carries `counterparty_profile`, resolved from the check its authorization rests on,
  whether that act needed an approval or not. Before, it was resolved only through an approval, so a
  repeated payment in a typed deal was not seen as a repeat by a rules checker. Untyped deals are
  unchanged.

### New deal records seal action taxonomy version 6

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"6"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `7ae6b95b37a3a1c4955af90f56ae4c1c0c404aa9` (file sha256
  `7563fcaeb6c192a27877fae3131bca1021526046c8976475c46eebf4fb374272`, JCS sha256
  `fc12eb90bc2b3c85bb49c4d3d7be8f384c6e0b8e189bc3b68a753a4f1af5cd94`).
- **Every `action_class` is unchanged.** Against version 5 (`e5ec2c1`), version 6 changes only
  `approver_role` (to `account_holder`) on `agreement.accept`, `marketplace.offer` and
  `marketplace.sale`. No class is renamed, removed or added, and none changes trigger class.
  - A seller's steps carry: an offer `external_commitment.other`, a commit (accepting a buyer)
    `agreement.accept`, and telling the buyer where the item is `disclosure.personal`. A rental's
    or service's commit or sign is also `agreement.accept`, whose meaning is unchanged.
  - Deal records never carry `marketplace.offer` or `marketplace.sale`.
- **Each step keeps its own version.** A record sealed under `"2"`, `"3"`, `"4"` or `"5"` re-derives
  with it.
- **Release pairing.** A rules checker compares `taxonomy_version` exactly, so a record sealing
  `"6"` needs a checker that holds version 6. The next release from `main` ships with such a
  checker; none is cut from `main` before one is available.

### A rules checker is told which side of the deal the user is on

- **Changed (checker input).** Every deal check now sends `party_role` (`buyer` or `seller`) at the
  top level of `external-check-input/v0`. It is the side the deal's opening intent sealed, or
  `buyer` when it names none, and it is always explicit, so a checker selects its rules without
  inferring the side.
- **Release pairing.** The input's top level refuses members it does not list, so a checker built
  before this change refuses every check that carries `party_role`. A release that sends it ships
  with a checker that accepts it.
- **The deal id has one source per record:** an x-deal-v0 record's `deal_id`, or a typed record's
  `chain_id`, which is exactly that value. No record carries both, and a test now holds it.

### A shared copy lists the counterparty_profile step under a neutral kind

- **Changed:** a shared copy (counterparty or adjudicator) lists every step of the log, withheld
  ones included, so a verifier can tell withheld from missing. The check's `counterparty_profile`
  companion was listed by that kind, which tells the reader the user keeps a fingerprint of
  merchants across deals. It is now listed as kind `private`, still withheld, with the line "A
  record kept for the user only (withheld)".
- **Unchanged:** the record itself, and the user's own copy, which names it as it is.

### A floor in force goes to the rules checker opened, or the check is refused

- **Fixed (fail closed).** A rules checker reads a floor only from its verified
  `commercial_bounds_opening`, and given none it finds the floor rule not applicable. When a floor
  was in force but its opening could not be built, the checker was run without it. Now:
  - `deal check` refuses before sealing anything, naming the cause and never the floor. The causes
    are: the deal's limits cannot be read, no opening on this device recomputes to the commitment,
    or the opening is not a commercial-bounds document.
  - A floor is in force when the limits in force set one, whatever can be recomputed from its
    opening.
  - With no floor in force, nothing changes.
- **Fixed: typed deals sent the wrong opening.** In a typed deal, the opening sent was the open
  step's, but a checker verifies it against the task authority it is given, which seals the floor
  under its own nonce. The two never matched, so a checker verifying the opening failed every such
  check. The task authority's own opening is now sent.

### The user's floor and limit are recognised in any written form

- **Fixed:** the check that keeps a seller's floor (`min_total_minor`) and a spending limit
  (`max_total_minor`) out of a shared copy only matched the form capsulectl writes ("$1700.00").
  It now reads money as prose writes it, and compares by value:
  - currency symbols before or after the amount (`$`, `€`, `£`, `¥` and others), ISO codes
    (`USD 1,700`, `1700 usd`, also written against the number: `USD1,700`, `1700usd`) and
    currency words (`dollars`, `euros`);
  - thousands separators as locales use them (comma, dot, space, apostrophe, no-break or thin
    space), with or without cents (`1.700,00 €`, `1'700.00 CHF`), and `k` (`$1.7k`);
  - a bare number a few words after a money word ("the lowest I'll take is 1700").

  Dates, times, ids and digests are not read as money. An amount the other party is shown anyway
  (the asked price, an offer, a payment) is still not protected.
- **Added:** a message the agent writes (`from: agent`), and, where the user sells, an agent's
  claim (which the buyer's copy opens), is refused before it is sealed when it states the floor or
  the limit in any of these forms. The refusal names the field, never the value. The user's own
  words are recorded as said.

## v0.1.0-rc13

### The payee keyed per profile, for the user's own history

- **Added (wire, additive; records sealed before re-derive unchanged).** A check that names a
  payee is now followed, before any rules checker runs, by a `counterparty_profile` record.
  - **What it holds:** only `{"fp_alg": "hmac-sha256-profile-key", "ids": {"payee": …}}`, the
    payee fingerprinted with the same construction as the per-deal fingerprints. Its key is
    derived from the profile store's own secret, so one merchant has one value across the
    profile's deals and another in any other profile.
  - **Where it points:** it names its check with an `about` ref. Each deal record's per-deal
    `counterparty` fingerprints are unchanged.
- **Never shared:** no shared copy, for the counterparty or an adjudicator, carries it; the check
  itself stays shareable.
- **The rules checker gets it:** `external-check-input/v0` gives the checker
  `record.counterparty_profile` for the check being decided, and `history[i].counterparty_profile`
  for each earlier act (from its approval's check). It is supplied beside the sealed capsule and
  never part of it. Acts checked before this release carry none.

### New deal records seal action taxonomy version 5

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"5"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `e5ec2c1ba84f9d9269cbb164eb6a92400e4d5d49` (file sha256
  `7d31e892cdea7352d0485c89430dba028bf02ce91fb0e932f4752c1f4066022f`, JCS sha256
  `af3a054cae880e13ce5d8d8bdaa64383e6f3af14fa2bf2ddb88aab226865b13a`).
- **Every `action_class` is unchanged.** Against version 4 (`5300e0b`), version 5 changes only
  `approver_role` (to `account_holder`) on `communication.send`, a class no deal record carries;
  no class is renamed, removed or added, and none changes trigger class.
- **Each step keeps its own version.** A record sealed under `"2"`, `"3"` or `"4"` re-derives with
  it.

### `deal checkpoint status`: a deal log's latest checkpoint, read-only

- **Added:** `capsulectl deal checkpoint status --deal ID` returns, as JSON:
  - the deal log's latest checkpoint: its `id` (the SHA-256 of the signed statement), `at`, and
    the log head it covers (`mmr_size`, `entries`, `root`);
  - the log's `log_entries`;
  - the checkpoint's witness state as stored.

  A deal with no checkpoint yet has `checkpoint` and `witness` `null`, and exits 0.
- **Read-only:** it never seals, never cuts a checkpoint and never contacts the witness, and every
  file under the store is byte-for-byte the same after it. It works on a `read_only` profile.
- **Fixed:** opening a log that does not exist could panic while cleaning up the failed open. It
  now returns the log's error.

### Where the user sells: a seller profile, an exact acceptance, and one item for several buyers

- **Added (wire, additive; every deal sealed before re-derives unchanged).** `party_role`
  (`buyer` | `seller`) on the intent, set when a deal opens and fixed; absent means buyer.
  `deliver_by` and `perform_by` obligations with a `due_by` date. A `not_selected` close.
- **A seller's floor stays private.** `min_total_minor` is never sealed. Records that state it
  seal `bounds_commitment`, a salted commitment (`sha256-jcs-nonce256`) to a
  `commercial-bounds/v0` document. Only the user's own copy carries its openings, and only the
  user's own rules checker is given the opening in force (`commercial_bounds_opening`). No
  shared copy, for any audience, carries the floor (see "Not yet"). A check below the floor asks
  (`under_floor`).
- **An exact acceptance.** `offer` is a check, allowed only where the user sells; a later offer
  supersedes the earlier one (`supersedes`). `deal note --kind acceptance` seals the buyer's
  acceptance of the exact offer by its digest. A seller's `commit` needs an acceptance of the
  latest offer with no change since, must equal that offer's amount, currency, terms and
  recourse, and cites the acceptance. A deal where the user sells must use typed records.
- **A sale is money in.** A seller's commit and offer count 0 toward any spend total.
- **What the buyer's copy shows (seller deals only).** The offer, the task authority in full,
  the acceptance, and the seller's agent's own representations to the buyer (claims, with an
  optional `class`: `condition`, `warranty`, `refund_terms`, `delivery_promise`, `other`) whose
  words hold none of the user's private details. A buyer's deal is shared exactly as before.
- **One item, several buyers.** `deal sale new` seals a sale's own log; `deal open --sale ID`
  opens a buyer's thread under it. Each thread seals the item reference and the sale's task
  authority only as per-thread salted commitments, so no digest or commitment links two buyers'
  copies; both still show the item's name and the offer, which are the seller's own listing.
  The user's own rules checker is given the plain `item_ref`.
- **Not yet:** the check that keeps the floor out of written text misses some formats
  ("$1,700.00", bare amounts); a fix follows.

### A refund's check names the payment it returns

- **Added (wire, additive; records sealed before re-derive unchanged).** A deal `check` of a
  `cancel` that returns a sealed payment (a refund) now carries `reverses_ref`: that payment's
  `action`, by its record digest, as a typed reference (`{"type": "record", "digest_alg":
  "SHA-256", "digest": …}`). It names the same payment, by the same digest, as the refund
  `action`'s `reverses` ref. A typed deal's `proposed-action/v0` carries it too.
- **Amounts unchanged:** a refund's `check` and `action` both keep `amount_minor`. A partial
  cancel's keep `cancelled_amount_minor` and name no payment. No `returned_minor` is sealed.

## v0.1.0-rc12

### New deal records seal action taxonomy version 4

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"4"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `5300e0b`.
  A reader that holds a later version found no class-keyed result for a record that named `"2"`.
- **Every `action_class` is unchanged.** Versions 2, 3 and 4 name the same classes. Versions 3
  and 4 add `approver_role` `account_holder`:
  - version 3 to `money.purchase`, `money.subscription`, `booking.create` and `booking.modify`;
  - version 4 to `booking.cancel`, `data.delete`, `communication.publish` and
    `disclosure.personal`.
- **Each step keeps its own version.** A record sealed under `"2"` or `"3"` re-derives with it.

### Rules a checker declares but does not measure are said quietly, not as a warning

- **Fixed:** a rules checker reports the rules its ruleset declares but it does not measure as
  `not_evaluable` with the reason `declared, not measured`. On every escalated purchase, the deal
  card listed each one as a reason to ask ("Your rules ask for approval: …: declared, not
  measured"); under an overall `not_evaluable` it would read "not fully checked".
- **Now:** such a finding is never a difference.
  - The approval text names the rules quietly beside the verdict: "3 rules are declared by the
    ruleset but not measured by this checker (r14, r24, r25)."
  - So does the user's own receipt, on the check's line. A shared copy does not name them.
  - The verdict is unchanged: an allowed purchase stays allowed.
  - Any other `not_evaluable` is still a difference, said as before.
- **Presentation goldens:** a new fixture, `7-receipt-declared-rules`.

### `doctor --check-witness` is ok only when the witness answers with success

- **Fixed:** the witness probe sent a HEAD to the endpoint and called any answer ok. A witness that
  serves no HEAD routes answered 405 and was reported `reachable: true, status_code: 405, ok: true`,
  while the first delivery could still fail.
- **Now:** the probe is a read-only GET of `<endpoint>/health`, the witness's health endpoint. It is
  ok only on a 2xx answer. Any 4xx or 5xx is `ok: false`, with an issue naming the status, the
  method and the URL.
- **Changed:** a delivery the witness answered with an error keeps the witness's HTTP status (never
  its response body), and its pending text says it: "pending: the witness answered with an error
  (HTTP 409); it is retried at every tick."

### A deal claim's words are sealed as a commitment; only whose it is shows

- **Changed (wire, additive; records sealed before re-derive unchanged).** A claim, on a `claim`
  step and in the baseline's `claims[]`, is now sealed as:
  - `text_commitment`: the claim's words, salted, like every other text a deal record carries;
  - `source_kind`: whose it is (`merchant`, `agent`, `platform`, `user` or `external`), in the
    clear;
  - optional `source_ref_commitment`: the free-form note of where it was read, committed.

  A verdict names the claims it found unverified by reference (`unverified_claims`), not by their
  words. The typed `action-evaluation/v0` does the same.
- **`source_kind` is stated by the caller.** A source that can only mean the other party
  (`counterparty`, `seller_message`, `merchant_email`) is taken as `merchant`. Any other source (a
  page, a photo, a snapshot) must state it, and a claim that leaves it open is refused, naming the
  field. The deal skill says how to choose.
- **Openings:**
  - the user's own report carries `claim_openings`;
  - an adjudicator's copy carries those of claims holding none of the user's private details;
  - a counterparty's copy shows each disclosed claim record (its `source_kind` and salted
    commitments) and no opening.
- **The profile checker:** `check_profile.py --openings=FILE` recomputes each opening against the
  sealed record. A shared vector (claim words and source note) is checked by both the Go and the
  Python implementation.

### A check carries the inputs a rules checker reads

- **Added (wire, additive):** a check sealed from this version carries scalars a rules checker
  reads. They go on the `x-deal-v0` check and the typed `proposed-action/v0`, and each is absent
  when not known:
  - `recipient_role` (`fulfilling_merchant` or `third_party`, on a share);
  - `channel` and `first_contact_channel` (channel kinds);
  - `upfront_amount_minor` (the stated deposit, never inferred);
  - `material_fields_changed` and `offer_fields_changed` (counts over two fixed field lists: against
    what was agreed, and against the keys the user's own words state, absent when they state none),
    with `material_fields_basis` and `offer_fields_basis` (each list's digest);
  - on typed records, `task_authority_ref`.

  None carries a name, a contact detail or free text. Records sealed before re-derive unchanged.
- **Added:** a typed deal's `task-authority/v0` also carries its plan: `outcome_id` (a fixed id per
  deal type, `capsulectl.deal.<purchase|rental|booking|service>/1.0.0`), its allowed actions as
  `allowed_actions`, and `preconditions: []`. The external-check input gains an optional `task_authority_record`
  member: that whole sealed record, whose SHA-256 over its JCS bytes is the check's
  `task_authority_ref` digest; its plan is at `body`.
- **The deal skill** now says to always state `recourse` (the rail and whether it is refundable) on
  a pay, and to put a deposit the checkout asks for in `terms`.

## v0.1.0-rc11

### A rules checker's answer that cannot be sealed pauses the check instead of failing it

- **Fixed:** on the second purchase in a week, a checker's finding value such as "2000 authorised
  (capture 2000); 7d total 4000 (2000 earlier)" reads as a phone number to the personal-data scan.
  `deal check` refused to seal its verdict and exited 2, with no verdict and no prompt.
- **Now:** a checker answer that cannot be sealed never fails the check. The check pauses with
  "Your rules were not checked: the rules checker's answer could not be sealed (…)", and the verdict
  seals the rules as not evaluated, with cause `unreadable`. The personal-data scan itself is
  unchanged.
- **Numeric limits and values:** a checker that reports `limit` and `value` as numbers (as the
  contract allows) is never caught this way.

### `result build` and `report build`

- `result build` seals a caller-supplied Evidence Result v0 into a jsonl profile's evidence book
  as one `evidence_result` record, after checking it against the vendored schema, recomputing its
  headline values, resolving every citation in the book, and walking each close claim's Close.
  Only UNILATERAL close claims can be sealed end to end today; see `capsulectl result build
  --help`.
- `report build` verifies a held, disclosed bundle rooted at a sealed Result v0 and renders an
  offline `report.html` (and, with `--permalink`, a viewer fragment over the same bytes), through
  the same vendored evidence-graph viewer `deal report` embeds.
- `disclose` / `permalink --attach-input-originals` (jsonl profiles): carry each published
  capsule's retained agent_input original in the bundle, in the
  `capsulectl/agent-input-originals/v1` extension, checked against the capsule's
  `agent_input_digest`. Opt-in at disclose time; the book never stores an original.
- **Changed:** `close --capsule-out`: a file already holding exactly the Close's bytes is now left
  alone, so a repeat with the same flags succeeds as a no-op; a file holding anything else is
  refused by name and nothing is overwritten. Before, any existing file at that path failed the
  command.
- **Changed:** `disclose --suppress agent_input` also withholds an agent_input original a book
  record commits as a payload (records written by a pre-release build that stored originals in
  the book); under `--payloads all` it refuses instead, since that mode cannot withhold a payload.
- **Removed:** the `--html` flag on `bundle`, `disclose` and `permalink`. It was a stub that never
  rendered a page; `report build` replaces it.

### `book` verbs read another party's evidence book through a mesh node

- **Added:** a `mesh-plugin` profile type, and read-only `book list|get|request|head` verbs that
  ask another party's evidence book through a mesh node's `evidence-request/1` tool.
  - Requests are sent and digested in canonical form, so a signed refusal binds to the request
    that was sent.
  - A refusal is accepted only if its Ed25519 signature verifies offline and it names that request.
    `--responder-key` pins its signer.
  - `book head` verifies the newest reported checkpoint's COSE statement and matches every reported
    field to it. `--responder-checkpoint-key` pins its signer.
- **The node token** is optional, redacted in `profile show`, sent only over https or to a loopback
  address, and never follows a redirect. Storage verbs refuse the new profile type.

### `book head` checks its pinned keys and requester id before sending

- **Fixed:** `--responder-checkpoint-key` is checked like `--responder-key` (a 32-byte Ed25519 key in
  hex) before anything is sent. A malformed one used to reach the node and fail only as a mismatch.
- **Changed:** a requester id the request names must be a node's full peer id (64 lowercase hex),
  not any non-empty string.

### Dependencies

- **The checkpointed-log library** moves from `github.com/action-state-group/cll-go` (an untagged
  pseudo-version) to its successor module `github.com/action-state-group/checkpointed-local-log/go`
  v0.1.0.
- **Pinned at untagged commits:** `github.com/action-state-group/agent-action-capsule/go` v0.7.0 →
  `v0.7.1-0.20261008155852-5d80d40097f6`, and `github.com/action-state-group/evidencebook` v0.1.1 →
  `v0.1.2-0.20261008160739-b970240ce88a`.

## v0.1.0-rc10

### A deal receipt's words for where the deal stands and the merchant's receipt

Consumer wording only, on the receipt page and in the emailed receipt. No record type, field or verb
changes. "receipt" is wording here, never a field, record type or key: a test keeps every deal and
external-check schema free of one.
- **An open deal** with no merchant email sealed yet reads "Open: waiting for the merchant's
  receipt." One with the merchant's email sealed reads "Open: the merchant's email is sealed; no
  close is sealed on this deal yet." Both are followed by the expected close date, as before.
- **A completed close** reads "Closed at …: matched, nothing left to match." Other outcomes are
  unchanged.
- **A merchant email sealed after the close** reads "The merchant's receipt arrived. It matches what
  you approved." or "The merchant's receipt arrived. It differs: you approved $48.00; their receipt
  says $64.00." This applies only when an approved amount is on record (not only a limit). A copy
  not confirmed by the merchant's signature says so. Shared copies keep their fixed words.
- **The user's choice on a check** reads as the option was shown: "you chose Pay anyway" over a
  finding, "you chose Hold". It used to read the option's id ("proceed", "hold").

### `deal check` runs the user's pinned rules checker and seals what it said

- **Added:** a deal profile may pin an external rules checker (`profile update --rules-checker
  FILE`): a command under a trusted plugin root, pinned by its executable's SHA-256 and,
  optionally, by the definition digest of the ruleset it must report. It is the user's policy,
  never the agent's.
  - **What it gets.** Every `deal check` runs it, from a private copy of the exact bytes it
    hashed, with one `external-check-input/v0` object on stdin
    (`skills/deal/profile/external-check-input-v0.schema.json`):
    - the capsule of the step being checked, with its disclosed record;
    - as history, the profile's sealed acts with an amount from every deal in the last 31
      days (at most 1,000), stating whether that is all of them.

    A rolling limit is evaluated over that history. A checker given too little reports it
    `not_evaluable`, and the check pauses.
  - **What it prints.** One `external-check-result/v0` object: the ruleset's id and definition
    digest, a verdict (`allow`, `deny`, `escalate` or `not_evaluable`), and its findings, each
    limit and value a number or a short line, and optionally how it reached the verdict (`tier`:
    `recomputed` or `judged`, as in Result v0; absent reads as judged, never recomputed) and
    the grade of its evidence (`grade`: `self-attested`, `witnessed` or `countersigned`, by
    reference to Result v0's Grade). The tier and grade are
    sealed as reported and named in the approval text; they change nothing. The schema ships at
    `skills/deal/profile/external-check-result-v0.schema.json`, and output outside it is
    refused.
- **One verdict, one prompt.** The checker's answer folds into the check's own differences.
  - **`deny`** is a new verdict: the card names the rule, its limit and the value, and offers
    only "Hold". An approval to proceed is refused.
  - **`escalate` and `not_evaluable`** pause.
  - **A checker that cannot answer pauses**, saying "Your rules were not checked:" and why. That
    covers a checker that changed since it was pinned, refused the record, timed out (10s by
    default, at most 60s), printed anything else, or reported another ruleset digest than the
    pinned one.
  - **No checker configured** does not pause, but the text says "Your rules were not checked:
    no rules checker configured."
- **Sealed (wire, additive).**
  - The `x-deal-v0` verdict gains `result: "deny"` and `rules`: the status, the ruleset id and
    definition digest, the checker's SHA-256, the verdict, every finding (id, verdict, limit,
    value), and `history` (how many earlier acts the checker was given, over how many days, and
    whether that is all of them). A checker that was not evaluated seals a
    `cause` token. No words the checker wrote are sealed in the clear: they reach the card,
    which is committed to.
  - Typed records: `action-evaluation/v0` gains disposition `DENY` and `rules_checks`, and
    `check-response/v0` gains `DENY`.
  - Shared copies carry none of the user's rules: the verdict record is withheld, and its
    findings read in fixed words.
- **Changed:** `approval_text` leads with the rules line and then "Before you go ahead:" in
  place of "Deal check:". The "Checked at … Stale after …" line has left the text; staleness
  stays in `checked_at`, `stale_after_minutes` and the sealed record.
- **Changed:** a card whose only finding is that the rules could not be checked offers "Pay" (or
  "Confirm", and so on), not "Pay anyway": there is nothing to override.

### A deal receipt's text is sealed with it

- **Fixed:** a deal receipt's readable text (summary lines, step lines, amounts, what was told)
  is now sealed. Writing a receipt (`deal report --html/--bundle/--email`, `bundle --deal`,
  `disclose --deal`) seals that copy's own text as a `deal_report` record on the deal's log,
  salted with a fresh nonce. The bundle's `x-deal-v0` extension only names it.
  - **What fails:** editing a line, removing the extension, or naming another record makes
    `verify --bundle` INVALID, and the page says the report did not verify.
  - **What the page says:** for a sealed copy, that it checked the text has not changed since
    it was sealed.
- **Changed (wire):**
  - The deal's log now holds a sealed report after its steps for each receipt written. A
    report that writes no file seals nothing.
  - Later copies carry every earlier report's record with its text withheld: a copy
    discloses its own text and no other copy's, and a shared copy seals the text already
    rewritten for its audience.
  - Witness coverage is counted in steps. A tick that holds the deal's steps, with only
    reports after them, still shows as witnessed, or as pending for its reason.
- Receipts written before this change verify as before, with their text reported
  `extension_unbound`.

### A deal receipt says its text is not verified; records declare their commitments' construction

- **Changed:** a deal receipt's readable text (summary lines, step lines, amounts, what was
  told) rides in the bundle's `x-deal-v0` extension, which no record seals: editing it left
  `verify --bundle` VALID and the page showing the edited text. `verify --bundle` now reports
  that extension with the finding `extension_unbound` and a note (the verdict is unchanged),
  and the receipt page says first that it does not check that text. Binding the text to a
  sealed record is planned.
- **Wire change (schema-visible, additive):** every deal record sealed from now on carries
  `commit_alg: "sha256-jcs-nonce256"` (in the `x-deal-v0` block; in a typed record's header),
  naming the construction of its `*_commitment` values: SHA-256 over the JCS of
  `{"nonce", "text"}`, a fresh 256-bit nonce per commitment, no key, recomputable only from the
  opening. Records sealed before carry none and re-derive unchanged. PROFILE.md section 4
  states the construction, its nonce and key scope, and where openings travel.

### A page is written only when it can stand behind what it shows

- **Behaviour change.** `bundle --html`, `disclose --html` and `deal report --html`
  write a page only for a bundle that `verify --bundle` calls VALID. For an INCOMPLETE
  or INVALID bundle no page is written: the command exits as `verify --bundle` would
  (3 or 1) and names the claims that did not pass. The page's viewer would otherwise
  have said "Bundle verification passed" for a bundle the CLI calls INCOMPLETE.
  `bundle` still writes the bundle (`--out`, or stdout), since `verify --bundle`
  states its verdict; `disclose` writes nothing and puts no disclosure on record when
  its page is refused.
- A page whose `report/v1` root would show fewer rows than it has (a row cites a
  record the bundle does not carry) is refused, naming how many rows it would show.
- A jsonl profile writes no page: its bundle carries its evidence book's records about
  what was sealed, not the records themselves, so a page cannot show them. Publish on
  a sqlite profile for a page; `bundle --out` still writes the bundle.
- Every page path measured on the previous release was VALID and is unchanged: deal
  receipts (own and shared copies) and the README's self-checking report.

### `profile retire NAME`: free a profile's name without touching its records

- Renames the profile to `NAME-retired-YYYYMMDD` (`-2`, `-3` … the same day), with
  its log id, keys and data directory unchanged: every record, checkpoint and witness
  receipt stays in its own log, and the retired profile still bundles and verifies
  them. Nothing is deleted, moved or re-logged. The freed name can be created anew,
  for example as a sqlite profile.
- The retired profile is not made read-only: a jsonl profile's bundle puts itself on
  record, which a read-only profile refuses.
- `--move-data NEW_DIR` also renames a jsonl profile's data directory (to a new path
  on the same filesystem), for an install that wants the old path. It refuses a sqlite
  profile, an existing path, and a key file inside the data directory.
- Anything that runs capsulectl with `--profile NAME` (a scheduled checkpoint cadence,
  a plugin) is not changed; the command says so.

### `verify --bundle` verifies composed/v1

- **Changed:** a bundle carrying a `composed/v1` extension (Evidence Bundle -01 §7.2) is now
  verified instead of being listed as uninterpreted: the composed digest, each member (with a full
  per-member assessment for carried member bundles), composition closure, joins and the
  redundancy flag. It counts toward the verdict: a failed block or an INVALID member bundle makes
  the bundle INVALID; a declared-missing member, a join that can't be derived or an unverified
  refusal signature makes it INCOMPLETE. Uses agent-action-capsule go/v0.7.0. Other extensions are
  still listed as carried but not checked.

### A spend limit binds the most a payment may take

- **Fixed: an authorization buffer evaded every limit.** A pay check carried only the expected
  charge, so a $4.54 order passed a $5 limit while the card approval let the merchant take up to
  $9.54.
- **New check field `authorized_max_minor`:** on a `pay`, the most the counterparty may take
  under the payment's authorization (a card hold, a pre-authorization with a buffer for tax
  settled later). It must be at least `amount_minor`, which stays the expected charge. The intent
  limit evaluates it, and the difference names it ("up to $9.54 may be taken (the authorized
  maximum; the expected charge is $4.54)", field `authorized_max_minor`).
- **Behaviour change, fail safe:** a pay by card, wallet, PayPal, or any rail not known to be
  holdless is refused (exit 2) unless it states `authorized_max_minor` (the same as
  `amount_minor` when no larger maximum is shown). A pay on a holdless rail (bank transfer,
  Zelle, wire, cash and the like) defaults it to the amount.
- **Sealed:** the check carries `authorized_max_minor`, and `spend_authorized_minor` beside
  `spend_minor` (which a per-action cap reads; a rolling cap sums the actions' `spend_minor`). A
  typed `proposed-action/v0` carries it, so an approval never extends to a larger maximum.
  Optional members, kept per step: records already sealed re-derive unchanged.
- **The capture:** an action that takes more than the authorized maximum differs from its
  check; one below it, though not the estimate, is what a hold is for.
- The deal skill tells the agent to read the maximum from the approval or card hold and pass it.

### A second report on a profile works after a checkpoint covers a disclosure

- **Fixed:** once a checkpoint covered a disclosure, every later `bundle` and `disclose` on that
  profile failed ("operation failed … sensitive details suppressed"). A re-run of an install, or
  any later report, hit it.
- **Wire change: what `disclose` puts on the log.** It used to append the bare digest of a
  disclosure record, kept nowhere else, which no later bundle could supply. It now seals the
  disclosure record as an ordinary `fyi` capsule (`action_id: capsulectl-disclosure`) with the
  profile's signing key, stores it, and appends its capsule id. Its input holds only ids, digests
  and labels, never disclosed payload bytes, plus a random 256-bit nonce, so the digest a later
  bundle shows cannot confirm a guess at what was disclosed.
- **A later bundle** proves an earlier disclosure capsule's place in the log but withholds its
  input, so a copy made for one party never shows what was disclosed to another. Such a bundle
  states `payloads_mode: selected`.
- **Logs written by an earlier release** that disclosed and then checkpointed cannot be bundled
  over: the record behind a bare digest is gone. The error now says what it can know, exit 2:
  "log entry N is checkpointed but not in this profile's store: either a disclosure an earlier
  release appended as a bare digest, or a lost record; no bundle over this log can include it;
  start a new profile (or log id) for new reports".

### A merchant email whose key record DNS could not reach: offer a supplied record

- **Fixed:** when DNS could not be reached, sealing a merchant email (`deal note --kind evidence
  --email`) stopped with an error that only said to retry or pass `--key-record`, and the deal
  skill never mentioned `--key-record`, so an agent stopped there.
- **The error now names the record it needed and the way on:** "could not reach DNS for the DKIM
  key record SELECTOR._domainkey.DOMAIN (…): retry, or get that TXT record another way (another
  resolver, or a lookup the user runs) and pass it with --key-record FILE; it is sealed as
  supplied, which is weaker evidence than a record read from DNS". A DMARC record it could not
  read names `--dmarc-record FILE` beside `--key-record FILE` (it used to name `--key-record`
  alone).
- **The deal skill** tells the agent to offer that way on instead of stopping. It says:
  - which record is needed, and where it can come from;
  - that the email is sealed as supplied (`key_source: supplied`), and the report and the emailed
    receipt say so;
  - that a supplied record is weaker evidence than one read from DNS.

### `verify --capsule` reads a bare capsule

- `verify --capsule` also reads a bare Agent Action Capsule as capsule-emit seals it (the
  capsule's own fields with an inline `signature` and `key_id`), with the same checks as the
  artifact.Record wrapper. The file's shape is read from its members, never guessed, and named
  in the output (`shape`); a file with the members of both shapes, or of neither, is refused.

## v0.1.0-rc9

### The checkpoint cadence: every 5m by default

- **Behaviour change for existing deal profiles.** A deal profile's witness cadence
  (its checkpoint cadence) now defaults to a tick every 5m, give or take 1m, instead
  of every 1h, give or take 10m. A profile that sets no cadence moves at upgrade; one
  that sets both `cadence.interval` and `cadence.jitter` keeps them.
- **A profile that sets `cadence.jitter` but not `cadence.interval` may now be
  refused.** Its jitter must be under half the 5m default, so under 2m30s. The error
  names both values; set `cadence.interval` (for example `1h`), or lower the jitter.
- The poll is `cll checkpoint cadence` (`deal tick` stays callable). Schedule it every
  5 minutes with `--wait-up-to 5m`. A run with no tick due publishes nothing.

### Deal checks: which agent picks pause is a pinned materiality predicate

The pinned predicate itself first shipped in v0.1.0-rc8, whose release notes did not describe
it. v0.1.0-rc9 added sealing it at a deal's opening, flagging a re-pin mid-deal, the
missing-file message, and the commitment to its name and version.

- **Behaviour change for existing deal profiles.** Which attributes the agent picked
  on its own (a size, a delivery option the user never named) pause a deal check is
  no longer compiled into capsulectl: it is a materiality predicate
  (`materiality-predicate/v0`, see `skills/deal/profile/materiality-predicate/`).
  **A profile with no predicate now pauses on every attribute the agent picked.**
  To keep the earlier behaviour, pin the example predicate, which reproduces it:
  `capsulectl --profile NAME profile update --materiality skills/deal/profile/materiality-predicate/neutral.json`.
  `capsulectl doctor --profile NAME` says which applies.
- The predicate is pinned in the profile by its digest (`materiality.digest`). A check
  refuses, sealing nothing, when the pinned file has changed; re-pin it with
  `profile update --materiality FILE`. Setting the predicate is the user's policy: a
  check takes no predicate of its own.
- A profile that names a predicate without a pinned digest (written by a build between
  the two changes) refuses checks until it is pinned the same way.
- Every check says which predicate decided (name, version and digest, or digest
  `none`) in its output, and seals it in its verdict (`materiality`, an additive
  x-deal-v0 field).
- `deal open` seals the predicate pinned when the deal opened into the baseline
  (`materiality`, additive), from the profile and never from the input. A later check
  under another predicate (a re-pin mid-deal) carries a `materiality_changed`
  difference: it shows on the card and pauses the check.
- A pinned predicate whose file is gone refuses with its own message ("materiality
  predicate file missing; re-pin it with profile update").
- Sealed records carry the predicate's digest and a commitment to its name and
  version (`label_commitment`); the user's own copy opens it (`materiality_openings`).
  A copy shared with a counterparty discloses the digest only: the name and version
  describe the user's own policy. This also fixes shared copies, which withheld every
  check step once the verdict began recording `materiality` in v0.1.0-rc8. Verdicts
  rc8 sealed with the name and version in the clear stay withheld from shared copies.
