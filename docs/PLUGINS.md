# capsulectl plugins: the `cli-plugin/v1` contract

This document is the whole contract. A third party writing a `capsulectl`
plugin needs nothing else -- not `capsule-engine`, not any other repository. The
plugin boundary is deliberately narrow: **base verbs work against any
service, carry no account logic, and a plugin can extend the surface but can
never change what a core verb -- especially `verify` -- says.**

## What a plugin is

A plugin is a single executable file named `capsulectl-<name>` on one of the
trusted discovery roots below. `capsulectl` discovers it, runs a handshake,
and if the handshake passes, wires `<name>` up as a top-level subcommand that
execs the launcher with the remaining CLI arguments passed through unchanged.

A launcher gets there by an operator copying it onto a root, or by
`capsulectl plugin install` (see "Installing plugins" below), which places it
on the per-user root. There is no enable/disable state: a launcher on a
trusted root is the whole of a plugin's installation.

## Discovery roots

`capsulectl` scans, in order:

1. `/usr/local/lib/capsulectl/plugins`
2. `$HOME/.local/lib/capsulectl/plugins`

An earlier root shadows a later one: if both roots contain a launcher for
the same name, the one in `/usr/local/lib/capsulectl/plugins` wins and the
other is never even handshaked.

Both roots can be replaced entirely by setting `CAPSULECTL_PLUGIN_ROOTS` to
one or more paths, separated by the platform's `PATH` list separator (`:` on
Unix). This is for tests and unusual deployments; it replaces the default
roots, it does not add to them.

## Trust: what a launcher must look like before it is even handshaked

Before `capsulectl` will run `cli-plugin-metadata` against a candidate, the
launcher's path must pass every one of these checks (Docker's plugin
discovery model is the reference):

- It resolves (after following symlinks) to a path **inside** one of the
  trusted roots. A symlink cannot be used to redirect discovery to a target
  outside the roots.
- The resolved target is a regular file.
- Walking from the resolved file up to (and including) the trusted root it
  matched, every directory and the file itself is:
  - owned by the current user or by root, and
  - not group-writable and not other-writable.

  The system root's parents are not walked: they are system directories.
  The per-user root's are, because `~/.local` can be group-writable or a
  symlink into a shared directory. Every directory a name is looked up in
  while `$HOME/.local/lib/capsulectl/plugins` resolves from `$HOME` is
  checked with the same rule, including those inside each symlink's target
  (every hop of a chain), along with the resolved root and the parents of
  each up to `$HOME` (up to `/` for one outside `$HOME`; a root-owned sticky
  directory such as `/tmp` is accepted there). Each symlink followed must
  be owned by the user or root, since in a sticky directory a link stays its
  owner's to repoint. `$HOME` itself is checked; its parents, and the
  directories a symlinked `$HOME` passes through, are not: they are the
  account's, as the system root's parents are the system's. No group
  is exempt, not even the user's own: a group's name or id says nothing
  about who else belongs to it. On stock Ubuntu (umask 002) that means a
  one-time `chmod go-w ~/.local ~/.local/lib ~/.local/lib/capsulectl`, which
  `plugin install` does itself (below); a refusal names each directory and
  the `chmod` that fixes it.
- The symlink hops on the launcher's own path below its root are checked
  the same way. The walk above follows only the resolved path, so a
  directory holding a hop off that path (`R/capsulectl-x` -> `R/a/link` ->
  `R/b/x` leaves `R/a` off it) is checked here: every directory a name is
  looked up in from the root down, and the owner of every link followed.
  Inside the root the strict rule applies; outside it (a hop through
  another directory), a root-owned sticky directory is accepted.

A launcher that fails any of these checks is skipped during discovery: it is
never wired up as a command, and `capsulectl plugin ls` lists it under
`refused` with the reason. This is re-checked again at dispatch time, immediately before
exec, so a launcher swapped out between discovery and invocation is still
caught.

**What runs is what was checked.** The handshake, dispatch and the rules
checker execute (or read) the resolved path the checks judged, never the
name the launcher was found by. A symlink on that name swapped after the
check reaches nothing that runs. argv[0] is still the name it was found by;
a shell-script launcher's `$0`, which is the path the kernel executed, is
the resolved path.

## The handshake

`capsulectl` runs `<launcher> cli-plugin-metadata` with a 3-second timeout
and expects exactly one line of JSON on stdout:

```json
{
  "name": "example",
  "vendor": "Example Org",
  "version": "1.2.3",
  "plugin_api": "cli-plugin/v1",
  "subcommands": ["do-a-thing", "do-another-thing"]
}
```

| Field         | Required | Meaning                                                            |
|---------------|----------|---------------------------------------------------------------------|
| `name`        | yes      | Must equal the launcher's filename with the `capsulectl-` prefix stripped. A mismatch is a handshake failure. |
| `vendor`      | no       | Free text, shown in `capsulectl plugin ls` and in the command's `--help`. |
| `version`     | no       | Free text, shown in `capsulectl plugin ls`. |
| `core_version` | no      | Free text. The capsulectl release a plugin that embeds the core's code was built against, which the version check below compares. A plugin without it is not checked, and its `version` is never read as one. |
| `plugin_api`  | yes      | Must be exactly `"cli-plugin/v1"`. Any other value (including a newer or older version string) is a handshake failure -- there is no negotiation. |
| `subcommands` | no       | Free text list, shown in the command's `--help` as a hint; `capsulectl` does not parse or validate it. |
| `wording`     | no       | The plugin's own built-in wording pack, an `aac.wording-pack/v0` object in the core's locale that holds no key of the core's pack. `profile update --wording` and `doctor` read it for a profile whose log the plugin keeps (see below); discovery and dispatch ignore it. A plugin built on the host package gets it from `cli.RegisterWording`. |

A launcher that does not exit zero, does not answer within the timeout,
does not print valid JSON, prints a `plugin_api` other than
`cli-plugin/v1`, or prints a `name` that does not match its own filename is
refused: it is not wired up, `capsulectl plugin ls` lists it under `refused`
with the reason, and discovery moves on to the next candidate. Discovery
deduplicates on the filename, so a refused launcher never shadows a valid one
of the same name on a later root.
There is no partial credit and no "wired up but broken" state.

## Dispatch

A launcher that passes discovery and the handshake is added as a top-level
`capsulectl <name>` command. `capsulectl` does not parse any flags for it:
every argument after `<name>` is passed straight through as `argv` to the
launcher, along with the parent process's stdin/stdout/stderr and an
additional environment variable, `CAPSULECTL_PLUGIN_API=cli-plugin/v1`, so a
launcher can assert at runtime which contract version invoked it, and
`CAPSULECTL_BIN`, the path of the `capsulectl` that invoked it (symlinks
resolved), unless the caller's environment already sets it to a value. A launcher that
calls core back uses that path rather than a bare `capsulectl`, which is not
on PATH on every host (a Muse chat never has `~/.local/bin` on it).

The launcher's exit code is propagated as `capsulectl`'s own exit code, and
`capsulectl` prints nothing of its own after a launcher that failed: the
launcher has said why on stderr.

### What a command tells the user: `voice` and `say`

Every result object a command prints carries, beside its own fields, what
an agent says about it (capsule-cli#345). A plugin's commands declare it
the same way, in their own output; `capsulectl` passes a plugin's stdout
through and adds nothing to it.

- `voice`: `silent` (on the record, say nothing), `line` (say `say`), `ask`
  (a card: `ask` holds its tag, question, facts, reasons and options; `say`
  is the same card as text), `report` (say `say`; `attach` names the page to
  send after it) or `error` (say `say` and stop; `hold` is `true`).
- `say`: one string, lines joined by `\n`, spoken verbatim. It never holds
  an id, a digest, a rule id, a flag or a UTC time.
- Words come from a wording pack in the published `aac.wording-pack/v0`
  format: the built-in packs, or the complete pack a profile pins with
  `profile update --wording FILE`. The built-in words are the core's neutral
  pack plus the plugin's own pack, which a plugin adds to its process with
  `cli.RegisterWording` (once, before anything speaks) and which its
  handshake carries as `wording`. A pinned pack holds every key of the
  core's pack and, for a profile whose log a plugin keeps, every key of that
  plugin's pack: one pack is never mixed with another. A pinned pack that is
  missing, changed or incomplete, or whose `card.terms.*` entries are blank
  or don't use exactly the built-in placeholders (so a card could hide a term
  a yes agrees to), whichever pack holds them, is not used: the whole
  built-in pack speaks, the result carries `"wording": "built-in"` and
  `doctor` names the problem. `profile update --wording` refuses such a pack
  up front, checking the plugin's keys from its handshake, and refuses to
  pin when that plugin is not installed. `doctor` checks a pin the same way;
  with the plugin gone it reports the pack the core's commands still speak
  and a `notice` that the plugin's keys were not checked.
