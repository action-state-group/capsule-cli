# deal reconcile

`deal reconcile` is the detective control behind the deal skill. Calling the
skill is advisory: an agent host with no hook before a tool call cannot make
the agent run the final review. So once a period, this pass reads the host's
own record of what was executed and lists every consequential action that
has no deal record. It seals nothing and changes no deal.

```sh
capsulectl --profile deal deal reconcile --executions executions.jsonl [--from TIME] [--to TIME] [--window 30m]
```

The default period is the 24 hours before `--to` (default: now). `--from` is
inclusive and `--to` exclusive, both RFC 3339.

## Read the execution layer, not the conversation

The payment is usually not an action of the agent the user talks to. That
agent starts a sub-task (a browser task, a delegated agent) and steers it; the
clicks, the form fills and the payment call are the sub-task's actions. A pass
that reads only the parent agent's visible actions sees the intent and the
approval, never the charge, and so misses every payment.

The input is therefore the host's **tool-call and execution records**: every
tool call that ran, by the agent and by every sub-task, with its result. It is
not the conversation transcript.

## Input: deal-execution-record/v0

A JSONL file: one JSON object per line, blank lines ignored, unknown fields
refused. A reader for the agent host writes it from the host's own tables.
capsulectl has no host-specific code; a reader for a new host is a small
script outside this repository that writes these lines.

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | The host's id for this tool call. |
| `at` | yes | When it ran, RFC 3339. |
| `task` | no | The task or sub-task that ran it. |
| `parent_task` | no | The task that started that sub-task. |
| `tool` | yes | The host's tool name, as the host records it. |
| `action` | yes | `pay`, `commit`, `sign`, `cancel`, `share_contact`, `share_credentials`, or `other`. |
| `amount_minor` | no | Integer minor units (cents). |
| `amount_kind` | no | `charged` (the default) or `ceiling`: the most a spend was approved for, when the host records only that. |
| `currency` | no | 3-letter code. |
| `merchant_domain` | no | The site the action was on. |
| `reference_sha256` | no | SHA-256 (64 lowercase hex) of the order or confirmation reference, never the reference itself. |
| `observed` | no | `action` (the default) or `approval`: the record is the host's approval of a spend for a task, not the payment itself, when the host records no payment action. |
| `status` | yes | `succeeded`, `failed` or `unknown` (the host cannot tell). |
| `deal_id` | no | The deal the agent said this belongs to, when the host recorded it. |

**Metadata and digests only.** A reader passes what a tool call *was*, never
what it carried: no form bodies, no card or payment fields, no verification
codes, no message text. There is no field for any of them, and unknown fields
refuse the file. `id`, `task`, `parent_task` and `tool` must look like
identifiers (letters, digits and `._:/+-`; no spaces and no `@`), and
`merchant_domain` like a host name. A value with a card-number-like run of
digits, or that contains `+` followed by a digit (phone-number shaped), is
refused, in execution and approval records alike.
A host's worker rows can hold filled form values (an email, a street
address, a phone number) in cleartext: a reader selects only ids, tool
names, timestamps, the merchant's domain and amounts, never a row's text.
The refusal names the line and the field, never the value. The pass reads
the file and writes nothing to the store.

`action` is a mechanical mapping the reader makes from the tool call alone:
what it does, never who the other side is. A payment call, a submit on a
checkout or booking form, a signature, a cancellation, or a form that sends
the user's contact details or credentials maps to the matching action;
everything else is `other`. A reader that is unsure maps to the consequential
action, not to `other`: a false entry costs one line in the list, a missed
one costs the record.

Example (a parent agent that starts a browser task, which pays):

