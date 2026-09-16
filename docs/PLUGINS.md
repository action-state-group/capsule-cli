# capsulectl plugins: the `cli-plugin/v1` contract

This document is the whole contract. A third party writing a `capsulectl`
plugin needs nothing else -- not `capsule-engine`, not any private repo. The
plugin boundary is deliberately narrow: **base verbs work against any
service, carry no account logic, and a plugin can extend the surface but can
never change what a core verb -- especially `verify` -- says.**

## What a plugin is

A plugin is a single executable file named `capsulectl-<name>`, installed by
an operator (never by `capsulectl` itself) into one of the trusted discovery
roots below. `capsulectl` discovers it, runs a handshake, and if the
handshake passes, wires `<name>` up as a top-level subcommand that execs the
launcher with the remaining CLI arguments passed through unchanged.

There is no install/enable/disable verb. The core never mutates plugin
state; putting a launcher on a trusted root, and taking it off, is entirely
an operator step outside `capsulectl`.

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

  The root's own parents are not walked -- they are system directories,
  trusted by definition -- but everything from the root down to the launcher
  is.

A launcher that fails any of these checks is silently skipped during
discovery (it never appears in `capsulectl plugin ls` and is never wired up
as a command). This is re-checked again at dispatch time, immediately before
exec, so a launcher swapped out between discovery and invocation is still
caught.

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
| `plugin_api`  | yes      | Must be exactly `"cli-plugin/v1"`. Any other value (including a newer or older version string) is a handshake failure -- there is no negotiation. |
| `subcommands` | no       | Free text list, shown in the command's `--help` as a hint; `capsulectl` does not parse or validate it. |

A launcher that does not exit zero, does not answer within the timeout,
does not print valid JSON, prints a `plugin_api` other than
`cli-plugin/v1`, or prints a `name` that does not match its own filename is
refused: it is not wired up, and discovery moves on to the next candidate.
There is no partial credit and no "wired up but broken" state.

## Dispatch

A launcher that passes discovery and the handshake is added as a top-level
`capsulectl <name>` command. `capsulectl` does not parse any flags for it:
every argument after `<name>` is passed straight through as `argv` to the
launcher, along with the parent process's stdin/stdout/stderr and an
additional environment variable, `CAPSULECTL_PLUGIN_API=cli-plugin/v1`, so a
launcher can assert at runtime which contract version invoked it.

The launcher's exit code is propagated as `capsulectl`'s own exit code.

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
something` -- a plugin is always exactly one new top-level verb, dispatched
whole to the launcher, which owns everything under it.

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
behavior beyond the handshake -- there is nothing else it will call into,
and nothing in `capsule-engine` (or any other private repository) that this
contract depends on.
