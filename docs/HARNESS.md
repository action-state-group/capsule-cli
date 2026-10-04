# A test harness for deal installs

A few disposable test accounts on an agent host, each installed fresh from the
published install page and given the same small task every day, with the
verdict taken from outside: from the public witness and from the receipt the
deal itself sends. Nothing here signs in to an account, operates one or spends
money; the operator does that, and the harness makes each day's run mechanical
and its verdict external.

## 1. The install check

Right after installing, before the first deal:

```bash
capsulectl doctor --install-check --profile deal \
  --expect-version v0.1.0-rc3 --expect-commit <sha> \
  --expect-skill-sha256 <sha256 of the release's skills/deal/SKILL.md> \
  --skills-dir <the directory the agent loads skills from>
```

It prints one JSON object on one line (`install_check`), which the run seals as
evidence, and exits 3 naming every issue when:

- the binary is not the expected release (version, and commit when given);
- there is not exactly one skill named `deal` under the skills directory. A
  backup copy left beside the installed one (`deal.bak-*/SKILL.md`) counts as a
  second one: the agent may load either;
- the deal skill is not the release's (`--expect-skill-sha256`);
- the deal profile lacks a witness endpoint, or has one without the witness's
  public key, which makes `deal open` fail closed.

## 2. The verdict from outside, in two halves

A deal profile publishes on a fixed cadence: its cadence log grows by one entry
every tick (hourly with jitter by default), whether or not any deal happened.
That keeps a witness from learning how many deals there are, and it means the
witness can show that an install is alive, but never whether a given deal took
place. Per-deal logs (`deal/<id>`) are never published.

**Liveness, from the witness.** `capsulectl canary watch` per account, against
the account's cadence log id (`deal tick` prints it as `cadence_log`). A
scheduled GitHub Actions job (`examples/deal-harness-watch.yml`, no secrets)
runs it for every account, prints one verdict per account in the run summary,
and fails the run when any log stopped advancing or was rewritten. It catches
whatever stops an install from ticking or publishing: a missing timer, a
profile that cannot sign or reach its witness, network consent withdrawn from
the tick.

**The deal, from its receipt.** The daily deal ends with the receipt email
(`deal report --email`), which carries `bundle.json`. A tick after the deal,
the receipt chains the deal's steps to a witnessed cadence checkpoint. The
operator saves each day's `bundle.json`, and:

```bash
capsulectl verify --bundle bundle.json --witness-directory witnesses.json
jq -e '[.disclosures[].agent_input.body | select(.choice? == "hold")] | length > 0' bundle.json
```

A receipt that verifies, is witnessed and holds a sealed Hold is a pass. **No
receipt for the day is the alarm**: the deal never reached its end, whatever
the reason. This half is the only place a deal that never started can show.

## 3. What catches what

The six failures seen on 2026-10-03/04:

| Failure | Caught by | Notes |
|---|---|---|
| A stale skill path: a backup copy of the skill left in the skills directory | Install check (more than one `deal` skill) | Only when `--skills-dir` is the directory the agent actually loads. |
| A profile setting making `deal open` fail closed (endpoint without key) | Install check; then no receipt; and, when ticks fail too, the witness | |
| A verb failing with an opaque message | No receipt (the deal never ends) | Detected, not explained: the message itself is only in the conversation. |
| Egress consent reverting to asking on every call | The witness (a tick whose delivery waits on consent never publishes); no receipt | Only if nobody answers the prompt: an operator who clicks through it hides it. The run's paste text must not approve network access. |
| The binary or the skill behind the release tag | Install check (`--expect-version`, `--expect-commit`, `--expect-skill-sha256`) | The release's SKILL.md digest has to be taken from the tag, not from the account. |
| The standing rule ceasing to fire (the agent never starts a deal) | No receipt | Only because the daily ask does not name the skill. The witness cannot see it: ticks go on without deals. |

## 4. Limits

- **A few accounts detect breakage, not prevalence.** Five green accounts say
  the install path works; they say nothing about how often it fails for users.
- **One account stays pinned to an older release, as the control.** When the
  pinned account breaks too, the host changed; when only the current ones
  break, we did.
- **Test accounts never share a mailbox or a card with a real account.** The
  receipt's email evidence and any bank-feed leg would otherwise mix two
  people's records.
- The witness half needs nothing from the accounts. The receipt half needs the
  operator to save each day's receipt; an unsaved receipt reads as a missing
  one.