- A failed command that printed nothing gets one error object on stdout
  from the binary that ran it, after the usual stderr line, with the same
  exit code. `capsulectl` never writes one for a launcher that ran. A
  result object with no `voice` comes from a plugin that predates this
  contract.

`--profile` reaches a launcher wherever it is written. Given after the
plugin's name, it is one of the launcher's own arguments. Given before it
(`capsulectl --profile a deal list`), it is passed through too, ahead of the
remaining arguments, and the profile it names is also set as
`CAPSULECTL_PROFILE` in the launcher's environment. A core command the
launcher runs back without `--profile` uses `CAPSULECTL_PROFILE`, so it acts on
the same profile; a `--profile` it does pass wins. When `--profile` is given
both before and after the plugin's name, the later one wins, as it does for
any repeated flag.

## Installing plugins

```
capsulectl plugin install ./capsulectl-x    # a launcher file (an operand containing /)
capsulectl plugin remove NAME
```

Both manage only the per-user root, `$HOME/.local/lib/capsulectl/plugins`. A
system-wide plugin stays an operator step. Windows is out of scope: plugins,
their trust checks and these commands are Unix-only.

**Who may run them.** They refuse to run through `sudo` (`SUDO_UID` set) or
as anyone other than the owner of `$HOME`. Root installing into its own home
is fine. For these two commands `capsulectl` skips plugin discovery, so no
installed plugin runs before their checks. With `CAPSULECTL_PLUGIN_ROOTS`
set, the list must contain the per-user root.

**The path check.** Install also runs the trust walk's per-user root
check above, and checks more. Before it writes or runs anything, it resolves
the path once and checks every component of the resolved path from `/`
down: each must be owned by the user or root and neither group- nor
other-writable. The only exception is a root-owned directory with the
sticky bit set (`/tmp`), as in the per-user root check, never for the walk
from a launcher up to its root. A symlinked `~/.local` is
judged by where it leads. Before checking, install heals the path as the
cadence and `doctor` do (below), over the existing part of the path from `/`
and every directory the walk looks a name up in so far; if any of them
cannot be healed, it changes none. Its output then carries
`plugin_root_healed`, in `doctor`'s `user_root` shape, naming each directory
with its mode before and after. Anything the heal leaves is refused: one
refusal lists every failing component and the fix, why the heal did not
apply, each chmod that failed and why, and which directories install had
already tightened, since those stay so (as when install fails later for
another reason).
From then on install and remove use only that checked canonical path: to
open the root, to run the handshake, and to inspect other roots' launchers.

**When a host loosens the chain again.** Some hosts reset these modes later:
a Muse VM replacement recreates the home as `2770`. The walk then refuses
every plugin in the per-user root. Two commands put back the `chmod go-w`
that install required: `cll checkpoint cadence`, when it finds no deal
plugin to run because the per-user one is refused, and `doctor`. They (and
`plugin install`, above) act
only when every refused component is a directory owned by the process's
user, at or under `$HOME`, not sticky, whose only fault is group or other
write; when the per-user root resolves and is a plugin root (install,
which creates the root, needs only the existing part of its path); and, as
root,
when `$HOME` is at least two levels below `/`. A setgid directory heals
only when the process is root or in the directory's group, since otherwise
the kernel drops setgid on chmod. They open each directory without
following a link at any level and recheck it before changing it, and
remove only the write bits. If the kernel still drops setgid (a root
without `CAP_FSETID`), the line lists that directory under
`setgid_dropped`: files created in it then take the creator's group, not
the directory's. Anything else stays refused as before. The walk itself is
unchanged and still judges every plugin run.

The cadence writes one JSON line to its log before it runs the tick:
`plugin_root_healed` names each directory with its mode before and after,
or `plugin_root_heal_failed`. When the deal plugin was not run, it writes
`plugin_unavailable` (not found, or refused). When it was started and then
killed, or could not be started, it writes `plugin_stopped`, and that tick
may have done part of its work. Each carries the message stderr shows.
`doctor` reports the outcome as `plugins.user_root`,
and `doctor --install-check` as `user_root`: `ok`, `absent`, `healed` or
`refused`. A refusal there adds no issue of its own; the plugin check
fails when the profile's plugin is the one refused.

**Install from a file.**

1. Missing directories of the root are created one at a time, 0755.
2. The file is copied into the root under a temporary name discovery never
   runs (`.install-*`), and its handshake runs.
3. The name it reports must match `^[a-z][a-z0-9-]{0,62}$` and must not
   be a core command.
