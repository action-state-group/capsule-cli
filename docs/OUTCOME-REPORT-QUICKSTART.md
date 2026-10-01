# Zero to a verified outcome report

One binary (`capsulectl`), a pack a person can edit and recompile, and a
scheduled skill (`daily-judge-and-close`) that judges a day's recorded
conversations against that pack's rubric and seals the result. The output, an
**outcome report**, reaches you two ways over the same sealed bytes: a
self-contained **`report.html`** you open offline in any browser, and a
**permalink** (the bundle lives in the URL fragment and is recomputed in your
browser; nothing server-side to trust). This page builds both from nothing,
using only public GitHub branches.

## Current status: pre-release (read this before you start)

Every command below runs today, on the open pull requests named, but **none of
it is a released artefact yet**:

- `capsulectl`'s `result build` / `report build` verbs are
  [capsule-cli PR #19](https://github.com/action-state-group/capsule-cli/pull/19)
  (`desk/result-report-verbs`, open). The outcome-report card, and
  `disclose --attach-input-originals`, are added on top of it by
  [capsule-cli PR #30](https://github.com/action-state-group/capsule-cli/pull/30)
  (`desk/m3-report-cards`, stacked on PR #19, also open). No `v*` tag contains
  either PR.
- The pack compiler, the `airline-support-outcomes` rubric pack, and the
  `daily-judge-and-close` / `rollup_day.py` skill scripts are
  [evidencebook-skills PR #7](https://github.com/action-state-group/evidencebook-skills/pull/7)
  (`desk/m3-adjudicator`, open). That repo has **no tags and no release
  process**; its CI is a neutrality scan only.
- The outcome-report card's TypeScript source
  (`agent-action-capsule`'s `ts/src/outcome-report.ts` and
  `outcome-report-view.ts`) is
  [agent-action-capsule PR #160](https://github.com/action-state-group/agent-action-capsule/pull/160)
  (open). You do **not** need to clone or build it:
  PR #30 already vendors the card's compiled bundle into
  `internal/cli/assets/evidence-graph.iife.js`. It is named here only for
  readers who want the card's source.

"[What a release must contain](#what-a-release-must-contain)" at the end of
this page says what closes this gap. Until then, checking out these branches
*is* the product.

## Prerequisites

`go` (the version in `capsule-cli/go.mod`), `python3` with `venv`, `git`, `jq`.
The only non-stdlib Python package is `jevals`, which step 2 installs into its
own virtualenv.

## 1. Install `capsulectl`

No released binary has the verbs this page needs, so build PR #30's branch,
which carries PR #19 and the vendored outcome-report card:

```console
$ git clone https://github.com/action-state-group/capsule-cli.git
$ cd capsule-cli
$ git fetch origin pull/30/head:m3-report-cards && git checkout m3-report-cards
$ go build -o capsulectl ./cmd/capsulectl
$ ./capsulectl --help | grep -E 'result|report'
  report      Render a verified Result-root Evidence Bundle into an offline report
  result      Inspect a Result v0 document, or seal one into the book
```

Put `capsulectl` on your `PATH` now: every command from step 4 on calls it
bare (`cp capsulectl ~/.local/bin/`, or add this directory to `PATH`). Then
leave this directory; `evidencebook-skills` is cloned as `capsule-cli`'s
**sibling**, not inside it:

```console
$ cd ..
```

Steps 2 onward all run from inside the `evidencebook-skills` checkout the next
step creates. Stay there: `capsulectl profile create` records its key-file
paths exactly as given, so generating keys in one directory and sealing from
another breaks the profile (seal fails with "invalid input or profile
configuration").

## 2. Get the skill and pack

`evidencebook-skills` carries the pack compiler, the `airline-support-outcomes`
pack source, and the `run_daily.py` / `rollup_day.py` skill scripts. No tag
exists; check out the open PR branch:

```console
$ git clone https://github.com/action-state-group/evidencebook-skills.git
$ cd evidencebook-skills
$ git fetch origin pull/7/head:m3-adjudicator && git checkout m3-adjudicator
$ python3 -m venv .venv && . .venv/bin/activate
$ pip install --quiet jevals
```

`jevals` is how `scripts/judges/jev_judge.py` talks to Jev, and how the mock
backend below stands in for it with no key and no network call.

A person who wants different criteria edits
`packs/airline-support-outcomes/pack-source.yaml` (tier, wording, the
`not_applicable_verdict` / `within_fare_rules_recomputed` /
`done_in_full_refusal_aware` switches, the report template) and recompiles it.
`--out` names the `demo/<pack>` directory the compiler writes into:

```console
$ python3 scripts/pack_compile.py compile packs/airline-support-outcomes/pack-source.yaml \
    --out demo/airline-support-outcomes
{"valid": true, "pack_id": "airline-support-outcomes", "pack_version": "1.4.0", ...,
 "files": ["demo/airline-support-outcomes/compiled.json", "demo/airline-support-outcomes/axes.json",
           "demo/airline-support-outcomes/judge-prompt.md", "demo/airline-support-outcomes/presentation.json"]}
```

Recompiling the unmodified pack source reproduces the committed `demo/`
files byte for byte. That is the whole edit-and-rerun loop.

## 3. Set the Jev key (real runs only)

Skip this step for the mock path below. For a real run against Jev, set the
key and do **not** set `JEV_JUDGE_BACKEND`:

```console
$ export TYPESAFE_API_KEY=<your-jev-api-key>   # from Jev onboarding; never commit it
```

and pass `--judge-model-id jev-1.13.0` exactly in steps 6 and 7.
`jev_judge.py` pins the model itself and refuses a `--judge-model-id` that
disagrees, so a report never seals under a pin that misstates which model
judged it.

## 4. Set up a book

Still inside `evidencebook-skills`; every path below is relative to it.

```console
$ capsulectl key generate --output producer.seed > producer.json
$ capsulectl key generate --output checkpoint.seed > checkpoint.json
$ capsulectl profile create --name tau2 --type jsonl --jsonl-path ./store \
    --namespace tau2 --log-id tau2-airline --operator tau2-demo \
    --signing-key-file producer.seed --trusted-key "$(jq -r .public_key producer.json)" \
    --checkpoint-signing-key-file checkpoint.seed --checkpoint-trusted-key "$(jq -r .public_key checkpoint.json)"
$ capsulectl store init --profile tau2
```

## 5. Seal the 50 tau2 scenarios

This uses the full 50-case trial-0 set from the shipped `claude-3-7-sonnet`
tau2 airline results file. A real producer seals its own agent interactions
here instead. `capsulectl seal` writes a sealed artifact; `tests/bookfixture`
(this repo's test fixture) places the sealed cases in the book at an exact
time, which is how step 6's `--date` finds them. It is its own Go module, so
build it from inside its directory:

```console
$ (cd tests/bookfixture && go build -o ../../bookfixture .)
$ python3 demo/backfill/backfill.py \
    --results airline-data/claude-3-7-sonnet-20250219_airline_default_gpt-4.1-2025-04-14_4trials.json \
    --out ./requests
$ mkdir -p ./cases
$ for request in $(ls ./requests/task-*.json | sort | head -n 50); do
    capsulectl seal --profile tau2 --request "$request" --output "./cases/$(basename "$request")"
  done
$ DAY=$(python3 -c 'import datetime as d; t=d.date.today(); m=t-d.timedelta(days=t.weekday()+7); print(m+d.timedelta(days=2))')
$ ./bookfixture seed --store ./store --log-id tau2-airline --operator tau2-demo --namespace tau2 \
    --signing-key-file producer.seed --checkpoint-key-file checkpoint.seed \
    --at "${DAY}T12:00:00Z" ./cases/*.json
$ capsulectl cll list --profile tau2 --limit 1000 | jq '[.entries[] | select(.record_type=="published_capsule")] | length'
50
```

`$DAY` is the previous ISO week's Wednesday: a day that has ended.

## 6. Run `daily-judge-and-close`

Nine criteria per case (450 reports), one judge request per conversation
(`judge_batch: true`). `--date` must be the `$DAY` step 5 seeded. Mock
backend, no key, no network call:

```console
$ JEV_JUDGE_BACKEND=mock python3 scripts/run_daily.py --profile tau2 \
    --spec demo/airline-support-outcomes/compiled.json --capsulectl capsulectl \
    --judge-cmd "python3 scripts/judges/jev_judge.py" \
    --judge-model-id "jev-1.13.0 (mock backend, no model read the conversation)" \
    --date "$DAY" --out ./runs
```

For a real run (step 3's key set, no `JEV_JUDGE_BACKEND`), pass
`--judge-model-id jev-1.13.0`; the longer mock-only string is refused by the
real backend's pin check.

## 7. Roll up, seal the Result v0, checkpoint, disclose

```console
$ JEV_JUDGE_BACKEND=mock python3 scripts/rollup_day.py --profile tau2 \
    --spec demo/airline-support-outcomes/compiled.json --capsulectl capsulectl \
    --judge-cmd "python3 scripts/judges/jev_judge.py" \
    --judge-model-id "jev-1.13.0 (mock backend, no model read the conversation)" \
    --generated-at "${DAY}T23:59:59Z" --out ./runs/rollup
$ capsulectl result build --profile tau2 --result "./runs/rollup/result-v0-${DAY}.json" --out ./result-sealed.json | tee ./result-build.json
$ RESULT_CAPSULE=$(jq -r '.record_id' ./result-build.json)
$ capsulectl cll checkpoint create --profile tau2
$ capsulectl disclose --profile tau2 --root "$RESULT_CAPSULE" --closure-depth 2 \
    --attach-input-originals --out ./bundle.json
```

`rollup_day.py` needs the **same** `--judge-cmd` and `--judge-model-id`
`run_daily.py` used: it recomputes the judge pin (which covers the
instruction template the judge reports) and counts only the reports sealed
under it. `cll checkpoint create` runs after `result build` because step 6's
checkpoint closed the day before the Result v0 existed; skip it and
`disclose` shows that record uncheckpointed.

`--attach-input-originals` is opt-in: it carries each published capsule's
retained input original in the bundle (extension
`capsulectl/agent-input-originals/v1`), each checked against the capsule's
`agent_input_digest` first. The book itself stores no originals. Leave the
flag off and the bundle carries digests only. The card shows a conversation's
transcript only when the Result also cites that conversation's case record;
the Result v0 `rollup_day.py` writes cites the reports alone, so this page's
report does not show transcripts.

## 8. Build and open the report

```console
$ python3 -c "
import json
bundle = json.load(open('bundle.json'))
settings = json.load(open('demo/airline-support-outcomes/presentation.json'))['outcome-report/v1']
bundle.setdefault('extensions', {})['outcome-report/v1'] = {
    'enabled': True, 'percentages': settings['percentages'],
}
json.dump(bundle, open('bundle-with-report.json', 'w'))
"
$ capsulectl report build --profile tau2 --bundle bundle-with-report.json \
    --card outcome --permalink --out report.html
```

`report build` prints `"verification":"pass"` and `"unsupported_claims":0`.
**`report.html` lands at the `--out` path**: here `./report.html` inside your
`evidencebook-skills` checkout. Open it in any browser. It is self-contained
(the viewer runtime, including its WebCrypto verification, is embedded) and it
**verifies offline**, with no server and no network call. The printed
`permalink` carries the same bytes in its URL fragment.

The `outcome-report/v1` extension is a producer-side display setting, not
evidence: it selects the card and says whether to show counts or percentages.
Every term the card prints (pack, version, criteria and their wording) is read
from the sealed claims and reports, or marked "not stated".

## What a release must contain

| Repo | Needs | State today |
| --- | --- | --- |
| `capsule-cli` | PR #19 and PR #30 merged to `main`, then a `v*` tag cut after that merge (the next after `v0.1.0-rc2`). `release.yml` already builds linux/amd64, linux/arm64, darwin/arm64 and `SHA256SUMS` on any `v*` tag. | Both PRs open; tags are `v0.1.0-rc1`, `v0.1.0-rc2`. The vendored card is pinned to agent-action-capsule PR #160's head and must be re-pinned to the merged commit first. |
| `evidencebook-skills` | PR #7 merged, then a tag naming the pack/skill version to pin to. | PR #7 open; no tags; the only CI is the neutrality scan. |
| `agent-action-capsule` | PR #160 merged, so capsule-cli can vendor the card from `main` (`scripts/iife-sync.sh` takes a checkout of this repo at the merged commit). | PR #160 open; latest tag `v0.6.0` predates the card. |

## What this page does not cover

- **A real Jev run.** The commands above use the mock backend, whose verdicts
  are a deterministic hash of the request, so the mock report resolves no
  conversations. Swap in a key per step 3 for real verdicts.
- **Judge accuracy.** Against a 15-conversation expert-rated set, the judge
  (`jev-1.13.0`, this rubric) returned 6 false passes; see
  `evidencebook-skills/docs/jev-judge.md`. A judged `met` is the judge's
  reading, not ground truth.
- **Transcripts in the drill-down** need the Result to cite each case record
  (step 7).