```json
{"id":"c1","at":"2026-09-27T18:00:00Z","task":"t-main","tool":"spawn_task","action":"other","status":"succeeded"}
{"id":"c2","at":"2026-09-27T18:00:30Z","task":"t-browser","parent_task":"t-main","tool":"fill_form","action":"other","status":"succeeded"}
{"id":"c3","at":"2026-09-27T18:01:00Z","task":"t-browser","parent_task":"t-main","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","merchant_domain":"stickers.example","reference_sha256":"71c270b6a6356140ce4d74549522926ab8c7f3f5f2a65d0589f99364bb823ab0","status":"succeeded"}
```

## Optional input: the host's approvals

`--approvals FILE` adds the approval requests the agent host raised (its own
approval card, often raised by a sub-task's spend approval). Many hosts can
list pending approvals but keep no documented history of resolved ones, so
this input is optional. Format, deal-host-approval-record/v0, one JSON object
per line:

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | The host's id for the approval request (an identifier, as above). |
| `at` | yes | When it was raised, RFC 3339. |
| `task` | no | The task or sub-task that raised it. |
| `execution_id` | no | The execution record it approved, when the host links them. |
| `amount_minor`, `currency`, `merchant_domain` | no | As in an execution record. |
| `decision` | yes | `approved`, `denied` or `unknown`. |

An approval belongs to the execution record it names, or else to a
consequential record within `--window` after it, in the same task when both
name one, with the same amount, currency and merchant domain wherever both
carry one. Each consequential row then carries `host_approval` (or `null`
when none was found). Without `--approvals`, rows carry no `host_approval`,
`coverage.host_approvals.available` is `false`, and `cannot_see` says the
host's approval history was not available.

## Matching

A consequential record (any action except `other`) counts as recorded when
a sealed step of a deal in the profile accounts for it:

1. a `deal note --kind act` step, or else
2. a `deal check` step made before it,

with the same action, and the same amount, currency and merchant domain
wherever both sides carry one. A `ceiling` amount covers a step for that
amount or less. When the record names a `deal_id`, the step
must be in that deal. Otherwise an act must be within `--window` of the
record, and a check within `--window` before it. Each step accounts for one
action. Every deal is verified as `deal report` verifies it before its steps
are used.

A `failed` record is listed under `failed_attempts`, not as unrecorded: a
failed attempt can still have had an effect, so it is shown for review. An
`unknown` record is treated as if it ran.

## Output

One JSON object:

- `scope`: always "This pass covers the execution records it was given, for
  this period. It is not a record of everything the agent did."
- `summary`: one plain line, for example "0 of 1 consequential actions have
  no deal record (3 execution records read for ... to ...). This lists what
  is missing from the records read; it cannot prove that nothing else
  happened."
- `recorded`: each consequential record, with the `deal_id`, the `step` and
  `capsule_id` that account for it, and `matched_by` (`act` or `check`).
- `unrecorded`: each consequential record that no deal accounts for.
- `failed_attempts`, `records_read`, `outside_period`, `not_consequential`,
  `consequential`, `period`.
- `coverage`: what was read, and `cannot_see`, which is printed every time:
  - only the execution records in the file it was given: a session, side
    conversation or sub-task whose records were not exported is not seen;
  - anything the host summarised away or never wrote to its execution records
    is not seen;
  - a failed or ambiguous attempt that left no local trace is not seen;
  - which tool calls are consequential is decided by the reader; a call it
    mapped to `other` is not checked;
  - matching does not see the merchant's or the payment provider's records;
  - always, that authority is not established: the user's authority for each
    action cannot be read from the host's records, and host approval ids,
    where given, are the host's own references, not a record of who approved
    or under what authority (also `coverage.authority: "not established"`);
  - when no approval history was given, that whether the user approved
    each action on the host is not shown;
  - when any amount is a `ceiling`, that the charged amount is not shown;
  - when any record is `observed: approval`, that whether and when the
    payment itself was made is not shown;
  - it runs on the agent's own machine, over the host's own records: it can
    list actions that have no deal record, but it cannot prove that nothing
    else happened.

The exit code is 0 when nothing is unrecorded and 3 when something is, so a
scheduled run can tell the user.