4. A plugin that declares a `core_version` has it checked (see "Versions").
5. If an earlier root holds a launcher of that name that discovery would
   accept, install refuses (exit 5), because that one would still run. A
   launcher there that sits on an unsafe path is never run, and is reported
   as a possible shadow. One the walk refuses does not shadow.
6. The temporary file is renamed over `capsulectl-<name>`. The rename is
   the commit point. If the directory cannot be synced afterwards, install
   says so ("durability not confirmed") rather than reporting a refusal.

Any refusal before the rename leaves the destination unchanged and removes
the temporary file. A killed install can leave a `.install-*` file behind.
It is inert, and `plugin ls` lists it under `interrupted_installs`.

**No install by name.** A plugin ships from its own release, which the
core's messages do not name: `plugin install NAME` refuses, and says to
install the plugin file from its release with the file form.

**Remove** deletes `capsulectl-NAME` from the per-user root if it is a
regular file. It leaves a `capsulectl-NAME.d/` presentation directory in
place, and reports which launcher, if any, dispatch would now select from
another root.

### Versions

- The handshake's `version` stays free text on the wire.
- **Plugins that embed the core.** A plugin built from this repository's host
  package carries the core release it embeds, its `core_version`, which is
  expected to equal the core's. A plugin that declares none is not checked:
  - `plugin install` refuses a mismatch on a release core unless
    `--allow-version-mismatch` is given.
  - A development core does not check it.
  - Dispatch, on a release core, prints a one-line warning naming the
    launcher and how to update it, then runs the command.
  - `doctor --install-check --profile P` fails when the plugin P needs is
    missing or mismatched.

## Plugins that keep a profile's log

