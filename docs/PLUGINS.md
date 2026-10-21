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
  one-time `chmod go-w ~/.local ~/.local/lib ~/.local/lib/capsulectl`; the
  refusal names each directory and the `chmod` that fixes it.
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
| `version`     | no       | Free text, shown in `capsulectl plugin ls`. For the first-party plugins it is the release tag, and see "Versions" below. |
| `plugin_api`  | yes      | Must be exactly `"cli-plugin/v1"`. Any other value (including a newer or older version string) is a handshake failure -- there is no negotiation. |
| `subcommands` | no       | Free text list, shown in the command's `--help` as a hint; `capsulectl` does not parse or validate it. |

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
capsulectl plugin install book|deal         # this release's first-party plugin
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
judged by where it leads. One refusal lists every failing component and the
fix (on stock Ubuntu, `chmod go-w ~/.local ~/.local/lib ~/.local/lib/capsulectl`).
From then on install and remove use only that checked canonical path: to
open the root, to run the handshake, and to inspect other roots' launchers.

**When a host loosens the chain again.** Some hosts reset these modes later:
a Muse VM replacement recreates the home as `2770`. The walk then refuses
every plugin in the per-user root. Two commands put back the `chmod go-w`
that install required: `cll checkpoint cadence`, when it finds no deal
plugin to run because the per-user one is refused, and `doctor`. They act
only when every refused component is a directory owned by the process's
user, at or under `$HOME`, not sticky, whose only fault is group or other
write; when the per-user root resolves and is a plugin root; and, as root,
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
4. A first-party plugin's version is checked (see "Versions").
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

**Install by name** (`book` or `deal`) needs a release build of the core.
- **Release tag.** A release build's version matches the release tag
  policy `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`,
  which the release workflow also enforces.
- **Download.** Install fetches `capsulectl-<tag>-plugin-<name>-<os>-<arch>`
  and `SHA256SUMS` from the core's own release, over HTTPS only (redirects
  included), with size caps.
- **Checksum.** It checks the asset against the one `SHA256SUMS` line that
  names it.
- **Provenance.** When `gh` is installed, install also runs the README's
  `gh attestation verify` against the release's attestation bundle. A
  failed verification, or a `gh` that could not run (network, trust root),
  refuses. Without `gh`, provenance is reported as not checked.

The output always lists the checks that ran. A development build refuses
the name form and gives the file-form command instead.

**Remove** deletes `capsulectl-NAME` from the per-user root if it is a
regular file. It leaves a `capsulectl-NAME.d/` presentation directory in
place, and reports which launcher, if any, dispatch would now select from
another root.

### Versions

- The handshake's `version` stays free text on the wire.
- **First-party plugins.** `book` and `deal` share core code, so their
  version is expected to equal the core's:
  - `plugin install` refuses a mismatch on a release core unless
    `--allow-version-mismatch` is given.
  - A development core does not check it.
  - Dispatch, on a release core, prints a one-line warning naming the
    launcher and how to update it, then runs the command.
  - `doctor --install-check --profile P` fails when the plugin P needs is
    missing or mismatched.

## The first-party plugins: `capsulectl-book` and `capsulectl-deal`

The core (capsule-cli#236) keeps no evidence book and no deal store, and
does not depend on the evidencebook library: an implementer needs neither to
conform. Two plugins from this repository's `plugins/` module carry them:

| Launcher | `capsulectl ...` | What it holds |
|---|---|---|
| `capsulectl-book` | `book close`, `reconcile`, `request`, `respond`, `remote list\|get\|head\|request`, `result inspect\|build`, `report build`, `backfill run\|status`, `migrate` | the evidence book behind every book profile |
| `capsulectl-deal` | `deal ...` (`init`, `open`, `check`, ..., `compose`, `request`, `respond`) | a deal profile's SQLite deal store |

They ship as two binaries, one per launcher, each answering the handshake
with the name its filename carries. A release names them
`capsulectl-<version>-plugin-book-<os>-<arch>` and
`capsulectl-<version>-plugin-deal-<os>-<arch>`, beside the core's own
`capsulectl-<version>-<os>-<arch>`, under the same `SHA256SUMS` and attestation. `capsulectl plugin install book` and `... deal` fetch
and install them; `make build` builds all three, and `make install`
installs all three.

Each plugin is a whole `capsulectl` command tree with its own commands
added, so the core can hand it a core command it cannot run itself:

- **A profile's log has one owner**, declared in the profile as `log: cll`
  or `log: book` (`profile create --log`), and never changed afterwards. A
  plain profile (mysql, sqlite, or a jsonl profile with `log: cll`) keeps a
  CLL the core writes, checkpoints and bundles with no plugin. A book profile
  (a jsonl profile with `log: book`; a jsonl profile with a log_id saved
  before the field reads as one) keeps an evidence book, and the core runs
  `publish`, `cll list|append`, `cll checkpoint create|status|publish`,
  `store init` and `bundle|disclose|permalink` on it in `capsulectl-book`,
  with the same arguments. Without the plugin those commands refuse and say
  which plugin to install.
- `bundle --deal`, `disclose --deal` and `cll checkpoint cadence` run in
  `capsulectl-deal` the same way.

This hand-over is the one place the core calls into a plugin for a core
verb, and it applies only to these two launcher names. The core finds them
as it finds any plugin, by name on a trusted root, with no vendor check: the
trusted roots' ownership and permission checks are what keep a lookalike
out. `verify` and `countersign verify` are never handed over.

The `plugins/` module builds against this checkout through `replace ../`,
so `go install .../plugins/cmd/capsulectl-book@VERSION` does not work:
build from a checkout (`make build`) or install the release assets.

### Transitional aliases

The commands that moved into the plugins keep their old names for one
release window (capsule-cli#238). Each old name runs its replacement with
the arguments unchanged, prints one line on stderr naming the replacement
and the release that removes it, and keeps the replacement's stdout and exit
code; its help says it is deprecated.

| Old | Now |
|---|---|
| `capsulectl close`, `reconcile` | `capsulectl book close`, `reconcile` |
| `capsulectl request`, `respond` | `capsulectl book request`, `respond`; on a deal (sqlite) profile `capsulectl deal request`, `respond` |
| `capsulectl compose` | `capsulectl deal compose` |
| `capsulectl book list`, `get`, `head`, `request` (another party's book, on a remote profile) | `capsulectl book remote list`, `get`, `head`, `request` |
| `capsulectl result`, `report`, `backfill` | `capsulectl book result`, `report`, `backfill` |
| `capsulectl store migrate` | `capsulectl book migrate` |

The aliases arrive with the release after v0.1.0-rc16 and are removed in
v0.2.0, once the skills, the Muse integration and the docs use only the new
names.

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
dispatched whole to the launcher, which owns everything under it. (The
first-party plugins above also receive the core verbs of their profiles.)

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
