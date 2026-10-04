# backfill: an agent host's own records, imported after the fact

`capsulectl backfill` reads what an agent host already wrote to its own store
(its tool calls, sub-tasks and spend approvals) and commits it to the
profile's evidence book. The agent does not have to call anything. It is the
**effects leg** of a trail, and only that:

| The backfill shows | It does not show |
|---|---|
| that the host recorded an action, and the status the host gave it | the amount charged: an approved ceiling is the approval limit the host stored, not the charge |
| when the host says it happened (`source_asserted_at`), and when we imported it (`imported_at`) | whether the user approved it |
| which rows each source held past the last pass, in cursor order | anything the host did not write, or expired before a pass read it |

Every backfilled Capsule's payload says this in its `scope` block, and its
`recorded` line says what the host recorded in the host's own terms, for
example *"The host recorded: spend approval, on www.stickers.example, approved
ceiling 11.00 USD (a limit, not the charge), status closed."* Render it as
that. Anything that renders it as a receipt for a payment is a defect. For
authority, pair it with the conversation's own records; for the amount, with
the merchant's own signed email.

```sh
capsulectl --profile NAME backfill run --source rows.jsonl [--promote-around 30m] [--expect-every 5m]
capsulectl --profile NAME backfill status
```

The profile must be a `jsonl` profile with an initialized book (`store init`).

## The structural limit: detectability of absence, not prevention

This importer is run by the agent it audits. The agent cannot rewrite the
host's store, but it can fail to run the importer or narrow its query. What
this provides is **detectability of absence** (an append-only book, witnessed
checkpoints, and an import record naming the window and every gap), **not
prevention**. Every import record carries that sentence in its `limits` field.

What is tamper-evident is **the book, against ourselves and the agent**,
once a pass is checkpointed: cut and witness a checkpoint after each pass
(`capsulectl checkpoint create`, then the profile's witness). A checkpoint a
third party already holds is what stops the book's history being rewritten
before anyone looks.

**The host's store is not covered.** There is no baseline of it before the
first pass, it is read-only to the importer, and rows the host reaps are
gone. The backfill records what the store held when each pass read it; it
says nothing about the store at any other time.

## A watermark poll, by monotonic cursor

Hosts commonly offer no push: only a bounded query. So each pass is a
watermark poll. The reader selects, per source, the rows whose **cursor** is
past the highest cursor already digested (`backfill status` prints them as
`cursors`). The cursor is the source's monotonic id (a sequence or bigint
key), **never a timestamp**: a `created_at` watermark skips a row that commits
late with an older time, silently, because it is below the watermark.

A new row at or below a cursor an earlier pass already passed is digested
like any other and listed under `anomalies` (it arrived late, or an earlier
query missed it), and the pass exits 3. It is never skipped silently.

## Tiers: capture broadly, disclose narrowly

The host's rows are perishable, so what a pass does not commit may be gone by
the time it matters, and which rows matter is often known only in hindsight.
So a pass commits everything cheaply and retains only some of it:

- **Tier A, every new row, by digest.** The import record lists each row's
  `source`, `cursor`, `id`, `at` and digest. No other field is copied. This
  pins what each source held, in order.
