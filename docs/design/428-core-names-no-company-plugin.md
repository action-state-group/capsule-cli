# Design note: the core names no company plugin

capsule-cli#428 (donation blocker, follow-up of #326). Status: converged for
the cross-review CLIs (Codex, Kimi; four rounds), open for Sherlock2 and for
one point in section 7. Checked against `main` at `ad10fe46`, evidencebook
`capsulectl` and evidencebook-deal `main` (read-only).

Scope: steps 1 and 2 of the How ack (generic plugin hand-over; `canary` and the
install check). Steps 3 and 4 (deal wording, deal-view) and step 5 (deal-format
spec text or verify code) follow in their own notes. The door is decided: before
donation the core names no company plugin, and a profile names the plugin that
owns its log. Sealed vocabulary (section 5) is not renamed.

## 1. What the core names today

- `pluginhost.go`: `BookPluginName`, `DealPluginName`, `SetBookBackend` and the
  `BookBackend`/`BookLog`/`BookBundleRequest` types, the hooks `DealBundleRun`,
  `DealCommandGroup`, `DealTickCommand`, `delegateBook`, the transitional aliases.
- `plugin.go` (reserved names, placeholder help), `plugin_install.go` (`isFirstParty`,
  its three uses), `profile.go` (`IsBook`, `Log`, `IsShelf`), `storage.go`
  (`OpenDealTarget`, `ErrDealProfileWrite`, the `deal_shelf` probe),
  `bundle.go` (`--deal`, `--share`, `--to`), `checkpoint.go` and
  `user_root_heal.go` (`tick`, `userDealRefused`), `canary.go`, `doctor.go:139`,
  `doctor_install.go` (`profilePlugin`, the deal skill), `profile_retire.go`,
  `result.go`, `command.go` (`store migrate`), `evidencedoor.go`
  (`RemoteBookCommand`), `evidence_request_cmd.go`, `presentation_list.go`.
- The plugins also use `Target.Book`, `ErrBookProfile`, `AliasRemovedIn`,
  `ErrorPhase` and many `Deal*` symbols; PR A starts from a grep of their
  `cli.` imports.

## 2. Ownership

`Profile.Log` is `cll` (the core keeps the log) or a plugin name. The name uses
`pluginNameGrammar` and is not in `reservedPluginNames`. Ownership is explicit
only: it never depends on which plugins are installed. A profile that today has
no `log:` but a `log_id` (or the sqlite `deal` namespace) has no declared
owner; commands that need an owner stop with
`profile X has no declared log owner: capsulectl profile update --profile X --log <plugin>`.
`profile update` reads such a profile without resolving an owner, so the fix
runs. Plain profiles without a `log_id` stay core-owned. `profile create` and the
plugins' `init` write `log:`. The `cli-plugin/v1` handshake does not change.

## 3. Dispatch

Forwarding is by owner and command path, on the raw argv before core flag
parsing (`CommandLine` rebuilds only parsed flags; it is not enough). Every
site that calls `delegateBook` today forwards by owner: `store init`, `publish`,
`cll append|list`, `checkpoint create|status|publish`, `bundle|disclose|permalink`,
`result open`, `profile retire --move-data`. Inside the owner's own process a
missing capability is an error and never a second forward.

Two kinds of non-core owner, told apart by the store, not by registration (the
plugin runs in another process):

- **Backend-owned** (the book): the plugin registers a `LogBackend`
  (`RegisterLogBackend(name, b)`, the renamed `BookBackend`). A core process
  without it forwards.