The core (capsule-cli#236, #428) keeps no evidence book and no deal store, and
does not depend on the evidencebook library: an implementer needs neither to
conform. A plugin keeps such a log, built and released from its own
repository (capsule-cli#326): the evidence book plugin
([evidencebook](https://github.com/action-state-group/evidencebook), the
nested module `capsulectl/`) and the deal plugin
([evidencebook-deal](https://github.com/action-state-group/evidencebook-deal))
are the ones the company ships. Each release publishes the plugin's binaries,
`SHA256SUMS` and a build attestation from that repository's workflow. A
binary is built against one capsulectl release, which its handshake reports as
`core_version` (see "Versions"). Download the one built for your capsulectl,
check it, and install the downloaded file, such as `capsulectl plugin install
./capsulectl-<name>-<version>-<os>-<arch>`. Install names the plugin by its
handshake, not by the file name.

A plugin is a whole `capsulectl` command tree with its own commands
added, so the core can hand it a core command it cannot run itself:

- **A profile's log has one owner**, declared in the profile as `log: cll`
  or `log: <plugin>` (`profile create --log`), and never changed afterwards;
  a jsonl profile with a `log_id` must say which. The plugin name is any plugin name that is not a core
  command; a jsonl profile with a `log_id` (a log the plugin keeps itself) or
  a sqlite profile with a namespace (a store the plugin writes and the core
  only reads) can name one. A plain profile (mysql, a sqlite profile that
  names no plugin, or a jsonl profile with `log: cll`) keeps a CLL the core
  writes, checkpoints and bundles with no plugin. When a plugin keeps a
  jsonl profile's log, the core runs `publish`, `cll list|append`,
  `cll checkpoint create|status|publish`, `store init` and
  `bundle|disclose|permalink` on it in the plugin the profile names, with the
  same arguments. A profile saved before the field, with a `log_id` and no
  `log:`, has no declared owner: its commands stop with the one-command fix,
  `capsulectl profile update --profile NAME --log <cll or the plugin>`. A
  plugin serves its own profiles in its own process by registering a
  `LogBackend` for its name (`cli.RegisterLogBackend`). Without the plugin
  those commands refuse and say which plugin to install.
- `bundle`, `disclose` and `permalink` read no flag before they know whose
  log the selected profile has: if a plugin keeps it and this process does not
  serve it, the whole command line goes to that plugin, so the plugin's own
  flags reach it. A sqlite profile with a namespace can name its plugin in
  `log:` too (the plugin writes that store; the core only reads it). In its
  own process a plugin adds those flags with `cli.ExtendBundle` and mounts a
  command under a core one with `cli.GraftCommand`.

  These two are a Go API of the host package, outside `cli-plugin/v1`. A
  `BundleExtension`'s `Flags` must skip a flag the command already defines
  because defining one twice panics. Its `Run` runs
  after the flags are parsed and before the command's own checks, and the
  first extension that reports the command handled ends it. `GraftCommand`
  takes a function that builds the command, called for each tree the process
  builds (a command keeps the flag values of its last run), replaces a child
  of the same name, needs a non-empty path of core commands,
  and panics on a path that names none. `bundle --profile <p> --help` shows
  the core's help, without a plugin's flags.

- **A store a plugin owns is written by that plugin only.** The plugin opens
  it with `cli.OpenOwnedTarget`, passing a `PluginStoreWriter` that names the
  plugin and reports whether it still holds the store's writer lock (a store
  marked for another plugin is refused before anything is written), and records its ownership
  with `Target.MarkOwned` (or `cli.MarkStoreOwned` on a sqlite handle it
  already holds) on its writable paths (never on a read). A store
  carrying that marker, or the older deal store marker, is refused by every
  core write verb whatever the profile says; the core reads it. The older
  marker records no owner, so it only refuses writes (and a `bundle`, which
  would read the store without the owner's rules for what a copy may carry); it is the one
  compatibility exception to the core knowing no plugin's stored data, and not
  a precedent for a second. `profile create` and
  `update` refuse a `log:` that disagrees with the owner the store records;
  `update` may bring a profile in line with it.

This hand-over is the one place the core calls into a plugin for a core
verb, and it goes only to the plugin the selected profile names. The core finds it
as it finds any plugin, by name on a trusted root, with no vendor check: the
trusted roots' ownership and permission checks are what keep a lookalike
out. `verify` and `countersign verify` are never handed over.

A plugin builds against capsule-cli's host package,
`github.com/action-state-group/capsule-cli/cli`, required at a capsulectl
release: build it in its own repository, or install its release assets.

## Reserved verbs

A plugin can never shadow a core verb. `capsulectl`'s reserved names are:

- every core top-level command: `profile`, `key`, `bundle`, `disclose`,
  `permalink`, `countersign`, `store`, `seal`, `get`, `verify`, `publish`,
  `cll`, `plugin`
- cobra's built-ins: `help`, `completion`

If a discovered, handshake-valid launcher's name collides with any of these,
it is silently dropped from dispatch (it still appears, harmlessly, in
`capsulectl plugin ls`, since `ls` only reports what discovery found -- it
does not apply the reservation filter). A launcher whose name contains
whitespace is also dropped, since cobra would otherwise derive its command
name from the first whitespace-delimited token and could collide with a
reserved name that way.

There is also no per-subcommand reservation below the top level: a plugin
cannot register `capsulectl countersign something` or `capsulectl bundle
something` -- a third-party plugin is always exactly one new top-level verb,
dispatched whole to the launcher, which owns everything under it. (A plugin that keeps a profile's log also receives the core verbs of its
profiles.)

## The rule that matters most: a plugin may never change what `verify` says

**`capsulectl verify` and `capsulectl countersign verify` are core verbs.
No plugin mechanism can intercept, wrap, override, or post-process their
output.** There is no hook, filter, or middleware point in the dispatch path
above -- a plugin is reachable only as its own separate top-level command
name, and that name is refused outright if it collides with `verify` (or
any other reserved name). A plugin cannot make `verify` print something a
plugin computed, cannot suppress a `verify` finding, and cannot inject a
finding of its own into `verify`'s output.

This is a boundary, not an oversight: the base verbs (`bundle`, `verify`,
`countersign request`, `countersign verify`, and everything else in this
CLI) work against **any** service and carry **no account logic**. Anything
that needs account-specific behavior, a private verification policy, or a
company-specific check belongs in the plugin's own top-level verb, which a
caller invokes explicitly and which is visibly separate from the neutral
core's verdict -- never folded into it. A relying party who runs
`capsulectl verify` or `capsulectl countersign verify` is reading this
repository's code and nothing else, regardless of which plugins happen to
be installed on the machine that ran it.

## Writing a plugin without reading `capsule-engine`

Everything above is the whole contract: name your executable
`capsulectl-<name>`, put it on a trusted root with sane permissions, answer
`cli-plugin-metadata` with `plugin_api: "cli-plugin/v1"` and a `name`
matching your filename, and implement whatever subcommands you want under
that one top-level verb. `capsulectl` never inspects your plugin's
behavior beyond the handshake -- for a third-party plugin there is nothing
else it will call into, and nothing in `capsule-engine` (or any other
repository) that this contract depends on.