- **Tier B, allow-listed fields of rows near a consequential signal.** A row
  the reader marked with a signal (`spend_request`, `handoff`,
  `payment_navigation`, `approval_ref`), and every row within
  `--promote-around` (default 30m) of one, becomes an ordinary Capsule in
  provenance mode `backfilled` (AAC -05, "Provenance mode and backfilled
  records") whose payload retains the allow-listed fields.
- **Tier C, raw rows, is never taken here.** Each row carries `raw_digest`,
  the reader's SHA-256 of the whole row as stored, so a raw copy kept where
  raw belongs (an open deal's own store, under its rules) can be checked
  against the Tier A commitment.

Disclosure is the existing bundle verbs, unchanged: `capsulectl bundle`,
`disclose` and `permalink` with `--root` a Tier B Capsule, `--payloads
all|selected` and `--suppress agent_input`. A Tier A row has no retained
original, so it can only ever be disclosed as withheld, which is why Tier B
exists.

## Retrospective promotion: hindsight may reveal, never rewrite

A pass may also be given rows it already digested: the reader re-reads a
stretch before the watermark so that a new signal can promote the rows just
before it, while the host still has them. A re-read row is promoted to Tier B
only when its digest equals the one Tier A committed for it
(`retrospective: true`, `pinned_by` naming the pass that digested it). A
re-read row whose digest differs (the host changed or replaced it after it
was digested) is **refused**: it is listed under `refused` with both digests
and the pass exits 3. A row's later state is never presented as the state
Tier A saw.

A host column that legitimately changes (a status that moves from pending to
closed) changes the digest, so a reader should select such a row's
consequential state once it has settled, or accept that the later state is
reported as refused against the earlier digest.

## Retention sets the cadence

A watermark poll heals across downtime **only while the source still holds
the rows**. Hosts expire transient rows, so **downtime longer than the host's
retention window is permanent loss**, and the rows that went are not there
to be listed. What a pass can do is notice: the reader reports each source's
**horizon** (the oldest cursor it still holds), and when the horizon has moved
past the last watermark, the cursors in between are declared as a gap:
expired before any pass read them, or ids the host never used (the two cannot
be told apart from here).

So the schedule is not a matter of taste: run passes at a period well inside
the measured retention window, every few minutes rather than seconds against
a bounded query interface. Until that window is measured, the schedule is a
guess, and the import record's `horizons` are the evidence to measure it from.

## Input: backfill-source-record/v0

A JSONL file (or stdin with `--source -`), one object per line, blank lines
ignored, unknown fields refused. A reader for a host writes it from the
host's own tables; capsulectl has no host-specific code. Any bad line refuses
the whole file, naming the line and the field, never the value. Nothing is
committed from a refused file, and the next pass starts from the same place.

`kind: "row"`:

| Field | Required | Meaning |
|---|---|---|
| `source` | yes | Which of the host's record streams this row is from (lowercase token). |
| `cursor` | yes | The row's monotonic id in that source (non-negative integer). Never a timestamp. |
| `id` | yes | The host's id for the row (identifier). |
| `at` | yes | When the host says it happened, RFC 3339. Becomes `source_asserted_at`. |
| `record_kind` | yes | `tool_call`, `subtask` or `spend_approval`. |
| `raw_digest` | yes | SHA-256 (64 lowercase hex) of the whole row as stored, every column. |
| `task`, `parent_task`, `tool_call_id` | no | The task that ran it, the task that started that one, the call that started it (identifiers). |
| `tool` | for `tool_call` | The host's tool name. |
| `merchant_domain` | no | The site, as a lowercase host name. |
| `line_items` | no | Up to 50 product names, 120 characters each. |
| `status` | no | The host's status for the row (lowercase token). |
| `approved_ceiling_minor`, `currency` | no, `spend_approval` only | The approval limit the host stored, in minor units, and its 3-letter code. Never the charge. |
| `signals` | no | Consequential signals the row carries: `spend_request`, `handoff`, `payment_navigation`, `approval_ref`. |

`kind: "horizon"`, one per source read: `source`, `cursor` (the oldest cursor
the source still holds), `at` (that row's time) and `limited` (true when the
reader's query hit its row limit, so more rows wait for the next pass).

`kind: "gap"`, for a source the reader could not read: `source` and `reason`
(a short plain sentence).

**Allow-list at read time.** A reader selects only ids, cursors, tool names,
times, merchant origin, line-item names, status and the ceiling, and never
message text or form values; there is no field for either. As a second line
of defence, an identifier shaped like an email address or card number, and a
line item or reason shaped like an email address, phone number or card
number, refuses the file.

## The pass and its import record

Each pass, whatever it found, commits one `backfill_import` record to the
book:

- `import_batch`, `imported_at`, `first_pass`, `previous_pass`;
- `window`: per source, `after` (the watermark before this pass) and
  `through` (the highest cursor digested now); `source_time_span`, the
  earliest and latest `at` among the new rows;
- `horizons`: per source, the oldest row still held when read;
- `tier_a`, `retained` (Tier B Capsules, with `pinned_by` and
  `retrospective`), `refused`, `anomalies`, `reread_verified`,
  `already_retained`, `rows_read`;
- `gaps`, always present: cursors the horizon passed before a pass read them,
  and every source the reader declared unread;
- `late`, when `--expect-every` is given and the pass ran more than twice that
  long after the previous one (the scheduler was gone);
- `notes` (for example a reader that hit its row limit), `scope`, `limits`
  and `cannot_see`, printed every time.

Re-running a pass over rows already read commits no second Capsule: the
importer reads the source digests from the Capsules in the book (not from
import records), so a pass interrupted before its import record is still
recognised.

The exit code is 0 for a clean pass, and 3 when the committed import record
declares a gap, a refused row or an anomaly, so a scheduler can tell the user.

## Every backfilled Capsule

An `fyi` Capsule (it disposes nothing, so it carries no approver) with
`action_id` `<source>/<id>`, `developer` `capsulectl-backfill`, and:

```json
"provenance_mode": {
  "mode": "backfilled",
  "source_ref": {"type": "x-external-ledger-entry", "digest_alg": "SHA-256", "digest": "<Tier A digest>"},
  "source_asserted_at": "<the host's at>",
  "imported_at": "<this pass>",
  "import_batch": "<this pass>",
  "time_rung": "self_attested"
}
```

The digest is SHA-256 over the JCS form of the payload's `host_record`, so
whoever holds the payload can recompute it. `time_rung` is always
`self_attested`: nothing independent corroborates the host's time, and a
`witnessed` claim without a reference citing `corroborates_source_time` is
refused when sealing and fails Class 1 verification as
`provenance_time_rung_overclaim`.

## Run it on a schedule, not as a daemon

An agent host may replace its machine while keeping the home directory.
Run each pass from cron (or the host's scheduler), with the period as
configuration, and re-establish the schedule from something that survives a
replacement (a login hook in the home directory). That a schedule survives a
replacement is an assumption to check on each host; when it does not, the
missed passes show as a `late` pass and, past retention, as horizon gaps,
rather than as silence. An importer with silent gaps is an alibi that happens
to be empty.