- **Store-owned** (the deal): the log is a plain CLL store. The core reads it
  in any process (`cll list --log-id`, `get`, `head`, plugin installed or not)
  and refuses to write it; only the plugin's verbs write. `--deal/--share/--to`
  and `checkpoint cadence` (the plugin's `tick`) are defined in the plugin's
  own tree; the core forwards a profile whose owner defines them. They are
  core-defined with hooks today and move to the plugin tree in PR B.

`bundle --profile <deal-owned>` forwards whole to the owner. `request` and
`respond` with no selectable profile say "select a profile" (the core cannot
default to a plugin). `checkpoint cadence` on a core-owned profile says its log
is kept by the core and has no cadence. The scheduled-run heal in
`user_root_heal.go` keys on the selected profile's owner launcher instead of
the deal launcher name.

## 4. Store guard

`IsShelf`, `ErrDealProfileWrite` and `OpenDealTarget` become a neutral
"store owned by a plugin" guard: `plugin_owned(owner, version)` marks the
store, and a core write on a marked store is refused whatever the profile says.
`OpenDealTarget` becomes `OpenOwnedTarget(ctx, p, use, PluginStoreWriter)`.

The old `deal_shelf` table records no plugin name, so nothing can be derived
from it. Migration: the plugin writes `plugin_owned` only on its writable
initialization and migration paths, under the writer lock (`deal init`,
`open`, `tick`, `check`, `report`, `history`: every verb that holds the lock).
Read paths never write it (`openDealStoreRead` stays write-free). Until PR B
the core keeps the old `deal_shelf` guard too. Tests, before and after
migration: a store with only the old table plus a profile declaring `log: cll`
is refused; a marked store with a disguised profile is refused; the plugin
with the lock writes; reads work with the plugin installed and absent.

## 5. Out of scope: sealed vocabulary

Action ids `capsulectl-deal-report` and `capsulectl-deal-history`,
`Developer: capsulectl-deal`, the log-id formats `deal/`, `deal-cadence/`,
`deal-history/` and the module id `capsulectl.deal-view/v0` are written into
bundles that core verify checks without the plugin. This note removes
plugin-name dispatch and hooks, not sealed values. Moving the verify code is
step 5. `CheckDealStandaloneReport`, `DealReportActionID` and `HistoryActionID`
stay until step 5 decides.

## 6. The rest of steps 1 and 2

- **Aliases.** The transitional aliases (`close`, `reconcile`, `request`,
  `respond`, `compose`, `result open`, `report`, `backfill`, `migrate`) are
  deleted in PR B. Due in v0.2.0 (`AliasRemovedIn`); PLUGINS.md says once the
  skills, the Muse integration and the docs use the new names. The PR greps
  skills, docs and examples first and updates PLUGINS.md. No install-time alias
  table.
- **`isFirstParty`** goes. Install keeps its path and `plugin_api` checks. A
  `core_version` is compared only when the plugin declares it (install, dispatch
  warning and doctor use one function); the fallback of `coreVersion()` to the
  plugin's own `version` applies to no plugin. Both plugins get `core_version`
  from `NewPluginCommand`.
- **`doctor --install-check`**: the core keeps the neutral checks and checks the
  selected profile's owner plugin (installed, version). `profilePlugin` uses
  `p.Log` (today it reports the deal plugin for every sqlite profile). The deal
  skill and witness acceptance move into the deal plugin.
- **`canary`** moves to the deal plugin; the core's `canary` is removed.

## 7. Staging and open points

PR A (capsule-cli): the generic API and dispatch, old names kept as thin
wrappers, no behavior change. Plugin PRs (evidencebook, evidencebook-deal):
switch to the generic API and write the marker. PR B (capsule-cli): delete the
old names, aliases, the `canary` verb and `isFirstParty`; the core builds
against neither plugin.

Open (needs a ruling): PR B cannot drop the old `deal_shelf` guard on release
order alone, because a store the migrated plugin never opened still has only
that table. Options: (a) the core keeps probing the legacy table as a
stored-data compatibility check, the one remaining deal word, a data
identifier and not a plugin name; (b) a plugin migration verb writes the
marker everywhere and the core refuses unmigrated legacy stores until run;
(c) the core refuses writes on any store whose schema it does not recognise.
Recommendation: (a), documented as the compatibility exception.
