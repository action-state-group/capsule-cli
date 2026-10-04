# The canary

A scheduled DEMO deal, and a watcher that raises the alarm from the public
witness alone. When the canary stops, nobody has to notice an error message:
the public log stops advancing, and anyone watching it sees that. Watching
needs no access to the host, the account or the agent.

## How it works

- `capsulectl --profile canary canary run` plays one scripted DEMO purchase on
  the canary profile: `deal open` (synthetic merchant Example Stickers,
  `demo: true`, never real money), `deal note --kind evidence` (the binary
  version and the skill file's sha256), `deal check` (it passes, so the
  standing intent's approval is sealed with it), `deal report`, and finally
  `deal tick`. **It ticks only if every step before it succeeded.** Any step
  that fails stops the run with one line naming the verb and its message, and
  nothing is published.
- A deal profile's cadence log grows by one entry per tick, whether or not
  anything happened (that is what keeps a witness from learning how many deals
  there are). So the canary needs **a profile of its own whose only ticker is
  `canary run`**. On a profile that another timer also ticks, the log would keep
  advancing while every canary deal fails, and nothing could be seen.
- `capsulectl canary watch --log-id <cadence log> --witness <url> --expect-every <duration>`
  reads the witness's last checkpoint for that log
  (`GET <witness>/checkpoints/<log id>`, read-only) and compares it with the
  previous observation, kept in a small state file it owns. It exits 0 while
  the log advances, and with exit code 6 and one plain line when:
  - the log has not advanced for longer than `--expect-every` (by the
    checkpoint's time, or because its size has not changed since this watcher
    first saw it);
  - the history was rewritten: the size went back; the same size has a
    different root; the checkpoint that directly follows the last one seen
    names a different root for it; the checkpoints are signed by a different
    key; or the witness has flagged conflicting checkpoints for the log;
  - the log was never witnessed at all.
- A witness it cannot read at all is not an alarm: `watch` exits 1 and says it
  cannot tell. Run it from more than one place if that matters.
- Between two polls the log usually moves by more than one checkpoint. `watch`
  checks the direct link when there is one; across a gap it relies on the
  witness, which refuses a checkpoint that does not chain from the last one it
  accepted for the log.

## The expected time

`--expect-every` is the canary's schedule, plus the cadence interval and its
jitter (default 1h ± 10m), plus slack for a slow witness. A tick is due only
once per interval, so a run that comes before it is due publishes nothing new
(`"tick": "not_due"`).

With the canary run once a day and the default cadence, every run is long past
due and ticks at once: `--expect-every 26h` alarms when a day's run did not
reach the witness. Running it more often than the cadence interval only adds
`not_due` runs.

Every deal stays in every tick's tree (up to 65535), so once a day is the
sensible schedule; hourly would make each tick slower year by year.

## What it catches

The five things that broke on 2026-10-04, and how each one ends up as an alarm:

| What broke | What the canary does | Caught? |
|---|---|---|
| A stale skill backup path: the agent loaded a skill file that was no longer there | `--skill` names the SKILL.md the agent loads. A missing or unreadable file stops the run before anything is sealed, so the log stalls. | Yes, if `--skill` is the same path the agent uses. |
| A profile setting that made `deal open` fail closed | The run stops at `deal open` with its message, before the tick. | Yes, when the canary profile carries the same setting (make it from the same template, and change both together). A setting only the agent's profile has is not seen. |
| A verb failing with an opaque message | The run stops at that verb and prints `canary: \`deal <verb>\` failed: <message>; stopped before the tick` on the host, and the log stalls. | Yes: the alarm comes from outside, the line on the host names the verb. |
| The egress consent prompt reverting to asking on every call | A run that waits on a prompt nobody answers never ticks. | Only if the canary is started **through the agent** (its scheduled task runs `canary run` through the same tool path). Run straight from cron, the canary bypasses the agent and does not see this. |
| The binary or the skill drifting behind the release tag | `--expect-version <tag>` and `--expect-skill-sha256 <digest of the tag's SKILL.md>` stop the run before anything is sealed when either differs. Every run also seals the version and the digest (in the evidence note, as a commitment, like every value a deal record holds) and prints them on the host. | Yes, in `canary run`. `watch` cannot compare them itself: the witness holds only the log's roots, by design. |

## Setting it up (the host is the operator's)

```bash
# A profile of its own, witnessed by default; nothing else ticks it.
capsulectl deal init --profile canary --dir ~/.local/share/capsule/canary
# The cadence log id to watch from outside:
capsulectl --profile canary canary run --skill /path/to/the/agent/skills/deal/SKILL.md | jq -r .cadence_log
```

A cron line for the host (once a day, at an odd minute):

```cron
17 6 * * * capsulectl --profile canary canary run --skill /path/to/the/agent/skills/deal/SKILL.md --expect-version v0.1.0 --expect-skill-sha256 <sha256 of that tag's SKILL.md> >>"$HOME/.local/state/capsule-canary.log" 2>&1
```

To catch the consent prompt too, have the agent's own scheduler start the same
command instead of cron.

Watching needs no credentials: the witness answers anyone. Run `watch` from
somewhere that is not the host, for example GitHub Actions on a schedule
(`examples/canary-watch.yml`), or any other machine:

```bash
capsulectl canary watch --log-id deal-cadence/<16 hex> \
  --witness https://witness.agentactioncapsule.org --expect-every 26h
```
